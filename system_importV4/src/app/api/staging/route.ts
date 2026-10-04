import "server-only";

import { requireSession } from "@/lib/require-session";
import { clearSelected, readInventory } from "@/lib/staging-inventory";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

/**
 * What is left over from unfinished imports, and the door to delete it.
 *
 * GET lists; POST removes exactly the ids it is given. Nothing here reaches the clinic
 * database or the V4 backend — it only touches this box's own staging directory.
 */
export async function GET(req: Request): Promise<Response> {
  const auth = await requireSession(req);
  if ("denied" in auth) return auth.denied;

  try {
    return Response.json({ ok: true, inventory: await readInventory() });
  } catch (err) {
    return Response.json(
      { ok: false, error: err instanceof Error ? err.message : "อ่านรายการไม่สำเร็จ" },
      { status: 500 },
    );
  }
}

/**
 * Removes the selected sessions and job directories.
 *
 * The ids are the whole request on purpose: there is no "clear everything" verb. A
 * screen that can only delete what it has listed and the operator has ticked cannot
 * surprise anyone, and the server re-checks every id against a fresh inventory before
 * unlinking anything.
 */
export async function POST(req: Request): Promise<Response> {
  const auth = await requireSession(req);
  if ("denied" in auth) return auth.denied;

  let body: unknown;
  try {
    body = await req.json();
  } catch {
    return Response.json({ ok: false, error: "อ่าน body ไม่ได้" }, { status: 400 });
  }

  const asIds = (v: unknown): string[] =>
    Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];

  const b = body as { sessions?: unknown; jobs?: unknown };
  const sessions = asIds(b.sessions);
  const jobs = asIds(b.jobs);
  if (sessions.length === 0 && jobs.length === 0) {
    return Response.json({ ok: false, error: "ไม่ได้เลือกรายการไหนเลย" }, { status: 400 });
  }

  try {
    const result = await clearSelected({ sessions, jobs });
    return Response.json({ ok: true, result, inventory: await readInventory() });
  } catch (err) {
    return Response.json(
      { ok: false, error: err instanceof Error ? err.message : "ล้างไม่สำเร็จ" },
      { status: 500 },
    );
  }
}
