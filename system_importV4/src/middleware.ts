import { NextResponse, type NextRequest } from "next/server";

import { SESSION_COOKIE, verifySessionToken } from "@/lib/auth-token";

// One gate in front of everything.
//
// The tool's own routes write to a clinic database, so the check cannot live in the page
// component: a page guard leaves /api/import-to-server reachable with a plain POST. This
// runs before any route handler, for pages and API alike.
//
// It fails CLOSED. If AUTH_SECRET is missing the answer is "denied", not "allowed" —
// a misconfigured deploy must be unusable rather than open.

/** Paths that must stay reachable without a session, or nobody could ever log in. */
const PUBLIC_PATHS = new Set(["/login", "/api/login", "/api/logout"]);

function isPublic(pathname: string): boolean {
  if (PUBLIC_PATHS.has(pathname)) return true;
  // The login page's own stylesheet and Next's client bundles.
  if (pathname.startsWith("/_next/")) return true;
  if (pathname === "/favicon.ico") return true;
  return false;
}

/**
 * publicOrigin returns the origin the BROWSER is using, which behind a reverse proxy is
 * not the one this process is listening on.
 *
 * `req.nextUrl.origin` is the internal address (http://127.0.0.1:3100), so redirecting to
 * it sends the operator to their own machine instead of the site. The middleware runtime
 * rejects a relative Location outright, so an absolute URL has to be assembled here.
 *
 * APP_PUBLIC_URL is preferred because it is configuration rather than something an
 * attacker can put in a header: a Host taken on trust turns a login redirect into an open
 * redirect, which is a phishing primitive. The header path exists so local development
 * needs no config, and the result is parsed before use — a value that is not a URL falls
 * back rather than being interpolated into one.
 */
function publicOrigin(req: NextRequest): string {
  const configured = (process.env.APP_PUBLIC_URL ?? "").trim();
  if (configured) {
    try {
      return new URL(configured).origin;
    } catch {
      // Misconfigured; fall through to the request headers.
    }
  }

  const host = req.headers.get("x-forwarded-host") ?? req.headers.get("host");
  const proto = req.headers.get("x-forwarded-proto")?.split(",")[0].trim();
  if (host) {
    try {
      return new URL(`${proto || "https"}://${host}`).origin;
    } catch {
      // Not a usable host header.
    }
  }

  return req.nextUrl.origin;
}

function denied(req: NextRequest, reason: string): NextResponse {
  const isApi = req.nextUrl.pathname.startsWith("/api/");
  if (isApi) {
    // JSON, because every caller of these paths is fetch() and expects to read a body.
    return NextResponse.json({ ok: false, error: reason }, { status: 401 });
  }

  const url = new URL("/login", publicOrigin(req));
  // Come back to where the operator was aiming once they are in.
  const target = `${req.nextUrl.pathname}${req.nextUrl.search}`;
  if (target && target !== "/") url.searchParams.set("next", target);

  return NextResponse.redirect(url, 307);
}

export async function middleware(req: NextRequest): Promise<NextResponse> {
  if (isPublic(req.nextUrl.pathname)) return NextResponse.next();

  const secret = (process.env.AUTH_SECRET ?? "").trim();
  if (secret.length < 32) {
    return denied(req, "ยังไม่ได้ตั้ง AUTH_SECRET บนเซิร์ฟเวอร์ — เครื่องมือถูกล็อกไว้ทั้งหมด");
  }

  const token = req.cookies.get(SESSION_COOKIE)?.value;
  const session = await verifySessionToken(token, secret);
  if (!session) return denied(req, "ยังไม่ได้เข้าสู่ระบบ หรือเซสชันหมดอายุ");

  return NextResponse.next();
}

export const config = {
  // Everything except Next's internals. Listing it here (rather than returning early for
  // more paths above) keeps the middleware off the hot path for static assets.
  matcher: ["/((?!_next/static|_next/image|favicon.ico).*)"],
};
