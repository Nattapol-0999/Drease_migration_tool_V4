import "server-only";

// Moving the legacy patient page images into V4.
//
// The images and the rows that point at them live on DIFFERENT machines: the bytes go to
// the backend host's uploads dir, the rows go to the database host. So this is not one
// write but two, and the order they happen in is a decision:
//
//   bytes first, then rows.
//
// A byte with no row is an unreferenced file nobody sees. A row with no byte is a broken
// image in a patient's record. Only one of those is recoverable by doing nothing, so the
// copy goes first and the INSERT second.
//
// WHAT THIS FILE DOES NOT DECIDE
//
// Nothing about the mapping. Which revision wins, which HN owns an image, what
// area_tag/doctor_code/taken_at become and where the bytes must land are all settled by
// system_CLIV4 (internal/legacyimport/page.go) and arrive here as a manifest. This file
// adds exactly one value the CLI could not know — file_size, read off the file it is
// about to copy — and then does as it is told. Two implementations of the same mapping
// would drift, and the one that drifts silently is the one writing medical records.

import { execFile } from "node:child_process";
import { mkdtemp, mkdir, open, readFile, rm, stat, copyFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";

/**
 * workRoot is where this module unpacks and re-bundles image bytes.
 *
 * NOT os.tmpdir(). On the deployed box /tmp is a 455 MB tmpfs — it spends RAM on a 908 MB
 * instance, and it is far too small for the job: a real clinic's archive is 530 MB
 * compressed and 538 MB unpacked, so `tar -xzf` into /tmp dies partway with "Cannot write:
 * Disk quota exceeded" and the step fails with no folder ever created on the backend host.
 * That is exactly how the photo import looked broken while the upload side was already
 * fixed.
 *
 * UPLOAD_STAGING_DIR is reused because it is already required to point at real disk (see
 * upload-staging.ts) and is already sized for these files — the archive itself is sitting
 * in it. os.tmpdir() stays as the fallback so a dev box with no env set behaves as before.
 */
function workRoot(): string {
  return process.env.UPLOAD_STAGING_DIR?.trim() || tmpdir();
}
import { promisify } from "node:util";

import { TARGET_IDS, type TargetId } from "@/lib/types";
import { shellQuote } from "@/lib/server-target";

const run = promisify(execFile);

/** One image, as the CLI planned it. Mirrors legacyimport.PhotoFile. */
export interface ManifestFile {
  source_path: string;
  source_url: string;
  target_path: string;
  /**
   * Where V2's own thumbnail for this image would be in the archive, and where it would
   * land. The CLI emits both unconditionally because it cannot open the tar; this file
   * reports whether the twin was actually there. Optional in the type so a manifest from
   * an older CLI still parses.
   */
  thumb_source_path?: string;
  thumb_target_path?: string;
  hn: string;
  clinic_code: string;
  taken_at: string;
  doctor_code?: string;
  created_by?: string;
  area_tag: string;
  note?: string;
}

/** The whole copy plan. Mirrors legacyimport.PhotoManifest. */
export interface Manifest {
  clinic_code: string;
  uploads_subdir: string;
  uploads_thumb_subdir?: string;
  files: ManifestFile[];
}

/**
 * Where the image BYTES go. Separate from the clinic-API target on purpose: that one
 * exists to reach :8090 and can legitimately be cleared to switch to a direct HTTP call,
 * which has nothing to do with whether this host holds the uploads directory.
 */
export interface UploadsTarget {
  id: TargetId;
  sshHost: string;
  sshUser: string;
  sshPort: string;
  sshKey: string;
  /** Absolute path of the uploads base on that host (the container's /app/uploads). */
  dir: string;
}

function env(k: string): string {
  return (process.env[k] ?? "").trim();
}

/**
 * readUploadsTarget resolves where one environment's image files live, or reports what is
 * missing. The default dir matches the bind mount in docker-compose.prod.yml
 * (./storage/uploads → /app/uploads); it is still overridable, because a host that was
 * laid out differently must not need a code change.
 */
export function readUploadsTarget(id: TargetId): {
  target: UploadsTarget | null;
  missing: string[];
} {
  const up = id.toUpperCase();
  const t: UploadsTarget = {
    id,
    sshHost: env(`TARGET_${up}_UPLOADS_SSH_HOST`),
    sshUser: env("IMPORT_SSH_USER") || "ubuntu",
    sshPort: env("IMPORT_SSH_PORT") || "22",
    sshKey: env("IMPORT_SSH_KEY"),
    dir: env(`TARGET_${up}_UPLOADS_DIR`) || "/var/www/dreasev4-back/storage/uploads",
  };
  const missing: string[] = [];
  if (!t.sshHost) missing.push(`TARGET_${up}_UPLOADS_SSH_HOST`);
  if (!t.sshKey) missing.push("IMPORT_SSH_KEY");
  return { target: missing.length === 0 ? t : null, missing };
}

/** A short, non-secret description for the UI. Never includes the key path. */
export function describeUploads(t: UploadsTarget): string {
  return `${t.sshUser}@${t.sshHost}:${t.dir}`;
}

/** TARGET_IDS re-exported so a caller validating an id needs one import. */
export { TARGET_IDS };

// ── the survey ───────────────────────────────────────────────────────────────

/**
 * What the LIVE upload endpoint would refuse.
 *
 * POST /v3/opd-record/:hn/photo enforces two things a legacy image never had to satisfy:
 * 3 MB (maxPhotoBytes) and JPEG-or-PNG only (opdPhotoExt — narrower than the rest of the
 * app because a clinical photo has to yield a thumbnail, and MakeThumbnail decodes those
 * two formats only).
 *
 * These are REPORTED, not enforced. The clinic owner's call: an image a doctor already
 * attached belongs in the record, and the read path agrees — ServePhoto streams bytes and
 * never consults the allow-list, so a legacy .gif keeps rendering. Refusing it here would
 * delete history to satisfy a rule that only governs new uploads.
 *
 * Worth surfacing anyway, because it answers "why can I see this photo but not re-upload
 * one like it", which is otherwise a mystery.
 */
const MAX_PHOTO_BYTES = 3 * 1024 * 1024;

/** sniffImage names the format from the file's own first bytes, like http.DetectContentType. */
async function sniffImage(abs: string): Promise<string> {
  // 12 bytes covers every signature below, including WebP's "RIFF....WEBP".
  const fh = await open(abs, "r");
  try {
    const buf = Buffer.alloc(12);
    const { bytesRead } = await fh.read(buf, 0, 12, 0);
    const b = buf.subarray(0, bytesRead);
    if (b.length >= 3 && b[0] === 0xff && b[1] === 0xd8 && b[2] === 0xff) return "image/jpeg";
    if (b.length >= 8 && b.subarray(0, 8).equals(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])))
      return "image/png";
    if (b.length >= 4 && b.subarray(0, 4).toString("latin1") === "GIF8") return "image/gif";
    if (b.length >= 12 && b.subarray(0, 4).toString("latin1") === "RIFF" &&
        b.subarray(8, 12).toString("latin1") === "WEBP") return "image/webp";
    if (b.length >= 2 && b[0] === 0x42 && b[1] === 0x4d) return "image/bmp";
    if (b.length >= 4 && (b.subarray(0, 4).toString("hex") === "49492a00" ||
                          b.subarray(0, 4).toString("hex") === "4d4d002a")) return "image/tiff";
    return "unknown";
  } finally {
    await fh.close();
  }
}

