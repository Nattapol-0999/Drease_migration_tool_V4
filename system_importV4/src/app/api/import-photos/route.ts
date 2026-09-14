import "server-only";

import { execFile } from "node:child_process";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";

import { describeTrim, trimMongoDump } from "@/lib/mongo-trim";
import { requireSession } from "@/lib/require-session";
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
import { sessionExists, stagedPath, stagedSize } from "@/lib/upload-staging";
import { TARGET_IDS, type TargetId } from "@/lib/types";
import { describeTarget, hint, psqlCommand, resolveTarget, runSSH, sshArgs } from "@/lib/server-target";

const run = promisify(execFile);

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// Importing the legacy patient page images.
//
// TWO MODES, ONE REQUEST SHAPE. `survey` does everything except write: it runs the CLI to
// get the copy plan, unpacks the archive, and reports what lines up. `apply` repeats that
// and then writes. They are the same route because the survey must describe the exact
// plan the apply will execute — two routes could drift, and a preview that does not match
// what happens is worse than no preview.
//
// TWO WAYS THE FILES ARRIVE, and why the big one exists.
//
//   sessionId — the files are already on disk, put there chunk by chunk by
//               /api/upload-chunk (fields `mongo` and `images`). Nothing large travels in
//               this request, so the only ceiling is staging disk (MAX_UPLOAD_BYTES).
//   attached  — the files ride in this request body. Kept for a small clinic and for any
//               caller that has not moved over, and still capped at MAX_TOTAL_BYTES.
//
// The second path is what the first clinic used and it worked because that export was
// small: mris.js 28 MB + public_img.tar.gz 25.6 MB = 53.7 MB, just under the cap. The next
// tenant was not: 437 photos came to 520 MB of images, and no proxy in front of this app
// will carry that in one body — nginx stops at 64 MB, Cloudflare's free plan at 100 MB.
// Chunked upload already solved this for the SQL and Mongo dumps (a 233 MB Mongo export
// goes up routinely); this route was the last one still demanding one request.
//
// WRITE ORDER: bytes to the backend host first, rows to the database second. A file with
// no row is invisible; a row with no file is a broken image inside a medical record.

// 64 MB matches nginx's client_max_body_size on the tool host and Next's own
// bodySizeLimit. It bounds the ATTACHED path only — a staged upload never puts the file
// in a request body, so the number does not apply to it.
const MAX_TOTAL_BYTES = 64 * 1024 * 1024;

const CLINIC_RE = /^[A-Za-z0-9_-]{1,64}$/;

function fail(error: string, status = 400, extra: Record<string, unknown> = {}): Response {
  return Response.json({ ok: false, error, ...extra }, { status });
}

