import "server-only";
import { validatePhotoManifest } from './photo-plan';
import { PHOTO_PUBLISH_SCRIPT } from './photo-publish';
import { createHash } from 'node:crypto';
import { createReadStream } from 'node:fs';

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

import { execFile, spawn } from "node:child_process";
import { mkdtemp, mkdir, open, readFile, writeFile, rm, stat, copyFile, link } from "node:fs/promises";
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
  visit_date: string;
  source_type: 'before_after' | 'form' | 'drawing' | 'unknown';
  source_ref: string;
  source_metadata: Record<string, unknown>;
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
  /**
   * photos.kind — CHECK (before|after|note). Derived by the CLI from the doctype family
   * (page.go: areaKind), never guessed here. Optional in the type so a manifest produced
   * by an older CLI still parses; those fall back to "note", which is what every imported
   * row used to be.
   */
  kind?: string;
  note?: string;
}

/** The whole copy plan. Mirrors legacyimport.PhotoManifest. */
export interface Manifest {
  version: number;
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
 * got. One archive is one round trip. The receiver verifies it in staging and
 * publishes complete files without replacing an existing path.
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
  const payload = join(stage, "payload");

  try {
    const byTarget = new Map(manifest.files.map((f) => [f.target_path, f]));
    let files = 0;
    let bytes = 0;
    let thumbs = 0;
    const expected: Record<string, { size: number; sha256: string }> = {};
    const rememberFile = async (path: string, relative: string, size: number) => {
      const digest = createHash('sha256');
      let actualSize = 0;
      for await (const block of createReadStream(path)) {
        digest.update(block);
        actualSize += block.length;
      }
      if (actualSize !== size) throw new Error('ไฟล์รูปเปลี่ยนขนาดหลังสำรวจ — หยุดก่อนส่ง');
      expected[relative] = { size, sha256: digest.digest('hex') };
    };

    for (const m of survey.matched) {
      const f = byTarget.get(m.target_path);
      if (!f) continue;
      const dest = join(payload, f.target_path);
      await mkdir(dirname(dest), { recursive: true });
      await placeFile(join(extractRoot, f.source_path), dest);
      await rememberFile(dest, f.target_path, m.bytes);
      files++;
      bytes += m.bytes;

      // The thumbnail travels in the SAME stream as its image. Two transfers would create a
      // window where a row could point at a thumbnail that had not arrived, and the whole
      // reason bytes go before rows is to have no such window.
      if (m.thumb) {
        const tdest = join(payload, m.thumb.target_path);
        await mkdir(dirname(tdest), { recursive: true });
        await placeFile(join(extractRoot, m.thumb.source_path), tdest);
        await rememberFile(tdest, m.thumb.target_path, m.thumb.bytes);
        thumbs++;
        bytes += m.thumb.bytes;
      }
    }
    if (files === 0) throw new Error("ไม่มีไฟล์ที่จับคู่ได้เลย จึงไม่มีอะไรจะส่ง");

    // Python stages and hashes the entire payload, then publishes complete files
    // atomically. An existing path must have identical bytes and is never touched.
    await writeFile(join(payload, '.photo-import-manifest.json'), JSON.stringify(expected));
    const remote = `sudo -n python3 -c ${shellQuote(PHOTO_PUBLISH_SCRIPT)} ${shellQuote(t.dir)}`;

    const { stdout, stderr } = await streamPayload(payload, t, remote, manifest.clinic_code);

    return {
      filesSent: files,
      thumbsSent: thumbs,
      bytesSent: bytes,
      log: [
        `ผลตรวจและเผยแพร่ไฟล์แบบไม่เขียนทับ: ${stdout.trim()}`,
        `ส่งรูปจริง ${files} ไฟล์ · thumbnail ${thumbs} ไฟล์`,
        stderr.trim(),
      ]
        .filter(Boolean)
        .join("\n"),
    };
  } finally {
    await rm(stage, { recursive: true, force: true });
  }
}

