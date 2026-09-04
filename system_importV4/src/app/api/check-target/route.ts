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

// READ-ONLY probe of the target, meant to be run before the green button.
//
// It answers the questions that decide whether an import will work at all: can we reach
// the server, can psql log in, and does this clinic_code exist there. It also reports how
// much is already stored under that code — the generated script aborts on a missing
// clinic, and an import onto a clinic that already holds data is a different decision
// from a first import.

interface Body {
  clinicCode?: string;
  targetId?: string;
}

// A single read-only statement set. No writes, no transaction needed.
function probeSQL(clinic: string): string {
  const lit = `'` + clinic.replaceAll(`'`, `''`) + `'`;
  return `\\pset pager off
SELECT current_database() AS database, current_user AS "user", version() AS server;
SELECT ${lit} AS clinic_code,
       EXISTS (SELECT 1 FROM clinic WHERE clinic_code = ${lit}) AS clinic_exists,
       (SELECT count(*) FROM clinic WHERE clinic_code = ${lit}) AS branch_rows,
       (SELECT count(*) FROM patient WHERE clinic_code = ${lit}) AS patients_now,
       (SELECT count(*) FROM catalog_item WHERE clinic_code = ${lit}) AS catalog_items_now,
       (SELECT count(*) FROM invoice WHERE clinic_code = ${lit}) AS invoices_now,
       (SELECT count(*) FROM receipt WHERE clinic_code = ${lit}) AS receipts_now,
       (SELECT count(*) FROM course_bought WHERE clinic_code = ${lit}) AS courses_now;
`;
}

export async function POST(req: Request): Promise<Response> {
  const auth = await requireSession(req);
  if ("denied" in auth) return auth.denied;

  let body: Body = {};
  try {
    body = (await req.json()) as Body;
  } catch {
    /* fields stay empty and are reported as missing below */
  }

  const target = resolveTarget(body.targetId);
  if (!target) {
    return Response.json(
      { ok: false, error: `ปลายทาง "${body.targetId ?? ""}" ไม่ถูกตั้งค่าไว้ — เลือกจากรายการที่มี` },
      { status: 400 },
    );
  }

  const clinic = String(body.clinicCode ?? "").trim();
  if (!clinic) {
    return Response.json({ ok: false, error: "ไม่มี clinic_code ให้ตรวจ" }, { status: 400 });
  }

  const r = await runSSH(sshArgs(target, psqlCommand(target)), probeSQL(clinic));
  const log = [r.stdout, r.stderr].filter(Boolean).join("\n").trim();

  if (r.code !== 0) {
    return Response.json(
      {
        ok: false,
        target: describeTarget(target),
        error: hint(r) || `ตรวจไม่ผ่าน (exit ${r.code})`,
        log,
      },
      { status: 422 },
    );
  }

  // psql prints `t`/`f` for booleans in its default aligned output.
  const clinicExists =
    /clinic_exists[\s\S]*?\n[^\n]*\bt\b/.test(r.stdout) || /\|\s*t\s*\|/.test(r.stdout);

  return Response.json({
    ok: true,
    target: describeTarget(target),
    isProduction: target.isProduction,
    clinicExists,
    log,
  });
}
