import "server-only";

import { execFile } from "node:child_process";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";

import { requireSession } from "@/lib/require-session";
import {
  buildInsertSQL,
  cleanup,
  describeUploads,
  extractArchive,
  readManifest,
  readUploadsTarget,
  shipFiles,
  surveyArchive,
} from "@/lib/photo-import";
import { TARGET_IDS, type TargetId } from "@/lib/types";
import { describeTarget, hint, psqlCommand, resolveTarget, runSSH, sshArgs } from "@/lib/server-target";

const run = promisify(execFile);

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// Importing the legacy patient page images.
//
// TWO MODES, ONE REQUEST SHAPE. `survey` does everything except write: it runs the CLI to
// get the copy plan, unpacks the archive, and reports what lines up. `apply` repeats that
// and then writes. They are the same route because the survey must describe the exact
// plan the apply will execute — two routes could drift, and a preview that does not match
// what happens is worse than no preview.
//
// The archive is uploaded again for the apply rather than cached between calls. This
// process is one server with no shared store, so a cache would be a temp file whose
// lifetime nobody owns; re-sending 20-odd MB from a File the browser already holds costs
// a few seconds and removes that whole class of problem.
//
// WRITE ORDER: bytes to the backend host first, rows to the database second. A file with
// no row is invisible; a row with no file is a broken image inside a medical record.

// 64 MB matches the generate route, nginx's client_max_body_size on the tool host, and
// Next's own bodySizeLimit. Cloudflare's free plan caps at 100 MB above all of them, so
// raising this alone would not help.
const MAX_TOTAL_BYTES = 64 * 1024 * 1024;

const CLINIC_RE = /^[A-Za-z0-9_-]{1,64}$/;

function fail(error: string, status = 400, extra: Record<string, unknown> = {}): Response {
  return Response.json({ ok: false, error, ...extra }, { status });
}

