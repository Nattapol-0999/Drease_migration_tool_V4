import "server-only";

import { stat } from "node:fs/promises";

import {
  checkHost,
  pullDumpsToFiles,
  pullImagesToFile,
  readLegacySource,
} from "@/lib/legacy-source";
import { startJob, sweepJobs, type JobHandle } from "@/lib/jobs";
import { requireSession } from "@/lib/require-session";
import { createSession, stagedPath, sweepStaging } from "@/lib/upload-staging";
import type { PullResponse, PulledExtraInfo, PulledImageInfo } from "@/lib/types";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// This endpoint READS a legacy clinic database over SSH and streams what it finds into a
// staging session on this server. It writes nothing to any clinic database — only
// /api/import-to-server can do that.
//
// The request carries the host and one flag. Credentials come from the environment, and the
// server re-checks the config and the allowlist itself: the UI's gating is a convenience,
// never the control.
//
// IT IS A JOB, NOT A SYNCHRONOUS CALL (22 Sep 2026)
//
// Cloudflare closes a proxied request after 100 seconds. Pulling a 15 GB image archive took
// about 50 minutes at the 4.5 MB/s measured against the real source, so the browser was always
// handed the proxy's HTML error page — "Unexpected token '<' … is not valid JSON" — while the
// transfer carried on running invisibly. Worse, the reply that would have named the staging
// session went nowhere, so 6 GB of correctly pulled files sat on disk with nothing able to
// address them.
//
// Starting the work and answering with a job id keeps every request short and lets the browser
// poll /api/job, which is what the generate, import and survey steps already do.

interface Body {
  host?: string;
  /**
   * Whether to pull the page-image archive. Defaults to true — absent means yes, so an older
   * client that does not send it behaves exactly as before.
   *
   * WHY IT CAN BE TURNED OFF (owner's decision, 21 Sep 2026)
   *
   * A source turned up with 15 GB of page images over 29,247 files. The clinic's DATA can be
   * taken now and its images dealt with separately: the two dumps are 4.8 MB and 96 MB for that
   * clinic — seconds, not an hour.
   */
  withImages?: boolean;
}

function fail(error: string, status = 400): Response {
  return Response.json({ ok: false, error } satisfies PullResponse, { status });
}

/** Sizes in the phase text. Bytes alone are unreadable at this scale. */
function human(n: number): string {
  if (n >= 1073741824) return `${(n / 1073741824).toFixed(2)} GB`;
  if (n >= 1048576) return `${Math.round(n / 1048576)} MB`;
  return `${Math.round(n / 1024)} KB`;
}

export async function POST(req: Request): Promise<Response> {
  // The job store scopes a job to the operator who started it, so this endpoint needs to know
  // who that is. It is also the same gate every other write-capable route already sits behind.
  const auth = await requireSession(req);
  if ("denied" in auth) return auth.denied;

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

  // Absent means yes: an older client that does not send the field keeps the old behaviour.
  const withImages = body.withImages !== false;

  // Everything below this line runs AFTER the response has gone out. Failures are recorded on
  // the job — startJob catches the rejection — so nothing here can become an unhandled
  // rejection, and nothing here can be lost by a browser that walked away.
  const jobId = await startJob(
    "pull",
    {
      user: auth.session.user,
      // A pull happens before any clinic is chosen, so there is no clinic code to record. The
      // host is what identifies this work, and it is in the phase text.
      clinicCode: "",
      phase: `เตรียมดึงข้อมูลจาก ${host}`,
    },
    (h) => runPull(h, source, host, withImages),
  );

  return Response.json({ ok: true, jobId } satisfies PullResponse);
}

