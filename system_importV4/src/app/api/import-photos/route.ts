import "server-only";

import { requireSession } from "@/lib/require-session";
import { executePhotoImport, PhotoRunError } from "@/lib/photo-run";
import { TARGET_IDS, type TargetId } from "@/lib/types";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// The photo import on its own, in two modes.
//
// WHY THIS STILL EXISTS NOW THAT /api/import-to-server DOES BOTH
// The SQL commits and the photos follow. If the copy fails after that — a dropped ssh
// connection, a full disk on the backend host — the data is in and only the images are
// missing, and re-running the whole import to fix that would mean deleting the tenant
// first. So the photo step stays reachable on its own, and this is the route that does it.
//
// `survey` plans and reports without writing anything; `apply` ships the bytes and then
// writes the rows. Both rebuild the plan from the dump on every call rather than caching
// one between them: what the operator confirmed is what runs, even if a file changed in
// between.
//
// The work itself is in lib/photo-run so this route and the combined one cannot drift into
// filing images differently.

const CLINIC_RE = /^[A-Za-z0-9_-]{1,64}$/;

function fail(error: string, status = 400, extra: Record<string, unknown> = {}): Response {
  return Response.json({ ok: false, error, ...extra }, { status });
}

export async function POST(req: Request): Promise<Response> {
  // Checked here as well as in the middleware: this route spawns processes and, in apply
  // mode, writes to two production hosts.
  const auth = await requireSession(req);
  if ("denied" in auth) return auth.denied;

  const cli = process.env.IMPORT_CLI_PATH;
  if (!cli) {
    return fail("ยังไม่ได้ตั้งค่า IMPORT_CLI_PATH — ชี้ไปที่ไฟล์ system_cliv4", 500);
  }

  const form = await req.formData();
  const mode = String(form.get("mode") ?? "survey");
  if (mode !== "survey" && mode !== "apply") {
    return fail(`mode ต้องเป็น survey หรือ apply (ได้ "${mode}")`);
  }

  const id = String(form.get("targetId") ?? "");
  if (!(TARGET_IDS as readonly string[]).includes(id)) {
    return fail(`ปลายทาง "${id}" ไม่อยู่ในรายการที่ตั้งค่าไว้`);
  }
  const targetId = id as TargetId;

  const clinic = String(form.get("clinicCode") ?? "").trim();
  if (!CLINIC_RE.test(clinic)) {
    return fail("clinic_code ไม่ถูกรูป (ใช้ A-Z a-z 0-9 _ - ยาวไม่เกิน 64)");
  }

  const sessionId = String(form.get("sessionId") ?? "").trim();
  if (!sessionId) {
    return fail(
      "ต้องส่ง sessionId ของไฟล์ที่อัปโหลดไว้ — ไฟล์รูปของคลินิกจริงมีขนาดหลายร้อย MB " +
        "จึงต้องอัปโหลดแบบแบ่งชิ้นก่อน (POST /api/upload-chunk field=mongo และ field=images)",
    );
  }

  // apply retypes the clinic_code, the same gate the SQL import uses. It is the last thing
  // standing between "wrong tenant selected ten minutes ago" and rows in a real patient
  // record.
  if (mode === "apply") {
    const confirm = String(form.get("confirm") ?? "").trim();
    if (confirm !== clinic) {
      return fail("ยืนยันไม่ผ่าน: พิมพ์ clinic_code ให้ตรงก่อนนำเข้า");
    }
  }

  try {
    const result = await executePhotoImport({
      cli,
      clinic,
      targetId,
      sessionId,
      mode,
      user: auth.session.user,
    });
    return Response.json({ ok: true, ...result });
  } catch (err) {
    if (err instanceof PhotoRunError) {
      // The counts gathered before it failed go back too: that is how an operator learns
      // the bytes went over even though the rows did not.
      return fail(err.message, err.status, { mode, ...(err.partial ?? {}) });
    }
    return fail((err as Error).message || "นำเข้ารูปไม่สำเร็จ", 422);
  }
}
