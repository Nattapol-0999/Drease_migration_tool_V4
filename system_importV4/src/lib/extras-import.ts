import "server-only";
// The clinic logo and the patients' profile photos: bytes only, no rows.
//
// WHY THIS IS A SEPARATE FILE FROM photo-import.ts
//
// The page-image path writes MEDICAL RECORDS — photos, visit_master, face_chart — and it took
// three production failures to get working. These two trees write nothing to the database at
// all. Folding them into shipFiles() would put untested code inside the one function whose
// failure leaves a clinic's records half-imported, for no gain: they share no manifest, no
// survey, no SQL and no ordering constraint.
//
// WHAT "NO ROWS" MEANS HERE
//
// V4 has no patient-profile-photo feature and no clinic-logo file field (the logo it does know
// about is a URL string in the clinic settings blob, used on the printed receipt header).
// Owner's decision, 18 Sep 2026: park the FILES in the uploads tree under a predictable path
// and write nothing, so whoever builds the feature later has the bytes already in place.
// Writing the settings logoUrl was explicitly NOT chosen — that is the tax-document header.
//
// So there is no ordering question of the kind photo-import.ts has to answer. Nothing here can
// leave a row pointing at a byte that is not there, because nothing here writes a row.
import { execFile } from "node:child_process";
import { mkdtemp, mkdir, rm, stat, copyFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { basename, dirname, join } from "node:path";
import { promisify } from "node:util";

import { extractArchive, cleanup, type UploadsTarget } from "@/lib/photo-import";
import { psqlCommand, runSSH, shellQuote, sshArgs, type ServerTarget } from "@/lib/server-target";
// The result shapes live in types.ts, not here: the browser renders these counters, and a
// server-only module is not something a client component can import even for a type.
import type { ExtrasLogoResult, ExtrasProfilesResult, ExtrasResult } from "@/lib/types";

const run = promisify(execFile);

/** Same reasoning as photo-import.ts: /tmp on the deployed box is a small tmpfs. */
function workRoot(): string {
  return process.env.UPLOAD_STAGING_DIR?.trim() || tmpdir();
}

/**
 * The uploads subfolder each tree lands in, under <uploads dir>/<clinic_code>/.
 * profile-image is the path the owner specified; clinic-logo mirrors it.
 */
const PROFILE_SUBDIR = "profile-image";
const LOGO_SUBDIR = "clinic-logo";

/**
 * Which file in img/clinic-logo/ is the clinic's logo.
 *
 * The measured source has TWO files — 01.png and T-STAR-logo.png — and nothing in the V2 dump
 * references either name, so this cannot be derived. Owner chose 01.png (18 Sep 2026), whose
 * name matches the legacy branch code `01` that the branch map is keyed on. Overridable
 * because the next clinic's folder will not necessarily agree.
 */
function logoFileName(): string {
  return process.env.LEGACY_LOGO_FILE?.trim() || "01.png";
}

/**
 * HN shapes we are willing to put in a SQL literal and in a path.
 *
 * These names come off a filesystem on a machine we do not control, and they end up in both a
 * query and a file path, so the check is an allow-list rather than an escape. The measured
 * names are 9 digits (016500001.jpg); letters, dash and underscore are allowed because other
 * V2 installs number patients differently. Anything else — a quote, a slash, "..", a space —
 * is not an HN and is counted as a bad name instead of being cleaned up into something else.
 */
const HN_RE = /^[0-9A-Za-z_-]{1,32}$/;

/** Nothing to do is a normal outcome, not an error: tab 1 never stages these. */
function empty(): ExtrasResult {
  return { logo: null, profiles: null, filesSent: 0, bytesSent: 0, log: "" };
}

/**
 * shipExtras copies the two trees to the uploads host and writes nothing to the database.
 *
 * `logoTar` / `profilesTar` are staged archive paths, or null when that tree was not pulled.
 * The database target is needed for one READ ONLY: which of the profile filenames correspond
 * to a patient that actually exists in this clinic.
 */
export async function shipExtras(opts: {
  uploads: UploadsTarget;
  dbTarget: ServerTarget;
  clinic: string;
  logoTar: string | null;
  profilesTar: string | null;
}): Promise<ExtrasResult> {
  const { uploads, dbTarget, clinic } = opts;
  if (!opts.logoTar && !opts.profilesTar) return empty();

  const logLines: string[] = [];
  const stage = await mkdtemp(join(workRoot(), "importv4-extras-"));
  const payload = join(stage, "payload");
  const bundle = join(stage, "bundle.tar.gz");
  const extracted: string[] = [];

  let logo: ExtrasLogoResult | null = null;
  let profiles: ExtrasProfilesResult | null = null;
  let filesSent = 0;
  let bytesSent = 0;

  try {
    // ── the logo ────────────────────────────────────────────────────────────────────────
    if (opts.logoTar) {
      const root = await extractArchive(opts.logoTar);
      extracted.push(root);
      const wanted = logoFileName();
      const all = await listFiles(root);
      const hit = all.find((p) => basename(p) === wanted) ?? null;
      const available = all.map((p) => basename(p)).sort();

      if (hit) {
        const size = (await stat(hit)).size;
        const dest = join(payload, clinic, LOGO_SUBDIR, wanted);
        await mkdir(dirname(dest), { recursive: true });
        await copyFile(hit, dest);
        filesSent += 1;
        bytesSent += size;
        logo = { wanted, found: true, bytes: size, available };
        logLines.push(`โลโก้: ${wanted} (${kb(size)}) → ${clinic}/${LOGO_SUBDIR}/`);
      } else {
        // Not fatal, and named rather than shrugged at: the operator can set
        // LEGACY_LOGO_FILE to one of the names printed here.
        logo = { wanted, found: false, bytes: 0, available };
        logLines.push(
          `โลโก้: ไม่พบ ${wanted} ในถุง — ไฟล์ที่มี: ${available.join(", ") || "(ไม่มีเลย)"}`,
        );
      }
    }

    // ── the profile photos ──────────────────────────────────────────────────────────────
    if (opts.profilesTar) {
      const root = await extractArchive(opts.profilesTar);
      extracted.push(root);
      const all = await listFiles(root);

      const byHN = new Map<string, string>();
      let skippedBadName = 0;
      for (const p of all) {
        const hn = basename(p).replace(/\.[^.]*$/, "");
        if (!HN_RE.test(hn)) {
          skippedBadName++;
          continue;
        }
        // Last one wins if a clinic somehow keeps <hn>.jpg and <hn>.png side by side. Which
        // one is arbitrary, and picking arbitrarily is honest here: there is no rule to apply
        // and no row whose meaning could change.
        byHN.set(hn, p);
      }

      const wantedHNs = [...byHN.keys()];
      const present = wantedHNs.length > 0 ? await hnsThatExist(dbTarget, clinic, wantedHNs) : new Set<string>();

      let sent = 0;
      let bytes = 0;
      const skippedSample: string[] = [];
      for (const [hn, src] of byHN) {
        if (!present.has(hn)) {
          if (skippedSample.length < 10) skippedSample.push(hn);
          continue;
        }
        const size = (await stat(src)).size;
        const dest = join(payload, clinic, PROFILE_SUBDIR, basename(src));
        await mkdir(dirname(dest), { recursive: true });
        await copyFile(src, dest);
        sent++;
        bytes += size;
      }

      filesSent += sent;
      bytesSent += bytes;
      profiles = {
        candidates: wantedHNs.length,
        sent,
        skippedNoPatient: wantedHNs.length - sent,
        skippedBadName,
        bytes,
        skippedSample,
      };
      logLines.push(
        `รูปโปรไฟล์: ส่ง ${sent}/${wantedHNs.length} ไฟล์ (${kb(bytes)}) → ${clinic}/${PROFILE_SUBDIR}/` +
          (profiles.skippedNoPatient > 0 ? ` · ข้าม ${profiles.skippedNoPatient} (ไม่มี HN ในตาราง patient)` : "") +
          (skippedBadName > 0 ? ` · ข้าม ${skippedBadName} (ชื่อไฟล์ไม่ใช่ HN)` : ""),
      );
    }

    if (filesSent === 0) {
      // Nothing matched. Not an error — a clinic with no logo file and no profile photo of any
      // imported patient is a real and unremarkable case.
      logLines.push("ไม่มีไฟล์ที่ต้องส่ง จึงไม่ได้แตะเครื่องปลายทาง");
      return { logo, profiles, filesSent: 0, bytesSent: 0, log: logLines.join("\n") };
    }

    await run("tar", ["-czf", bundle, "-C", payload, "."], { maxBuffer: 8 * 1024 * 1024 });

    const remoteTmp = `/tmp/importv4-extras-${Date.now()}.tar.gz`;
    await run(
      "scp",
      [
        "-i", uploads.sshKey,
        "-P", String(uploads.sshPort),
        "-o", "BatchMode=yes",
        "-o", "StrictHostKeyChecking=accept-new",
        "-o", "ConnectTimeout=15",
        "-o", "IdentitiesOnly=yes",
        bundle,
        `${uploads.sshUser}@${uploads.sshHost}:${remoteTmp}`,
      ],
      { maxBuffer: 8 * 1024 * 1024 },
    );

    // Same shape as shipFiles: the uploads tree is root-owned, so every step is sudo -n, and
    // the chown/chmod are scoped to the two directories this touched rather than the tree.
    const dirs = [
      join(uploads.dir, clinic, LOGO_SUBDIR).replaceAll("\\", "/"),
      join(uploads.dir, clinic, PROFILE_SUBDIR).replaceAll("\\", "/"),
    ].map(shellQuote);
    const remote = [
      `set -e`,
      ...dirs.map((d) => `sudo -n mkdir -p ${d}`),
      `sudo -n tar --no-same-owner --no-same-permissions -xzf ${shellQuote(remoteTmp)} -C ${shellQuote(uploads.dir)}`,
      ...dirs.map((d) => `sudo -n chown -R root:root ${d}`),
      ...dirs.map((d) => `sudo -n find ${d} -type d -exec chmod 755 {} +`),
      ...dirs.map((d) => `sudo -n find ${d} -type f -exec chmod 644 {} +`),
      `rm -f ${shellQuote(remoteTmp)}`,
      `find ${dirs.join(" ")} -type f | wc -l`,
    ].join(" && ");

    const { stdout, stderr } = await run(
      "ssh",
      [
        "-i", uploads.sshKey,
        "-p", String(uploads.sshPort),
        "-o", "BatchMode=yes",
        "-o", "StrictHostKeyChecking=accept-new",
        "-o", "ConnectTimeout=15",
        "-o", "IdentitiesOnly=yes",
        `${uploads.sshUser}@${uploads.sshHost}`,
        remote,
      ],
      { maxBuffer: 8 * 1024 * 1024 },
    );

    logLines.push(`ไฟล์ในสองโฟลเดอร์ปลายทางหลังแตกไฟล์: ${stdout.trim()}`);
    if (stderr.trim()) logLines.push(stderr.trim());

    return { logo, profiles, filesSent, bytesSent, log: logLines.join("\n") };
  } finally {
    await cleanup(...extracted);
    await rm(stage, { recursive: true, force: true }).catch(() => {});
  }
}

/**
 * hnsThatExist asks the target database which of these HNs this clinic actually has.
 *
 * WHY ASK AT ALL (owner's choice C2, 18 Sep 2026)
 *
 * A V2 folder can hold a profile photo for a patient the import skipped — a row with no HN, a
 * patient outside the imported branches. Copying it would put a stranger's photograph in this
 * clinic's uploads tree under a filename that suggests it belongs to someone. Asking first
 * costs one query and makes the skipped count reportable.
 *
 * The HNs are checked against HN_RE before they get here, so the IN list cannot carry a quote.
 * The psql \pset lines are needed because psqlCommand runs `-f -` without -tA: without them
 * the answer comes back as an ASCII table with a header and a row count.
 */
async function hnsThatExist(t: ServerTarget, clinic: string, hns: string[]): Promise<Set<string>> {
  const safeClinic = clinic.replaceAll("'", "''");
  const list = hns.map((h) => `'${h}'`).join(", ");
  const sql = [
    `\\pset tuples_only on`,
    `\\pset format unaligned`,
    `SELECT p."HN" FROM patient p`,
    ` WHERE p.clinic_code = '${safeClinic}'`,
    `   AND p."HN" IN (${list});`,
    ``,
  ].join("\n");

  const r = await runSSH(sshArgs(t, psqlCommand(t)), sql);
  if (r.code !== 0) {
    // Loud on purpose. Coming back with an empty set would ship nothing and report "0 of 5
    // matched", which reads as clean data rather than as a query that never ran.
    throw new Error(
      `ตรวจ HN กับตาราง patient ไม่สำเร็จ (exit ${r.code})` +
        (r.stderr.trim() ? ` — ${r.stderr.trim().slice(0, 300)}` : ""),
    );
  }
  return new Set(
    r.stdout
      .split(/\r?\n/)
      .map((s) => s.trim())
      .filter(Boolean),
  );
}

/** Every regular file under root, absolute paths. */
async function listFiles(root: string): Promise<string[]> {
  const { stdout } = await run("find", [root, "-type", "f"], { maxBuffer: 64 * 1024 * 1024 });
  return stdout.split("\n").map((s) => s.trim()).filter(Boolean);
}

function kb(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}
