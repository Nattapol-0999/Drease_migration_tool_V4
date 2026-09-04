import "server-only";

import { SESSION_COOKIE } from "@/lib/auth-token";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

/**
 * POST /api/logout clears the session cookie.
 *
 * Nothing to invalidate server-side: the token is self-contained, so dropping the cookie
 * is the logout. The short TTL is what limits a token that was already copied elsewhere.
 */
export async function POST(): Promise<Response> {
  const res = Response.json({ ok: true });
  res.headers.append(
    "set-cookie",
    [
      `${SESSION_COOKIE}=`,
      "Path=/",
      "HttpOnly",
      "SameSite=Lax",
      "Max-Age=0",
      process.env.NODE_ENV === "production" ? "Secure" : "",
    ]
      .filter(Boolean)
      .join("; "),
  );
  return res;
}
