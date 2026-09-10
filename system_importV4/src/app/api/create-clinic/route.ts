import "server-only";

import { requireSession } from "@/lib/require-session";
import { callBackend, describeClinicAPI, explainFailure, readClinicAPI } from "@/lib/clinic-api";
import {
  TARGET_IDS,
  type ProvisionBranchInput,
  type TargetId,
  type TaxProfileInput,
} from "@/lib/types";

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
  /**
   * The branches this clinic should have, read out of the legacy `branch` table by
   * /api/survey-branches and chosen by the operator. Omitted keeps the old behaviour:
   * one branch named after the clinic.
   */
  branches?: ProvisionBranchInput[];
  /** Receipt/tax header taken from the main branch's row. Written only if none is saved. */
  taxProfile?: TaxProfileInput | null;
}

/**
 * clinic.branch_no is a smallint constrained to 1..99 (migration 0321) and the number is
 * permanent — it is the prefix every HN issued at that branch carries for life. Ninety-nine
 * is therefore a hard architectural ceiling, not a policy choice this tool may relax.
 */
const MAX_BRANCHES = 99;

/**
 * A legacy branch code has to survive being written into the CLI's -branch-map argument,
 * which is parsed as `code=BRANCH,code=BRANCH`. A code containing `=` or `,` would split
 * in the wrong place and silently attribute one branch's rows to another, so it is refused
 * here rather than discovered as a wrong branch on 14,000 patients.
 */
const SOURCE_CODE_RE = /^[^=,\s][^=,]{0,31}$/;

