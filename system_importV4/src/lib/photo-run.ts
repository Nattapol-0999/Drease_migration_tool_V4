import "server-only";
import { executePhotoImport as executeLegacyPhotoImport, PhotoRunError as LegacyPhotoRunError } from "@/lib/photo-run-legacy";
import { usesPhotoManifestV2 } from "@/lib/photo-rollout";

import { execFile } from "node:child_process";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";

import { shipExtras } from "@/lib/extras-import";
import { describeTrim, trimMongoDump } from "@/lib/mongo-trim";
import {
  buildInsertSQL,
  cleanup,
  describeUploads,
  extractArchive,
  readManifest,
  readUploadsTarget,
  shipFiles,
  surveyArchive,
} from "@/lib/photo-import";
import type { Survey } from "@/lib/photo-import";
import { describeTarget, psqlCommand, resolveTarget, runSSH, sshArgs } from "@/lib/server-target";
import { sessionExists, stagedPath, stagedSize } from "@/lib/upload-staging";
import type { ExtrasResult, TargetId } from "@/lib/types";

const run = promisify(execFile);

/**
 * The photo import, as one callable step.
 *
 * WHY THIS IS NOT INSIDE THE ROUTE
 * Two callers need the identical behaviour and must not drift apart:
 *
 *   /api/import-photos     the standalone survey/apply pair, still there because a failed
 *                          photo copy has to be retryable without re-running the SQL
 *   /api/import-to-server  the one button an operator presses, which runs the SQL and then
 *                          the photos in the same job
 *
 * A second copy of this logic in the second caller is how the two would end up filing
 * images differently, so it lives here and both call it.
 *
 * ERRORS THROW, they are not returned. The route turns a PhotoRunError into its HTTP
 * response; the job-based caller lets it fail the job. Either way the message is the one
 * the operator sees, so it says what happened and what to do — never a bare status code.
 */

export interface PhotoRunOptions {
  /** Explicit site-specific doctype mapping; never infer a type from the image bytes. */
  pageTypeMap?: Record<string, 'before_after' | 'form' | 'drawing'>;
  /** Path to system_cliv4. */
  cli: string;
  clinic: string;
  targetId: TargetId;
  /** Staging session holding fields `mongo` and `images`. */
  sessionId: string;
  /** survey plans and reports; apply also ships bytes and writes rows. */
  mode: "survey" | "apply";
  /**
   * What the CLI hashes with the revision id to NAME each imported image, when it must
   * not be the clinic code. Empty is the normal case and means the clinic code.
   *
   * It exists because `photos` has no migrate_source_ref column — the owner declined one
   * — so the file name is the only thing that tells "already imported" from "import
   * again" by identity. A clinic whose CODE changed between two imports therefore has
   * rows named under the OLD code, and a top-up that re-derives names from the current
   * one matches none of them.
   *
   * Measured on the-gloss (C2EC1E3) on 20 Oct 2026: BR00001's 14,385 rows are named under
   * CD1B8AF and BR00002's 10,734 under C2EC1E3, the two branches having been imported
   * either side of the rename. Left unset for BR00001, a top-up gives all 14,526 of its
   * pages fresh names.
   *
   * Only the name moves: the bytes still land under the clinic's own uploads dir and
   * photos.clinic_code is still the clinic.
   */
  photoNameSalt?: string;
  /** Recorded in the audit line, so "who did this" survives in the log. */
  user: string;
  /** Optional progress sink — the job-based caller shows these to the operator. */
  onPhase?: (phase: string) => void;
}

export interface PhotoRunResult {
  mode: "survey" | "apply";
  target: string;
  uploads: string;
  isProduction: boolean;
  planned: number;
  matched: number;
  // The shapes come straight from Survey rather than being restated, so a change there
  // cannot silently narrow what this reports.
  missing: Survey["missing"];
  extraCount: number;
  extraSample: string[];
  totalBytes: number;
  wouldReject: Survey["wouldReject"];
  wouldRejectCount: number;
  oversizeCount: number;
  unsupportedCount: number;
  extMismatchCount: number;
  thumbFound: number;
  thumbBytes: number;
  rows: number;
  report: string;
  trim: string;
  /**
   * The value the file names were actually derived from — the clinic code unless the
   * operator overrode it. Reported rather than assumed: this is the field that decides
   * whether a top-up recognises the rows already at the target.
   */
  photoNameSalt: string;
  /** True when the salt is not the clinic code, so the UI can say so loudly. */
  photoNameSaltOverridden: boolean;
  source: "staged";
  sourceBytes: { page: number; images: number };
  /** survey only: the INSERT statements that apply would run. */
  sql?: string;
  /** apply only. */
  filesSent?: number;
  thumbsSent?: number;
  bytesSent?: number;
  seconds?: string;
  log?: string;
  /**
   * apply only: the clinic logo and profile photos, when the session had them staged.
   * null means there was nothing to do, which is the normal tab-1 case.
   */
  extras?: ExtrasResult | null;
  /**
   * apply only: why the extras step failed, when it did. Separate from the run's own failure
   * because these are decorative — the medical records were already written successfully by
   * the time this ran, and the import must not be reported as failed over a logo.
   */
  extrasError?: string | null;
}