export async function POST(req: Request): Promise<Response> {
  // Checked here as well as in the middleware: this route spawns processes and, in apply
  // mode, writes to two production hosts.
  const auth = await requireSession(req);
  if ("denied" in auth) return auth.denied;

  const cli = process.env.IMPORT_CLI_PATH;
  if (!cli) {
    return fail("ยังไม่ได้ตั้งค่า IMPORT_CLI_PATH — ชี้ไปที่ไฟล์ system_cliv4", 500);
  }

  const form = await req.formData();
  const mode = String(form.get("mode") ?? "survey");
  if (mode !== "survey" && mode !== "apply") {
    return fail(`mode ต้องเป็น survey หรือ apply (ได้ "${mode}")`);
  }

  const id = String(form.get("targetId") ?? "");
  if (!(TARGET_IDS as readonly string[]).includes(id)) {
    return fail(`ปลายทาง "${id}" ไม่อยู่ในรายการที่ตั้งค่าไว้`);
  }
  const targetId = id as TargetId;

  const clinic = String(form.get("clinicCode") ?? "").trim();
  if (!CLINIC_RE.test(clinic)) {
    return fail("clinic_code ไม่ถูกรูป (ใช้ A-Z a-z 0-9 _ - ยาวไม่เกิน 64)");
  }

  // Where the rows go (database host) and where the bytes go (backend host) are two
  // different machines, and BOTH must be configured before anything is unpacked.
  const dbTarget = resolveTarget(targetId);
  if (!dbTarget) {
    return fail(`ปลายทางฐานข้อมูลของ ${targetId} ยังตั้งค่าไม่ครบ`, 503);
  }
  const { target: uploads, missing } = readUploadsTarget(targetId);
  if (!uploads) {
    return fail(
      `ยังตั้งค่าที่เก็บไฟล์รูปของ ${targetId} ไม่ครบ — ขาด ${missing.join(", ")}`,
      503,
      { missing },
    );
  }

  const pageFile = form.get("page");
  const imagesFile = form.get("images");
  if (!(pageFile instanceof File) || pageFile.size === 0) {
    return fail("ยังไม่ได้แนบไฟล์ export ของ mris.page (.js)");
  }
  if (!(imagesFile instanceof File) || imagesFile.size === 0) {
    return fail("ยังไม่ได้แนบไฟล์รูป (public_img.tar.gz)");
  }
  const total = pageFile.size + imagesFile.size;
  if (total > MAX_TOTAL_BYTES) {
    return fail(
      `ไฟล์รวมกัน ${(total / 1024 / 1024).toFixed(1)} MB ใหญ่เกิน ${MAX_TOTAL_BYTES / 1024 / 1024} MB`,
    );
  }

  // apply retypes the clinic_code, the same gate the SQL import uses. It is the last
  // thing standing between "wrong tenant selected ten minutes ago" and rows in a real
  // patient record.
  if (mode === "apply") {
    const confirm = String(form.get("confirm") ?? "").trim();
    if (confirm !== clinic) {
      return fail("ยืนยันไม่ผ่าน: พิมพ์ clinic_code ให้ตรงก่อนนำเข้า");
    }
  }

  const work = await mkdtemp(join(tmpdir(), "importv4-page-"));
  let extractRoot = "";
  try {
    // 1 · the copy plan comes from the CLI, which owns every mapping decision.
    const pagePath = join(work, "page.js");
    await writeFile(pagePath, Buffer.from(await pageFile.arrayBuffer()));

    let report: string;
    try {
      const { stdout } = await run(cli, ["-clinic", clinic, "-page", pagePath, "-out", work], {
        maxBuffer: 32 * 1024 * 1024,
        windowsHide: true,
      });
      report = stdout;
    } catch (err) {
      const e = err as { stderr?: string; message?: string };
      return fail((e.stderr || e.message || "").trim() || "รัน CLI ไม่สำเร็จ", 422);
    }

    const manifest = await readManifest(join(work, `photos_${clinic}.json`), clinic);

    // 2 · unpack the operator's archive and weigh every planned file.
    const tarPath = join(work, "images.tar.gz");
    await writeFile(tarPath, Buffer.from(await imagesFile.arrayBuffer()));
    extractRoot = await extractArchive(tarPath);
    const survey = await surveyArchive(manifest, extractRoot);

    const { sql, rows } = buildInsertSQL(clinic, manifest, survey);

    const base = {
      target: describeTarget(dbTarget),
      uploads: describeUploads(uploads),
      isProduction: dbTarget.isProduction,
      planned: manifest.files.length,
      matched: survey.matched.length,
      missing: survey.missing,
      extraCount: survey.extraCount,
      extraSample: survey.extraSample,
      totalBytes: survey.totalBytes,
      rows,
      report,
    };

    if (mode === "survey") {
      return Response.json({ ok: true, mode, ...base, sql });
    }

    if (survey.matched.length === 0) {
      return fail(
        "ไม่มีรูปที่จับคู่กับไฟล์ในถุงได้เลย — ไม่มีอะไรจะนำเข้า (ตรวจว่า tar ถูก export จาก public/ และมีโฟลเดอร์ nas-doctorease/)",
        422,
        base,
      );
    }

    // 3 · bytes first. A row pointing at a file that never arrived is a broken image in
    // somebody's record, so nothing is inserted until the copy has succeeded.
    const started = Date.now();
    let ship;
    try {
      ship = await shipFiles(uploads, manifest, survey, extractRoot);
    } catch (err) {
      const e = err as { stderr?: string; message?: string };
      return fail(
        `คัดลอกไฟล์รูปไปที่ ${describeUploads(uploads)} ไม่สำเร็จ — ยังไม่ได้เขียนแถวใดลงฐานข้อมูล: ${(e.stderr || e.message || "").trim()}`,
        422,
        base,
      );
    }

    // 4 · then the rows, in one transaction on the database host.
    const r = await runSSH(sshArgs(dbTarget, psqlCommand(dbTarget)), sql);
    const seconds = ((Date.now() - started) / 1000).toFixed(1);
    const log = [ship.log, r.stdout, r.stderr].filter(Boolean).join("\n").trim();

    // Recorded whether it worked or not: without this there is no answer to "who moved
    // which clinic's images, where, and when".
    console.log(
      JSON.stringify({
        event: "import-photos",
        at: new Date().toISOString(),
        user: auth.session.user,
        target: targetId,
        clinic,
        uploads_host: uploads.sshHost,
        database: dbTarget.dbName,
        planned: manifest.files.length,
        files_sent: ship.filesSent,
        bytes_sent: ship.bytesSent,
        rows,
        exit: r.code,
        seconds,
      }),
    );

    if (r.code !== 0) {
      return Response.json(
        {
          ok: false,
          mode,
          ...base,
          error:
            hint(r) ||
            `เขียนแถวไม่สำเร็จ (exit ${r.code}) — ฐานข้อมูลไม่ถูกเปลี่ยนเพราะอยู่ในทรานแซกชันเดียว แต่ไฟล์รูป ${ship.filesSent} ไฟล์ถูกคัดลอกขึ้นเครื่องปลายทางแล้ว (ไม่มีแถวอ้างถึง จึงไม่มีใครเห็น)`,
          filesSent: ship.filesSent,
          bytesSent: ship.bytesSent,
          seconds,
          log,
        },
        { status: 422 },
      );
    }

    return Response.json({
      ok: true,
      mode,
      ...base,
      filesSent: ship.filesSent,
      bytesSent: ship.bytesSent,
      seconds,
      log: log || "psql ไม่ได้ส่งข้อความใด (สำเร็จ)",
    });
  } catch (err) {
    return fail((err as Error).message || "นำเข้ารูปไม่สำเร็จ", 422);
  } finally {
    await cleanup(extractRoot);
    await rm(work, { recursive: true, force: true });
  }
}
