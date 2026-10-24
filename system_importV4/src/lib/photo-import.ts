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

import { execFile, spawn } from "node:child_process";
import { mkdtemp, mkdir, open, readFile, rm, stat, copyFile, link } from "node:fs/promises";
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
  /**
   * The day this image is filed under, YYYY-MM-DD: V2's own วันตรวจ (page.vd_first), or the
   * Bangkok day of the scan when the page had none. Decided by the CLI (page.go:
   * photoVisitDate). Optional so a manifest from an older CLI still parses; see visitDateOf
   * for what those fall back to.
   */
  visit_date?: string;
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
      await placeFile(join(extractRoot, f.source_path), dest);
      files++;
      bytes += m.bytes;

      // The thumbnail travels in the SAME stream as its image. Two transfers would create a
      // window where a row could point at a thumbnail that had not arrived, and the whole
      // reason bytes go before rows is to have no such window.
      if (m.thumb) {
        const tdest = join(payload, m.thumb.target_path);
        await mkdir(dirname(tdest), { recursive: true });
        await placeFile(join(extractRoot, m.thumb.source_path), tdest);
        thumbs++;
        bytes += m.thumb.bytes;
      }
    }
    if (files === 0) throw new Error("ไม่มีไฟล์ที่จับคู่ได้เลย จึงไม่มีอะไรจะส่ง");

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
      // `-xzf -`: the archive arrives on stdin, so it is never a file on either machine.
      `sudo -n tar --no-same-owner --no-same-permissions -xzf - -C ${shellQuote(t.dir)}`,
      ...quoted.map((d) => `sudo -n chown -R root:root ${d}`),
      ...quoted.map((d) => `sudo -n find ${d} -type d -exec chmod 755 {} +`),
      ...quoted.map((d) => `sudo -n find ${d} -type f -exec chmod 644 {} +`),
      `find ${quoted.join(" ")} -type f | wc -l`,
    ].join(" && ");

    const { stdout, stderr } = await streamPayload(payload, t, remote);

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
): Promise<{ stdout: string; stderr: string }> {
  return new Promise((resolve, reject) => {
    const tar = spawn("tar", ["-czf", "-", "-C", payload, "."], {
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
 * kindOf reads photos.kind off the manifest, refusing anything the column's CHECK would
 * reject.
 *
 * The value is decided by the CLI (page.go: areaKind) — this is not a second opinion, it is a
 * seatbelt for a manifest from an older CLI (no kind at all) or a hand-edited one. "note" is
 * the safe fallback because it is what every imported page used to be: it lands the page in
 * the record without claiming it is a before or after photo.
 */
function kindOf(f: ManifestFile): string {
  const k = (f.kind ?? "").trim();
  return k === "before" || k === "after" || k === "note" ? k : "note";
}

/**
 * Images are filed one visit per patient per DAY — the day V2 showed the page under (owner's
 * decision, 24 Oct 2026).
 *
 * That replaces the owner's choice of 15 Sep 2026, which filed every page of a patient under a
 * single visit dated 2026-01-01. The cost of that choice turned out to be on screen: one visit
 * for a patient's whole history collapses years of treatment into one encounter on a date
 * nobody attended, and the before/after page — which places a photo by the date of its visit —
 * could only ever show all of them on that one day.
 *
 * FALLBACK_VISIT_DATE is that old date, kept only for an image whose day cannot be read at all
 * (see visitDateOf). Rows already imported under it stay where they are (same decision): this
 * changes how new images are filed, it does not move old ones.
 */
const FALLBACK_VISIT_DATE = "2026-01-01";

/** Title shown on a visit this import opens, so nobody mistakes it for one keyed in v4. */
const IMPORT_VISIT_TITLE = "นำเข้าจากระบบเดิม";

/**
 * isCalendarDay accepts exactly YYYY-MM-DD naming a real day from 1900 on — the same floor as
 * page.go, and for the same reason: PostgreSQL refuses year 0000, and one such value would cost
 * the whole transaction rather than one date.
 */
function isCalendarDay(s: string): boolean {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(s)) return false;
  const [y, m, d] = s.split("-").map(Number);
  if (y < 1900) return false;
  const t = new Date(Date.UTC(y, m - 1, d));
  return t.getUTCFullYear() === y && t.getUTCMonth() === m - 1 && t.getUTCDate() === d;
}

/** clinicDay is the calendar day an instant falls on at the clinic: UTC+7, no daylight saving. */
function clinicDay(ms: number): string {
  return new Date(ms + 7 * 60 * 60 * 1000).toISOString().slice(0, 10);
}

/**
 * visitDateOf is the day an image is filed under.
 *
 * The CLI decides it (page.go: photoVisitDate); this reads it back. The rest is a seatbelt, as
 * kindOf is: a manifest from an older CLI has no visit_date at all, and a hand-edited one could
 * carry anything. Without one, the Bangkok day of taken_at is exactly what the CLI picks for a
 * page with no วันตรวจ (owner's fallback, 24 Oct 2026); without even a usable taken_at — a
 * revision with no timestamp leaves year 1 behind — the old fixed date.
 */
function visitDateOf(f: ManifestFile): string {
  const v = (f.visit_date ?? "").trim();
  if (isCalendarDay(v)) return v;
  const ms = Date.parse(f.taken_at);
  if (Number.isFinite(ms)) {
    const day = clinicDay(ms);
    if (isCalendarDay(day)) return day;
  }
  return FALLBACK_VISIT_DATE;
}

/** One shipped image and the manifest entry that planned it. */
interface PlannedPhoto {
  file: ManifestFile;
  shipped: SurveyFile;
}

/** The images of one patient on one day — one visit at the target. */
interface PlannedVisit {
  hn: string;
  visitDate: string;
  /** For a visit this import has to open; an existing visit keeps its own doctor. */
  doctorCode?: string;
  photos: PlannedPhoto[];
}

/** takenMs orders images by when they were taken; an unreadable time never wins. */
function takenMs(f: ManifestFile): number {
  const ms = Date.parse(f.taken_at);
  return Number.isFinite(ms) ? ms : Number.POSITIVE_INFINITY;
}

function compareText(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/**
 * visitPlan groups the shipped images by patient and day.
 *
 * doctor_code comes from the EARLIEST image of that patient-day — the owner's rule of 15 Sep
 * 2026, now applied per day instead of per patient: one visit carries one doctor, and the first
 * image is the only pick that does not depend on iteration order. Times are compared as
 * instants, not as text, because RFC3339Nano drops trailing zeros: "…38.79Z" sorts after
 * "…38.796Z" as a string although it is earlier.
 *
 * Ordered by day, then HN, and that is not cosmetic: the app's visit-number lock is taken only
 * for a day that can still be today (see buildInsertSQL), and with the days ascending today
 * comes last, so that lock is held for the tail of the transaction instead of all of it.
 */
function visitPlan(photos: PlannedPhoto[]): PlannedVisit[] {
  const byKey = new Map<string, { visit: PlannedVisit; first: ManifestFile }>();
  for (const p of photos) {
    const visitDate = visitDateOf(p.file);
    // The day is a fixed 10 characters, so the key cannot be ambiguous whatever the HN holds.
    const key = `${visitDate}|${p.file.hn}`;
    const got = byKey.get(key);
    if (!got) {
      byKey.set(key, { visit: { hn: p.file.hn, visitDate, photos: [p] }, first: p.file });
      continue;
    }
    got.visit.photos.push(p);
    if (takenMs(p.file) < takenMs(got.first)) got.first = p.file;
  }
  return [...byKey.values()]
    .map(({ visit, first }) => ({ ...visit, doctorCode: first.doctor_code }))
    .sort((a, b) => compareText(a.visitDate, b.visitDate) || compareText(a.hn, b.hn));
}

/**
 * buildInsertSQL renders the rows for the images that were actually shipped, one block per
 * patient-day: the visit they file into (an existing one when V4 has it, a new one when not),
 * the `photos` rows themselves, and a `face_chart` on that visit that turns every imported
 * chart page into a page the doctor can draw on.
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
 *   - visit_id points at the visit of that patient on the image's day (visitDateOf): the one
 *     V4 already holds when there is one, else one this script opens. It used to be NULL
 *     because SaveMedia hard-deletes a visit's rows before rewriting them. That danger is
 *     real and has not gone away — it is now handled at the other end: v4 migration 0420
 *     adds photos.legacy_import and scopes DeleteMediaByVisit to legacy_import = false, so
 *     the replace can no longer reach an imported page. Running this SQL against a database
 *     WITHOUT 0420 is refused by a guard below rather than left to fail on "column does not
 *     exist".
 *
 *     The reason for binding at all: the before/after modal reads media per visit
 *     (ListMediaByVisit → WHERE visit_id = ?), so a page with visit_id NULL cannot appear
 *     in it under any circumstances. Measured on a migrated clinic: 435 of 437 rows had
 *     visit_id NULL and the modal was empty on screen.
 *
 *     A patient with no `patient` row gets NO visit (and so keeps visit_id NULL): a visit
 *     hanging off a patient that does not exist is worse than a page that is only reachable
 *     from the record's photo list. The count is reported.
 *   - kind from the manifest (before|after|note), decided by the CLI from the doctype
 *     family. It is what the modal uses to choose a slot.
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

  // GUARD: without 0420 the rows below would bind pages to a visit while the media lane can
  // still hard-delete them. Refuse with a sentence that names the migration instead of
  // letting Postgres say "column legacy_import does not exist" a thousand rows in.
  out.push("-- GUARD: ปลายทางต้องมี migration 0420 (photos.legacy_import) ก่อน — ถ้าไม่มีแล้วผูก visit");
  out.push("-- การกดบันทึกในโมดัลก่อน-หลังจะลบรูปที่นำเข้าทิ้งถาวร");
  out.push("DO $guard418$");
  out.push("BEGIN");
  out.push("    IF NOT EXISTS (");
  out.push("        SELECT 1 FROM information_schema.columns");
  out.push("         WHERE table_name = 'photos' AND column_name = 'legacy_import'");
  out.push("    ) THEN");
  out.push(
    "        RAISE EXCEPTION 'ฐานปลายทางยังไม่มีคอลัมน์ photos.legacy_import — " +
      "รัน migration 0420 ของ drease-v4-backend ก่อน แล้วนำเข้ารูปอีกครั้ง';",
  );
  out.push("    END IF;");
  out.push("END $guard418$;");
  out.push("");

  // GUARD: since 0716 a deleted visit stays in visit_master with a row in opd_visit_deletion,
  // and the statements below read that table twice — to never file a page into a deleted
  // visit (RLS hides it and every photo bound to it from the app) and to never hand out a VN a
  // deleted visit already held. Refused here in a sentence rather than by "relation does not
  // exist" halfway through.
  out.push("-- GUARD: ปลายทางต้องมี migration 0716 (opd_visit_deletion) ก่อน — visit ที่ถูกลบยังอยู่ใน");
  out.push("-- visit_master จึงต้องอ่านตารางนี้เพื่อไม่ผูกรูปเข้าใบที่ลบแล้ว และไม่ใช้ VN ที่ใบที่ลบเคยถือ");
  out.push("DO $guard716$");
  out.push("BEGIN");
  out.push("    IF to_regclass('public.opd_visit_deletion') IS NULL THEN");
  out.push(
    "        RAISE EXCEPTION 'ฐานปลายทางยังไม่มีตาราง opd_visit_deletion — " +
      "รัน migration 0716 ของ drease-v4-backend ก่อน แล้วนำเข้ารูปอีกครั้ง';",
  );
  out.push("    END IF;");
  out.push("END $guard716$;");
  out.push("");

  // ── one block per patient-day: its visit, its images, its chart ─────────────────────────
  const plan = visitPlan(
    survey.matched.flatMap((m) => {
      const f = byTarget.get(m.target_path);
      return f ? [{ file: f, shipped: m }] : [];
    }),
  );
  const days = new Set(plan.map((v) => v.visitDate)).size;
  const c = lit(clinic);
  const dateLit = (day: string): string => `DATE ${lit(day)}`;

  // The visit a patient-day files into. ONE definition, used by every statement that needs it
  // — "is there one already" before opening a visit, each image's visit_id, the chart, the
  // closing count — because copies that drift would bind an image to one visit and draw its
  // chart on another. It takes SQL expressions so the closing count can hand it columns.
  //
  //   - An existing visit is REUSED (owner's decision, 24 Oct 2026), lowest id first so the
  //     pick is the same on every run. Most days of a migrated clinic already hold the visit
  //     its V2 encounter became.
  //   - A deleted visit is never picked. Since 0716 it stays in visit_master with a row in
  //     opd_visit_deletion, and RLS hides it and every photo bound to it from the app — a page
  //     filed there would vanish.
  //   - A signed visit is never picked. The app changes a signed visit's media or face chart
  //     only through an amendment (opdamend.Guard), so writing into one here would alter a
  //     locked medical record with no amendment trail. A day whose only visits are signed gets
  //     a visit of its own instead.
  const liveVisit = (hnSQL: string, daySQL: string): string =>
    `SELECT lv.id FROM visit_master lv
         WHERE lv.clinic_code = ${c} AND lv."HN" = ${hnSQL} AND lv.visit_date = ${daySQL}
           AND lv.signed_at IS NULL
           AND NOT EXISTS (SELECT 1 FROM opd_visit_deletion dl
                            WHERE dl.clinic_code = lv.clinic_code AND dl.visit_id = lv.id)
         ORDER BY lv.id
         LIMIT 1`;

  // The next VN of a day, allocated the way the app allocates it (opdrecord.sql MaxVnForDay +
  // nextVN): the highest number used that day — deleted visits included, their numbers stay
  // spent ("VN allocations must include this ledger", migration 0716) — plus one, 3 digits.
  //
  // Compared as NUMBERS where the app compares text. The app can: it only ever numbers today,
  // in fixed-width VNs it wrote itself. This numbers past days whose VNs came from elsewhere —
  // 9,494 of Anjali's 36,369 visits are not three characters wide — and as text '99' > '100'.
  //
  // Padded only up to 999, then written as is, because PostgreSQL's lpad TRUNCATES:
  // lpad('1000', 3, '0') is '100', the VN of visit 100 (met twice on 18 Sep 2026 — HN
  // 016900214, visit 1,000 of 1,533).
  const nextVN = (day: string): string =>
    `(SELECT CASE WHEN q.n <= 999 THEN lpad(q.n::text, 3, '0') ELSE q.n::text END
          FROM (SELECT coalesce(max(a.vn::bigint), 0) + 1 AS n
                  FROM (SELECT vm."VN"::text AS vn FROM visit_master vm
                         WHERE vm.clinic_code = ${c} AND vm.visit_date = ${dateLit(day)}
                        UNION ALL
                        SELECT dl.vn FROM opd_visit_deletion dl
                         WHERE dl.clinic_code = ${c} AND dl.visit_date = ${dateLit(day)}) a
                 WHERE a.vn ~ '^[0-9]{1,18}$') q)`;

  // The app's own visit-number lock (opdrecord.sql LockVisitDay), so a visit this opens for
  // TODAY cannot be handed the VN a receptionist is being handed at that moment.
  //
  // Taken only for a day that can still be today. The app numbers today and nothing else —
  // OpenVisit is its only writer of visit_master, and it uses the clinic's current day — so no
  // other day has anyone to race. Locking every day would not be free either: advisory locks
  // share the lock table with relation locks and are held until COMMIT, and a clinic with
  // years of pages has well over a thousand days against a budget of a few thousand locks (the
  // CLI's guards.go: the patient import has run within 1% of it). current_date - 1 because
  // current_date is the server's day (UTC on production) and the clinic's today can be a day
  // ahead of it.
  //
  // The key is the literal string the app hashes ('opd_vn:<clinic>:<YYYY-MM-DD>'), not a date
  // cast to text, whose form would follow DateStyle.
  const lockDay = (day: string): string =>
    `DO $lock$
BEGIN
    IF ${dateLit(day)} >= current_date - 1 THEN
        PERFORM pg_advisory_xact_lock(hashtext(${lit(`opd_vn:${clinic}:${day}`)}));
    END IF;
END $lock$;`;

  out.push("-- ══════════════════════════════════════════════════════════════════════════");
  out.push(`-- รูปแยกตามวันของ visit — ${plan.length} ชุด (คนไข้ × วัน) ใน ${days} วัน`);
  out.push("--");
  out.push("-- วันของรูป = วันตรวจของ V2 (page.vd_first) ถ้าหน้านั้นไม่มีจึงใช้วันที่สแกนตามเวลาไทย");
  out.push("-- (CLI เป็นคนตัดสิน) · แต่ละชุดทำสามอย่างตามลำดับ:");
  out.push("--   1) visit ของคนไข้ในวันนั้น — V4 มีอยู่แล้วใช้ใบเดิม (id ต่ำสุด ข้ามใบที่ลบหรือเซ็นแล้ว)");
  out.push('--      ไม่มีจึงเปิดใบใหม่ "นำเข้าจากระบบเดิม" และเปิดเฉพาะเมื่อมีรูปใหม่จริงอย่างน้อย 1 รูป');
  out.push("--      รันซ้ำจึงไม่ได้ visit เปล่า");
  out.push("--   2) รูปของชุดนั้น ผูกกับ visit ข้อ 1");
  out.push("--   3) ผังหน้าของ visit นั้นจากรูปแฟ้ม (kind note) เท่านั้น — base_type ชี้หน้าที่ใหม่สุด");
  out.push("--      strokes ไล่ทุกหน้าตามเวลาถ่าย (รูปแบบที่แอปเขียนเอง) · ถ้า visit มีผังอยู่แล้วไม่เขียนทับ");
  out.push("--      รูปก่อน/หลังไม่เข้าแฟ้ม แต่ไปอยู่หน้ารูปก่อน-หลังตามวันของ visit");
  out.push("--");
  out.push("-- VN ของใบใหม่ = เลขสูงสุดของวันนั้น (รวมใบที่ถูกลบ) + 1 เทียบเป็นตัวเลข เติมศูนย์ถึง 999");
  out.push("-- แล้วใช้ตัวเลขตรง ๆ เพราะ lpad ของ PostgreSQL ตัดสตริงที่ยาวเกิน: lpad('1000',3,'0') = '100'");
  out.push("-- ล็อกเลข VN ของแอปเฉพาะวันที่ยังอาจเป็นวันนี้ — แอปออกเลขให้วันนี้เท่านั้น");
  out.push("-- ══════════════════════════════════════════════════════════════════════════");
  out.push("");

  let rows = 0;
  let lockedDay = "";
  for (const v of plan) {
    const hn = lit(v.hn);
    const day = dateLit(v.visitDate);

    if (v.visitDate !== lockedDay) {
      out.push(`-- ── ${v.visitDate} ──`);
      out.push(lockDay(v.visitDate));
      out.push("");
      lockedDay = v.visitDate;
    }

    // Opened only when the patient-day has no visit to reuse AND at least one of its images is
    // not in photos yet. The second half is what keeps a re-run from leaving an empty visit in
    // a patient's history: an image imported last time is skipped by the duplicate key below,
    // so a visit opened for a day made only of those would hold nothing. The VALUES list is
    // that same key, image by image.
    //
    // A patient whose branch is blank is SKIPPED rather than allowed to abort the run (owner's
    // decision, 16 Sep 2026): visit_master.branch_code is NOT NULL with CHECK (branch_code <>
    // ''), so one bad patient row would take every image down with it. `patient` carries the
    // same constraints, so today this filters nothing; it costs one comparison.
    const keys = v.photos
      .map((p) => `           (${lit(p.file.taken_at)}::timestamptz, ${p.shipped.bytes})`)
      .join(",\n");
    out.push(`-- hn ${v.hn} · ${v.visitDate} · ${v.photos.length} รูป`);
    out.push(
      `INSERT INTO visit_master (clinic_code, "HN", "VN", visit_date, branch_code, doctor_code, title)
SELECT ${c}, ${hn},
       ${nextVN(v.visitDate)},
       ${day}, p.branch_code,
       ${nullLit(v.doctorCode)}, ${lit(IMPORT_VISIT_TITLE)}
  FROM patient p
 WHERE p.clinic_code = ${c} AND p."HN" = ${hn}
   AND coalesce(p.branch_code, '') <> ''
   AND NOT EXISTS (${liveVisit(hn, day)})
   AND EXISTS (
       SELECT 1 FROM (VALUES
${keys}
       ) AS w(taken_at, file_size)
        WHERE NOT EXISTS (
            SELECT 1 FROM photos ph
             WHERE ph.clinic_code = ${c} AND ph.hn = ${hn}
               AND ph.taken_at = w.taken_at AND ph.file_size = w.file_size)
   );`,
    );
    out.push("");

    for (const { file: f, shipped: m } of v.photos) {
      rows++;
      const taken = `${lit(f.taken_at)}::timestamptz`;
      const size = String(m.bytes);
      out.push(`-- hn ${f.hn} · ${f.source_url}`);
      out.push(
        `INSERT INTO photos (clinic_code, hn, branch_code, visit_id, area_tag, kind, file_path,
                    thumb_path, file_size, consent_given, note, doctor_code, created_by,
                    taken_at, annotations, legacy_import, created_at, updated_at)
SELECT ${c}, ${hn},
       (SELECT p.branch_code FROM patient p WHERE p.clinic_code = ${c} AND p."HN" = ${hn}),
       (${liveVisit(hn, day)}),
       ${nullLit(f.area_tag) === "NULL" ? lit("other") : lit(f.area_tag)}, ${lit(kindOf(f))},
       ${lit(f.target_path)}, ${m.thumb ? lit(m.thumb.target_path) : "NULL"},
       ${size}, true, ${nullLit(f.note)}, ${nullLit(f.doctor_code)}, ${intOrNull(f.created_by)}, ${taken},
       '[]'::jsonb, true, now(), now()
WHERE NOT EXISTS (
    SELECT 1 FROM photos
     WHERE clinic_code = ${c} AND hn = ${hn}
       AND taken_at = ${taken} AND file_size = ${size}
);`,
      );
      out.push("");
    }

    // ── face_chart: the day's chart pages as pages the doctor can draw on ──
    //
    // base_type = 'photo:<photos.id>' and a strokes element {"page":true,"view":"photo:<id>"}
    // are the app's OWN convention, not something devised here: verified against rows the
    // running app wrote (8 of 8 face_chart rows shaped photo:<n> point at a photos.id of the
    // same clinic, HN and visit), and normalizeFaceView (clinicalengine/service.go) accepts
    // photo:<positive int> as a page — TestNormalizeFaceViewKeepsEveryPage pins "photo:42".
    //
    // face_chart is UNIQUE (clinic_code, visit_id): one chart per visit, listing that visit's
    // imported pages in the order they were taken. base_type holds one value and names the
    // NEWEST (owner's decision, 15 Sep 2026). Only chart pages — kind note — go in (owner's
    // decision, 24 Oct 2026): a before/after photo is not a page of the record, and the
    // before/after page shows it by its visit's day instead. A day with no chart page in it
    // gets no chart statement at all.
    //
    // Computed in SQL because photos.id is generated by the inserts just above; reading it
    // back is exact, carrying it across would be a guess. ON CONFLICT DO NOTHING, not DO
    // UPDATE: when the visit already has a chart — a doctor's drawing on a reused visit — the
    // import must not overwrite it. Those pages stay reachable from the visit's photo strip,
    // and the closing count says how many.
    if (!v.photos.some((p) => kindOf(p.file) === "note")) continue;
    out.push(`-- ผังหน้า hn ${v.hn} · ${v.visitDate}`);
    out.push(
      `INSERT INTO face_chart (clinic_code, "HN", visit_id, base_type, doctor_code, strokes,
                        version, created_at, updated_at)
SELECT ${c}, ${hn}, vm.id,
       'photo:' || (SELECT p.id FROM photos p
                     WHERE p.clinic_code = ${c} AND p.visit_id = vm.id
                       AND p.legacy_import AND p.deleted_at IS NULL AND p.kind = 'note'
                     ORDER BY p.taken_at DESC, p.id DESC
                     LIMIT 1)::text,
       vm.doctor_code,
       (SELECT coalesce(jsonb_agg(jsonb_build_object('page', true, 'view', 'photo:' || p.id::text)
                                  ORDER BY p.taken_at, p.id), '[]'::jsonb)
          FROM photos p
         WHERE p.clinic_code = ${c} AND p.visit_id = vm.id
           AND p.legacy_import AND p.deleted_at IS NULL AND p.kind = 'note'),
       1, now(), now()
  FROM visit_master vm
 WHERE vm.id = (${liveVisit(hn, day)})
   AND EXISTS (
       SELECT 1 FROM photos p
        WHERE p.clinic_code = ${c} AND p.visit_id = vm.id
          AND p.legacy_import AND p.deleted_at IS NULL AND p.kind = 'note')
ON CONFLICT (clinic_code, visit_id) DO NOTHING;`,
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
  out.push("    n         bigint;");
  out.push("    total     bigint;");
  out.push("    noPatient bigint;");
  out.push("    noBranch  bigint;");
  out.push("BEGIN");
  out.push("    SELECT count(*) FILTER (WHERE branch_code IS NULL), count(*)");
  out.push("      INTO n, total");
  out.push(`      FROM photos WHERE clinic_code = ${lit(clinic)};`);
  // Why the two reasons are separated: a missing `patient` row is fixed by importing the
  // patients group first, a blank branch is a data problem in the source. Counted here rather
  // than in the visit block above because that block runs BEFORE these rows exist.
  out.push("    SELECT count(*) FILTER (WHERE p.id IS NULL),");
  out.push("           count(*) FILTER (WHERE p.id IS NOT NULL AND coalesce(p.branch_code, '') = '')");
  out.push("      INTO noPatient, noBranch");
  out.push("      FROM photos ph");
  out.push('      LEFT JOIN patient p ON p.clinic_code = ph.clinic_code AND p."HN" = ph.hn');
  out.push(`     WHERE ph.clinic_code = ${lit(clinic)} AND ph.legacy_import AND ph.visit_id IS NULL;`);
  out.push("    IF noPatient > 0 OR noBranch > 0 THEN");
  out.push(
    "        RAISE NOTICE 'รูปที่ไม่ได้ผูก visit: [HN ไม่มีในตาราง patient: %, คนไข้ไม่มีสาขา: %] " +
      "— รูปเข้าฐานแล้วแต่เปิดในโมดัลก่อน-หลังไม่ได้ · นำเข้ากลุ่ม patients ก่อนแล้วรันรูปซ้ำจะผูกให้', " +
      "noPatient, noBranch;",
  );
  out.push("    END IF;");
  out.push("    IF noBranch > 0 THEN");
  out.push(
    "        RAISE NOTICE 'ข้ามคนไข้ที่ไม่มีสาขา (รูป % ใบ) — visit_master.branch_code ห้ามว่าง " +
      "จึงไม่สร้างใบให้ แทนที่จะล้มทั้งการนำเข้า · เลขนี้ควรเป็น 0 เพราะตาราง patient เองก็มี CHECK " +
      "ห้ามสาขาว่าง ถ้าไม่ใช่ 0 แปลว่า constraint นั้นถูกถอดไปแล้ว', noBranch;",
  );
  out.push("    END IF;");
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

  // What the plan came to, counted after every statement above has run, so a shortfall is read
  // off the log rather than found by a doctor opening an empty visit. It goes through the same
  // liveVisit with the plan's own columns, so it counts exactly the visits the rows went to.
  //
  //   - bound / opened: patient-days that found a visit, and how many of those visits this run
  //     opened. A visit opened here takes visit_master.created_at's default, now(), which is
  //     this transaction's start and nobody else's; the rest were reused, as the owner chose.
  //   - paired: before/after photos now on a visit, i.e. placeable on the before/after page.
  //   - inChart / outChart: chart pages listed in their visit's chart, and those that are not
  //     because the visit already had a chart of its own (ON CONFLICT DO NOTHING above).
  if (plan.length > 0) {
    const planRows = plan.map((v) => `        (${lit(v.hn)}, ${dateLit(v.visitDate)})`).join(",\n");
    const listed =
      "pg.strokes @> jsonb_build_array(jsonb_build_object('page', true, 'view', 'photo:' || pg.id::text))";
    out.push("-- visit · รูปก่อน/หลัง · ผังหน้า ที่ได้จริงของแผนนี้");
    out.push("DO $plan$");
    out.push("DECLARE");
    out.push("    bound    bigint;");
    out.push("    opened   bigint;");
    out.push("    paired   bigint;");
    out.push("    inChart  bigint;");
    out.push("    outChart bigint;");
    out.push("BEGIN");
    out.push(`    WITH pl(hn, visit_date) AS (VALUES
${planRows}
    ),
    v AS (
        SELECT x.id, x.created_at
          FROM pl
          JOIN visit_master x ON x.id = (${liveVisit("pl.hn", "pl.visit_date")})
    ),
    pg AS (
        SELECT p.id, p.kind, fc.strokes
          FROM v
          JOIN photos p ON p.clinic_code = ${c} AND p.visit_id = v.id
                       AND p.legacy_import AND p.deleted_at IS NULL
          LEFT JOIN face_chart fc ON fc.clinic_code = ${c} AND fc.visit_id = v.id
    )
    SELECT (SELECT count(*) FROM v),
           (SELECT count(*) FROM v WHERE v.created_at = now()),
           (SELECT count(*) FROM pg WHERE pg.kind IN ('before', 'after')),
           (SELECT count(*) FROM pg WHERE pg.kind = 'note' AND ${listed}),
           (SELECT count(*) FROM pg WHERE pg.kind = 'note' AND (pg.strokes IS NULL OR NOT ${listed}))
      INTO bound, opened, paired, inChart, outChart;`);
    out.push(
      `    RAISE NOTICE 'visit: แผน ${plan.length} ชุด (คนไข้ × วัน) · ผูก visit ได้ % ชุด — ` +
        "ใช้ใบที่มีอยู่แล้ว % ใบ · เปิดใบใหม่ % ใบ', bound, bound - opened, opened;",
    );
    out.push(`    IF bound < ${plan.length} THEN`);
    out.push(
      "        RAISE NOTICE 'visit: % ชุดไม่มี visit — คนไข้ยังไม่มีในตาราง patient, ไม่มีสาขา " +
        "หรือรูปทุกใบของวันนั้นมีอยู่แล้วจากรอบก่อน (จึงไม่เปิดใบเปล่า) · " +
        `รูปที่เข้าฐานแต่ไม่มี visit นับแยกไว้ในบล็อกสาขาด้านบน', ${plan.length} - bound;`,
    );
    out.push("    END IF;");
    out.push(
      "    RAISE NOTICE 'ก่อน/หลัง: % รูปผูก visit ตามวันแล้ว — หน้ารูปก่อน-หลังวางตามวันของ visit', paired;",
    );
    out.push("    RAISE NOTICE 'face_chart: หน้ารูปแฟ้มอยู่ในผังหน้าของ visit แล้ว % หน้า', inChart;");
    out.push("    IF outChart > 0 THEN");
    out.push(
      "        RAISE NOTICE 'face_chart: % หน้ายังไม่อยู่ในผังหน้า — visit นั้นมีผังหน้าอยู่ก่อนแล้ว " +
        "จึงไม่เขียนทับ · รูปยังเปิดได้จากแถบรูปของ visit', outChart;",
    );
    out.push("    END IF;");
    out.push("END $plan$;");
    out.push("");
  }
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