export interface SurveyFile {
  hn: string;
  source_url: string;
  target_path: string;
  bytes: number;
  /** Sniffed from the file's own bytes, never from its name. */
  mime: string;
  /**
   * Set only when V2's thumbnail for this image is actually in the archive. Absent means
   * thumb_path stays NULL and the app falls back to the full image — which is why a
   * missing twin is not an error.
   */
  thumb?: { source_path: string; target_path: string; bytes: number };
}

/** One image the live upload endpoint would turn away, with the reason. */
export interface WouldRejectFile {
  hn: string;
  target_path: string;
  bytes: number;
  mime: string;
  reason: string;
}

export interface Survey {
  /** Every manifest entry whose file is present in the archive, with its real size. */
  matched: SurveyFile[];
  /** Manifest entries with no file in the archive. These CANNOT be imported. */
  missing: { hn: string; source_url: string; expected_path: string }[];
  /** Files in the archive that no manifest entry asked for. Harmless, but worth knowing. */
  extraCount: number;
  extraSample: string[];
  totalBytes: number;
  /**
   * Imported anyway. See MAX_PHOTO_BYTES: these are the images the upload endpoint would
   * reject today, listed so nobody has to guess why later.
   */
  wouldReject: WouldRejectFile[];
  oversizeCount: number;
  unsupportedCount: number;
  /** How many of the matched images found V2's thumbnail beside them, and its total size. */
  thumbFound: number;
  thumbBytes: number;
  /**
   * file_path ends .jpg but the bytes are a PNG, or the reverse. ServePhoto picks the
   * Content-Type from the extension, so the header would be wrong — browsers sniff and
   * still render it, which is why this is a note and not a failure.
   */
  extMismatch: { target_path: string; mime: string }[];
}

