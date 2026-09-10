import "server-only";

import { execFile } from "node:child_process";
import { copyFile, open, rm, stat } from "node:fs/promises";
import { join } from "node:path";
import { promisify } from "node:util";

import { requireSession } from "@/lib/require-session";
import { describeTrim, trimMongoDump } from "@/lib/mongo-trim";
import { startJob, type JobHandle } from "@/lib/jobs";
import { dropSession, stagedFields, stagedPath, sessionExists } from "@/lib/upload-staging";
import { MAX_CLI_INPUT_BYTES, MONGO_COLLECTIONS_USED } from "@/lib/types";

const run = promisify(execFile);

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// clinic_code and owner_username are passed to a child process. execFile takes an ARGV
// array (no shell), so there is no interpolation to escape — but they are still
// validated, because a value that cannot be a real code is a mistake worth catching
// before a file is generated for the wrong tenant.
const CLINIC_RE = /^[A-Za-z0-9_-]{1,64}$/;
const OWNER_RE = /^[\w.@-]{1,64}$/;

/** A V4 branch_code as the backend allocates it. Matches the varchar(32) column. */
const BRANCH_CODE_RE = /^[A-Za-z0-9_-]{1,32}$/;

/**
 * Parses and checks the branch map the browser assembled from the provision answer.
 *
 * The map is the whole reason the multi-branch path works: it says which V4 branch each
 * legacy code means, and the CLI stamps every patient, bill, room and stock lot with the
 * answer. A map that is wrong in a way nobody notices files a branch's entire history
 * under another branch, and no constraint in the database will object — branch_code is a
 * plain varchar with no foreign key. So every part of it is checked here.
 *
 * Returns the argument to pass through, or an error message.
 */
function checkBranchMap(
  raw: string,
  defaultBranch: string,
): { map: string; def: string } | { error: string } {
  const map = raw.trim();
  const def = defaultBranch.trim();

  if (!map) {
    // No map is a legitimate request: it is the original single-branch behaviour, where
    // every row is stamped with the CLI's own constant. A default branch on its own would
    // be meaningless, so it is refused rather than quietly ignored.
    if (def) {
      return {
        error: "ส่ง default-branch มาโดยไม่มี branch-map — ถ้าไม่มีแผนที่สาขา ทุกแถวจะไปสาขาเดียวอยู่แล้ว",
      };
    }
    return { map: "", def: "" };
  }

  const targets = new Set<string>();
  const sources = new Set<string>();
  for (const pair of map.split(",")) {
    const eq = pair.indexOf("=");
    if (eq < 0) {
      return { error: `branch-map ผิดรูปที่ ${JSON.stringify(pair)} — ต้องเป็น รหัสเดิม=BRxxxxx` };
    }
    const source = pair.slice(0, eq).trim();
    const target = pair.slice(eq + 1).trim();
    if (!source) return { error: `branch-map มีคู่ที่ไม่มีรหัสสาขาเดิม: ${JSON.stringify(pair)}` };
    if (!BRANCH_CODE_RE.test(target)) {
      return { error: `branch-map มีรหัสสาขา V4 ที่ไม่ถูกรูป: ${JSON.stringify(target)}` };
    }
    if (sources.has(source)) {
      return { error: `branch-map มีรหัสสาขาเดิม ${JSON.stringify(source)} ซ้ำ — ชี้ได้ที่เดียว` };
    }
    sources.add(source);
    targets.add(target);
  }

  // The default is where rows with no branch, and rows whose legacy code is a typo, are
  // filed. Pointing it at a branch that is not in the map means those rows get a
  // branch_code no clinic row has — orphans that every screen filtering by branch hides.
  if (def && !targets.has(def)) {
    return {
      error:
        `default-branch ${JSON.stringify(def)} ไม่อยู่ในสาขาที่ส่งมาใน branch-map ` +
        `(${[...targets].join(", ")}) — แถวที่หาสาขาไม่ได้จะถูกเขียนไปที่สาขาที่ไม่มีจริง`,
    };
  }

  return { map, def };
}

/** How much of the script the UI is shown. Enough to review the header, not the data. */
const PREVIEW_BYTES = 8 * 1024;

function fail(error: string, status = 400, extra: Record<string, unknown> = {}): Response {
  return Response.json({ ok: false, error, ...extra }, { status });
}

const mb = (n: number) => `${(n / 1048576).toFixed(1)} MB`;

/**
 * Starts script generation and answers with a job id.
 *
 * It does not wait for the work: a real clinic takes minutes (measured at 347 seconds for
 * a 897 MB input), and Cloudflare closes a proxied request that has not answered in 100
 * seconds. The browser polls /api/job instead.
 *
 * The dumps are already on disk from /api/upload-chunk, so no request body carries them,
 * and the Mongo dump is filtered before the CLI sees it (lib/mongo-trim) because the CLI
 * loads its whole input into memory.
 */
