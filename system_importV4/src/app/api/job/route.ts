import "server-only";

import { requireSession } from "@/lib/require-session";
import { getJob, jobView } from "@/lib/jobs";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

/**
 * Status of one job. The browser polls this while a generate or import runs.
 *
 * Deliberately cheap: it reads process memory and returns counters and log lines. The
 * script a generate produced is never included — it can be hundreds of megabytes and is
 * fetched from /api/job/download instead, which streams it.
 */
export async function GET(req: Request): Promise<Response> {
  const auth = await requireSession(req);
  if ("denied" in auth) return auth.denied;

  const id = new URL(req.url).searchParams.get("jobId")?.trim() ?? "";
  if (!id) return Response.json({ ok: false, error: "ไม่ได้ระบุ jobId" }, { status: 400 });

  const job = getJob(id, auth.session.user);
  if (!job) {
    // Also the answer when the job belongs to someone else: the id is the only thing
    // needed to reach a clinic's data, so a wrong owner is treated as a wrong id.
    return Response.json(
      { ok: false, error: "ไม่พบงานนี้ (อาจถูกล้างแล้ว หรือเซิร์ฟเวอร์รีสตาร์ตระหว่างทำงาน)" },
      { status: 404 },
    );
  }

  return Response.json({ ok: true, job: jobView(job) });
}
