import "server-only";

import { createSessionToken, sessionCookie } from "@/lib/auth-token";
import { readAuthConfig, verifyPassword, verifyUsername } from "@/lib/auth-password";

export const runtime = "nodejs"; // scrypt lives in node:crypto
export const dynamic = "force-dynamic";

// Brute-force brake.
//
// The tool runs as one process, so an in-memory counter is the whole story — there is no
// second instance to share state with. It slows guessing without a dependency, and it is
// deliberately per-IP-and-username so one attacker cannot lock out the real operator by
// spamming their username from elsewhere.
const MAX_ATTEMPTS = 8;
const WINDOW_MS = 15 * 60 * 1000;

interface Attempts {
  count: number;
  first: number;
}
const attempts = new Map<string, Attempts>();

function attemptKey(ip: string, user: string): string {
  return `${ip}\u0000${user}`;
}

function tooManyAttempts(key: string, now: number): boolean {
  const a = attempts.get(key);
  if (!a) return false;
  if (now - a.first > WINDOW_MS) {
    attempts.delete(key);
    return false;
  }
  return a.count >= MAX_ATTEMPTS;
}

function recordFailure(key: string, now: number): void {
  const a = attempts.get(key);
  if (!a || now - a.first > WINDOW_MS) {
    attempts.set(key, { count: 1, first: now });
    return;
  }
  a.count += 1;
  // Bound the map so a flood of distinct keys cannot grow it without limit.
  if (attempts.size > 5000) attempts.clear();
}

function clientIP(req: Request): string {
  // Behind Cloudflare/nginx the socket address is the proxy, so prefer the forwarded
  // header. This only feeds the rate-limit key, never an authorisation decision, so a
  // spoofed value costs an attacker their own bucket and nothing else.
  const fwd = req.headers.get("cf-connecting-ip") ?? req.headers.get("x-forwarded-for");
  if (fwd) return fwd.split(",")[0].trim();
  return "unknown";
}

export async function POST(req: Request): Promise<Response> {
  const { config, missing } = readAuthConfig();
  if (!config) {
    // Say what is unset, but only to a request that reached a locked deploy — there is
    // nothing secret in the NAME of a missing variable, and without it the operator has
    // no way to tell a wrong password from a broken config.
    return Response.json(
      { ok: false, error: `เซิร์ฟเวอร์ยังตั้งค่าไม่ครบ: ${missing.join(", ")}` },
      { status: 500 },
    );
  }

  let user = "";
  let password = "";
  const contentType = req.headers.get("content-type") ?? "";
  try {
    if (contentType.includes("application/json")) {
      const body = (await req.json()) as { user?: unknown; password?: unknown };
      user = typeof body.user === "string" ? body.user : "";
      password = typeof body.password === "string" ? body.password : "";
    } else {
      const form = await req.formData();
      user = String(form.get("user") ?? "");
      password = String(form.get("password") ?? "");
    }
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    return Response.json({ ok: false, error: `อ่านคำขอไม่ได้: ${message}` }, { status: 400 });
  }

  user = user.trim();
  if (!user || !password) {
    return Response.json({ ok: false, error: "กรอกชื่อผู้ใช้และรหัสผ่านให้ครบ" }, { status: 400 });
  }

  const now = Date.now();
  const key = attemptKey(clientIP(req), user);
  if (tooManyAttempts(key, now)) {
    return Response.json(
      { ok: false, error: "ลองผิดหลายครั้งเกินไป รอ 15 นาทีแล้วลองใหม่" },
      { status: 429 },
    );
  }

  const okUser = verifyUsername(user, config.user);
  const okPassword = verifyPassword(password, config.passwordHash);
  // Both checks always run, so a wrong username does not answer faster than a wrong
  // password — that difference is how an attacker learns the username is real.
  if (!okUser || !okPassword) {
    recordFailure(key, now);
    return Response.json({ ok: false, error: "ชื่อผู้ใช้หรือรหัสผ่านไม่ถูกต้อง" }, { status: 401 });
  }

  attempts.delete(key);

  const token = await createSessionToken(config.user, config.secret);
  const res = Response.json({ ok: true });
  // The same cookie the middleware re-issues while the session is in use — built in one
  // place so the two cannot disagree on its attributes.
  res.headers.append("set-cookie", sessionCookie(token, process.env.NODE_ENV === "production"));
  return res;
}
