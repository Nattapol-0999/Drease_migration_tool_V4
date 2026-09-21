import "server-only";

import {
  checkHost,
  pullDumpsToFiles,
  pullImagesToFile,
  readLegacySource,
} from "@/lib/legacy-source";
import { sweepJobs } from "@/lib/jobs";
import { createSession, stagedPath, sweepStaging } from "@/lib/upload-staging";
import type { PullResponse, PulledExtraInfo, PulledImageInfo } from "@/lib/types";

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
  /**
   * Whether to pull the page-image archive. Defaults to true — absent means yes, so an older
   * client that does not send it behaves exactly as before.
   *
   * WHY IT CAN BE TURNED OFF (owner's decision, 21 Sep 2026)
   *
   * A source turned up with 15 GB of page images over 29,247 files. That does not fit: the
   * request is closed at 100 seconds by the proxy, and this server's whole disk is 28 GB while
   * the import needs the archive AND its extracted copy. The pull ran on regardless of the
   * disconnected browser and had written 11 GB before it was stopped, taking the disk to 94%.
   *
   * So the clinic's DATA can be taken now and its images dealt with separately. The two dumps
   * are 4.8 MB and 96 MB for that clinic — seconds, not minutes.
   */
  withImages?: boolean;
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

  // Sweep BEFORE anything is created, so this can never touch what this request is about to
  // write. Both sweepers are age-based and a session seconds old is not a candidate, but the
  // ordering makes that true by construction rather than by reading their thresholds.
  //
  // WHY THE SWEEP HAD TO BE ADDED HERE
  //
  // sweepStaging was only ever called from /api/upload-chunk — the browser-upload lane. This
  // endpoint streams from the source straight to disk and never touches that route, so a pull
  // swept nothing, ever. Measured on 21 Sep 2026: 2.4 GB of abandoned sessions and generated
  // scripts had to be deleted by hand, on a 28 GB disk that a 15 GB image archive had already
  // taken to 94%.
  //
  // Failure here is swallowed on purpose and reported, not raised: housekeeping must never be
  // the reason a pull does not happen.
  const sweptLines: string[] = [];
  try {
    const st = await sweepStaging();
    if (st.removed > 0) {
      sweptLines.push(`เก็บกวาด staging: ลบ ${st.removed} เซสชันที่ค้าง (${Math.round(st.bytes / 1048576)} MB)`);
    }
  } catch (e) {
    sweptLines.push(`เก็บกวาด staging ไม่สำเร็จ (ไม่กระทบการดึงข้อมูล): ${e instanceof Error ? e.message : String(e)}`);
  }
  try {
    await sweepJobs();
  } catch (e) {
    sweptLines.push(`เก็บกวาดงานเก่าไม่สำเร็จ (ไม่กระทบการดึงข้อมูล): ${e instanceof Error ? e.message : String(e)}`);
  }

  try {
    // The session is created FIRST, because everything this endpoint pulls now lands in it —
    // the two dumps included. They used to come back as base64 in this response and be
    // re-uploaded by the browser into a second session, which meant the same bytes crossed the
    // wire twice and were held in memory at least twice.
    //
    // That is what broke on 18 Sep 2026: a source with bills 43.6 MB + payment_summary 30.5 MB
    // in Mongo took the Node process past its 467 MB heap and nginx answered the browser with a
    // 502 HTML page, which arrived as "Unexpected token '<' … is not valid JSON".
    const sessionId = await createSession();
    const r = await pullDumpsToFiles(source, host, {
      sql: stagedPath(sessionId, "sql"),
      mongo: stagedPath(sessionId, "mongo"),
    });

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
    const extras: PulledExtraInfo[] = [];
    const warnings = [...r.warnings];

    // Absent means yes: an older client that does not send the field keeps the old behaviour.
    const withImages = body.withImages !== false;

    if (!withImages) {
      // Said as a warning, not passed over in silence. `images: null` is also what a FAILED
      // pull looks like, and the two have to be told apart — one means try again, the other
      // means this is what was asked for.
      warnings.push(
        "ข้ามการดึงไฟล์รูปหน้าเวชระเบียนตามที่เลือกไว้ — ได้ข้อมูลและโลโก้/รูปโปรไฟล์ครบ " +
          "แต่ขั้นนำเข้ารูปจะยังทำไม่ได้จนกว่าจะมีไฟล์รูป",
      );
    }

    try {
      if (withImages) {
        // Both dumps are already in this session — pullDumpsToFiles put them there. The photo
        // step reads `page` out of the Mongo file and the images out of the archive and needs
        // both in ONE session, which is now simply where they are.
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
      }

      // The clinic logo and the patient profile photos, into the SAME session so the import
      // step finds all three together without the operator choosing anything.
      //
      // OUTSIDE the images branch, and no longer conditional on the archive having arrived.
      // They used to sit inside it because the browser addressed the session through
      // images.sessionId; the session is now a top-level field of the reply, so that reason is
      // gone. Measured at 356 KB + 556 KB — worth having even when the 15 GB archive is not.
      //
      // Each tree still gets its OWN try: a V2 box without an img/clinic-logo folder is normal,
      // and one missing folder must not cost the other tree or the archive that did arrive.
      for (const t of [
        { field: "logo" as const, member: source.logoMember, what: "โลโก้คลินิก" },
        { field: "profiles" as const, member: source.profileMember, what: "รูปโปรไฟล์คนไข้" },
      ]) {
        try {
          const got = await pullImagesToFile(source, host, stagedPath(sessionId, t.field), t.member);
          extras.push({ field: t.field, member: got.member, bytes: got.bytes });
          r.log.push(...got.log);
          console.log(
            JSON.stringify({
              event: "pull-extra-done",
              at: new Date().toISOString(),
              host: got.host,
              session: sessionId,
              field: t.field,
              bytes: got.bytes,
              member: got.member,
            }),
          );
        } catch (e) {
          warnings.push(
            `ไม่ได้${t.what} จาก ${t.member} (ข้อมูลและรูปหน้าเวชระเบียนได้มาแล้ว ใช้งานต่อได้): ` +
              (e instanceof Error ? e.message : String(e)),
          );
        }
      }
    } catch (e) {
      warnings.push(
        "ดึงไฟล์รูปไม่สำเร็จ (ไฟล์ .sql และ .js ได้มาแล้ว ใช้งานต่อได้): " +
          (e instanceof Error ? e.message : String(e)),
      );
    }

    console.log(
      JSON.stringify({
        event: "pull-dumps-done",
        at: new Date().toISOString(),
        host: r.host,
        session: sessionId,
        sql_bytes: r.sqlBytes,
        mongo_bytes: r.mongoBytes,
      }),
    );

    // The response is now a few hundred bytes instead of 12 MB: it describes what is on this
    // server rather than carrying it. `sessionId` is at the top level because the dumps live
    // there too now — the browser needs it even when the image archive failed.
    return Response.json({
      ok: true,
      host: r.host,
      fingerprint: r.fingerprint,
      sessionId,
      files: [
        { field: "sql" as const, name: `${source.mysqlDatabase}.sql`, bytes: r.sqlBytes },
        ...(r.mongoBytes !== null
          ? [{ field: "mongo" as const, name: `${source.mongoDatabase}.js`, bytes: r.mongoBytes }]
          : []),
      ],
      images,
      extras,
      // Housekeeping first, so "why is there less on disk than last time" has an answer in the
      // same place the operator is already reading.
      log: [...sweptLines, ...r.log],
      warnings,
    } satisfies PullResponse);
  } catch (e) {
    // The message is already operator-facing (which setting to fix, what the remote
    // said). 502: the failure is the far side's, not the request's.
    return fail(e instanceof Error ? e.message : String(e), 502);
  }
}
