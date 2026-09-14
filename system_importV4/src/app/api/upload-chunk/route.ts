import "server-only";

import { requireSession } from "@/lib/require-session";
import {
  appendChunk,
  createSession,
  isValidField,
  sessionBytes,
  stagedFieldNames,
  sessionExists,
  sweepStaging,
} from "@/lib/upload-staging";
import { CHUNK_BYTES, MAX_UPLOAD_BYTES } from "@/lib/types";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

/**
 * Receives one slice of a dump and appends it to a staged file on disk.
 *
 * WHY SLICES
 * A dump can be over a gigabyte. Cloudflare refuses a body over 100 MB on this plan and
 * nginx over 64 MB, and reading a body of that size with formData() would buffer it in the
 * RAM of a 908 MB box. Slicing solves all three at once: every request is small, and the
 * bytes land on disk without ever being held whole in memory.
 *
 * The slice is written straight through — no parsing, no decoding, no accumulation — so
 * memory here is one chunk, not one file.
 */

// A slice is allowed a little headroom over the client's CHUNK_BYTES for multipart
// overhead, but must stay under nginx's 64 MB or the proxy answers instead of this route.
const MAX_SLICE_BYTES = CHUNK_BYTES + 4 * 1024 * 1024;

function fail(error: string, status = 400, extra: Record<string, unknown> = {}): Response {
  return Response.json({ ok: false, error, ...extra }, { status });
}

export async function POST(req: Request): Promise<Response> {
  const auth = await requireSession(req);
  if ("denied" in auth) return auth.denied;

  let form: FormData;
  try {
    form = await req.formData();
  } catch (e) {
    // Usually the proxy cut the body. Say so instead of a generic parse error.
    return fail(
      `อ่านชิ้นไฟล์ไม่ได้ — ${e instanceof Error ? e.message : String(e)} ` +
        `(ชิ้นใหญ่เกินที่ proxy ยอมรับก็ขึ้นแบบนี้)`,
    );
  }

  const field = String(form.get("field") ?? "");
  if (!isValidField(field)) {
    // The list comes from the one place that decides it, so adding a field cannot leave
    // this message telling the operator something untrue.
    return fail(`field "${field}" ไม่อยู่ในรายการที่รับ (${stagedFieldNames().join(", ")})`);
  }

  const offsetRaw = String(form.get("offset") ?? "");
  const offset = Number(offsetRaw);
  if (!Number.isSafeInteger(offset) || offset < 0) {
    return fail(`offset "${offsetRaw}" ไม่ถูกรูป`);
  }

  const blob = form.get("chunk");
  if (!(blob instanceof File) || blob.size === 0) {
    return fail("ไม่มีข้อมูลในชิ้นนี้");
  }
  if (blob.size > MAX_SLICE_BYTES) {
    return fail(`ชิ้นนี้ใหญ่ ${blob.size} ไบต์ เกินที่รับได้ ${MAX_SLICE_BYTES} ไบต์`);
  }

  // A first slice (offset 0 with no session) opens the session. Sweeping here keeps
  // abandoned uploads from filling the disk without needing a scheduler.
  let sessionId = String(form.get("sessionId") ?? "").trim();
  let swept: { removed: number; bytes: number } | null = null;
  if (!sessionId) {
    swept = await sweepStaging();
    sessionId = await createSession();
  } else if (!(await sessionExists(sessionId))) {
    return fail(
      "ไม่พบ session ของการอัปโหลดนี้ (อาจถูกล้างไปแล้ว) — เริ่มแนบไฟล์ใหม่",
      409,
    );
  }

  const before = await sessionBytes(sessionId);
  if (before + blob.size > MAX_UPLOAD_BYTES) {
    return fail(
      `ไฟล์รวมกันเกินเพดาน ${MAX_UPLOAD_BYTES} ไบต์`,
      413,
      { sessionId },
    );
  }

  let written: number;
  try {
    written = await appendChunk(sessionId, field, offset, Buffer.from(await blob.arrayBuffer()));
  } catch (e) {
    // Out-of-order or duplicate slices land here. Reported, never patched over: a dump
    // assembled in the wrong order would parse into the wrong data.
    return fail(e instanceof Error ? e.message : String(e), 409, { sessionId });
  }

  return Response.json({
    ok: true,
    sessionId,
    field,
    bytes: written,
    totalBytes: before + blob.size,
    ...(swept && swept.removed > 0
      ? { swept: { sessions: swept.removed, bytes: swept.bytes } }
      : {}),
  });
}
