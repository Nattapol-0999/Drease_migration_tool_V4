import "server-only";


import { requireSession } from "@/lib/require-session";
import { getJob, startJob, type JobHandle } from "@/lib/jobs";
import { clinicGuard, scriptHasGuard } from "@/lib/script-guard";
import { executePhotoImport, PhotoRunError } from "@/lib/photo-run";
import { dropSession, sessionExists, stagedSize } from "@/lib/upload-staging";
import type { TargetId } from "@/lib/types";
import {
  describeTarget,
  hint,
  psqlCommand,
  resolveTarget,
  runSSHFromFile,
  sshArgs,
  type ServerTarget,
} from "@/lib/server-target";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// This endpoint WRITES to a real database. Everything below exists to make that
// deliberate: the environment must be one that was configured on the server, the caller
// must retype the clinic_code, and the SQL it runs is exactly the artifact the generator
// produced — identified by job id, so nothing the operator has not seen can be submitted.
//
// Like /api/generate it starts a job and returns an id: psql on a several-hundred-megabyte
// script runs far longer than the 100 seconds Cloudflare allows a proxied request.

interface Body {
  jobId?: string;
  clinicCode?: string;
  confirm?: string;
  targetId?: string;
  /**
   * Staging session holding the photo inputs (`mongo` + `images`), when the operator
   * attached an archive. Present means "run the photos in this job too".
   *
   * ONE BUTTON, TWO WRITES. The photo step used to be a separate survey-then-apply the
   * operator had to find and press after the SQL had finished, and in practice it was never
   * pressed: three tenants were imported and not one of them got its images. So it runs
   * here, after psql succeeds, in the same job.
   *
   * The order is not negotiable: photos.branch_code is resolved from the patient's branch
   * and every row is keyed on an HN, so the patients have to be in the database before the
   * images can be filed. SQL first, images second, in that job.
   */
  photoSessionId?: string;
  /**
   * Optional. What the imported image file names are hashed from, when it must not be the
   * clinic code — see PhotoRunOptions.photoNameSalt. Only a clinic that was RENAMED
   * between two imports needs it, and getting it wrong on such a clinic re-imports every
   * scan it already has rather than failing.
   */
  photoNameSalt?: string;
}


/**
 * Shape of a clinic_code. Declared here the same way /api/generate and
 * /api/import-photos declare it, rather than shared: three one-line copies of a regex
 * are cheaper to read than an import that makes each route's input rules live elsewhere.
 */
const CLINIC_RE = /^[A-Za-z0-9_-]{1,64}$/;

/** How many characters of psql's own output are put in front of the operator. */
const PSQL_TAIL_CHARS = 1600;

/**
 * tailOfLog returns the end of psql's output, which is where the reason is.
 *
 * ON_ERROR_STOP makes psql abort on the first error, so the last lines are the ERROR, its
 * DETAIL and the STATEMENT that raised it — exactly what names the row to fix. Before
 * this, the output was captured into the job's psqlLog and then never rendered anywhere,
 * so the cause of every SQL failure lived only in the target's Postgres log.
 */
function tailOfLog(log: string): string {
  const t = log.trim();
  if (t === "") return "";
  const tail = t.length > PSQL_TAIL_CHARS ? `…\n${t.slice(-PSQL_TAIL_CHARS)}` : t;
  return `สิ่งที่ psql ตอบกลับมา (ท้ายสุดคือสาเหตุ):\n${tail}`;
}

function fail(error: string, status = 400): Response {
  return Response.json({ ok: false, error }, { status });
}

