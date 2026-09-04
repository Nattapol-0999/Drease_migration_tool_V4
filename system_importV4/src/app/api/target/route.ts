import "server-only";

import { describeTarget, readServerTarget } from "@/lib/server-target";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// Tells the UI whether the "import to server" button may be shown, and what it points
// at. Only the non-secret description crosses to the browser — never the key path,
// never the password.
export async function GET(): Promise<Response> {
  const { target, missing } = readServerTarget();
  if (!target) {
    return Response.json({ configured: false, missing });
  }
  return Response.json({ configured: true, description: describeTarget(target) });
}
