import "server-only";

import { checkHost, pullFromHost, readLegacySource } from "@/lib/legacy-source";
import type { PullResponse } from "@/lib/types";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// This endpoint READS a legacy clinic database over SSH. It writes nothing anywhere —
// the dumps come back to the browser and only /api/import-to-server can write.
//
// The request carries ONE value, the host. Credentials come from the environment, and
// the server re-checks the config and the allowlist itself: the UI's gating is a
// convenience, never the control.

interface Body {
  host?: string;
}

function fail(error: string, status = 400): Response {
  return Response.json({ ok: false, error } satisfies PullResponse, { status });
}

export async function POST(req: Request): Promise<Response> {
  const { source, missing } = readLegacySource();
  if (!source) {
    return fail(
      `ยังไม่ได้ตั้งค่าเซิร์ฟเวอร์ต้นทาง — ขาด ${missing.join(", ")} ใน .env.local แล้วรีสตาร์ต dev server`,
      503,
    );
  }

  let body: Body;
  try {
    body = (await req.json()) as Body;
  } catch {
    return fail("อ่าน body ไม่ได้");
  }

  const host = (body.host ?? "").trim();
  const refused = checkHost(source, host);
  if (refused) return fail(refused);

  try {
    const r = await pullFromHost(source, host);
    return Response.json({ ok: true, ...r } satisfies PullResponse);
  } catch (e) {
    // The message is already operator-facing (which setting to fix, what the remote
    // said). 502: the failure is the far side's, not the request's.
    return fail(e instanceof Error ? e.message : String(e), 502);
  }
}