/**
 * surveyArchive matches the manifest against the extracted archive and weighs each file.
 *
 * A manifest entry whose file is absent is reported and NEVER imported: a photos row
 * whose file_path points at nothing renders as a broken image inside a medical record,
 * which is worse than the image simply not having been migrated yet.
 */
export async function surveyArchive(manifest: Manifest, root: string): Promise<Survey> {
  const matched: SurveyFile[] = [];
  const missing: Survey["missing"] = [];
  const wouldReject: WouldRejectFile[] = [];
  const extMismatch: Survey["extMismatch"] = [];
  let totalBytes = 0;
  let oversizeCount = 0;
  let unsupportedCount = 0;
  let thumbFound = 0;
  let thumbBytes = 0;

  for (const f of manifest.files) {
    const abs = join(root, f.source_path);
    try {
      const st = await stat(abs);
      if (!st.isFile()) throw new Error("not a file");
      const mime = await sniffImage(abs);

      // V2's own thumbnail, if the export carried one. Only stat'd — nothing is decided
      // here about where it goes; the CLI already said that.
      let thumb: SurveyFile["thumb"];
      if (f.thumb_source_path && f.thumb_target_path) {
        try {
          const tst = await stat(join(root, f.thumb_source_path));
          if (tst.isFile() && tst.size > 0) {
            thumb = {
              source_path: f.thumb_source_path,
              target_path: f.thumb_target_path,
              bytes: tst.size,
            };
            thumbFound++;
            thumbBytes += tst.size;
          }
        } catch {
          // No twin for this one. thumb_path stays NULL and the app serves the full
          // image, so there is nothing to report per file — the count below says how
          // many were missing.
        }
      }

      matched.push({
        hn: f.hn,
        source_url: f.source_url,
        target_path: f.target_path,
        bytes: st.size,
        mime,
        thumb,
      });
      totalBytes += st.size;

      // Reported, never enforced. Both counted separately because they are different
      // problems: one is a photo too big to re-upload, the other a format the new
      // thumbnailer cannot read.
      const supported = mime === "image/jpeg" || mime === "image/png";
      if (!supported) {
        unsupportedCount++;
        wouldReject.push({
          hn: f.hn,
          target_path: f.target_path,
          bytes: st.size,
          mime,
          reason: `ชนิดไฟล์ ${mime} — endpoint อัปโหลดรับเฉพาะ JPG/PNG`,
        });
      } else if (st.size > MAX_PHOTO_BYTES) {
        oversizeCount++;
        wouldReject.push({
          hn: f.hn,
          target_path: f.target_path,
          bytes: st.size,
          mime,
          reason: `${(st.size / 1024 / 1024).toFixed(1)} MB — endpoint อัปโหลดจำกัด 3 MB`,
        });
      }

      // The extension came from the legacy `url`; the bytes are what they are.
      const lower = f.target_path.toLowerCase();
      const claimsPng = lower.endsWith(".png");
      const claimsJpg = lower.endsWith(".jpg") || lower.endsWith(".jpeg");
      if ((claimsPng && mime === "image/jpeg") || (claimsJpg && mime === "image/png")) {
        extMismatch.push({ target_path: f.target_path, mime });
      }
    } catch {
      missing.push({ hn: f.hn, source_url: f.source_url, expected_path: f.source_path });
    }
  }

  // Counting the extras needs the archive's own listing, not the manifest's.
  const listed = await listArchiveFiles(root);
  const wanted = new Set(manifest.files.map((f) => f.source_path));
  const extras = listed.filter((p) => !wanted.has(p));

  return {
    matched,
    missing,
    extraCount: extras.length,
    extraSample: extras.slice(0, 10),
    totalBytes,
    wouldReject,
    oversizeCount,
    unsupportedCount,
    thumbFound,
    thumbBytes,
    extMismatch,
  };
}