export async function POST(req: Request): Promise<Response> {
  // Checked here as well as in the middleware: this route spawns processes and, in apply
  // mode, writes to two production hosts.
  const auth = await requireSession(req);
  if ("denied" in auth) return auth.denied;

  const cli = process.env.IMPORT_CLI_PATH;
  if (!cli) {
    return fail("ยังไม่ได้ตั้งค่า IMPORT_CLI_PATH — ชี้ไปที่ไฟล์ system_cliv4", 500);
  }

  const form = await req.formData();
  const mode = String(form.get("mode") ?? "survey");
  if (mode !== "survey" && mode !== "apply") {
    return fail(`mode ต้องเป็น survey หรือ apply (ได้ "${mode}")`);
  }

  const id = String(form.get("targetId") ?? "");
  if (!(TARGET_IDS as readonly string[]).includes(id)) {
    return fail(`ปลายทาง "${id}" ไม่อยู่ในรายการที่ตั้งค่าไว้`);
  }
  const targetId = id as TargetId;

  const clinic = String(form.get("clinicCode") ?? "").trim();
  if (!CLINIC_RE.test(clinic)) {
    return fail("clinic_code ไม่ถูกรูป (ใช้ A-Z a-z 0-9 _ - ยาวไม่เกิน 64)");
  }

  // Where the rows go (database host) and where the bytes go (backend host) are two
  // different machines, and BOTH must be configured before anything is unpacked.
  const dbTarget = resolveTarget(targetId);
  if (!dbTarget) {
    return fail(`ปลายทางฐานข้อมูลของ ${targetId} ยังตั้งค่าไม่ครบ`, 503);
  }
  const { target: uploads, missing } = readUploadsTarget(targetId);
  if (!uploads) {
    return fail(
      `ยังตั้งค่าที่เก็บไฟล์รูปของ ${targetId} ไม่ครบ — ขาด ${missing.join(", ")}`,
      503,
      { missing },
    );
  }

  // Staged first: when a session id is supplied the bytes are already on disk and this
  // request carries only the form fields.
  const sessionId = String(form.get("sessionId") ?? "").trim();
  const pageFile = form.get("page");
  const imagesFile = form.get("images");

  let stagedPageBytes = 0;
  let stagedImageBytes = 0;
  if (sessionId) {
    if (!(await sessionExists(sessionId))) {
      return fail(
        "ไม่พบไฟล์ที่อัปโหลดไว้ (session หมดอายุหรือถูกล้าง) — แนบไฟล์ใหม่",
        409,
      );
    }
    stagedPageBytes = await stagedSize(sessionId, "mongo");
    stagedImageBytes = await stagedSize(sessionId, "images");
    if (stagedPageBytes === 0) {
      return fail("ในการอัปโหลดนี้ไม่มีไฟล์ MongoDB (.js) — ต้องมีเพื่ออ่าน collection page");
    }
    if (stagedImageBytes === 0) {
      return fail("ในการอัปโหลดนี้ไม่มีไฟล์รูป (public_img.tar.gz)");
    }
  } else {
    if (!(pageFile instanceof File) || pageFile.size === 0) {
      return fail("ยังไม่ได้แนบไฟล์ export ของ mris.page (.js)");
    }
    if (!(imagesFile instanceof File) || imagesFile.size === 0) {
      return fail("ยังไม่ได้แนบไฟล์รูป (public_img.tar.gz)");
    }
    const total = pageFile.size + imagesFile.size;
    if (total > MAX_TOTAL_BYTES) {
      return fail(
        `ไฟล์รวมกัน ${(total / 1024 / 1024).toFixed(1)} MB ใหญ่เกิน ${MAX_TOTAL_BYTES / 1024 / 1024} MB ` +
          `— ไฟล์ขนาดนี้ต้องอัปโหลดแบบแบ่งชิ้นก่อน แล้วส่ง sessionId มาแทนการแนบไฟล์`,
      );
    }
  }

  // apply retypes the clinic_code, the same gate the SQL import uses. It is the last
  // thing standing between "wrong tenant selected ten minutes ago" and rows in a real
  // patient record.
  if (mode === "apply") {
    const confirm = String(form.get("confirm") ?? "").trim();
    if (confirm !== clinic) {
      return fail("ยืนยันไม่ผ่าน: พิมพ์ clinic_code ให้ตรงก่อนนำเข้า");
    }
  }

  const work = await mkdtemp(join(tmpdir(), "importv4-page-"));
  let extractRoot = "";
  let trimLog = "";
  try {
    // 1 · the copy plan comes from the CLI, which owns every mapping decision.
    //
    // The dump is FILTERED to the one collection this route reads before the CLI sees it,
    // the same way /api/generate filters before its own run. It matters more here than it
    // looks: the CLI reads its input with os.ReadFile and keeps every parsed document, and
    // a legacy mris export is mostly audit logs. Measured on the agafar export, `page` is
    // 0.4 MB of a 233 MB file — the rest is bills (57 MB), payment_summary (44 MB) and
    // userlogsv2 (43 MB), none of which page.go looks at. Handing over the whole thing on
    // a 908 MB box means a gigabyte of swap for nothing.
    const pagePath = join(work, "page.js");
    const rawPagePath = sessionId ? stagedPath(sessionId, "mongo") : join(work, "page-raw.js");
    if (!sessionId) {
      await writeFile(rawPagePath, Buffer.from(await (pageFile as File).arrayBuffer()));
    }
    try {
      const trimmed = await trimMongoDump(rawPagePath, pagePath, ["page"]);
      trimLog = describeTrim(trimmed);
      if (trimmed.keptBytes === 0) {
        return fail(
          "ในไฟล์ MongoDB ที่ส่งมาไม่มี collection `page` เลย — รูปหน้าเวชระเบียนอยู่ในฐาน mris " +
            "ต้อง export collection page มาด้วย",
          422,
        );
      }
    } catch (err) {
      return fail(`กรอง collection page จากไฟล์ MongoDB ไม่สำเร็จ: ${(err as Error).message}`, 422);
    }

    let report: string;
    try {
      const { stdout } = await run(cli, ["-clinic", clinic, "-page", pagePath, "-out", work], {
        maxBuffer: 32 * 1024 * 1024,
        windowsHide: true,
      });
      report = stdout;
    } catch (err) {
      const e = err as { stderr?: string; message?: string };
      return fail((e.stderr || e.message || "").trim() || "รัน CLI ไม่สำเร็จ", 422);
    }

    const manifest = await readManifest(join(work, `photos_${clinic}.json`), clinic);

    // 2 · unpack the operator's archive and weigh every planned file.
    //
    // A staged archive is read where it already is. Copying it first would double the disk
    // a 530 MB upload costs and buy nothing: extractArchive only reads it.
    let tarPath: string;
    if (sessionId) {
      tarPath = stagedPath(sessionId, "images");
    } else {
      tarPath = join(work, "images.tar.gz");
      await writeFile(tarPath, Buffer.from(await (imagesFile as File).arrayBuffer()));
    }
    extractRoot = await extractArchive(tarPath);
    const survey = await surveyArchive(manifest, extractRoot);

    const { sql, rows } = buildInsertSQL(clinic, manifest, survey);

    const base = {
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
      // turn away (3 MB / JPG-PNG only). Capped in the response because the list is for
      // recognising a pattern, not for reading end to end.
      wouldReject: survey.wouldReject.slice(0, 50),
      wouldRejectCount: survey.wouldReject.length,
      oversizeCount: survey.oversizeCount,
      unsupportedCount: survey.unsupportedCount,
      extMismatchCount: survey.extMismatch.length,
      // V2's own thumbnails, carried across rather than rendered.
      thumbFound: survey.thumbFound,
      thumbBytes: survey.thumbBytes,
      rows,
      report,
      // What filtering the dump saved, and how the files got here. Both belong in the
      // preview: an operator comparing two runs needs to know which path each took.
      trim: trimLog,
      source: sessionId ? "staged" : "attached",
      sourceBytes: sessionId
        ? { page: stagedPageBytes, images: stagedImageBytes }
        : { page: (pageFile as File).size, images: (imagesFile as File).size },
    };

    if (mode === "survey") {
      return Response.json({ ok: true, mode, ...base, sql });
    }

    if (survey.matched.length === 0) {
      return fail(
        "ไม่มีรูปที่จับคู่กับไฟล์ในถุงได้เลย — ไม่มีอะไรจะนำเข้า (ตรวจว่า tar ถูก export จาก public/ และมีโฟลเดอร์ nas-doctorease/)",
        422,
        base,
      );
    }

    // 3 · bytes first. A row pointing at a file that never arrived is a broken image in
    // somebody's record, so nothing is inserted until the copy has succeeded.
    const started = Date.now();
    let ship;
    try {
      ship = await shipFiles(uploads, manifest, survey, extractRoot);
    } catch (err) {
      const e = err as { stderr?: string; message?: string };
      return fail(
        `คัดลอกไฟล์รูปไปที่ ${describeUploads(uploads)} ไม่สำเร็จ — ยังไม่ได้เขียนแถวใดลงฐานข้อมูล: ${(e.stderr || e.message || "").trim()}`,
        422,
        base,
      );
    }

    // 4 · then the rows, in one transaction on the database host.
    const r = await runSSH(sshArgs(dbTarget, psqlCommand(dbTarget)), sql);
    const seconds = ((Date.now() - started) / 1000).toFixed(1);
    const log = [trimLog, ship.log, r.stdout, r.stderr].filter(Boolean).join("\n").trim();

    // Recorded whether it worked or not: without this there is no answer to "who moved
    // which clinic's images, where, and when".
    console.log(
      JSON.stringify({
        event: "import-photos",
        at: new Date().toISOString(),
        user: auth.session.user,
        target: targetId,
        clinic,
        uploads_host: uploads.sshHost,
        database: dbTarget.dbName,
        // Which path the bytes took, so a failure can be traced without guessing.
        source: sessionId ? "staged" : "attached",
        session: sessionId || undefined,
        planned: manifest.files.length,
        files_sent: ship.filesSent,
        bytes_sent: ship.bytesSent,
        rows,
        exit: r.code,
        seconds,
      }),
    );

    if (r.code !== 0) {
      return Response.json(
        {
          ok: false,
          mode,
          ...base,
          error:
            hint(r) ||
            `เขียนแถวไม่สำเร็จ (exit ${r.code}) — ฐานข้อมูลไม่ถูกเปลี่ยนเพราะอยู่ในทรานแซกชันเดียว แต่ไฟล์รูป ${ship.filesSent} ไฟล์ถูกคัดลอกขึ้นเครื่องปลายทางแล้ว (ไม่มีแถวอ้างถึง จึงไม่มีใครเห็น)`,
          filesSent: ship.filesSent,
          bytesSent: ship.bytesSent,
          seconds,
          log,
        },
        { status: 422 },
      );
    }

    return Response.json({
      ok: true,
      mode,
      ...base,
      filesSent: ship.filesSent,
      thumbsSent: ship.thumbsSent,
      bytesSent: ship.bytesSent,
      seconds,
      log: log || "psql ไม่ได้ส่งข้อความใด (สำเร็จ)",
    });
  } catch (err) {
    return fail((err as Error).message || "นำเข้ารูปไม่สำเร็จ", 422);
  } finally {
    await cleanup(extractRoot);
    await rm(work, { recursive: true, force: true });
  }
}
