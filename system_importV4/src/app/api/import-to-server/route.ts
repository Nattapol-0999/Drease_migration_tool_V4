import "server-only";

import { open } from "node:fs/promises";

import { requireSession } from "@/lib/require-session";
import { getJob, startJob, type JobHandle } from "@/lib/jobs";
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
}

/** How much of the script's head is checked for the clinic guard. */
const GUARD_PEEK_BYTES = 64 * 1024;

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

  // The artifact must be the one the generator produced for THIS clinic. Checked by
  // reading only the head of the file: the guard is emitted near the top, and the file
  // itself may be hundreds of megabytes.
  const sqlPath = genJob.sqlPath;
  const guard = `SET LOCAL app.clinic_code = '${clinic}'`;
  const fh = await open(sqlPath, "r");
  let head: string;
  try {
    const buf = Buffer.alloc(Math.min(GUARD_PEEK_BYTES, genJob.sqlBytes ?? GUARD_PEEK_BYTES));
    await fh.read(buf, 0, buf.length, 0);
    head = buf.toString("utf8");
  } finally {
    await fh.close();
  }
  if (!head.includes(guard)) {
    return fail(`สคริปต์นี้ไม่ใช่ของ ${clinic} — สร้างใหม่ก่อนนำเข้า`);
  }

  const bytes = genJob.sqlBytes ?? 0;
  const importJobId = await startJob(
    "import",
    {
      user: auth.session.user,
      clinicCode: clinic,
      phase: `กำลังส่งสคริปต์ ${(bytes / 1048576).toFixed(1)} MB เข้า psql ที่ ${target.id}`,
    },
    (h) => runImport(h, { target, clinic, sqlPath, bytes, user: auth.session.user }),
  );

  return Response.json({ ok: true, jobId: importJobId, target: describeTarget(target) });
}

async function runImport(
  h: JobHandle,
  args: {
    target: ServerTarget;
    clinic: string;
    sqlPath: string;
    bytes: number;
    user: string;
  },
): Promise<void> {
  const { target, clinic, sqlPath, bytes, user } = args;

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
    throw new Error(
      hint(r) ||
        `นำเข้าไม่สำเร็จ (exit ${r.code}) — ฐานข้อมูลไม่ถูกเปลี่ยน เพราะสคริปต์อยู่ในทรานแซกชันเดียว`,
    );
  }
}
