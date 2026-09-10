import "server-only";

import { execFile } from "node:child_process";
import { promisify } from "node:util";

import { requireSession } from "@/lib/require-session";
import { sessionExists, stagedPath, stagedSize } from "@/lib/upload-staging";
import { MAX_CLI_INPUT_BYTES, type BranchSurvey } from "@/lib/types";

const run = promisify(execFile);

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

/**
 * Reads the legacy `branch` table out of an already-uploaded MySQL dump.
 *
 * WHY THIS STEP EXISTS AT ALL
 * V4 owns branch identity: a branch_code comes from a sequence and carries a branch_no
 * that every HN issued at that branch keeps for life. The legacy "01" therefore cannot be
 * written through — it has to be TRANSLATED. That translation table can only be built
 * after the branches exist in V4, and they can only be created once somebody has seen what
 * the export actually contains. So the order is: read the dump, let the operator choose,
 * create, then hand the resulting map to the CLI as -branch-map.
 *
 * Skipping it is not a smaller version of the same import. One export held nine branches
 * and 14,264 patients; loaded without a map every one of those patients, and every bill and
 * room with them, is filed at a branch they were never at.
 *
 * WHY IT IS READ-ONLY AND WHY IT RUNS INLINE
 * The CLI's -survey-branches mode writes nothing — no -clinic, no output file — so this
 * route cannot change the target. It also finishes in 1.37 s on a 48 MB whole-database
 * dump, which is why it answers directly instead of starting a job like /api/generate: the
 * job machinery exists for work that outlives the 100 s a proxied request may stay open,
 * and this is two orders of magnitude short of that.
 */

function fail(error: string, status = 400): Response {
  return Response.json({ ok: false, error }, { status });
}

const mb = (n: number) => `${(n / 1048576).toFixed(1)} MB`;

/**
 * A survey that somehow runs long must not hang the request until the proxy kills it with
 * no explanation. Generous against the measured 1.4 s, strict against the 100 s ceiling.
 */
const SURVEY_TIMEOUT_MS = 90_000;

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

  // The branch registry is a MySQL table, so the Mongo half cannot answer this.
  const sqlBytes = await stagedSize(sessionId, "sql");
  if (sqlBytes === 0) {
    return fail(
      "ยังไม่มีไฟล์ MySQL ในการอัปโหลดนี้ — ตารางสาขา (branch) อยู่ในไฟล์ .sql ไม่ใช่ไฟล์ MongoDB",
    );
  }
  // Same ceiling as /api/generate and for the same reason: the CLI reads its input whole.
  if (sqlBytes > MAX_CLI_INPUT_BYTES) {
    return fail(
      `ไฟล์ SQL ${mb(sqlBytes)} เกินที่ประมวลผลได้บนเครื่องนี้ (${mb(MAX_CLI_INPUT_BYTES)})`,
    );
  }

  const started = Date.now();
  let stdout: string;
  try {
    const res = await run(cli, ["-survey-branches", "-json", "-sql", stagedPath(sessionId, "sql")], {
      maxBuffer: 8 * 1024 * 1024,
      timeout: SURVEY_TIMEOUT_MS,
      windowsHide: true,
    });
    stdout = res.stdout;
  } catch (err) {
    // The CLI fails closed and says why on stderr. Pass that through: it names the problem
    // with the dump, which a generic message would throw away.
    const e = err as { stderr?: string; message?: string; killed?: boolean };
    if (e.killed) {
      return fail(`อ่านตารางสาขาไม่เสร็จใน ${SURVEY_TIMEOUT_MS / 1000} วินาที — ไฟล์ใหญ่เกินไป`, 504);
    }
    return fail((e.stderr || e.message || "").trim() || "รัน CLI โหมดสำรวจสาขาไม่สำเร็จ", 500);
  }

  let survey: BranchSurvey;
  try {
    survey = JSON.parse(stdout) as BranchSurvey;
  } catch {
    // Not swallowed: the head of what came back is the only clue to why it was not JSON.
    return fail(
      "CLI ไม่ได้คืน JSON ตามที่ขอ — ขึ้นต้นด้วย: " + stdout.slice(0, 300).trim(),
      500,
    );
  }
  if (!Array.isArray(survey.branches)) {
    return fail("ผลสำรวจไม่มีรายการสาขา (ฟิลด์ branches) — เวอร์ชันของ CLI อาจไม่ตรงกับหน้านี้", 500);
  }

  const seconds = ((Date.now() - started) / 1000).toFixed(1);

  console.log(
    JSON.stringify({
      event: "survey-branches",
      at: new Date().toISOString(),
      user: auth.session.user,
      sqlBytes,
      seconds,
      hasBranchTable: survey.has_branch_table,
      branches: survey.branches.length,
      legalEntities: survey.legal_entities?.length ?? 0,
    }),
  );

  return Response.json({ ok: true, survey, seconds, sqlBytes });
}