export async function POST(req: Request): Promise<Response> {
  // Checked here as well as in the middleware: this is the one route that writes.
  const auth = await requireSession(req);
  if ("denied" in auth) return auth.denied;

  let body: Body;
  try {
    body = (await req.json()) as Body;
  } catch {
    return fail("อ่าน body ไม่ได้");
  }

  // The environment is resolved from a fixed list on the server. The browser sends an ID,
  // never a host: a request cannot point this at a database that was not configured here,
  // and there is no fallback default, because falling back is how a run meant for test
  // lands on production.
  const target = resolveTarget(body.targetId);
  if (!target) {
    return fail(`ปลายทาง "${body.targetId ?? ""}" ไม่ถูกตั้งค่าไว้ — เลือกจากรายการที่มี`);
  }

  const clinic = (body.clinicCode ?? "").trim();
  const confirm = (body.confirm ?? "").trim();
  const jobId = (body.jobId ?? "").trim();

  if (!clinic) return fail("ไม่มี clinic_code");
  if (confirm !== clinic) {
    return fail("ยืนยันไม่ผ่าน: พิมพ์ clinic_code ให้ตรงก่อนนำเข้า");
  }

  const genJob = jobId ? getJob(jobId, auth.session.user) : null;
  if (!genJob || genJob.kind !== "generate") {
    return fail("ไม่พบสคริปต์ของงานนี้ — สร้างไฟล์ .sql ก่อน", 404);
  }
  if (genJob.state !== "done" || !genJob.sqlPath) {
    return fail(`งานสร้างสคริปต์ยังไม่เสร็จ (สถานะ: ${genJob.state})`, 409);
  }
  if (genJob.clinicCode !== clinic) {
    return fail(`สคริปต์นี้เป็นของ ${genJob.clinicCode} ไม่ใช่ ${clinic} — สร้างใหม่ก่อนนำเข้า`);
  }

  // The artifact must be the one the generator produced for THIS clinic. The guard sits
  // right after the header, and the header grows with every warning of the run, so the
  // file is scanned until the guard turns up (src/lib/script-guard.ts) instead of only its
  // first 64 KB being read — that fixed peek refused premier-hoslistic's correct script
  // on 8 Oct 2026, its guard at byte 67,210. The file itself is never read whole.
  const sqlPath = genJob.sqlPath;
  if (!(await scriptHasGuard(sqlPath, clinicGuard(clinic)))) {
    return fail(`สคริปต์นี้ไม่ใช่ของ ${clinic} — สร้างใหม่ก่อนนำเข้า`);
  }

  // The photo inputs are checked BEFORE the job starts, so "you forgot the archive" is an
  // immediate answer on the button rather than a surprise two minutes into a write.
  const photoSessionId = (body.photoSessionId ?? "").trim();
  let photoImageBytes = 0;
  if (photoSessionId) {
    const cli = process.env.IMPORT_CLI_PATH;
    if (!cli) {
      return fail("ยังไม่ได้ตั้งค่า IMPORT_CLI_PATH — ชี้ไปที่ไฟล์ system_cliv4", 500);
    }
    if (!(await sessionExists(photoSessionId))) {
      return fail(
        "ไม่พบไฟล์รูปที่อัปโหลดไว้ (session หมดอายุหรือถูกล้าง) — แนบไฟล์รูปใหม่ หรือกดนำเข้าโดยไม่เอารูป",
        409,
      );
    }
    photoImageBytes = await stagedSize(photoSessionId, "images");
    if (photoImageBytes === 0) {
      return fail("ในการอัปโหลดนี้ไม่มีไฟล์รูป (public_img.tar.gz)");
    }
    if ((await stagedSize(photoSessionId, "mongo")) === 0) {
      return fail("นำเข้ารูปต้องมีไฟล์ MongoDB (.js) ด้วย — collection page อยู่ในไฟล์นั้น");
    }
  }

  // Checked here with the other photo inputs rather than inside the job: a malformed salt
  // is an immediate answer on the button, not a surprise after the SQL has committed.
  const photoNameSalt = (body.photoNameSalt ?? "").trim();
  if (photoNameSalt && !CLINIC_RE.test(photoNameSalt)) {
    return fail("photoNameSalt ไม่ถูกรูป — ต้องเป็นรูปแบบ clinic_code (A-Z a-z 0-9 _ - ยาวไม่เกิน 64)");
  }
  if (photoNameSalt && !photoSessionId) {
    return fail("ส่ง photoNameSalt มาแต่ไม่ได้แนบไฟล์รูป — salt ใช้กับการนำเข้ารูปเท่านั้น");
  }

  const bytes = genJob.sqlBytes ?? 0;
  const importJobId = await startJob(
    "import",
    {
      user: auth.session.user,
      clinicCode: clinic,
      phase: `กำลังส่งสคริปต์ ${(bytes / 1048576).toFixed(1)} MB เข้า psql ที่ ${target.id}`,
    },
    (h) =>
      runImport(h, {
        target,
        clinic,
        sqlPath,
        bytes,
        user: auth.session.user,
        photoSessionId,
        photoImageBytes,
        photoNameSalt,
      }),
  );

  return Response.json({
    ok: true,
    jobId: importJobId,
    target: describeTarget(target),
    withPhotos: photoSessionId !== "",
  });
}