/** Validates the branch list, returning a Thai error message or null. */
function checkBranches(branches: ProvisionBranchInput[]): string | null {
  if (branches.length > MAX_BRANCHES) {
    return `เลือกสาขามา ${branches.length} สาขา แต่ V4 รองรับได้ไม่เกิน ${MAX_BRANCHES} สาขาต่อคลินิก (branch_no เป็น 1..99 และเลขนี้ติดไปกับ HN ตลอดชีวิต)`;
  }

  const seen = new Set<string>();
  let mains = 0;
  for (const [i, b] of branches.entries()) {
    const at = `สาขาลำดับที่ ${i + 1}`;
    const source = (b.source_code ?? "").trim();
    if (!source) {
      return `${at}: ไม่มีรหัสสาขาเดิม (source_code) — ถ้าไม่มีรหัส จะบอก CLI ไม่ได้ว่าแถวไหนของสาขานี้`;
    }
    if (!SOURCE_CODE_RE.test(source)) {
      return `${at}: รหัสสาขาเดิม ${JSON.stringify(source)} มีอักขระที่ใช้ไม่ได้ — ห้ามมี = หรือ , เพราะเป็นตัวคั่นของ -branch-map`;
    }
    if (seen.has(source)) {
      return `รหัสสาขาเดิม ${JSON.stringify(source)} ซ้ำกันในรายการที่ส่งมา — แผนที่สาขาจะชี้ได้ที่เดียว`;
    }
    seen.add(source);

    if (!(b.name ?? "").trim()) {
      return `${at} (${source}): ไม่มีชื่อสาขา แต่ clinic.tha_name เป็น NOT NULL`;
    }
    if (b.is_main) mains++;
  }

  // Exactly one, not "at least one": the main branch is also the destination for every row
  // whose branch cannot be resolved (-default-branch), so leaving it ambiguous decides
  // where unattributed patients and stock lots land by accident.
  if (mains !== 1) {
    return `ต้องเลือกสาขาหลักให้ได้หนึ่งสาขาพอดี (ตอนนี้เลือกไว้ ${mains} สาขา) — สาขาหลักเป็นทั้งสาขาตั้งต้นของเอกสารและปลายทางของแถวที่หาสาขาไม่ได้`;
  }
  return null;
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

  const branches = Array.isArray(body.branches) ? body.branches : [];
  if (branches.length > 0) {
    const branchErr = checkBranches(branches);
    if (branchErr) return fail(branchErr);
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
    // Omitted entirely when empty so the backend keeps its "one branch named after the
    // clinic" default instead of receiving an empty list to interpret.
    ...(branches.length > 0
      ? {
          branches: branches.map((b) => ({
            source_code: (b.source_code ?? "").trim(),
            name: (b.name ?? "").trim(),
            name_en: (b.name_en ?? "").trim(),
            address: (b.address ?? "").trim(),
            phone: (b.phone ?? "").trim(),
            is_main: Boolean(b.is_main),
          })),
        }
      : {}),
    ...(body.taxProfile
      ? {
          tax_profile: {
            legal_name: (body.taxProfile.legal_name ?? "").trim(),
            tax_id: (body.taxProfile.tax_id ?? "").trim(),
            tax_branch_type: (body.taxProfile.tax_branch_type ?? "").trim(),
            tax_branch_code: (body.taxProfile.tax_branch_code ?? "").trim(),
            tax_address: (body.taxProfile.tax_address ?? "").trim(),
            phone: (body.taxProfile.phone ?? "").trim(),
            email: (body.taxProfile.email ?? "").trim(),
            website: (body.taxProfile.website ?? "").trim(),
            receipt_footer: (body.taxProfile.receipt_footer ?? "").trim(),
            logo_url: (body.taxProfile.logo_url ?? "").trim(),
          },
        }
      : {}),
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
      branches: branches.length,
      taxProfile: body.taxProfile ? "sent" : "(none)",
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

  // ── the silent-drop guard ──
  //
  // Gin's ShouldBindJSON ignores fields it does not know, so a backend built before
  // multi-branch provisioning answers 200 OK to a nine-branch request having created ONE
  // branch, and says nothing about it. Everything after this point would then look healthy:
  // the map would come back empty, the CLI would fall back to its single-branch behaviour,
  // and 14,264 patients would land at one branch with no error anywhere in the chain.
  //
  // The response's own `branches` array is the proof the list was understood, so its
  // absence is treated as a failure rather than a formatting difference. Reported as 502 —
  // the request was fine, the target is not what this tool needs.
  if (branches.length > 0) {
    const echoed = (clinic as { branches?: unknown } | null)?.branches;
    if (!Array.isArray(echoed) || echoed.length === 0) {
      return fail(
        `ส่งรายการสาขาไป ${branches.length} สาขา แต่ backend ตอบกลับมาโดยไม่มีรายการสาขาเลย ` +
          "— เวอร์ชันของ backend ที่ปลายทางนี้ยังไม่รองรับ branches[] (Gin ทิ้งฟิลด์ที่ไม่รู้จักแบบไม่ฟ้อง) " +
          "จึงสร้างสาขาเดียวไปแล้ว และแผนที่สาขาที่ CLI ต้องใช้จะว่าง " +
          "— ต้อง deploy backend ตัวใหม่ก่อน แล้วตรวจสาขาของคลินิกนี้ด้วยมือ",
        502,
        { target: describeClinicAPI(api), clinic },
      );
    }
    // A branch answered without its source_code cannot be attributed to a legacy code, and
    // a partial map is worse than none: the branches it misses fall to the default and look
    // imported. Named individually so the operator can see which ones.
    const unlabelled = (echoed as { source_code?: string; branch_code?: string }[])
      .filter((b) => !(b.source_code ?? "").trim())
      .map((b) => b.branch_code ?? "(ไม่มีรหัส)");
    if (unlabelled.length > 0) {
      return fail(
        `backend คืนสาขามา ${echoed.length} สาขา แต่ ${unlabelled.length} สาขาไม่มี source_code ` +
          `(${unlabelled.join(", ")}) — จับคู่กับรหัสสาขาเดิมไม่ได้ แผนที่สาขาจะไม่ครบ`,
        502,
        { target: describeClinicAPI(api), clinic },
      );
    }
  }

  return Response.json({
    ok: true,
    target: describeClinicAPI(api),
    seconds,
    clinic,
  });
}