/** listArchiveFiles walks the extracted tree and returns paths relative to root. */
async function listArchiveFiles(root: string): Promise<string[]> {
  // `find` is used rather than a recursive JS walk because the archive can hold tens of
  // thousands of entries and this is one process instead of that many syscall round trips
  // through the event loop.
  const { stdout } = await run("find", [root, "-type", "f"], { maxBuffer: 64 * 1024 * 1024 });
  const prefix = root.endsWith("/") ? root : root + "/";
  return stdout
    .split("\n")
    .map((s) => s.trim())
    .filter(Boolean)
    .map((p) => (p.startsWith(prefix) ? p.slice(prefix.length) : p));
}

// ── extracting ───────────────────────────────────────────────────────────────

/**
 * extractArchive unpacks the operator's tar into a fresh directory and returns it.
 *
 * The whole archive is extracted even for a survey. Listing it without unpacking would
 * mean parsing `tar -tv` output, whose columns shift with tar version and locale, to
 * learn a file size — and getting a size wrong here corrupts the duplicate key the import
 * relies on. Twenty-odd MB of scans is cheap; a misparsed column is not.
 */
export async function extractArchive(tarPath: string): Promise<string> {
  const dir = await mkdtemp(join(workRoot(), "importv4-img-"));
  try {
    await run("tar", ["-xzf", tarPath, "-C", dir], { maxBuffer: 8 * 1024 * 1024 });
  } catch (err) {
    await rm(dir, { recursive: true, force: true });
    const e = err as { stderr?: string; message?: string };
    throw new Error(`แตกไฟล์ .tar.gz ไม่ได้: ${(e.stderr || e.message || "").trim()}`);
  }
  return dir;
}

// ── shipping the bytes ───────────────────────────────────────────────────────

export interface ShipResult {
  filesSent: number;
  /** Of those, how many were V2 thumbnails carried across rather than full images. */
  thumbsSent: number;
  bytesSent: number;
  log: string;
}

/**
 * shipFiles copies the matched images to the target host under their FINAL relative
 * paths.
 *
 * They travel as one repacked archive rather than N scp calls: several hundred separate
 * ssh sessions would take minutes and fail half way through with no way to say how far it
 * got. One archive is one round trip, and tar on the far side either unpacks it or does
 * not.
 *
 * Nothing is deleted on the far side and nothing outside the clinic's own subdirectory is
 * written: every entry in the archive is <clinic>/opd-photo/<name>, built here from the
 * manifest, never from a path inside the operator's upload. That matters more than usual
 * because the remote half runs under sudo — the paths it is handed are derived from the
 * manifest and shell-quoted, so a crafted value in the dump cannot become a path outside
 * the clinic's folder or a second command.
 */
