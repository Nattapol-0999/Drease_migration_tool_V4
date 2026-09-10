import "server-only";

import { createReadStream } from "node:fs";
import { Readable } from "node:stream";

import { requireSession } from "@/lib/require-session";
import { getJob } from "@/lib/jobs";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

/**
 * Streams a generated script to the browser.
 *
 * Streaming rather than reading it: the script for a large clinic runs to hundreds of
 * megabytes, and `readFile` would put all of it in the server's memory and then in a JSON
 * response. Handing the file handle to the response keeps memory flat regardless of size,
 * and the browser saves it straight to disk.
 */
export async function GET(req: Request): Promise<Response> {
  const auth = await requireSession(req);
  if ("denied" in auth) return auth.denied;

  const id = new URL(req.url).searchParams.get("jobId")?.trim() ?? "";
  const job = id ? getJob(id, auth.session.user) : null;
  if (!job) {
    return Response.json({ ok: false, error: "ไม่พบงานนี้" }, { status: 404 });
  }
  if (job.state !== "done" || !job.sqlPath || !job.filename) {
    return Response.json(
      { ok: false, error: `งานนี้ยังไม่มีไฟล์ให้ดาวน์โหลด (สถานะ: ${job.state})` },
      { status: 409 },
    );
  }

  const stream = Readable.toWeb(createReadStream(job.sqlPath)) as ReadableStream<Uint8Array>;

  return new Response(stream, {
    headers: {
      "content-type": "application/sql; charset=utf-8",
      "content-length": String(job.sqlBytes ?? 0),
      // The filename is `import_<clinic_code>.sql`, built from a validated code, so it
      // cannot carry characters that would break the header.
      "content-disposition": `attachment; filename="${job.filename}"`,
      "cache-control": "no-store",
    },
  });
}
