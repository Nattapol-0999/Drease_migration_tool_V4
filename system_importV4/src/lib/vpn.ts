import "server-only";

// Asking for the migration VPN, from a process that is not allowed to start it.
//
// WHY IT IS A FILE AND NOT A COMMAND
//
// This app runs as ubuntu with NoNewPrivileges=yes in its unit, which blocks sudo outright,
// and openvpn needs root to open a tun device and add a route. The alternative to what is
// below was to drop that hardening from the one service on the box that holds customer
// database passwords, which is a bad trade for a convenience.
//
// So the app writes ONE WORD into a file it is already permitted to write (the unit's
// ReadWritePaths=/opt/migration-tool), a systemd path unit notices the change, and a root
// helper reads it. The helper accepts "up" and "down" and records anything else as ignored:
// the app cannot ask for an arbitrary command even if this file were reachable by someone
// else.
//
// The answer comes back the same way, through a state file the helper writes as root and
// the app's group can read. Status is READ FROM THAT FILE rather than inferred from a
// successful write, because "I asked for it" and "it is up" are different facts and the one
// the operator needs is the second.

import { readFile, stat, writeFile } from "node:fs/promises";

/** Where the request goes. Overridable so a dev box can point it at a scratch path. */
const REQUEST_PATH =
  (process.env.VPN_REQUEST_PATH ?? "").trim() || "/opt/migration-tool/vpn-request";
const STATE_PATH = (process.env.VPN_STATE_PATH ?? "").trim() || "/opt/migration-tool/vpn-state";

/** How long to wait after asking. Measured: authenticating and creating tun0 takes ~2s. */
const UP_TIMEOUT_MS = 45_000;
const DOWN_TIMEOUT_MS = 20_000;
const POLL_MS = 1_000;

export interface VpnStatus {
  /** False when this host has no VPN plumbing — a dev machine, or a box set up before it existed. */
  configured: boolean;
  /** True only when the tunnel interface actually exists. */
  up: boolean;
  /** The routed network, when the tunnel is up. Empty otherwise. */
  lan: string;
  /** The helper's own words, for the log panel. Never holds a secret. */
  detail: string;
}

/**
 * parseState reads the four lines the helper writes:
 *
 *   request=up at=<iso>
 *   unit=active
 *   tun0   UNKNOWN   10.74.228.8/22 ...        (or "tun0 absent")
 *   10.0.128.0/20 via 10.74.228.1 dev tun0     (or "lan route absent")
 *
 * "unit=active" alone is not treated as up: systemctl reports the service active as soon as
 * the process starts, which is before openvpn has authenticated. The interface is the fact.
 */
function parseState(text: string): { up: boolean; lan: string } {
  const up = /^unit=active$/m.test(text) && /\btun0\b/.test(text) && !/tun0 absent/.test(text);
  const lan = /^(\d+\.\d+\.\d+\.\d+\/\d+) via .* dev tun\d+/m.exec(text)?.[1] ?? "";
  return { up, lan };
}

/** readVpnStatus reports what the helper last recorded, without asking for anything. */
export async function readVpnStatus(): Promise<VpnStatus> {
  try {
    await stat(REQUEST_PATH);
  } catch {
    return {
      configured: false,
      up: false,
      lan: "",
      detail: `ไม่พบ ${REQUEST_PATH} — เครื่องนี้ยังไม่ได้ติดตั้งกลไก VPN (vpn-switch.path + /usr/local/sbin/vpn-switch)`,
    };
  }
  let text = "";
  try {
    text = await readFile(STATE_PATH, "utf8");
  } catch {
    // The request file exists but the helper has never run. Not an error: nobody has asked
    // for the tunnel since this box was set up.
    return { configured: true, up: false, lan: "", detail: "ยังไม่เคยสั่ง VPN บนเครื่องนี้" };
  }
  const { up, lan } = parseState(text);
  return { configured: true, up, lan, detail: text.trim() };
}

/**
 * requestVpn writes the request and then WAITS for the tunnel to actually be there.
 *
 * Returning as soon as the file is written would hand the caller a lie, and the caller is
 * about to ssh to a private address: the pull would fail with a connect timeout, which names
 * the firewall or the far side rather than a tunnel that had not finished coming up.
 */
export async function requestVpn(want: "up" | "down"): Promise<VpnStatus> {
  const before = await readVpnStatus();
  if (!before.configured) return before;

  await writeFile(REQUEST_PATH, `${want}\n`, "utf8");

  const budget = want === "up" ? UP_TIMEOUT_MS : DOWN_TIMEOUT_MS;
  const deadline = Date.now() + budget;
  let last = before;
  while (Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, POLL_MS));
    last = await readVpnStatus();
    if (want === "up" ? last.up : !last.up) return last;
  }
  return {
    ...last,
    detail:
      `สั่ง ${want} แล้วรอ ${budget / 1000}s ยังไม่เป็นไปตามที่สั่ง — ` +
      `สถานะล่าสุดที่ตัวช่วยรายงาน:\n${last.detail}`,
  };
}