export async function shipFiles(
  t: UploadsTarget,
  manifest: Manifest,
  survey: Survey,
  extractRoot: string,
): Promise<ShipResult> {
  const stage = await mkdtemp(join(workRoot(), "importv4-stage-"));
  const bundle = join(stage, "..", `importv4-bundle-${process.pid}.tar.gz`);
  const payload = join(stage, "payload");

  try {
    const byTarget = new Map(manifest.files.map((f) => [f.target_path, f]));
    let files = 0;
    let bytes = 0;
    let thumbs = 0;

    for (const m of survey.matched) {
      const f = byTarget.get(m.target_path);
      if (!f) continue;
      const dest = join(payload, f.target_path);
      await mkdir(dirname(dest), { recursive: true });
      await copyFile(join(extractRoot, f.source_path), dest);
      files++;
      bytes += m.bytes;

      // The thumbnail travels in the SAME bundle as its image. Two archives would create a
      // window where a row could point at a thumbnail that had not arrived, and the whole
      // reason bytes go before rows is to have no such window.
      if (m.thumb) {
        const tdest = join(payload, m.thumb.target_path);
        await mkdir(dirname(tdest), { recursive: true });
        await copyFile(join(extractRoot, m.thumb.source_path), tdest);
        thumbs++;
        bytes += m.thumb.bytes;
      }
    }
    if (files === 0) throw new Error("ไม่มีไฟล์ที่จับคู่ได้เลย จึงไม่มีอะไรจะส่ง");

    await run("tar", ["-czf", bundle, "-C", payload, "."], { maxBuffer: 8 * 1024 * 1024 });

    // scp the bundle, then unpack it on the far side into the uploads base. `-m` on
    // mkdir keeps the mode explicit rather than inheriting a surprising umask; the
    // container reads these files as a different uid than the one writing them.
    const remoteTmp = `/tmp/importv4-photos-${Date.now()}.tar.gz`;
    await run(
      "scp",
      [
        "-i", t.sshKey,
        "-P", t.sshPort,
        "-o", "BatchMode=yes",
        "-o", "StrictHostKeyChecking=accept-new",
        "-o", "ConnectTimeout=15",
        "-o", "IdentitiesOnly=yes",
        bundle,
        `${t.sshUser}@${t.sshHost}:${remoteTmp}`,
      ],
      { maxBuffer: 8 * 1024 * 1024 },
    );

    // The uploads tree is root-owned: the API container runs as uid 0 and every file
    // already in there is root:root 0644. The ssh account is ubuntu (uid 1000) and cannot
    // even mkdir inside it, so the copy runs under sudo -n.
    //
    // -n, never an interactive prompt: stdin here carries no keyboard, so a sudo that
    // decided to ask for a password would hang the request until the timeout instead of
    // failing with a reason.
    //
    // Ownership and mode are then set EXPLICITLY rather than left to tar. For root, GNU
    // tar defaults to --same-owner --same-permissions, so it would faithfully restore
    // uid 1000 and whatever mode the legacy server happened to use — leaving imported
    // files that differ from every file the app writes itself. That difference is not
    // cosmetic: it is the kind of thing somebody has to investigate a year from now. The
    // flags below plus the explicit chown/chmod make the result identical to
    // storage.Write's own output (root:root, dirs 755, files 644), and they are scoped to
    // this clinic's opd-photo directory so nothing else in the tree is touched.
    // Two directories now: the images and, when the export carried them, V2's thumbnails.
    // Both are derived from the manifest and shell-quoted, and both are scoped to this
    // clinic — the remote half runs under sudo, so nothing outside these may be touched.
    const subdir = join(t.dir, manifest.uploads_subdir).replaceAll("\\", "/");
    const dirs = [subdir];
    if (thumbs > 0 && manifest.uploads_thumb_subdir) {
      dirs.push(join(t.dir, manifest.uploads_thumb_subdir).replaceAll("\\", "/"));
    }
    const quoted = dirs.map(shellQuote);
    const remote = [
      `set -e`,
      ...quoted.map((d) => `sudo -n mkdir -p ${d}`),
      `sudo -n tar --no-same-owner --no-same-permissions -xzf ${shellQuote(remoteTmp)} -C ${shellQuote(t.dir)}`,
      ...quoted.map((d) => `sudo -n chown -R root:root ${d}`),
      ...quoted.map((d) => `sudo -n find ${d} -type d -exec chmod 755 {} +`),
      ...quoted.map((d) => `sudo -n find ${d} -type f -exec chmod 644 {} +`),
      `rm -f ${shellQuote(remoteTmp)}`,
      `find ${quoted.join(" ")} -type f | wc -l`,
    ].join(" && ");

    const { stdout, stderr } = await run(
      "ssh",
      [
        "-i", t.sshKey,
        "-p", t.sshPort,
        "-o", "BatchMode=yes",
        "-o", "StrictHostKeyChecking=accept-new",
        "-o", "ConnectTimeout=15",
        "-o", "IdentitiesOnly=yes",
        `${t.sshUser}@${t.sshHost}`,
        remote,
      ],
      { maxBuffer: 8 * 1024 * 1024 },
    );

    return {
      filesSent: files,
      thumbsSent: thumbs,
      bytesSent: bytes,
      log: [
        `ไฟล์ในโฟลเดอร์ปลายทางหลังแตกไฟล์: ${stdout.trim()}`,
        `ส่งรูปจริง ${files} ไฟล์ · thumbnail ${thumbs} ไฟล์`,
        stderr.trim(),
      ]
        .filter(Boolean)
        .join("\n"),
    };
  } finally {
    await rm(stage, { recursive: true, force: true });
    await rm(bundle, { force: true });
  }
}