async function runImport(
  h: JobHandle,
  args: {
    target: ServerTarget;
    clinic: string;
    sqlPath: string;
    bytes: number;
    user: string;
    photoSessionId: string;
    photoImageBytes: number;
    photoNameSalt: string;
  },
): Promise<void> {
  const { target, clinic, sqlPath, bytes, user, photoSessionId, photoImageBytes, photoNameSalt } =
    args;

  const started = Date.now();
  const r = await runSSHFromFile(sshArgs(target, psqlCommand(target)), sqlPath);
  const seconds = ((Date.now() - started) / 1000).toFixed(1);
  const log = [r.stdout, r.stderr].filter(Boolean).join("\n").trim();

  // Every write attempt is recorded, successful or not. Without it there is no way to
  // answer "who ran what, where, and when" after the fact.
  console.log(
    JSON.stringify({
      event: "import-to-server",
      at: new Date().toISOString(),
      user,
      target: target.id,
      host: target.sshHost,
      database: target.dbName,
      clinic,
      bytes,
      exit: r.code,
      seconds,
    }),
  );

  h.setImportResult({
    target: describeTarget(target),
    isProduction: target.isProduction,
    psqlLog: log || "psql ไม่ได้ส่งข้อความใด (สำเร็จ)",
  });
  h.addLog(`psql ใช้เวลา ${seconds} วินาที · ส่งไป ${(bytes / 1048576).toFixed(1)} MB`);

  if (r.code !== 0) {
    // The SQL failed, so the photos must not run: their rows are keyed on HNs that may
    // not be there. The staged archive is left alone for a retry.
    if (photoSessionId) {
      h.setPhotoResult({
        ran: false,
        ok: false,
        message: "ไม่ได้นำเข้ารูป เพราะสคริปต์ SQL ล้มก่อน — ไฟล์รูปที่อัปโหลดไว้ยังอยู่ กดนำเข้าอีกครั้งได้",
      });
    }
    // WHAT THIS USED TO SAY, AND WHY IT WAS WRONG
    //
    // "ฐานข้อมูลไม่ถูกเปลี่ยน เพราะสคริปต์อยู่ในทรานแซกชันเดียว" — the script is NOT one
    // transaction. It is one per phase (27 of them on a full clinic) because Postgres
    // locks per relation and a single transaction over 26 tables goes past
    // max_locks_per_transaction. See the comment in script.go.
    //
    // Measured twice this week: C52AEF6 failed in the money phase and left 2,057
    // patients, 19,313 invoices and 29,267 invoice lines committed. CBE8806 failed in the
    // stock phase and left 231 patients, 533 appointments and 188 catalogue rows. Both
    // times this message told the operator nothing had happened, and both times the
    // tenant had to be deleted before another attempt.
    //
    // The psql output is attached as well. It is the only place the real reason exists —
    // hint() only recognises connection problems, so every SQL-level failure used to
    // arrive as the same sentence with the cause left in a log nobody was told to read.
    throw new Error(
      [
        hint(r) || `นำเข้าไม่สำเร็จ (exit ${r.code})`,
        "⚠ ฐานข้อมูลอาจถูกเปลี่ยนไปแล้วบางส่วน — สคริปต์ถูกแบ่งเป็นหลายทรานแซกชัน (กลุ่มละหนึ่ง) " +
          "กลุ่มที่ผ่านไปก่อนจุดที่ล้มจะ commit ค้างอยู่ในฐาน ตรวจคลินิกนี้ที่ปลายทางก่อน " +
          "และถ้ามีแถวค้าง ต้องลบคลินิกให้เกลี้ยงก่อนนำเข้าอีกครั้ง ไม่งั้นรอบหน้าจะชน unique index",
        tailOfLog(log),
      ]
        .filter(Boolean)
        .join("\n\n"),
    );
  }

  if (!photoSessionId) {
    return;
  }

  // ── photos, in the same job ────────────────────────────────────────────────────
  //
  // The SQL is committed by now. A photo failure from here on is NOT an import failure:
  // patients, bills and the catalogue are in, and only the images are missing. Re-running
  // the whole import to fix that would mean deleting the tenant first, so this reports what
  // happened and leaves /api/import-photos able to finish the job on its own.
  //
  // That is why it does not throw. A thrown error would mark the job failed and tell the
  // operator their data did not land, which would be false.
  const cli = process.env.IMPORT_CLI_PATH ?? "";
  h.addLog(`เริ่มนำเข้ารูป — ไฟล์รูป ${(photoImageBytes / 1048576).toFixed(1)} MB`);
  // Said in the log, not only in the request: which naming rule was used decides whether a
  // later top-up sees these rows or imports them again, and the log is what survives.
  if (photoNameSalt && photoNameSalt !== clinic) {
    h.addLog(
      `ชื่อไฟล์รูปใช้ salt "${photoNameSalt}" ไม่ใช่ "${clinic}" — ใช้เมื่อคลินิกเปลี่ยนโค้ดหลังนำเข้ารอบก่อน ` +
        `เพื่อให้ชื่อไฟล์ตรงกับแถวที่ปลายทางมีอยู่แล้ว (ไฟล์ยังลงใต้ ${clinic} และ photos.clinic_code ยังเป็น ${clinic})`,
    );
  }
  try {
    const photo = await executePhotoImport({
      cli,
      clinic,
      targetId: target.id as TargetId,
      sessionId: photoSessionId,
      mode: "apply",
      user,
      photoNameSalt,
      onPhase: (p) => h.setPhase(p),
    });
    h.setPhotoResult({
      ran: true,
      ok: true,
      planned: photo.planned,
      matched: photo.matched,
      filesSent: photo.filesSent,
      thumbsSent: photo.thumbsSent,
      bytesSent: photo.bytesSent,
      rows: photo.rows,
      seconds: photo.seconds,
      uploads: photo.uploads,
      log: photo.log,
      logoFound: photo.extras?.logo?.found,
      profilesSent: photo.extras?.profiles?.sent,
      profilesCandidates: photo.extras?.profiles?.candidates,
      profilesSkipped: photo.extras?.profiles?.skippedNoPatient,
      extrasError: photo.extrasError ?? undefined,
    });
    h.addLog(
      `นำเข้ารูปแล้ว · คัดลอก ${photo.filesSent} ไฟล์ (${((photo.bytesSent ?? 0) / 1048576).toFixed(1)} MB) → ${photo.uploads} · เขียน ${photo.rows} แถว (${photo.seconds}s)`,
    );
    // The two decorative trees, reported on their own line so they can never be read as part
    // of the medical-record count above.
    if (photo.extras) {
      const bits: string[] = [];
      if (photo.extras.logo) {
        bits.push(
          photo.extras.logo.found
            ? `โลโก้ ${photo.extras.logo.wanted}`
            : `ไม่พบโลโก้ ${photo.extras.logo.wanted} (มีในถุง: ${photo.extras.logo.available.join(", ") || "ไม่มีไฟล์"})`,
        );
      }
      if (photo.extras.profiles) {
        const p = photo.extras.profiles;
        bits.push(
          `รูปโปรไฟล์ ${p.sent}/${p.candidates} ไฟล์` +
            (p.skippedNoPatient > 0 ? ` (ข้าม ${p.skippedNoPatient} — ไม่มี HN ในตาราง patient)` : ""),
        );
      }
      if (bits.length > 0) h.addLog(`เพิ่มเติม · ${bits.join(" · ")}`);
    }
    if (photo.extrasError) {
      h.addLog(`โลโก้/รูปโปรไฟล์ไม่สำเร็จ (ข้อมูลและรูปเวชระเบียนเข้าครบแล้ว): ${photo.extrasError}`);
    }
    // Only now is the upload finished with. Left until here so a photo failure keeps the
    // archive available for the standalone retry.
    await dropSession(photoSessionId);
  } catch (err) {
    const message =
      err instanceof PhotoRunError
        ? err.message
        : (err as Error).message || "นำเข้ารูปไม่สำเร็จ";
    h.setPhotoResult({
      ran: true,
      ok: false,
      message,
      planned: err instanceof PhotoRunError ? err.partial?.planned : undefined,
      matched: err instanceof PhotoRunError ? err.partial?.matched : undefined,
      filesSent: err instanceof PhotoRunError ? err.partial?.filesSent : undefined,
      bytesSent: err instanceof PhotoRunError ? err.partial?.bytesSent : undefined,
      log: err instanceof PhotoRunError ? err.partial?.log : undefined,
    });
    h.addLog(`⚠ ข้อมูลเข้าฐานเรียบร้อยแล้ว แต่นำเข้ารูปไม่สำเร็จ: ${message}`);
    h.addLog(
      "ไฟล์รูปที่อัปโหลดไว้ยังอยู่บนเซิร์ฟเวอร์ — กดนำเข้ารูปซ้ำได้ที่ส่วน “รูปหน้าเวชระเบียน” โดยไม่ต้องนำเข้า SQL ใหม่",
    );
  }
}
