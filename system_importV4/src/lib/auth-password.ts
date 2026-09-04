import "server-only";

// Password verification for the single operator account.
//
// The password is never in the repository. It lives in the environment as a scrypt
// hash, so reading the config file does not hand over the password itself:
//
//   AUTH_USER=Adm1n
//   AUTH_PASSWORD_HASH=scrypt:<N>:<r>:<p>:<saltB64>:<hashB64>
//
// The separator is ":" and not the conventional "$" on purpose. Every layer this value
// passes through — Next's dotenv-expand, systemd EnvironmentFile, docker-compose, a
// shell — treats "$" as the start of a variable reference, and each of them would
// silently rewrite the hash into something that can never match. ":" cannot appear in
// base64, so it separates without ambiguity.
//
// scrypt comes from node's own crypto, so there is no password-hashing dependency to
// keep patched. Use `node scripts/hash-password.mjs` to produce the value.

import { randomBytes, scryptSync, timingSafeEqual } from "node:crypto";

/** Work factors. N=16384 keeps a login under ~100ms on a small VM while staying costly to brute force. */
const N = 16384;
const R = 8;
const P = 1;
const KEY_LEN = 32;

/** hashPassword returns the encoded form to put in AUTH_PASSWORD_HASH. */
export function hashPassword(plain: string): string {
  const salt = randomBytes(16);
  const key = scryptSync(plain, salt, KEY_LEN, { N, r: R, p: P });
  return [
    "scrypt",
    String(N),
    String(R),
    String(P),
    salt.toString("base64"),
    key.toString("base64"),
  ].join(":");
}

/**
 * verifyPassword compares a submitted password against the encoded hash.
 *
 * Returns false for a malformed hash rather than throwing: a bad AUTH_PASSWORD_HASH must
 * fail every login (closed), never accidentally pass one.
 */
export function verifyPassword(plain: string, encoded: string): boolean {
  const parts = encoded.split(":");
  if (parts.length !== 6 || parts[0] !== "scrypt") return false;

  const n = Number(parts[1]);
  const r = Number(parts[2]);
  const p = Number(parts[3]);
  if (!Number.isInteger(n) || !Number.isInteger(r) || !Number.isInteger(p)) return false;
  // Guard the cost parameters: a hostile or corrupted value could otherwise ask scrypt
  // for gigabytes of memory and take the process down on every login attempt.
  if (n < 1024 || n > 1 << 20 || r < 1 || r > 32 || p < 1 || p > 16) return false;

  let salt: Buffer;
  let expected: Buffer;
  try {
    salt = Buffer.from(parts[4], "base64");
    expected = Buffer.from(parts[5], "base64");
  } catch {
    return false;
  }
  if (salt.length === 0 || expected.length === 0) return false;

  let actual: Buffer;
  try {
    actual = scryptSync(plain, salt, expected.length, { N: n, r, p });
  } catch {
    // Cost parameters node refuses (e.g. memory limit) — deny.
    return false;
  }

  if (actual.length !== expected.length) return false;
  return timingSafeEqual(actual, expected);
}

/**
 * verifyUsername compares the submitted user to the configured one without an early
 * return on the first differing character.
 */
export function verifyUsername(submitted: string, configured: string): boolean {
  const a = Buffer.from(submitted, "utf8");
  const b = Buffer.from(configured, "utf8");
  if (a.length !== b.length) {
    // Still burn a comparison so the answer does not come back measurably sooner for a
    // wrong-length username.
    timingSafeEqual(b, b);
    return false;
  }
  return timingSafeEqual(a, b);
}

export interface AuthConfig {
  user: string;
  passwordHash: string;
  secret: string;
}

/**
 * readAuthConfig returns the configured credentials, or the list of what is missing.
 *
 * A missing value is never treated as "no login required" — the caller denies instead.
 * An import tool that silently opens itself up when a variable is unset is the exact
 * failure this login exists to prevent.
 */
export function readAuthConfig(): { config: AuthConfig | null; missing: string[] } {
  const env = (k: string) => (process.env[k] ?? "").trim();
  const config: AuthConfig = {
    user: env("AUTH_USER"),
    passwordHash: env("AUTH_PASSWORD_HASH"),
    secret: env("AUTH_SECRET"),
  };

  const missing: string[] = [];
  if (!config.user) missing.push("AUTH_USER");
  if (!config.passwordHash) missing.push("AUTH_PASSWORD_HASH");
  // A short secret makes the cookie signature guessable, so treat it as absent.
  if (config.secret.length < 32) missing.push("AUTH_SECRET (ยาวอย่างน้อย 32 ตัวอักษร)");

  return { config: missing.length === 0 ? config : null, missing };
}
