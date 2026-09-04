import "server-only";

import { requireSession } from "@/lib/require-session";
import {
  describeTarget,
  hint,
  psqlCommand,
  resolveTarget,
  runSSH,
  sshArgs,
} from "@/lib/server-target";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// This endpoint WRITES to a real database. Everything below exists to make that
// deliberate: the environment must be one that was configured on the server, the caller
// must retype the clinic_code, and the SQL it runs is exactly the reviewed artifact —
// the server is never handed anything the operator has not seen.

interface Body {
  sql?: string;
  clinicCode?: string;
  confirm?: string;
  targetId?: string;
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
    return fail(
      `ปลายทาง "${body.targetId ?? ""}" ไม่ถูกตั้งค่าไว้ — เลือกจากรายการที่มี`,
      400,
    );
  }

  const sql = (body.sql ?? "").trim();
  const clinic = (body.clinicCode ?? "").trim();
  const confirm = (body.confirm ?? "").trim();

  if (!sql) return fail("ไม่มีสคริปต์ให้รัน — สร้างไฟล์ .sql ก่อน");
  if (!clinic) return fail("ไม่มี clinic_code");
  if (confirm !== clinic) {
    return fail("ยืนยันไม่ผ่าน: พิมพ์ clinic_code ให้ตรงก่อนนำเข้า");
  }
  // The artifact must be the one the generator produced for THIS clinic. A mismatch
  // means the operator is about to write one clinic's data under another's code.
  if (!sql.includes(`SET LOCAL app.clinic_code = '${clinic}'`)) {
    return fail(`สคริปต์นี้ไม่ใช่ของ ${clinic} — สร้างใหม่ก่อนนำเข้า`);
  }

  const started = Date.now();
  const r = await runSSH(sshArgs(target, psqlCommand(target)), sql);
  const seconds = ((Date.now() - started) / 1000).toFixed(1);
  const log = [r.stdout, r.stderr].filter(Boolean).join("\n").trim();

  // Every write attempt is recorded, successful or not. Without it there is no way to
  // answer "who ran what, where, and when" after the fact.
  console.log(
    JSON.stringify({
      event: "import-to-server",
      at: new Date().toISOString(),
      user: auth.session.user,
      target: target.id,
      host: target.sshHost,
      database: target.dbName,
      clinic,
      bytes: sql.length,
      exit: r.code,
      seconds,
    }),
  );

  if (r.code !== 0) {
    return Response.json(
      {
        ok: false,
        error:
          hint(r) ||
          `นำเข้าไม่สำเร็จ (exit ${r.code}) — ฐานข้อมูลไม่ถูกเปลี่ยน เพราะสคริปต์อยู่ในทรานแซกชันเดียว`,
        target: describeTarget(target),
        isProduction: target.isProduction,
        seconds,
        log,
      },
      { status: 422 },
    );
  }

  return Response.json({
    ok: true,
    target: describeTarget(target),
    isProduction: target.isProduction,
    seconds,
    log: log || "psql ไม่ได้ส่งข้อความใด (สำเร็จ)",
  });
}
