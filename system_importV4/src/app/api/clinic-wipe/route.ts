import "server-only";

import {
  CLINIC_CODE_RE,
  surveyClinic,
  wipeClinic,
} from "@/lib/clinic-wipe";
import { requireSession } from "@/lib/require-session";
import { describeTarget, resolveTarget } from "@/lib/server-target";
import type { ClinicWipeRunResponse, ClinicWipeSurveyResponse } from "@/lib/types";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

/**
 * Removing a clinic from a target database.
 *
 * This is the most destructive thing the tool can do, and every guard below is one of the
 * ways it could go wrong:
 *
 *   requireSession    — not reachable without a login, like every other route here
 *   resolveTarget     — the environment must be one configured ON THE SERVER. It returns
 *                       null for anything else and this refuses, rather than defaulting:
 *                       "guessing a default here is how a request meant for test lands on
 *                       production" (server-target.ts), and that applies even more to a
 *                       delete than to an import
 *   CLINIC_CODE_RE    — checked before the value reaches any SQL
 *   action "survey"   — read only, and the UI must run it before it can offer the delete
 *   confirm           — the code has to be retyped, the same way /api/import-to-server
 *                       makes an operator retype it before it writes
 *   rehearse + commit — inside lib/clinic-wipe.ts; a failed rehearsal changes nothing
 *
 * NOT A JOB, UNLIKE IMPORT
 *
 * /api/import-to-server starts a job because psql on a several-hundred-megabyte script runs
 * far longer than the 100 seconds Cloudflare allows. A wipe is a few dozen DELETEs over
 * indexed columns: the largest clinic removed by hand this week was 55,042 rows and the
 * whole transaction took under two seconds. The SQL is sent twice (rehearsal, then real)
 * and both runs plus two surveys still answer inside one request. If a tenant ever appears
 * where that stops being true, this becomes a job — the measurement above is the warning
 * sign, not a promise.
 */

interface Body {
  action?: unknown;
  targetId?: unknown;
  clinicCode?: unknown;
  /** Required for action "wipe": the clinic code, retyped. */
  confirm?: unknown;
}

function bad(error: string, status = 400): Response {
  return Response.json({ ok: false, error } satisfies ClinicWipeSurveyResponse, { status });
}

export async function POST(req: Request): Promise<Response> {
  const auth = await requireSession(req);
  if ("denied" in auth) return auth.denied;

  let body: Body;
  try {
    body = (await req.json()) as Body;
  } catch {
    return bad("อ่าน body ไม่ได้");
  }

  const action = typeof body.action === "string" ? body.action : "";
  if (action !== "survey" && action !== "wipe") {
    return bad('action ต้องเป็น "survey" หรือ "wipe"');
  }

  const target = resolveTarget(body.targetId);
  if (!target) {
    // No default on purpose. An unconfigured or unknown id is a refusal, never a guess.
    return bad("ยังไม่ได้เลือกปลายทาง หรือปลายทางนั้นไม่ได้ตั้งค่าไว้บนเครื่องนี้");
  }

  const code = (typeof body.clinicCode === "string" ? body.clinicCode : "").trim();
  if (!CLINIC_CODE_RE.test(code)) {
    return bad("clinic_code ไม่ถูกรูป (ใช้ A-Z a-z 0-9 _ - ยาวไม่เกิน 64)");
  }

  const where = describeTarget(target);
  const started = Date.now();
  const secs = () => ((Date.now() - started) / 1000).toFixed(1);

  if (action === "survey") {
    try {
      const { survey } = await surveyClinic(target, code);
      return Response.json({
        ok: true,
        target: where,
        isProduction: target.isProduction,
        survey,
        seconds: secs(),
      } satisfies ClinicWipeSurveyResponse);
    } catch (err) {
      return Response.json(
        {
          ok: false,
          error: err instanceof Error ? err.message : "สำรวจไม่สำเร็จ",
        } satisfies ClinicWipeSurveyResponse,
        { status: 500 },
      );
    }
  }

  // ── action "wipe" ──────────────────────────────────────────────────────────
  const confirm = (typeof body.confirm === "string" ? body.confirm : "").trim();
  if (confirm !== code) {
    return bad("ต้องพิมพ์ clinic_code ซ้ำให้ตรงกันก่อนจึงจะลบได้");
  }

  try {
    // Surveyed again here, on the server, immediately before the delete. The browser
    // already showed one — but that answer is now however many seconds old the operator
    // took to read it, and this is also what makes "nothing to delete" a refusal rather
    // than a wipe that reports zero rows.
    const before = (await surveyClinic(target, code)).survey;
    if (!before.found) {
      return bad(`ไม่พบคลินิก ${code} ที่ ${where} — ไม่ลบอะไรเลย`, 404);
    }

    const outcome = await wipeClinic(target, code);

    // And confirmed from a fresh connection, the way the hand-written wipes all ended:
    // the transaction's own verifier runs inside the transaction, so it cannot see what
    // another session would. This can.
    const after = (await surveyClinic(target, code)).survey;

    return Response.json({
      ok: true,
      target: where,
      isProduction: target.isProduction,
      before,
      after,
      deletedRows: outcome.deletedRows,
      bizEventKept: outcome.bizEventKept,
      rehearsal: outcome.rehearsal,
      applied: outcome.applied,
      seconds: secs(),
    } satisfies ClinicWipeRunResponse);
  } catch (err) {
    const e = err as Error & { stage?: "rehearsal" | "commit"; rehearsal?: string };
    return Response.json(
      {
        ok: false,
        error: e instanceof Error ? e.message : "ลบไม่สำเร็จ",
        stage: e.stage,
        rehearsal: e.rehearsal,
      } satisfies ClinicWipeRunResponse,
      { status: 500 },
    );
  }
}
