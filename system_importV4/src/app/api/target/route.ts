import "server-only";

import { describeTarget, readTargets } from "@/lib/server-target";
import type { TargetsResponse } from "@/lib/types";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// Tells the UI which environments it may offer, and what each points at. Only the
// non-secret description crosses to the browser — never the key path, never a password.
export async function GET(): Promise<Response> {
  const { available, problems } = readTargets();
  const body: TargetsResponse = {
    targets: available.map((t) => ({
      id: t.id,
      label: t.label,
      description: describeTarget(t),
      isProduction: t.isProduction,
    })),
  };
  // Reported so a half-configured environment shows up as a fixable message instead of
  // silently missing from the dropdown.
  if (problems.length > 0) body.problems = problems;
  return Response.json(body);
}