async function runPull(
  h: JobHandle,
  source: NonNullable<ReturnType<typeof readLegacySource>["source"]>,
  host: string,
  withImages: boolean,
): Promise<void> {
  // Sweep BEFORE anything is created, so this can never touch what this job is about to write.
  // Both sweepers are age-based and a session seconds old is not a candidate, but the ordering
  // makes that true by construction rather than by reading their thresholds.
  //
  // WHY THE SWEEP IS HERE AT ALL
  //
  // sweepStaging was only ever called from /api/upload-chunk — the browser-upload lane. This
  // path streams from the source straight to disk and never touches that route, so a pull swept
  // nothing, ever. Measured on 21 Sep 2026: 2.4 GB of abandoned sessions and generated scripts
  // had to be deleted by hand.
  //
  // Failure here is reported and swallowed on purpose: housekeeping must never be the reason a
  // pull does not happen.
  h.setPhase("เก็บกวาดไฟล์ค้างก่อนเริ่ม");
  try {
    const st = await sweepStaging();
    if (st.removed > 0) {
      h.addLog(`เก็บกวาด staging: ลบ ${st.removed} เซสชันที่ค้าง (${human(st.bytes)})`);
    }
  } catch (e) {
    h.addLog(
      `เก็บกวาด staging ไม่สำเร็จ (ไม่กระทบการดึงข้อมูล): ${e instanceof Error ? e.message : String(e)}`,
    );
  }
  try {
    await sweepJobs();
  } catch (e) {
    h.addLog(
      `เก็บกวาดงานเก่าไม่สำเร็จ (ไม่กระทบการดึงข้อมูล): ${e instanceof Error ? e.message : String(e)}`,
    );
  }

  // The session is created FIRST, because everything this job pulls lands in it — the two dumps
  // included. They used to come back as base64 in the reply and be re-uploaded by the browser
  // into a second session, which meant the same bytes crossed the wire twice and were held in
  // memory at least twice. On 18 Sep 2026 that killed the process: a source with bills 43.6 MB
  // + payment_summary 30.5 MB in Mongo went past the 467 MB V8 heap.
  const sessionId = await createSession();
  h.addLog(`เซสชันที่เก็บไฟล์: ${sessionId}`);

  h.setPhase("ดึง MySQL และ MongoDB");
  const r = await pullDumpsToFiles(source, host, {
    sql: stagedPath(sessionId, "sql"),
    mongo: stagedPath(sessionId, "mongo"),
  });
  h.addLog(
    `ได้ข้อมูลแล้ว — MySQL ${human(r.sqlBytes)}` +
      (r.mongoBytes !== null ? ` · MongoDB ${human(r.mongoBytes)}` : " · ไม่มี MongoDB"),
  );
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

  let images: PulledImageInfo | null = null;
  const extras: PulledExtraInfo[] = [];
  const warnings = [...r.warnings];

  if (!withImages) {
    // Said as a warning, not passed over in silence. `images: null` is also what a FAILED pull
    // looks like, and the two have to be told apart — one means try again, the other means this
    // is what was asked for.
    warnings.push(
      "ข้ามการดึงไฟล์รูปหน้าเวชระเบียนตามที่เลือกไว้ — ได้ข้อมูลและโลโก้/รูปโปรไฟล์ครบ " +
        "แต่ขั้นนำเข้ารูปจะยังทำไม่ได้จนกว่าจะมีไฟล์รูป",
    );
  }

  // A failure in this whole block is a WARNING, not a failure of the job. The two dumps are in
  // hand and independently useful; discarding them because the images did not arrive would cost
  // another full pull of the data as well.
  try {
    if (withImages) {
      const imagesPath = stagedPath(sessionId, "images");
      h.setPhase("ดึงไฟล์รูปหน้าเวชระเบียน");

      // The only progress signal that exists. The source streams a tar and never says how big
      // it will be, so there is no total and no percentage — just the file growing on disk.
      // Without this the modal would show one unchanging sentence for the best part of an hour,
      // which reads exactly like being stuck.
      const ticker = setInterval(() => {
        void stat(imagesPath)
          .then((s) => {
            h.setPullBytes(s.size);
            h.setPhase(`ดึงไฟล์รูปหน้าเวชระเบียน — ได้ ${human(s.size)} แล้ว`);
          })
          // The file does not exist for the first moments, and a stat failure must never be the
          // thing that stops a transfer. Nothing is lost by missing one tick.
          .catch(() => {});
      }, 3000);

      try {
        const img = await pullImagesToFile(source, host, imagesPath);
        images = {
          sessionId,
          name: "public_img.tar.gz",
          bytes: img.bytes,
          member: img.member,
        };
        h.setPullBytes(img.bytes);
        h.addLog(`ได้ไฟล์รูปแล้ว ${human(img.bytes)} (${img.member})`);
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
      } finally {
        clearInterval(ticker);
      }
    }

    // The clinic logo and the patient profile photos, into the SAME session so the import step
    // finds all three together without the operator choosing anything.
    //
    // OUTSIDE the images branch, and not conditional on the archive having arrived: the browser
    // addresses the session through a top-level field now. Measured at 356 KB + 556 KB — worth
    // having even when the 15 GB archive is not.
    //
    // Each tree gets its OWN try: a V2 box without an img/clinic-logo folder is normal, and one
    // missing folder must not cost the other tree or the archive that did arrive.
    h.setPhase("ดึงโลโก้คลินิกและรูปโปรไฟล์คนไข้");
    for (const t of [
      { field: "logo" as const, member: source.logoMember, what: "โลโก้คลินิก" },
      { field: "profiles" as const, member: source.profileMember, what: "รูปโปรไฟล์คนไข้" },
    ]) {
      try {
        const got = await pullImagesToFile(source, host, stagedPath(sessionId, t.field), t.member);
        extras.push({ field: t.field, member: got.member, bytes: got.bytes });
        h.addLog(`ได้${t.what} ${human(got.bytes)} (${got.member})`);
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

  // The result describes what is on this server rather than carrying it. sessionId is at the
  // top level because the dumps live there too — the browser needs it even when the image
  // archive failed.
  h.setPullResult({
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
    log: r.log,
    warnings,
  });
}