// ── the rows ─────────────────────────────────────────────────────────────────

/** lit renders a Postgres string literal, doubling embedded single quotes. */
function lit(s: string): string {
  return "'" + s.replaceAll("'", "''") + "'";
}

function nullLit(s: string | undefined): string {
  const v = (s ?? "").trim();
  return v === "" ? "NULL" : lit(v);
}

/** intOrNull keeps a non-numeric legacy value out of an integer column. */
function intOrNull(s: string | undefined): string {
  const v = (s ?? "").trim();
  return /^-?\d+$/.test(v) ? v : "NULL";
}

/**
 * buildInsertSQL renders the rows for the images that were actually shipped.
 *
 * Shape of each statement, and why:
 *
 *   - INSERT … SELECT … WHERE NOT EXISTS, keyed on
 *     (clinic_code, hn, taken_at, file_size). There is no migrate_source_ref column on
 *     photos and the owner chose not to add one, so this triple is the only thing that
 *     can tell "already imported" from "import again". taken_at carries millisecond
 *     precision from the source revision, so two genuinely different pages colliding on
 *     it AND on byte count is not a case that occurs; when the manifest itself contains
 *     such a pair the CLI says so in its warnings.
 *   - visit_id NULL. See page.go: SaveMedia hard-deletes a visit's rows before rewriting
 *     them, so a row bound to a visit would be destroyed the first time a doctor saved
 *     media there.
 *   - branch_code read from the patient, in SQL, at insert time. The legacy page export
 *     does not say which branch a scan belongs to, but the patient it belongs to does, and
 *     that is the same answer: a scan is filed at the branch that holds the record. Doing
 *     it as a subquery keeps the CLI and the manifest out of it, and it is exact rather
 *     than a guess because uq_patient_clinic_hn is UNIQUE on (clinic_code, "HN") — the
 *     lookup can match one row or none.
 *     NULL when the patient is not there yet (photos have no FK to patient, and a
 *     photos-only run into a clinic whose patients come later is allowed).
 *     NOTE ON WIDTH: patient.branch_code is varchar(20), photos.branch_code varchar(16).
 *     Real codes are BR00001-shaped (7 chars) so this does not bite, and it is left to
 *     fail loudly rather than truncated — a silently shortened code points at no branch or
 *     the wrong one, which is worse than a refused transaction.
 *   - thumb_path set when V2's own thumbnail was in the archive, NULL when it was not.
 *     The export already carries them beside the full images (250x250, ~7 KB against
 *     ~313 KB), so this is a file copy, not a render — nothing here decodes an image. A
 *     row without one is not broken: the app falls back to the full image, which is why
 *     the spec says thumb_path needs no backfill.
 *   - consent_given true, by the clinic owner's decision.
 *   - One transaction, with a guard that the clinic exists first. `photos` has no foreign
 *     key to `clinic`, so a wrong clinic_code would insert rows that no tenant can ever
 *     see — the failure that looks like success.
 */