export class PhotoRunError extends Error {
  readonly status: number;
  /** Whatever was already known when it failed, so a caller can still show the counts. */
  readonly partial?: Partial<PhotoRunResult>;

  constructor(message: string, status = 422, partial?: Partial<PhotoRunResult>) {
    super(message);
    this.name = "PhotoRunError";
    this.status = status;
    this.partial = partial;
  }
}

export async function executePhotoImport(opts: PhotoRunOptions): Promise<PhotoRunResult> {
  // Roll out to v4demo first. Production retains its deployed CLI and original importer.
  if (!usesPhotoManifestV2(opts.targetId, process.env.PHOTO_IMPORT_V2_TARGETS)) {
    try { return await executeLegacyPhotoImport(opts); }
    catch (err) {
      if (err instanceof LegacyPhotoRunError) throw new PhotoRunError(err.message, err.status, err.partial);
      throw err;
    }
  }
  opts = { ...opts, cli: process.env.IMPORT_PHOTO_CLI_PATH?.trim() || opts.cli };
  const { cli, clinic, targetId, sessionId, mode, user, onPhase } = opts;
  const phase = (s: string) => onPhase?.(s);
  const photoNameSalt = (opts.photoNameSalt ?? "").trim();

  // Where the rows go (database host) and where the bytes go (backend host) are two
  // different machines, and BOTH must be configured before anything is unpacked.
  const dbTarget = resolveTarget(targetId);
  if (!dbTarget) {
    throw new PhotoRunError(`ปลายทางฐานข้อมูลของ ${targetId} ยังตั้งค่าไม่ครบ`, 503);
  }
  const { target: uploads, missing } = readUploadsTarget(targetId);
  if (!uploads) {
    throw new PhotoRunError(
      `ยังตั้งค่าที่เก็บไฟล์รูปของ ${targetId} ไม่ครบ — ขาด ${missing.join(", ")}`,
      503,
    );
  }

  if (!(await sessionExists(sessionId))) {
    throw new PhotoRunError(
      "ไม่พบไฟล์ที่อัปโหลดไว้ (session หมดอายุหรือถูกล้าง) — แนบไฟล์ใหม่",
      409,
    );
  }
  const stagedPageBytes = await stagedSize(sessionId, "mongo");
  const stagedImageBytes = await stagedSize(sessionId, "images");
  if (stagedPageBytes === 0) {
    throw new PhotoRunError(
      "ในการอัปโหลดนี้ไม่มีไฟล์ MongoDB (.js) — ต้องมีเพื่ออ่าน collection page",
      400,
    );
  }
  if (stagedImageBytes === 0) {
    throw new PhotoRunError("ในการอัปโหลดนี้ไม่มีไฟล์รูป (public_img.tar.gz)", 400);
  }

  // Real disk, not /tmp: on the deployed box /tmp is a 455 MB tmpfs and the unpacked
  // archive is 538 MB. See workRoot() in lib/photo-import for the failure that taught us.
  const work = await mkdtemp(
    join(process.env.UPLOAD_STAGING_DIR?.trim() || tmpdir(), "importv4-page-"),
  );
  let extractRoot = "";
  let trimLog = "";
  try {
    // 1 · the copy plan comes from the CLI, which owns every mapping decision.
    //
    // The dump is filtered to the one collection this step reads before the CLI sees it.
    // The CLI reads its input with os.ReadFile and keeps every parsed document, and a
    // legacy mris export is mostly audit logs: measured, `page` is 0.4 MB of a 233 MB file
    // while bills (57 MB), payment_summary (44 MB) and userlogsv2 (43 MB) are never read.
    phase("กรอง collection page ออกจากไฟล์ MongoDB");
    const pagePath = join(work, "page.js");
    try {
      const trimmed = await trimMongoDump(stagedPath(sessionId, "mongo"), pagePath, ["page"]);
      trimLog = describeTrim(trimmed);
      if (trimmed.keptBytes === 0) {
        throw new PhotoRunError(
          "ในไฟล์ MongoDB ที่ส่งมาไม่มี collection `page` เลย — รูปหน้าเวชระเบียนอยู่ในฐาน mris " +
            "ต้อง export collection page มาด้วย",
        );
      }
    } catch (err) {
      if (err instanceof PhotoRunError) throw err;
      throw new PhotoRunError(
        `กรอง collection page จากไฟล์ MongoDB ไม่สำเร็จ: ${(err as Error).message}`,
      );
    }

    phase(
      photoNameSalt
        ? `อ่านแผนคัดลอกรูปจาก CLI (salt ชื่อไฟล์ ${photoNameSalt})`
        : "อ่านแผนคัดลอกรูปจาก CLI",
    );
    let report: string;
    try {
      // The salt is passed ONLY when the operator asked for one, so the command line of
      // an ordinary run is byte-identical to every run before this option existed.
      const cliArgs = ["-clinic", clinic, "-page", pagePath, "-out", work];
      if (photoNameSalt) cliArgs.push("-photo-name-salt", photoNameSalt);
      if (opts.pageTypeMap) cliArgs.push('-page-type-map', JSON.stringify(opts.pageTypeMap));
      const { stdout } = await run(cli, cliArgs, {
        maxBuffer: 32 * 1024 * 1024,
        windowsHide: true,
      });
      report = stdout;
    } catch (err) {
      const e = err as { stderr?: string; message?: string };
      throw new PhotoRunError((e.stderr || e.message || "").trim() || "รัน CLI ไม่สำเร็จ");
    }

    const manifest = await readManifest(join(work, `photos_${clinic}.json`), clinic);

    // 2 · unpack the archive where it already lies and weigh every planned file. Copying it
    // first would double the disk a 530 MB upload costs; extractArchive only reads it.
    phase(`แตกไฟล์รูป ${(stagedImageBytes / 1048576).toFixed(1)} MB`);
    extractRoot = await extractArchive(stagedPath(sessionId, "images"));
    const survey = await surveyArchive(manifest, extractRoot);

    const { sql, rows } = buildInsertSQL(clinic, manifest, survey);

    const base: PhotoRunResult = {
      mode,
      target: describeTarget(dbTarget),
      uploads: describeUploads(uploads),
      isProduction: dbTarget.isProduction,
      planned: manifest.files.length,
      matched: survey.matched.length,
      missing: survey.missing,
      extraCount: survey.extraCount,
      extraSample: survey.extraSample,
      totalBytes: survey.totalBytes,
      // Imported anyway, but reported: these are the images today's upload endpoint would
      // turn away (3 MB / JPG-PNG only). Capped because the list is for recognising a
      // pattern, not for reading end to end.
      wouldReject: survey.wouldReject.slice(0, 50),
      wouldRejectCount: survey.wouldReject.length,
      oversizeCount: survey.oversizeCount,
      unsupportedCount: survey.unsupportedCount,
      extMismatchCount: survey.extMismatch.length,
      thumbFound: survey.thumbFound,
      thumbBytes: survey.thumbBytes,
      rows,
      report,
      trim: trimLog,
      source: "staged",
      sourceBytes: { page: stagedPageBytes, images: stagedImageBytes },
      // Echoed back so the operator can see on screen which naming rule the run used.
      // A salt that silently differs from the clinic code is how a top-up writes a second
      // copy of every scan the clinic already has.
      photoNameSalt: photoNameSalt || clinic,
      photoNameSaltOverridden: photoNameSalt !== "" && photoNameSalt !== clinic,
    };

    if (mode === "survey") {
      return { ...base, sql };
    }

    // Nothing matched means the archive is not the one this dump describes — the wrong
    // tenant's images, or a tar made from the wrong directory. Refusing here is the last
    // point at which that costs nothing.
    if (survey.matched.length === 0) {
      throw new PhotoRunError(
        "ไม่มีรูปที่จับคู่กับไฟล์ในถุงได้เลย — ไม่มีอะไรจะนำเข้า (ตรวจว่า tar ถูก export จาก public/ และมีโฟลเดอร์ nas-doctorease/)",
        422,
        base,
      );
    }

    // 3 · bytes first. A row pointing at a file that never arrived is a broken image in
    // somebody's record, so nothing is inserted until the copy has succeeded.
    phase(`คัดลอก ${survey.matched.length} ไฟล์ (${(survey.totalBytes / 1048576).toFixed(1)} MB) ไปเครื่องปลายทาง`);
    const started = Date.now();
    let ship;
    try {
      ship = await shipFiles(uploads, manifest, survey, extractRoot);
    } catch (err) {
      const e = err as { stderr?: string; message?: string };
      throw new PhotoRunError(
        `คัดลอกไฟล์รูปไปที่ ${describeUploads(uploads)} ไม่สำเร็จ — ยังไม่ได้เขียนแถวใดลงฐานข้อมูล: ${(e.stderr || e.message || "").trim()}`,
        422,
        base,
      );
    }

    // 4 · Then the rows, in one transaction. `rows` is the number of planned photos;
    // the script also matches visits by examination day and creates form/drawing pages.
    phase(`เขียน ${rows} รูป → Visit ตามวันตรวจ + Before–After/ฟอร์ม/วาดเขียน`);
    const r = await runSSH(sshArgs(dbTarget, psqlCommand(dbTarget)), sql);
    const seconds = ((Date.now() - started) / 1000).toFixed(1);
    const log = [trimLog, ship.log, r.stdout, r.stderr].filter(Boolean).join("\n").trim();

    // Recorded whether it worked or not: without this there is no answer to "who moved
    // which clinic's images, where, and when".
    console.log(
      JSON.stringify({
        event: "import-photos",
        at: new Date().toISOString(),
        user,
        target: targetId,
        clinic,
        uploads_host: uploads.sshHost,
        database: dbTarget.dbName,
        source: "staged",
        session: sessionId,
        planned: manifest.files.length,
        files_sent: ship.filesSent,
        bytes_sent: ship.bytesSent,
        rows,
        exit: r.code,
        seconds,
      }),
    );

    if (r.code !== 0) {
      throw new PhotoRunError(
        `เขียนแถวไม่สำเร็จ (exit ${r.code}) — ฐานข้อมูลไม่ถูกเปลี่ยนเพราะอยู่ในทรานแซกชันเดียว ` +
          `ตรวจและส่งไฟล์รูป ${ship.filesSent} ไฟล์แล้ว — ไฟล์เดิมไม่ถูกเขียนทับ ส่วนไฟล์ใหม่อาจค้างโดยยังไม่มีแถวอ้างถึง`,
        422,
        { ...base, filesSent: ship.filesSent, bytesSent: ship.bytesSent, seconds, log },
      );
    }

    // 5 · last, and only once the records are safely written: the clinic logo and the patient
    // profile photos. Bytes only, no rows.
    //
    // AFTER the medical records, and non-fatal.
    //
    // These are a convenience — V4 has no feature that reads either one yet, so a clinic whose
    // logo failed to copy is still a fully imported clinic. Running them first, or letting them
    // throw, would mean a decorative file could cost an import that had already succeeded. So
    // the failure is carried back as a message, and the caller shows it as a warning.
    //
    // They are absent entirely on the tab-1 (import from files) path, which stages neither.
    let extras: ExtrasResult | null = null;
    let extrasError: string | null = null;
    try {
      const logoBytes = await stagedSize(sessionId, "logo");
      const profileBytes = await stagedSize(sessionId, "profiles");
      if (logoBytes > 0 || profileBytes > 0) {
        phase("คัดลอกโลโก้คลินิกและรูปโปรไฟล์คนไข้");
        extras = await shipExtras({
          uploads,
          dbTarget,
          clinic,
          logoTar: logoBytes > 0 ? stagedPath(sessionId, "logo") : null,
          profilesTar: profileBytes > 0 ? stagedPath(sessionId, "profiles") : null,
        });
        console.log(
          JSON.stringify({
            event: "import-extras",
            at: new Date().toISOString(),
            user,
            target: targetId,
            clinic,
            uploads_host: uploads.sshHost,
            session: sessionId,
            logo_found: extras.logo?.found ?? null,
            profiles_candidates: extras.profiles?.candidates ?? null,
            profiles_sent: extras.profiles?.sent ?? null,
            profiles_skipped_no_patient: extras.profiles?.skippedNoPatient ?? null,
            files_sent: extras.filesSent,
            bytes_sent: extras.bytesSent,
          }),
        );
      }
    } catch (e) {
      extrasError = e instanceof Error ? e.message : String(e);
      console.log(
        JSON.stringify({
          event: "import-extras-failed",
          at: new Date().toISOString(),
          user,
          clinic,
          session: sessionId,
          error: extrasError,
        }),
      );
    }

    return {
      ...base,
      filesSent: ship.filesSent,
      thumbsSent: ship.thumbsSent,
      bytesSent: ship.bytesSent,
      seconds,
      log: [log, extras?.log, extrasError ? `โลโก้/รูปโปรไฟล์: ${extrasError}` : ""]
        .filter(Boolean)
        .join("\n") || "psql ไม่ได้ส่งข้อความใด (สำเร็จ)",
      extras,
      extrasError,
    };
  } finally {
    await cleanup(extractRoot);
    await rm(work, { recursive: true, force: true });
  }
}