export async function POST(req: Request): Promise<Response> {
  const auth = await requireSession(req);
  if ("denied" in auth) return auth.denied;

  const cli = process.env.IMPORT_CLI_PATH;
  if (!cli) {
    return fail(
      "ยังไม่ได้ตั้งค่า IMPORT_CLI_PATH — ชี้ไปที่ไฟล์ system_cliv4.exe แล้วรีสตาร์ต dev server",
      500,
    );
  }

  let body: {
    sessionId?: string;
    clinicCode?: string;
    owner?: string;
    only?: string;
    /** "01=BR00001,02=BR00002" — built by the browser from the provision answer. */
    branchMap?: string;
    /** The V4 branch that rows with no resolvable branch are filed under. */
    defaultBranch?: string;
  };
  try {
    body = (await req.json()) as typeof body;
  } catch {
    return fail("อ่าน body ไม่ได้");
  }

  const clinic = String(body.clinicCode ?? "").trim();
  const owner = String(body.owner ?? "").trim();
  const sessionId = String(body.sessionId ?? "").trim();

  const branch = checkBranchMap(String(body.branchMap ?? ""), String(body.defaultBranch ?? ""));
  if ("error" in branch) return fail(branch.error);

  if (!CLINIC_RE.test(clinic)) {
    return fail("clinic_code ไม่ถูกรูป (ใช้ A-Z a-z 0-9 _ - ยาวไม่เกิน 64)");
  }
  if (owner && !OWNER_RE.test(owner)) {
    return fail("owner_username ไม่ถูกรูป");
  }
  if (!(await sessionExists(sessionId))) {
    return fail("ไม่พบไฟล์ที่อัปโหลดไว้ (session หมดอายุหรือถูกล้าง) — แนบไฟล์ใหม่", 409);
  }

  const staged = await stagedFields(sessionId);
  if (staged.length === 0) {
    return fail("ยังไม่มีไฟล์ dump ในการอัปโหลดนี้");
  }

  const only = String(body.only ?? "")
    .split(",")
    .map((s) => s.trim())
    .filter((s) => /^[a-z_]+$/.test(s));

  // An owner is needed only when accounts are actually being imported. It used to be
  // demanded for any SQL attachment, which meant an operator importing a price list had to
  // name the clinic's owner for no reason — and the CLI would have refused the run anyway
  // over rows in a table that selection does not touch. The CLI now filters by group before
  // it parses; this check matches it so the two cannot disagree.
  const importingPeople = only.length === 0 || only.includes("people");
  if (staged.some((s) => s.field === "sql") && importingPeople && !owner) {
    return fail(
      "กลุ่ม people ต้องระบุ owner_username (legacy users.code ของบัญชีที่จะเป็น owner) — " +
        "หรือติ๊กกลุ่ม people ออกถ้ารอบนี้ยังไม่นำเข้าบุคลากร",
    );
  }

  const jobId = await startJob(
    "generate",
    { user: auth.session.user, clinicCode: clinic, phase: "กำลังเตรียมไฟล์" },
    (h) =>
      generate(h, {
        cli,
        clinic,
        owner,
        sessionId,
        staged,
        only,
        branchMap: branch.map,
        defaultBranch: branch.def,
      }),
  );

  console.log(
    JSON.stringify({
      event: "generate-start",
      at: new Date().toISOString(),
      user: auth.session.user,
      clinic,
      job: jobId,
      inputs: staged.map((s) => `${s.field}:${s.bytes}`),
      // Recorded because it decides where every imported row is filed, and the artifact
      // itself only shows the resulting branch_code, not the map that produced it.
      branchMap: branch.map || "(single branch)",
      defaultBranch: branch.def || "(none)",
    }),
  );

  return Response.json({ ok: true, jobId });
}

