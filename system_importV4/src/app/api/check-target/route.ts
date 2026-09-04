import "server-only";

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

// READ-ONLY probe of the target, meant to be run before the green button.
//
// It answers the three questions that decide whether an import will work at all:
// can we reach the server, can psql log in, and does this clinic_code exist there.
// The last one matters most: the generated script aborts on a missing clinic, and
// finding that out here costs nothing.

interface Body {
  clinicCode?: string;
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
       (SELECT count(*) FROM catalog_item WHERE clinic_code = ${lit}) AS catalog_items_now;
`;
}

export async function POST(req: Request): Promise<Response> {
  const { target, missing } = readServerTarget();
  if (!target) {
    return Response.json(
      { ok: false, error: `ยังไม่ได้ตั้งค่าปลายทาง — ขาด ${missing.join(", ")}` },
      { status: 503 },
    );
  }

  let clinic = "";
  try {
    clinic = String(((await req.json()) as Body).clinicCode ?? "").trim();
  } catch {
    /* clinic stays empty; the probe still reports connectivity */
  }
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
  const clinicExists = /clinic_exists[\s\S]*?\n[^\n]*\bt\b/.test(r.stdout) || /\|\s*t\s*\|/.test(r.stdout);

  return Response.json({
    ok: true,
    target: describeTarget(target),
    clinicExists,
    log,
  });
}
