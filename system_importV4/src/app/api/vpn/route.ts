import "server-only";

import { readVpnStatus, requestVpn, type VpnStatus } from "@/lib/vpn";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// GET reports whether the tunnel is up. POST asks for it to go up or down.
//
// The body carries one of exactly two words, and that is checked HERE as well as in the root
// helper. Two independent checks, because this is the only endpoint in the app that changes
// the host's networking.

interface Body {
  want?: string;
}

export async function GET(): Promise<Response> {
  const status = await readVpnStatus();
  return Response.json({ ok: true, ...status } satisfies { ok: true } & VpnStatus);
}

export async function POST(req: Request): Promise<Response> {
  let body: Body;
  try {
    body = (await req.json()) as Body;
  } catch {
    return Response.json({ ok: false, error: "อ่าน body ไม่ได้" }, { status: 400 });
  }

  const want = (body.want ?? "").trim();
  if (want !== "up" && want !== "down") {
    return Response.json(
      { ok: false, error: `สั่งได้แค่ "up" หรือ "down" — ได้รับ "${want}"` },
      { status: 400 },
    );
  }

  const status = await requestVpn(want);
  if (!status.configured) {
    return Response.json({ ok: false, error: status.detail, ...status }, { status: 503 });
  }
  // A request that did not take effect is reported as a failure carrying the helper's own
  // words, not as a success with a quiet flag.
  const took = want === "up" ? status.up : !status.up;
  if (!took) {
    return Response.json({ ok: false, error: status.detail, ...status }, { status: 504 });
  }
  return Response.json({ ok: true, ...status });
}
