import "server-only";

import { requireSession } from "@/lib/require-session";
import { callBackend, describeClinicAPI, explainFailure, readClinicAPI } from "@/lib/clinic-api";
import { TARGET_IDS, type TargetId } from "@/lib/types";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// Creates the clinic on the selected environment by asking its V4 backend to do it.
//
// The backend is the only thing that should write a tenant: it owns the two-row
// identity/branch model, the branch-code sequence, the feature flags and the
// subscription. This route carries the request and reports the answer verbatim.

interface Body {
  targetId?: string;
  clinicCode?: string;
  thaName?: string;
  engName?: string;
  businessType?: string;
  clinicType?: string;
  tier?: string;
  /** Supplying an owner email makes the backend seed the account in the same transaction. */
  ownerName?: string;
  ownerEmail?: string;
  ownerPassword?: string;
}

function fail(error: string, status = 400, extra: Record<string, unknown> = {}): Response {
  return Response.json({ ok: false, error, ...extra }, { status });
}

export async function POST(req: Request): Promise<Response> {
  const auth = await requireSession(req);
  if ("denied" in auth) return auth.denied;

  let body: Body;
  try {
    body = (await req.json()) as Body;
  } catch {
    return fail("อ่าน body ไม่ได้");
  }

  const id = String(body.targetId ?? "");
  if (!(TARGET_IDS as readonly string[]).includes(id)) {
    return fail(`ปลายทาง "${id}" ไม่อยู่ในรายการที่ตั้งค่าไว้`);
  }
  const { api, missing } = readClinicAPI(id as TargetId);
  if (!api) {
    return fail(
      `ยังตั้งค่าการเรียก backend ของ ${id} ไม่ครบ — ขาด ${missing.join(", ")}`,
      503,
      { missing },
    );
  }

  // clinic_code may be empty: that asks the backend to allocate one. It is validated
  // there too, but a value that cannot be a code is worth refusing before a round trip.
  const clinicCode = (body.clinicCode ?? "").trim();
  if (clinicCode && !/^[A-Za-z0-9_-]{1,32}$/.test(clinicCode)) {
    return fail("clinic_code ใช้ได้เฉพาะ A-Z a-z 0-9 - _ และยาวไม่เกิน 32");
  }

  const ownerEmail = (body.ownerEmail ?? "").trim();
  // Checked here as well so an obvious typo does not cost a round trip. The backend
  // validates it too, because this route is not the only way in.
  if (ownerEmail && (!ownerEmail.includes("@") || ownerEmail.length > 255)) {
    return fail("อีเมล owner ไม่ถูกรูป");
  }

  const started = Date.now();
  const call = await callBackend(api, "POST", "/v3/import/clinics", {
    clinic_code: clinicCode,
    tha_name: (body.thaName ?? "").trim(),
    eng_name: (body.engName ?? "").trim(),
    business_type: (body.businessType ?? "").trim(),
    clinic_type: (body.clinicType ?? "").trim(),
    tier: (body.tier ?? "").trim(),
    owner_name: (body.ownerName ?? "").trim(),
    owner_email: ownerEmail,
    owner_password: body.ownerPassword ?? "",
  });
  const seconds = ((Date.now() - started) / 1000).toFixed(1);

  // The password is never logged, generated or supplied. Whether an owner was requested
  // is worth recording; the credential itself is not.
  console.log(
    JSON.stringify({
      event: "create-clinic",
      at: new Date().toISOString(),
      user: auth.session.user,
      target: id,
      requested: clinicCode || "(generate)",
      owner: ownerEmail || "(none)",
      status: call.status,
      seconds,
    }),
  );

  if (call.transportError) {
    return fail(call.transportError, 502, { target: describeClinicAPI(api) });
  }

  // The backend's own body is the most accurate description of what happened, so it is
  // parsed rather than replaced. When it is not JSON (a proxy answered, or the route does
  // not exist) the raw text is passed through instead of a guess.
  let parsed: unknown = null;
  try {
    parsed = JSON.parse(call.body);
  } catch {
    /* keep parsed null and fall back to the raw body below */
  }

  if (call.status < 200 || call.status >= 300) {
    const fromBackend =
      parsed && typeof parsed === "object" && "error" in (parsed as Record<string, unknown>)
        ? String((parsed as Record<string, unknown>).error)
        : "";
    const hint = explainFailure(call.status, call.body);
    const detail = [fromBackend, hint].filter(Boolean).join(" · ");
    return fail(detail || `backend ตอบ HTTP ${call.status}`, 422, {
      status: call.status,
      target: describeClinicAPI(api),
      raw: fromBackend ? undefined : call.body.slice(0, 500),
    });
  }

  const clinic =
    parsed && typeof parsed === "object" && "clinic" in (parsed as Record<string, unknown>)
      ? (parsed as Record<string, unknown>).clinic
      : parsed;

  return Response.json({
    ok: true,
    target: describeClinicAPI(api),
    seconds,
    clinic,
  });
}
