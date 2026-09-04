import "server-only";

import { requireSession } from "@/lib/require-session";
import { callBackend, describeClinicAPI, explainFailure, readClinicAPI } from "@/lib/clinic-api";
import { TARGET_IDS, type TargetId } from "@/lib/types";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// Reports, per environment, whether creating a clinic is actually possible right now.
//
// This exists so the UI never shows a control that cannot work. Three different things
// have to be true — the tool must be configured, the backend's import lane must be
// mounted, and the running build must contain the endpoint — and each fails with its own
// fix. Guessing which one it is from a failed click is the experience this replaces.
//
// The probe is a POST with a deliberately invalid clinic_code. That is the cheapest
// request that proves the endpoint exists AND the token is accepted while being certain
// not to write anything: it is rejected by validation (422) before any row is touched.

interface Status {
  id: TargetId;
  available: boolean;
  /** Present when available is false: what to fix, in the operator's terms. */
  reason?: string;
  missing?: string[];
  /** Where the request goes. Never includes the token. */
  api?: string;
  /** The HTTP status the probe saw, for the log line in the UI. */
  probeStatus?: number;
}

export async function GET(req: Request): Promise<Response> {
  const auth = await requireSession(req);
  if ("denied" in auth) return auth.denied;

  const out: Status[] = [];
  for (const id of TARGET_IDS) {
    const { api, missing } = readClinicAPI(id);
    if (!api) {
      out.push({
        id,
        available: false,
        reason: `ยังไม่ได้ตั้งค่าใน .env.local — ขาด ${missing.join(", ")}`,
        missing,
      });
      continue;
    }

    const probe = await callBackend(api, "POST", "/v3/import/clinics", {
      // A code the backend must refuse: the space fails validateClinicCode, so the
      // request cannot create anything even if everything else is wired.
      clinic_code: "probe check",
      tier: "none",
    });

    if (probe.transportError) {
      out.push({
        id,
        available: false,
        reason: probe.transportError,
        api: describeClinicAPI(api),
      });
      continue;
    }

    // 422 is the SUCCESS signal here: the endpoint exists, the token was accepted, and
    // validation ran. 200 would also mean reachable, but must not happen for this input.
    const reachable = probe.status === 422 || probe.status === 200;
    out.push({
      id,
      available: reachable,
      reason: reachable
        ? undefined
        : explainFailure(probe.status, probe.body) || `backend ตอบ HTTP ${probe.status}`,
      api: describeClinicAPI(api),
      probeStatus: probe.status,
    });
  }

  return Response.json({ targets: out });
}
