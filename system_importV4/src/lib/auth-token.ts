// Session tokens for the operator login.
//
// This module is imported by BOTH the Next.js middleware (Edge runtime) and normal
// route handlers (Node runtime), so it may only use Web Crypto — `node:crypto` does not
// exist on the Edge. Everything here is async for that reason.
//
// The token is a signed statement, not an opaque id: there is no session store to keep
// in sync, and a single-instance tool does not need one. Tampering is caught by the
// HMAC, and age is caught by the embedded expiry.

/** Cookie that carries the signed session. */
export const SESSION_COOKIE = "importv4_session";

/** How long a login lasts. Short enough that a forgotten open tab stops mattering. */
export const SESSION_TTL_SECONDS = 8 * 60 * 60;

export interface Session {
  user: string;
  /** Unix seconds. */
  exp: number;
}

function base64UrlEncode(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}

function base64UrlDecode(text: string): Uint8Array | null {
  const padded = text.replaceAll("-", "+").replaceAll("_", "/");
  try {
    const raw = atob(padded + "=".repeat((4 - (padded.length % 4)) % 4));
    const out = new Uint8Array(raw.length);
    for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
    return out;
  } catch {
    // Not valid base64 — a malformed cookie, which is a failed verification, not a crash.
    return null;
  }
}

async function hmacKey(secret: string): Promise<CryptoKey> {
  return crypto.subtle.importKey(
    "raw",
    new TextEncoder().encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign", "verify"],
  );
}

/**
 * timingSafeEqualStrings compares without leaking where the first difference is.
 * Edge has no `crypto.timingSafeEqual`, so this does the same job by always walking the
 * full length of both inputs.
 */
function timingSafeEqualStrings(a: string, b: string): boolean {
  // Length itself is not secret (signatures are fixed width) but the comparison still
  // runs over a constant number of characters for a given pair.
  let diff = a.length ^ b.length;
  const n = Math.max(a.length, b.length);
  for (let i = 0; i < n; i++) {
    diff |= (a.charCodeAt(i) || 0) ^ (b.charCodeAt(i) || 0);
  }
  return diff === 0;
}

/**
 * How old a session in use gets before it is issued again (owner's decision 1ก, 7 Oct 2026).
 *
 * The middleware re-mints the token once it is this old, so a session now ends
 * SESSION_TTL_SECONDS after the operator last USED the tool, not after they logged in. That
 * is what a long import needs: on 7 Oct a 57 GB photo run outlived a morning login by
 * minutes and its tab lost the job. Nothing changes for a tab nobody touches — the page sends
 * no requests of its own while idle, so an idle session still closes 8 hours after its last
 * use, give or take this interval.
 *
 * Not on every request: the job panel polls every 2.5 s, and a Set-Cookie on each poll would
 * be noise for no gain.
 */
export const SESSION_RENEW_AFTER_SECONDS = 10 * 60;

/** shouldRenewSession reports whether a valid session is old enough to be issued again. */
export function shouldRenewSession(
  session: Session,
  nowSeconds: number = Math.floor(Date.now() / 1000),
): boolean {
  return session.exp - nowSeconds <= SESSION_TTL_SECONDS - SESSION_RENEW_AFTER_SECONDS;
}

/**
 * sessionCookie is the Set-Cookie value that carries a token. One place, so the login route
 * and the renewal in the middleware cannot drift apart on HttpOnly / Secure / SameSite.
 *
 * `secure` is false only in development: on plain-http localhost the browser drops a Secure
 * cookie, and login would appear to succeed but never stick.
 */
export function sessionCookie(token: string, secure: boolean): string {
  return [
    `${SESSION_COOKIE}=${token}`,
    "Path=/",
    "HttpOnly",
    "SameSite=Lax",
    `Max-Age=${SESSION_TTL_SECONDS}`,
    secure ? "Secure" : "",
  ]
    .filter(Boolean)
    .join("; ");
}

/** createSessionToken returns `payload.signature`, both base64url. */
export async function createSessionToken(
  user: string,
  secret: string,
  nowSeconds: number = Math.floor(Date.now() / 1000),
): Promise<string> {
  const session: Session = { user, exp: nowSeconds + SESSION_TTL_SECONDS };
  const payload = base64UrlEncode(new TextEncoder().encode(JSON.stringify(session)));
  const sig = await crypto.subtle.sign(
    "HMAC",
    await hmacKey(secret),
    new TextEncoder().encode(payload),
  );
  return `${payload}.${base64UrlEncode(new Uint8Array(sig))}`;
}

/**
 * verifySessionToken returns the session, or null for anything that is not a currently
 * valid token. It never throws: a caller in middleware has no way to recover from an
 * exception other than to deny, so denying is the only outcome worth expressing.
 */
export async function verifySessionToken(
  token: string | undefined | null,
  secret: string,
  nowSeconds: number = Math.floor(Date.now() / 1000),
): Promise<Session | null> {
  if (!token || !secret) return null;

  const dot = token.indexOf(".");
  if (dot <= 0 || dot === token.length - 1) return null;
  const payload = token.slice(0, dot);
  const providedSig = token.slice(dot + 1);

  const expectedSigBytes = await crypto.subtle.sign(
    "HMAC",
    await hmacKey(secret),
    new TextEncoder().encode(payload),
  );
  const expectedSig = base64UrlEncode(new Uint8Array(expectedSigBytes));
  if (!timingSafeEqualStrings(providedSig, expectedSig)) return null;

  const raw = base64UrlDecode(payload);
  if (!raw) return null;

  let parsed: unknown;
  try {
    parsed = JSON.parse(new TextDecoder().decode(raw));
  } catch {
    return null;
  }

  if (typeof parsed !== "object" || parsed === null) return null;
  const { user, exp } = parsed as Partial<Session>;
  if (typeof user !== "string" || user.length === 0) return null;
  if (typeof exp !== "number" || !Number.isFinite(exp)) return null;
  if (exp <= nowSeconds) return null;

  return { user, exp };
}