export function buildInsertSQL(
  clinic: string,
  manifest: Manifest,
  survey: Survey,
): { sql: string; rows: number } {
  const byTarget = new Map(manifest.files.map((f) => [f.target_path, f]));
  const out: string[] = [];

  out.push("-- ══════════════════════════════════════════════════════════════════════════");
  out.push("-- Dr.Ease V2 → V4: รูปหน้าเวชระเบียน (mris.page → public.photos)");
  out.push(`-- clinic_code : ${clinic}`);
  out.push(`-- รูป         : ${survey.matched.length} ไฟล์ (rev ล่าสุดของแต่ละหน้า)`);
  out.push(
    `-- thumbnail   : ${survey.thumbFound} ไฟล์จากถุงของ V2 เอง (คัดลอกมา ไม่ได้สร้างใหม่) · ` +
      `อีก ${survey.matched.length - survey.thumbFound} ไฟล์ thumb_path เป็น NULL แล้วระบบใช้รูปจริงแทน`,
  );
  out.push("--");
  if (survey.wouldReject.length > 0) {
    out.push(
      `-- นำเข้าอยู่ แต่ endpoint อัปโหลดวันนี้จะปฏิเสธ ${survey.wouldReject.length} ไฟล์ ` +
        `(เกิน 3 MB: ${survey.oversizeCount} · ไม่ใช่ JPG/PNG: ${survey.unsupportedCount}) —`,
    );
    out.push("-- รูปที่หมอแนบไปแล้วยังควรอยู่ในเวชระเบียน และ ServePhoto ส่งไบต์ตรง ๆ ไม่ดูรายการชนิดไฟล์");
    out.push("--");
  }
  if (survey.extMismatch.length > 0) {
    out.push(
      `-- นามสกุลไม่ตรงกับไบต์จริง ${survey.extMismatch.length} ไฟล์ — Content-Type ที่ส่งจะผิด ` +
        "แต่เบราว์เซอร์ sniff เองจึงยังแสดงได้",
    );
    out.push("--");
  }
  out.push("-- ไบต์ของรูปถูกคัดลอกขึ้นเครื่องปลายทางก่อนสคริปต์นี้จะรัน — แถวที่ไม่มีไฟล์คู่กัน");
  out.push("-- คือรูปเสียในเวชระเบียน จึงไม่มีแถวใดถูกเขียนก่อนไฟล์ถึงที่");
  out.push("--");
  out.push("-- insert-only · รันซ้ำไม่เพิ่มของซ้ำ (กันด้วย clinic_code + hn + taken_at + file_size)");
  out.push("-- ══════════════════════════════════════════════════════════════════════════");
  out.push("");
  out.push("BEGIN;");
  out.push("");
  out.push("SET LOCAL standard_conforming_strings = on;");
  out.push(`SET LOCAL app.clinic_code = ${lit(clinic)};`);
  out.push("");
  out.push("-- GUARD: คลินิกต้องมีอยู่จริง — photos ไม่มี FK ไป clinic ฉะนั้นรหัสผิดจะเขียนผ่าน");
  out.push("-- เงียบ ๆ แล้วไม่มี tenant ไหนมองเห็นแถวนั้นได้เลย");
  out.push("DO $guard$");
  out.push("BEGIN");
  out.push(`    IF NOT EXISTS (SELECT 1 FROM clinic WHERE clinic_code = ${lit(clinic)}) THEN`);
  out.push(
    `        RAISE EXCEPTION 'clinic_code % ไม่มีในระบบ — สร้างคลินิกก่อนแล้วนำเข้ารูปอีกครั้ง', ${lit(clinic)};`,
  );
  out.push("    END IF;");
  out.push("END $guard$;");
  out.push("");

  let rows = 0;
  for (const m of survey.matched) {
    const f = byTarget.get(m.target_path);
    if (!f) continue;
    rows++;

    const c = lit(clinic);
    const hn = lit(f.hn);
    const taken = `${lit(f.taken_at)}::timestamptz`;
    const size = String(m.bytes);

    out.push(`-- hn ${f.hn} · ${f.source_url}`);
    out.push(
      `INSERT INTO photos (clinic_code, hn, branch_code, visit_id, area_tag, kind, file_path,
                    thumb_path, file_size, consent_given, note, doctor_code, created_by,
                    taken_at, annotations, created_at, updated_at)
SELECT ${c}, ${hn},
       (SELECT p.branch_code FROM patient p WHERE p.clinic_code = ${c} AND p."HN" = ${hn}),
       NULL, ${nullLit(f.area_tag) === "NULL" ? lit("other") : lit(f.area_tag)}, 'note',
       ${lit(f.target_path)}, ${m.thumb ? lit(m.thumb.target_path) : "NULL"},
       ${size}, true, ${nullLit(f.note)}, ${nullLit(f.doctor_code)}, ${intOrNull(f.created_by)}, ${taken},
       '[]'::jsonb, now(), now()
WHERE NOT EXISTS (
    SELECT 1 FROM photos
     WHERE clinic_code = ${c} AND hn = ${hn}
       AND taken_at = ${taken} AND file_size = ${size}
);`,
    );
    out.push("");
  }

  // How many rows ended up with no branch. Raised rather than left to be noticed later: a
  // photos-only run into a clinic whose patients have not been imported yet resolves NOTHING,
  // and every row would silently sit outside every branch filter. The count is the only way
  // to tell that apart from a normal run.
  out.push("-- สาขาที่ผูกได้จริง (อ่านจาก patient ของ HN นั้น ตอน insert)");
  out.push("DO $branch$");
  out.push("DECLARE");
  out.push("    n     bigint;");
  out.push("    total bigint;");
  out.push("BEGIN");
  out.push("    SELECT count(*) FILTER (WHERE branch_code IS NULL), count(*)");
  out.push("      INTO n, total");
  out.push(`      FROM photos WHERE clinic_code = ${lit(clinic)};`);
  out.push("    IF n > 0 THEN");
  out.push(
    "        RAISE NOTICE 'photos: % จาก % แถวยังไม่มี branch_code เพราะหา patient ของ HN นั้นไม่เจอ" +
      " — นำเข้ากลุ่ม patients ก่อนแล้วรูปรอบถัดไปจะผูกสาขาได้', n, total;",
  );
  out.push("    ELSE");
  out.push("        RAISE NOTICE 'photos: ผูกสาขาได้ครบทั้ง % แถว', total;");
  out.push("    END IF;");
  out.push("END $branch$;");
  out.push("");
  out.push("COMMIT;");
  out.push("");
  return { sql: out.join("\n"), rows };
}

