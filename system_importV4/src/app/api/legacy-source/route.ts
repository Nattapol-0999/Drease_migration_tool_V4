import "server-only";

import { describeSource, readLegacySource } from "@/lib/legacy-source";
import type { LegacySourceStatus } from "@/lib/types";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// Whether tab 2 ("import จากเซิร์ฟเวอร์") can be used at all. Only the non-secret
// description crosses to the browser — never the password, and never the host list's
// purpose beyond what an operator needs to fix the config themselves.
export async function GET(): Promise<Response> {
  const { source, missing } = readLegacySource();
  if (!source) {
    return Response.json({ configured: false, missing } satisfies LegacySourceStatus);
  }
  return Response.json({
    configured: true,
    description: describeSource(source),
    hostsAllowed: source.allowedHosts,
  } satisfies LegacySourceStatus);
}
