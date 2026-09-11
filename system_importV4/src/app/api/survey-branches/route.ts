import "server-only";

import { execFile } from "node:child_process";
import { promisify } from "node:util";

import { startJob, type JobHandle } from "@/lib/jobs";
import { requireSession } from "@/lib/require-session";
import { sessionExists, stagedPath, stagedSize } from "@/lib/upload-staging";
import { MAX_CLI_INPUT_BYTES } from "@/lib/types";

const run = promisify(execFile);

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

/**
 * Reads the legacy `branch` table, and the counts that say whether each branch is real.
 *
 * WHY THIS STEP EXISTS AT ALL
 * V4 owns branch identity: a branch_code comes from a sequence and carries a branch_no that
 * every HN issued at that branch keeps for life. The legacy "01" therefore cannot be written
 * through — it has to be TRANSLATED. That translation table can only be built after the
 * branches exist in V4, and they can only be created once somebody has seen what the export
 * actually contains. So the order is: read the dump, let the operator choose, create, then
 * hand the resulting map to the CLI as -branch-map.
 *
 * Skipping it is not a smaller version of the same import. One export held nine branches and
 * 14,264 patients; loaded without a map every one of those patients, and every bill, room and
 * stock lot with them, is filed at a branch they were never at.
 *
 * WHY IT IS A JOB AND NOT AN INLINE ANSWER
 * It used to answer inline, in about a second, because it only read the SQL branch table.
 * That was also the bug: the Mongo dump was never passed, so every count sourced from it —
 * stock lots, bills, receipts, courses bought, duplicate document numbers — came back 0 on a
 * screen that gives no hint the number was never looked up. A branch with 531 lots read as a
 * branch holding nothing, which is exactly the signal an operator uses to decide a branch is
 * a leftover worth leaving out.
 *
 * Passing it costs time: 8m01s for a 921 MB export on this box, against 1.0s for the SQL
 * alone. Cloudflare closes a proxied request at 100 seconds, so the work has to outlive the
 * request. Same machinery as /api/generate — start it, answer with an id, let the browser
 * poll /api/job.
 *
 * It still writes nothing. -survey-branches takes no -clinic and no -out, so this cannot
 * change the target however long it runs.
 */

function fail(error: string, status = 400): Response {
  return Response.json({ ok: false, error }, { status });
}

const mb = (n: number) => `${(n / 1048576).toFixed(1)} MB`;

/**
 * A ceiling well past the 8 minutes measured on the largest export seen, so a survey that
 * hangs is reported rather than left running against a file it cannot finish.
 */
const SURVEY_TIMEOUT_MS = 30 * 60 * 1000;

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

  let body: { sessionId?: string };
  try {
    body = (await req.json()) as typeof body;
  } catch {
    return fail("อ่าน body ไม่ได้");
  }

  const sessionId = String(body.sessionId ?? "").trim();
  if (!(await sessionExists(sessionId))) {
    return fail("ไม่พบไฟล์ที่อัปโหลดไว้ (session หมดอายุหรือถูกล้าง) — แนบไฟล์ใหม่", 409);
  }

  // The branch registry is a MySQL table, so the Mongo half cannot answer the question this
  // step exists for. It can only add the counts.
  const sqlBytes = await stagedSize(sessionId, "sql");
  if (sqlBytes === 0) {
    return fail(
      "ยังไม่มีไฟล์ MySQL ในการอัปโหลดนี้ — ตารางสาขา (branch) อยู่ในไฟล์ .sql ไม่ใช่ไฟล์ MongoDB",
    );
  }
  const mongoBytes = await stagedSize(sessionId, "mongo");

  // Each file is read whole by the CLI, so each is checked against the ceiling on its own.
  for (const [label, bytes] of [
    ["SQL", sqlBytes],
    ["MongoDB", mongoBytes],
  ] as const) {
    if (bytes > MAX_CLI_INPUT_BYTES) {
      return fail(
        `ไฟล์ ${label} ${mb(bytes)} เกินที่ประมวลผลได้บนเครื่องนี้ (${mb(MAX_CLI_INPUT_BYTES)})`,
      );
    }
  }

  const jobId = await startJob(
    "survey",
    {
      user: auth.session.user,
      clinicCode: "",
      phase: mongoBytes > 0 ? `กำลังอ่านไฟล์ (SQL ${mb(sqlBytes)} · Mongo ${mb(mongoBytes)})` : "กำลังอ่านตารางสาขา",
    },
    (h) => survey(h, { cli, sessionId, sqlBytes, mongoBytes }),
  );

  console.log(
    JSON.stringify({
      event: "survey-branches-start",
      at: new Date().toISOString(),
      user: auth.session.user,
      job: jobId,
      sqlBytes,
      mongoBytes,
    }),
  );

  return Response.json({ ok: true, jobId });
}

