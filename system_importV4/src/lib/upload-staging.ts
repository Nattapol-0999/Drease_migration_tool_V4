import "server-only";

import { randomUUID } from "node:crypto";
import { appendFile, mkdir, readdir, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { CLI_INPUTS, type CliInputField } from "./types";

/**
 * Where a chunked upload is assembled, and how it is cleaned up.
 *
 * WHY NOT /tmp
 * On the deployed box `/tmp` is a 455 MB tmpfs — writing there spends RAM, which is the
 * scarcest thing on a 908 MB instance and the reason large dumps failed in the first
 * place. UPLOAD_STAGING_DIR must point at real disk (the root volume has ~22 GB free).
 * The default below is only a development convenience.
 *
 * WHY THE SERVER OWNS THE SESSION ID
 * The id becomes a directory name. A client-supplied value is a path traversal waiting to
 * happen, so it is generated here and every incoming id is matched against the UUID shape
 * before it is used in a path.
 */

const STAGING_ROOT =
  process.env.UPLOAD_STAGING_DIR?.trim() || join(tmpdir(), "system-importv4-staging");

/** Sessions older than this are abandoned uploads and get swept away. */
const MAX_SESSION_AGE_MS = 6 * 60 * 60 * 1000;

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

const FIELDS: readonly CliInputField[] = CLI_INPUTS.map((i) => i.field);

export function stagingRoot(): string {
  return STAGING_ROOT;
}

export function isValidSessionId(id: string): boolean {
  return UUID_RE.test(id);
}

export function isValidField(field: string): field is CliInputField {
  return (FIELDS as readonly string[]).includes(field);
}

/** Path of one staged input. Refuses anything that is not a known id and field. */
export function stagedPath(sessionId: string, field: CliInputField): string {
  if (!isValidSessionId(sessionId)) throw new Error("sessionId ไม่ถูกรูป");
  if (!isValidField(field)) throw new Error("field ไม่ถูกรูป");
  return join(STAGING_ROOT, sessionId, field);
}

export async function createSession(): Promise<string> {
  const id = randomUUID();
  await mkdir(join(STAGING_ROOT, id), { recursive: true });
  return id;
}

export async function sessionExists(sessionId: string): Promise<boolean> {
  if (!isValidSessionId(sessionId)) return false;
  try {
    const s = await stat(join(STAGING_ROOT, sessionId));
    return s.isDirectory();
  } catch {
    return false;
  }
}

/** Size of one staged input, or 0 when nothing has been written yet. */
export async function stagedSize(sessionId: string, field: CliInputField): Promise<number> {
  try {
    return (await stat(stagedPath(sessionId, field))).size;
  } catch {
    return 0;
  }
}

/**
 * appendChunk adds bytes to the end of a staged input.
 *
 * `expectedOffset` is checked against the file's real size, so a chunk that arrives out of
 * order or twice is refused instead of corrupting the file silently. A dump that is
 * scrambled but plausible-looking is far worse than a failed upload.
 */
export async function appendChunk(
  sessionId: string,
  field: CliInputField,
  expectedOffset: number,
  bytes: Buffer,
): Promise<number> {
  const path = stagedPath(sessionId, field);
  const current = await stagedSize(sessionId, field);
  if (current !== expectedOffset) {
    throw new Error(
      `ชิ้นไฟล์มาไม่ตรงลำดับ — เซิร์ฟเวอร์มี ${current} ไบต์ แต่ชิ้นนี้บอกว่าเริ่มที่ ${expectedOffset} ไบต์`,
    );
  }
  if (current === 0) await writeFile(path, bytes);
  else await appendFile(path, bytes);
  return current + bytes.length;
}

/** Total bytes staged in a session across all inputs. */
export async function sessionBytes(sessionId: string): Promise<number> {
  let total = 0;
  for (const f of FIELDS) total += await stagedSize(sessionId, f);
  return total;
}

/** Which inputs a session actually holds. */
export async function stagedFields(
  sessionId: string,
): Promise<{ field: CliInputField; bytes: number }[]> {
  const out: { field: CliInputField; bytes: number }[] = [];
  for (const f of FIELDS) {
    const bytes = await stagedSize(sessionId, f);
    if (bytes > 0) out.push({ field: f, bytes });
  }
  return out;
}

export async function dropSession(sessionId: string): Promise<void> {
  if (!isValidSessionId(sessionId)) return;
  await rm(join(STAGING_ROOT, sessionId), { recursive: true, force: true });
}

/**
 * sweepStaging removes abandoned sessions. Called opportunistically at the start of an
 * upload rather than on a timer: a tab closed mid-upload leaves bytes on disk, and disk
 * that fills up silently is how the next import fails for an unrelated reason.
 */
export async function sweepStaging(): Promise<{ removed: number; bytes: number }> {
  let removed = 0;
  let bytes = 0;
  let entries: string[];
  try {
    entries = await readdir(STAGING_ROOT);
  } catch {
    return { removed, bytes };
  }
  const cutoff = Date.now() - MAX_SESSION_AGE_MS;
  for (const id of entries) {
    if (!isValidSessionId(id)) continue;
    const dir = join(STAGING_ROOT, id);
    try {
      const s = await stat(dir);
      if (s.mtimeMs > cutoff) continue;
      for (const f of FIELDS) {
        try {
          bytes += (await stat(join(dir, f))).size;
        } catch {
          /* input not present in this session */
        }
      }
      await rm(dir, { recursive: true, force: true });
      removed++;
    } catch {
      /* raced with another sweep or already gone */
    }
  }
  return { removed, bytes };
}
