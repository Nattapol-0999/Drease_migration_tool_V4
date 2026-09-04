import "server-only";

import { execFile } from "node:child_process";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";

import { requireSession } from "@/lib/require-session";
import { CLI_INPUTS, type CliInputField, type GenerateResponse } from "@/lib/types";

const run = promisify(execFile);

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// The upload is held in memory only long enough to hand it to the CLI, so cap it. The
// CLI itself streams nothing; a clinic's six-file export is a few MB.
const MAX_TOTAL_BYTES = 64 * 1024 * 1024;

// clinic_code and owner_username are passed to a child process. execFile takes an ARGV
// array (no shell), so there is no interpolation to escape — but they are still
// validated, because a value that cannot be a real code is a mistake worth catching
// before a file is generated for the wrong tenant.
const CLINIC_RE = /^[A-Za-z0-9_-]{1,64}$/;
const OWNER_RE = /^[\w.@-]{1,64}$/;

function fail(error: string, status = 400): Response {
  return Response.json({ ok: false, error } satisfies GenerateResponse, { status });
}

export async function POST(req: Request): Promise<Response> {
  // Checked here as well as in the middleware: this route spawns a process.
  const auth = await requireSession(req);
  if ("denied" in auth) return auth.denied;

  const cli = process.env.IMPORT_CLI_PATH;
  if (!cli) {
    return fail(
      "ยังไม่ได้ตั้งค่า IMPORT_CLI_PATH — ชี้ไปที่ไฟล์ system_cliv4.exe แล้วรีสตาร์ต dev server",
      500,
    );
  }

  const form = await req.formData();
  const clinic = String(form.get("clinic_code") ?? "").trim();
  const owner = String(form.get("owner_username") ?? "").trim();

  if (!CLINIC_RE.test(clinic)) {
    return fail("clinic_code ไม่ถูกรูป (ใช้ A-Z a-z 0-9 _ - ยาวไม่เกิน 64)");
  }
  if (owner && !OWNER_RE.test(owner)) {
    return fail("owner_username ไม่ถูกรูป");
  }

  // Collect the uploaded dumps.
  const uploads: { field: CliInputField; file: File }[] = [];
  let total = 0;
  for (const { field } of CLI_INPUTS) {
    const v = form.get(field);
    if (v instanceof File && v.size > 0) {
      total += v.size;
      uploads.push({ field, file: v });
    }
  }
  if (uploads.length === 0) {
    return fail("ยังไม่ได้เลือกไฟล์ dump แม้แต่ไฟล์เดียว");
  }
  if (total > MAX_TOTAL_BYTES) {
    return fail(`ไฟล์รวมกันใหญ่เกิน ${MAX_TOTAL_BYTES / 1024 / 1024} MB`);
  }
  // The combined SQL dump carries the users table, so an owner has to be named: a
  // clinic imported without one has nobody who can administer it.
  if (uploads.some((u) => u.field === "sql") && !owner) {
    return fail("ไฟล์ SQL มีตาราง users → ต้องระบุ owner_username (legacy users.code ของบัญชีที่จะเป็น owner)");
  }

  // The CLI reads and writes files, so the request is staged in a private temp dir that
  // is removed again whatever happens.
  const work = await mkdtemp(join(tmpdir(), "system-importv4-"));
  try {
    const args: string[] = ["-clinic", clinic, "-out", work];
    if (owner) args.push("-owner", owner);
    // Empty means everything, which is what the CLI already does.
    const only = String(form.get("only") ?? "")
      .split(",")
      .map((s) => s.trim())
      .filter((s) => /^[a-z_]+$/.test(s));
    if (only.length > 0) args.push("-only", only.join(","));

    for (const { field, file } of uploads) {
      // Keep the original extension. The CLI chooses its parser from the FLAG, not the
      // name, but a faithful name makes the temp dir diagnosable when something fails.
      const lower = file.name.toLowerCase();
      let ext = ".sql";
      if (lower.endsWith(".ods")) ext = ".ods";
      else if (lower.endsWith(".js")) ext = ".js";
      else if (lower.endsWith(".json")) ext = ".json";
      const path = join(work, `${field}${ext}`);
      await writeFile(path, Buffer.from(await file.arrayBuffer()));
      args.push(`-${field}`, path);
    }

    let report: string;
    try {
      const { stdout } = await run(cli, args, {
        maxBuffer: 32 * 1024 * 1024,
        windowsHide: true,
      });
      report = stdout;
    } catch (err) {
      // The CLI fails closed and puts the reason on stderr — surface it as-is rather
      // than a generic message, because it names the rows that need fixing.
      const e = err as { stderr?: string; message?: string };
      const detail = (e.stderr || e.message || "").trim();
      return fail(detail || "รัน CLI ไม่สำเร็จ", 422);
    }

    const filename = `import_${clinic}.sql`;
    const sql = await readFile(join(work, filename), "utf8");

    return Response.json({ ok: true, filename, sql, report } satisfies GenerateResponse);
  } finally {
    await rm(work, { recursive: true, force: true });
  }
}