async function generate(
  h: JobHandle,
  args: {
    cli: string;
    clinic: string;
    owner: string;
    sessionId: string;
    staged: { field: "sql" | "mongo"; bytes: number }[];
    only: string[];
    branchMap: string;
    defaultBranch: string;
  },
): Promise<void> {
  const { cli, clinic, owner, sessionId, staged, only, branchMap, defaultBranch } = args;
  const cliArgs: string[] = ["-clinic", clinic, "-out", h.dir];
  if (owner) cliArgs.push("-owner", owner);
  if (only.length > 0) cliArgs.push("-only", only.join(","));
  // Without these the CLI stamps every row with its single-branch constant, which is
  // correct for a one-branch tenant and silently wrong for any other.
  if (branchMap) cliArgs.push("-branch-map", branchMap);
  if (defaultBranch) cliArgs.push("-default-branch", defaultBranch);

  try {
    h.addLog(
      branchMap
        ? `แผนที่สาขา: ${branchMap}${defaultBranch ? ` · สาขาตั้งต้นของแถวที่หาสาขาไม่ได้: ${defaultBranch}` : ""}`
        : "ไม่ได้ส่งแผนที่สาขา — ทุกแถวจะไปสาขาเดียว (พฤติกรรมเดิมของคลินิกสาขาเดียว)",
    );
    for (const { field, bytes } of staged) {
      const src = stagedPath(sessionId, field);
      const dst = join(h.dir, field === "mongo" ? "mongo.js" : "sql.sql");

      if (field === "mongo") {
        h.setPhase(`กำลังกรอง collection ที่ไม่ใช้ออกจากไฟล์ MongoDB (${mb(bytes)})`);
        const result = await trimMongoDump(src, dst, MONGO_COLLECTIONS_USED);
        h.addLog(describeTrim(result));

        if (result.keptBytes === 0) {
          throw new Error(
            "ไฟล์ MongoDB ไม่มี collection ที่ระบบนี้อ่านเลย — " +
              `ต้องมีอย่างน้อยหนึ่งใน: ${MONGO_COLLECTIONS_USED.join(", ")}\n` +
              `ที่พบในไฟล์: ${Object.keys(result.dropped).sort().join(", ") || "(ไม่พบ statement insert)"}`,
          );
        }
        if (result.keptBytes > MAX_CLI_INPUT_BYTES) {
          throw new Error(
            `กรองแล้วยังเหลือ ${mb(result.keptBytes)} เกินที่ประมวลผลได้บนเครื่องนี้ ` +
              `(${mb(MAX_CLI_INPUT_BYTES)}) — นำเข้าแยกรอบได้โดยเลือกเฉพาะบางกลุ่มในกล่อง 3`,
          );
        }
      } else {
        // The SQL dump is not filtered: the CLI picks its five tables out of it itself.
        // Only the size is checked, because it is read whole like the Mongo file.
        if (bytes > MAX_CLI_INPUT_BYTES) {
          throw new Error(
            `ไฟล์ SQL ${mb(bytes)} เกินที่ประมวลผลได้บนเครื่องนี้ (${mb(MAX_CLI_INPUT_BYTES)})`,
          );
        }
        h.setPhase(`กำลังเตรียมไฟล์ SQL (${mb(bytes)})`);
        await copyFile(src, dst);
        h.addLog(`ไฟล์ SQL: ${mb(bytes)} (ไม่ต้องกรอง)`);
      }
      cliArgs.push(`-${field}`, dst);
    }

    h.setPhase("CLI กำลังอ่าน dump และสร้างสคริปต์ (ขั้นนี้ใช้เวลานานที่สุด)");
    let report: string;
    try {
      const { stdout } = await run(cli, cliArgs, {
        maxBuffer: 32 * 1024 * 1024,
        windowsHide: true,
      });
      report = stdout;
    } catch (err) {
      // The CLI fails closed and puts the reason on stderr — surface it as-is rather
      // than a generic message, because it names the rows that need fixing.
      const e = err as { stderr?: string; message?: string };
      throw new Error((e.stderr || e.message || "").trim() || "รัน CLI ไม่สำเร็จ");
    }

    const filename = `import_${clinic}.sql`;
    const sqlPath = join(h.dir, filename);
    const sqlBytes = (await stat(sqlPath)).size;

    // Only the head of the script is read. The whole file can be hundreds of megabytes
    // and is never loaded into memory or sent through JSON: it is downloaded or imported
    // straight from disk.
    const fh = await open(sqlPath, "r");
    let preview: string;
    try {
      const buf = Buffer.alloc(Math.min(PREVIEW_BYTES, sqlBytes));
      await fh.read(buf, 0, buf.length, 0);
      preview = buf.toString("utf8");
    } finally {
      await fh.close();
    }

    h.addLog(`สคริปต์ที่ได้: ${filename} · ${mb(sqlBytes)}`);
    h.setResult({ filename, sqlPath, sqlBytes, report, preview });
  } finally {
    // The upload has been consumed (or has failed in a way that needs a new file), and the
    // form opens a new session on every attempt, so keeping it would only leave a
    // multi-gigabyte file on disk until the sweep noticed.
    await dropSession(sessionId);
    // The filtered copies fed to the CLI are dead weight once it has run — the script is
    // the artifact worth keeping. On a 921 MB input these two files are most of the disk
    // this job touches.
    for (const { field } of staged) {
      await rm(join(h.dir, field === "mongo" ? "mongo.js" : "sql.sql"), { force: true });
    }
  }
}
