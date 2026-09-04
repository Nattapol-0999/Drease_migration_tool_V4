import "server-only";

import { SESSION_COOKIE, verifySessionToken, type Session } from "@/lib/auth-token";

// Defence in depth for the routes that actually do something.
//
// `src/middleware.ts` already blocks unauthenticated requests, but a matcher is one
// regex away from letting a path through, and the mistake would be silent. The routes
// that run the CLI or write to a clinic database check for themselves too, so a
// middleware misconfiguration downgrades to "still protected" instead of "wide open".

/** parseCookie reads one cookie out of a raw Cookie header. */
function parseCookie(header: string | null, name: string): string | undefined {
  if (!header) return undefined;
  for (const part of header.split(";")) {
    const eq = part.indexOf("=");
    if (eq < 0) continue;
    if (part.slice(0, eq).trim() === name) return part.slice(eq + 1).trim();
  }
  return undefined;
}

/**
 * requireSession returns the session, or a 401 Response to return as-is.
 *
 * Fails closed on a missing AUTH_SECRET, matching the middleware.
 */
export async function requireSession(
  req: Request,
): Promise<{ session: Session } | { denied: Response }> {
  const secret = (process.env.AUTH_SECRET ?? "").trim();
  if (secret.length < 32) {
    return {
      denied: Response.json(
        { ok: false, error: "ยังไม่ได้ตั้ง AUTH_SECRET บนเซิร์ฟเวอร์" },
        { status: 401 },
      ),
    };
  }

  const token = parseCookie(req.headers.get("cookie"), SESSION_COOKIE);
  const session = await verifySessionToken(token, secret);
  if (!session) {
    return {
      denied: Response.json(
        { ok: false, error: "ยังไม่ได้เข้าสู่ระบบ หรือเซสชันหมดอายุ — รีเฟรชหน้าแล้วล็อกอินใหม่" },
        { status: 401 },
      ),
    };
  }

  return { session };
}