async function survey(
  h: JobHandle,
  args: { cli: string; sessionId: string; sqlBytes: number; mongoBytes: number },
): Promise<void> {
  const { cli, sessionId, sqlBytes, mongoBytes } = args;

  const cliArgs = ["-survey-branches", "-json", "-sql", stagedPath(sessionId, "sql")];
  if (mongoBytes > 0) {
    cliArgs.push("-mongo", stagedPath(sessionId, "mongo"));
  }

  h.addLog(
    mongoBytes > 0
      ? `อ่าน SQL ${mb(sqlBytes)} + MongoDB ${mb(mongoBytes)} — ไฟล์ Mongo ทำให้ได้ยอดบิล/ใบเสร็จ/คอร์ส/ล็อตสต็อกรายสาขา แต่ใช้เวลาหลายนาที`
      : `อ่าน SQL ${mb(sqlBytes)} · ไม่มีไฟล์ MongoDB จึงไม่มียอดบิล/ใบเสร็จ/คอร์ส/ล็อตสต็อก`,
  );
  h.setPhase(mongoBytes > 0 ? "CLI กำลังอ่าน dump (ขั้นนี้ใช้เวลานานที่สุด)" : "CLI กำลังอ่านตารางสาขา");

  const started = Date.now();
  let stdout: string;
  try {
    const res = await run(cli, cliArgs, {
      maxBuffer: 32 * 1024 * 1024,
      timeout: SURVEY_TIMEOUT_MS,
      windowsHide: true,
    });
    stdout = res.stdout;
  } catch (err) {
    // The CLI fails closed and says why on stderr. Pass that through: it names the problem
    // with the dump, which a generic message would throw away.
    const e = err as { stderr?: string; message?: string; killed?: boolean };
    if (e.killed) {
      throw new Error(`อ่านไม่เสร็จใน ${SURVEY_TIMEOUT_MS / 60000} นาที — ไฟล์ใหญ่เกินไปสำหรับเครื่องนี้`);
    }
    throw new Error((e.stderr || e.message || "").trim() || "รัน CLI โหมดสำรวจสาขาไม่สำเร็จ");
  }

  let parsed: { branches?: unknown };
  try {
    parsed = JSON.parse(stdout) as typeof parsed;
  } catch {
    // Not swallowed: the head of what came back is the only clue to why it was not JSON.
    throw new Error("CLI ไม่ได้คืน JSON ตามที่ขอ — ขึ้นต้นด้วย: " + stdout.slice(0, 300).trim());
  }
  if (!Array.isArray(parsed.branches)) {
    throw new Error("ผลสำรวจไม่มีรายการสาขา (ฟิลด์ branches) — เวอร์ชันของ CLI อาจไม่ตรงกับหน้านี้");
  }

  const seconds = ((Date.now() - started) / 1000).toFixed(1);
  h.addLog(`อ่านเสร็จใน ${seconds} วินาที · พบ ${parsed.branches.length} สาขา`);
  h.setSurveyResult({ survey: parsed, sqlBytes, mongoBytes });

  console.log(
    JSON.stringify({
      event: "survey-branches-done",
      at: new Date().toISOString(),
      job: h.id,
      seconds,
      sqlBytes,
      mongoBytes,
      branches: parsed.branches.length,
    }),
  );
}
