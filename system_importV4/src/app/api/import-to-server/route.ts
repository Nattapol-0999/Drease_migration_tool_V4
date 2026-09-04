import "server-only";

import { requireSession } from "@/lib/require-session";
import {
  describeTarget,
  hint,
  psqlCommand,
  readServerTarget,
  runSSH,
  sshArgs,
} from "@/lib/server-target";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// This endpoint WRITES to a real database. Everything below exists to make that
// deliberate: the target must be fully configured, the caller must retype the
// clinic_code, and the SQL it runs is exactly the reviewed artifact — the server is
// never handed anything the operator has not seen.

interface Body {
  sql?: string;
  clinicCode?: string;
  confirm?: string;
}

function fail(error: string, status = 400): Response {
  return Response.json({ ok: false, error }, { status });
}

export async function POST(req: Request): Promise<Response> {
  // Checked here as well as in the middleware: this is the one route that writes.
  const auth = await requireSession(req);
  if ("denied" in auth) return auth.denied;

  const { target, missing } = readServerTarget();
  if (!target) {
    return fail(
      `ยังไม่ได้ตั้งค่าปลายทาง — ขาด ${missing.join(", ")} ใน .env.local แล้วรีสตาร์ต dev server`,
      503,
    );
  }

  let body: Body;
  try {
    body = (await req.json()) as Body;
  } catch {
    return fail("อ่าน body ไม่ได้");
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

  if (r.code !== 0) {
    return Response.json(
      {
        ok: false,
        error:
          hint(r) ||
          `นำเข้าไม่สำเร็จ (exit ${r.code}) — ฐานข้อมูลไม่ถูกเปลี่ยน เพราะสคริปต์อยู่ในทรานแซกชันเดียว`,
        target: describeTarget(target),
        seconds,
        log,
      },
      { status: 422 },
    );
  }

  return Response.json({
    ok: true,
    target: describeTarget(target),
    seconds,
    log: log || "psql ไม่ได้ส่งข้อความใด (สำเร็จ)",
  });
}