/** cleanup removes a temp tree, ignoring a tree that is already gone. */
export async function cleanup(...dirs: string[]): Promise<void> {
  for (const d of dirs) {
    if (d) await rm(d, { recursive: true, force: true });
  }
}

/** readManifest loads and shape-checks the CLI's photos_<clinic>.json. */
export async function readManifest(path: string, clinic: string): Promise<Manifest> {
  const raw = await readFile(path, "utf8");
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch (e) {
    throw new Error(`อ่าน manifest ที่ CLI สร้างไม่ได้: ${(e as Error).message}`);
  }
  const m = parsed as Manifest;
  if (!m || typeof m !== "object" || !Array.isArray(m.files)) {
    throw new Error("manifest ที่ CLI สร้างมีรูปแบบไม่ถูกต้อง");
  }
  // The clinic in the manifest must be the clinic the caller asked for, or the paths
  // being written belong to a different tenant.
  if (m.clinic_code !== clinic) {
    throw new Error(
      `manifest เป็นของคลินิก ${m.clinic_code} แต่คำขอนี้เป็นของ ${clinic} — สร้างใหม่ก่อน`,
    );
  }
  if (!m.uploads_subdir.startsWith(`${clinic}/`)) {
    throw new Error(`manifest ชี้โฟลเดอร์ ${m.uploads_subdir} ที่ไม่ใช่ของคลินิก ${clinic}`);
  }
  // Both target paths are checked, and both source paths too: everything below is used to
  // build a filesystem path on a host where the far half runs under sudo, so a crafted
  // value in the dump must not be able to climb out of the clinic's folder.
  for (const f of m.files) {
    for (const [label, p] of [
      ["target_path", f.target_path],
      ["thumb_target_path", f.thumb_target_path],
    ] as const) {
      if (p === undefined) continue;
      if (!p.startsWith(`${clinic}/`) || p.includes("..")) {
        throw new Error(`manifest มี ${label} ที่ออกนอกโฟลเดอร์คลินิก: ${p}`);
      }
    }
    for (const [label, p] of [
      ["source_path", f.source_path],
      ["thumb_source_path", f.thumb_source_path],
    ] as const) {
      if (p === undefined) continue;
      if (p.startsWith("/") || p.includes("..")) {
        throw new Error(`manifest มี ${label} ที่ออกนอกถุง: ${p}`);
      }
    }
  }
  return m;
}
