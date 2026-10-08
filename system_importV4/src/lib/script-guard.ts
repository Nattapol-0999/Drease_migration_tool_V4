import { open } from "node:fs/promises";

// The clinic guard check /api/import-to-server makes before it writes anything.
//
// Kept out of the route so it can be exercised with plain node: the route imports
// "server-only" and cannot be loaded outside Next.

/**
 * How far into a script the guard is looked for.
 *
 * The guard follows the header, and the header carries every warning of the run. On
 * 8 Oct 2026 premier-hoslistic's warnings (91 invoices named one by one on a single line)
 * pushed it to byte 67,210, and a fixed 64 KB peek refused a correct script — twice, under
 * two clinic codes. 16 MB is a ceiling, not a read: the scan stops at the guard.
 */
export const GUARD_SCAN_LIMIT_BYTES = 16 * 1024 * 1024;

/** How much is read per step. */
const GUARD_SCAN_CHUNK_BYTES = 64 * 1024;

/** clinicGuard is the line the generator writes right after BEGIN for this clinic. */
export function clinicGuard(clinic: string): string {
  return `SET LOCAL app.clinic_code = '${clinic}'`;
}

/**
 * scriptHasGuard reports whether `guard` appears in the first `limit` bytes of the file.
 *
 * Reads in chunks and stops at the first hit, so a correct script costs as much as its
 * header and a multi-hundred-megabyte one is never read whole. The last guard.length − 1
 * bytes of each window are carried into the next, so a guard split across two reads is
 * still found.
 */
export async function scriptHasGuard(
  path: string,
  guard: string,
  limit: number = GUARD_SCAN_LIMIT_BYTES,
): Promise<boolean> {
  const needle = Buffer.from(guard, "utf8");
  if (needle.length === 0) return false;
  const fh = await open(path, "r");
  try {
    let carry = Buffer.alloc(0);
    let pos = 0;
    while (pos < limit) {
      const buf = Buffer.alloc(Math.min(GUARD_SCAN_CHUNK_BYTES, limit - pos));
      const { bytesRead } = await fh.read(buf, 0, buf.length, pos);
      if (bytesRead === 0) return false;
      const window = Buffer.concat([carry, buf.subarray(0, bytesRead)]);
      if (window.includes(needle)) return true;
      carry = window.subarray(Math.max(0, window.length - (needle.length - 1)));
      pos += bytesRead;
    }
    return false;
  } finally {
    await fh.close();
  }
}
