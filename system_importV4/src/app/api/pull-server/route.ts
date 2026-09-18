import "server-only";

import {
  checkHost,
  pullFromHost,
  pullImagesToFile,
  readLegacySource,
} from "@/lib/legacy-source";
import { createSession, stagedPath } from "@/lib/upload-staging";
import type { PullResponse, PulledImageInfo } from "@/lib/types";

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

    // The image archive is pulled in the SAME request, by the owner's decision on 17 Sep 2026:
    // one button, one answer. It does not travel in the response — it is streamed from the
    // legacy box into the staging directory here (736 MB and 1.12 GB measured), and the reply
    // names the session it landed in.
    //
    // KNOWN LIMIT OF DOING IT THIS WAY: a proxied request is closed at 100 seconds. The
    // transfer that was measured took 37s over the VPN, but 1.12 GB over the public internet
    // at 10 MB/s is 112s, and the browser would see a network error for a transfer that in
    // fact completed. The alternative that has no such ceiling is the job pattern this tool
    // already uses for the branch survey; it was offered and this shape was chosen instead.
    //
    // A failure here is a WARNING, not an error. The two dumps are in hand and independently
    // useful; discarding them because the images did not arrive would cost another full pull.
    let images: PulledImageInfo | null = null;
    const warnings = [...r.warnings];
    try {
      const sessionId = await createSession();

      // The Mongo dump is written into the SAME session as the archive, not only handed to
      // the browser.
      //
      // The photo step reads `page` out of the Mongo file and the images out of the archive,
      // and it reads BOTH from one staging session. Leaving the dump in the browser meant the
      // two halves lived apart: the archive in this session, the dump in whichever session the
      // generate step happened to create. The import then refused with "นำเข้ารูปต้องมีไฟล์
      // MongoDB (.js) ด้วย" while a perfectly good dump sat on the same disk under another id.
      //
      // It is written from the buffer already in hand, so this costs a file write and no
      // second export.
      const mongoFile = r.files.find((f) => f.field === "mongo");
      if (mongoFile) {
        const { writeFile } = await import("node:fs/promises");
        await writeFile(stagedPath(sessionId, "mongo"), Buffer.from(mongoFile.base64, "base64"));
      }

      const img = await pullImagesToFile(source, host, stagedPath(sessionId, "images"));
      images = {
        sessionId,
        name: "public_img.tar.gz",
        bytes: img.bytes,
        member: img.member,
      };
      r.log.push(...img.log);
      console.log(
        JSON.stringify({
          event: "pull-images-done",
          at: new Date().toISOString(),
          host: img.host,
          session: sessionId,
          bytes: img.bytes,
          member: img.member,
        }),
      );
    } catch (e) {
      warnings.push(
        "ดึงไฟล์รูปไม่สำเร็จ (ไฟล์ .sql และ .js ได้มาแล้ว ใช้งานต่อได้): " +
          (e instanceof Error ? e.message : String(e)),
      );
    }

    return Response.json({ ok: true, ...r, images, warnings } satisfies PullResponse);
  } catch (e) {
    // The message is already operator-facing (which setting to fix, what the remote
    // said). 502: the failure is the far side's, not the request's.
    return fail(e instanceof Error ? e.message : String(e), 502);
  }
}