/**
 * placeFile puts one image into the payload tree, as a HARD LINK when it can.
 *
 * WHY NOT A COPY
 *
 * The images exist four times over on the way to the uploads host: the pulled archive, its
 * extracted tree, the payload assembled here, and the tar that used to be written before
 * being sent. On a 14.17 GB archive of JPEGs — which gzip cannot shrink — that is about 42 GB
 * of disk for one clinic, and on 22 Sep 2026 it ran a 77 GB box out of space mid-transfer:
 *
 *	gzip: stdout: No space left on device
 *	tar: Wrote only 8192 of 10240 bytes
 *
 * The SQL had already committed by then, so the clinic was left with data and no images.
 *
 * A hard link removes the payload copy entirely: the directory tree is real (tar walks it
 * exactly as before) but the bytes are the extracted file's own. payload and extractRoot are
 * both under UPLOAD_STAGING_DIR, so they are always on the same filesystem.
 *
 * Falls back to copying if the link cannot be made — a filesystem that refuses links must
 * still produce a correct import, just a hungrier one.
 */
async function placeFile(src: string, dest: string): Promise<void> {
  try {
    await link(src, dest);
  } catch {
    await copyFile(src, dest);
  }
}

/**
 * streamPayload tars the payload straight into the remote command's stdin.
 *
 * The tar used to be written to disk and then scp'd, which cost a fourth full copy of every
 * image here and a fifth in the far side's /tmp. Nothing needs either: `tar -czf -` on this
 * end and `tar -xzf -` on the other means the bytes only ever exist in the pipe.
 *
 * BOTH exit codes are checked. Piping hides failures by default — if tar dies halfway, ssh
 * still sees a clean end-of-stream and can exit 0 while the far side holds a truncated tree.
 * A non-zero tar is therefore an error even when the remote reported success.
 *
 * stdout of the remote command is returned because the last thing it runs is a file count,
 * which is the only independent check that the bytes landed.
 */
function streamPayload(
  payload: string,
  t: UploadsTarget,
  remoteCmd: string,
  clinic: string,
): Promise<{ stdout: string; stderr: string }> {
  return new Promise((resolve, reject) => {
    // The digest manifest travels first, avoiding command-line size limits.
    const tar = spawn("tar", ["-czf", "-", "-C", payload, "--", ".photo-import-manifest.json", clinic], {
      stdio: ["ignore", "pipe", "pipe"],
    });
    const ssh = spawn(
      "ssh",
      [
        "-i", t.sshKey,
        "-p", t.sshPort,
        "-o", "BatchMode=yes",
        "-o", "StrictHostKeyChecking=accept-new",
        "-o", "ConnectTimeout=15",
        "-o", "IdentitiesOnly=yes",
        `${t.sshUser}@${t.sshHost}`,
        remoteCmd,
      ],
      { stdio: ["pipe", "pipe", "pipe"] },
    );

    let out = "";
    let err = "";
    let tarErr = "";
    // Bounded on purpose: a runaway remote must not be able to grow this process's heap.
    const cap = (s: string, add: string) => (s.length > 65536 ? s : s + add);
    tar.stderr.on("data", (d: Buffer) => {
      tarErr = cap(tarErr, d.toString());
    });
    ssh.stdout.on("data", (d: Buffer) => {
      out = cap(out, d.toString());
    });
    ssh.stderr.on("data", (d: Buffer) => {
      err = cap(err, d.toString());
    });

    tar.stdout.pipe(ssh.stdin);
    // EPIPE when the remote exits first: that failure is already reported through ssh's own
    // exit code and stderr, and an unhandled error event here would crash the process.
    ssh.stdin.on("error", () => {});

    let tarCode: number | null = null;
    let sshCode: number | null = null;
    let settled = false;
    const finish = () => {
      if (settled || tarCode === null || sshCode === null) return;
      settled = true;
      if (tarCode !== 0) {
        reject(new Error(`อ่าน/บีบอัดไฟล์รูปไม่สำเร็จ (tar exit ${tarCode}): ${tarErr.trim()}`));
        return;
      }
      if (sshCode !== 0) {
        reject(
          new Error(
            `ส่งไฟล์รูปไปเครื่องปลายทางไม่สำเร็จ (ssh exit ${sshCode}): ${err.trim() || out.trim()}`,
          ),
        );
        return;
      }
      resolve({ stdout: out, stderr: err });
    };
    tar.on("close", (c) => {
      tarCode = c ?? -1;
      finish();
    });
    ssh.on("close", (c) => {
      sshCode = c ?? -1;
      finish();
    });
    const bail = (e: Error) => {
      if (settled) return;
      settled = true;
      reject(e);
    };
    tar.on("error", bail);
    ssh.on("error", bail);
  });
}

// SQL planning is pure and can be tested without SSH, file copies or a database.
export { buildInsertSQL } from "./photo-plan";

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
  validatePhotoManifest(m, clinic);
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
