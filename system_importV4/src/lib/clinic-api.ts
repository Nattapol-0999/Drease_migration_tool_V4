import "server-only";

// Creating the clinic before importing into it.
//
// The V4 backend owns this: it knows the two-row identity/branch model, the branch-code
// sequence, the feature flags and the subscription. Writing those rows from here would be
// a second implementation of a tenant boundary, and the two would drift.
//
// Reaching it needs one wrinkle. The backends listen on :8090 but that port is closed to
// this host, so an HTTP call cannot be made directly — the same situation as Postgres on
// :5432, which this tool already solves by running the client ON the far side over ssh.
// So the request is issued by curl on the backend host itself, against its own loopback.
// If the port is ever opened, clearing the ssh host switches to a direct call with no
// other change.

import { TARGET_IDS, type TargetId } from "@/lib/types";
import { runSSH, shellQuote, type SSHResult } from "@/lib/server-target";

export interface ClinicAPITarget {
  id: TargetId;
  /** ssh host of the BACKEND (not the database). Empty means call the URL directly. */
  sshHost: string;
  sshUser: string;
  sshPort: string;
  sshKey: string;
  /** Base URL of the backend AS SEEN from wherever the request is issued. */
  baseURL: string;
  /** X-Import-Service-Token. Empty on the backend leaves the whole lane unmounted. */
  token: string;
  /**
   * X-App-Key. A separate layer from the token: the app key answers "is this one of our
   * client channels", the service token answers "may this caller import". The backends run
   * with APP_AUTH_ENFORCE=true, so a request without a known key is refused before the
   * import lane is ever reached. Optional here because a backend with enforcement off
   * needs none, and sending an empty header would be worse than sending nothing.
   */
  appKey: string;
}

function env(k: string): string {
  return (process.env[k] ?? "").trim();
}

/** readClinicAPI returns the backend access for one environment, or what is missing. */
export function readClinicAPI(id: TargetId): {
  api: ClinicAPITarget | null;
  missing: string[];
} {
  const up = id.toUpperCase();
  const api: ClinicAPITarget = {
    id,
    sshHost: env(`TARGET_${up}_API_SSH_HOST`),
    sshUser: env("IMPORT_SSH_USER") || "ubuntu",
    sshPort: env("IMPORT_SSH_PORT") || "22",
    sshKey: env("IMPORT_SSH_KEY"),
    baseURL: env(`TARGET_${up}_API_URL`),
    token: env(`TARGET_${up}_IMPORT_TOKEN`),
    appKey: env(`TARGET_${up}_APP_KEY`),
  };

  const missing: string[] = [];
  if (!api.baseURL) missing.push(`TARGET_${up}_API_URL`);
  if (!api.token) missing.push(`TARGET_${up}_IMPORT_TOKEN`);
  // Only the ssh path needs a key; a direct call does not.
  if (api.sshHost && !api.sshKey) missing.push("IMPORT_SSH_KEY");

  return { api: missing.length === 0 ? api : null, missing };
}

/** A short, non-secret description for the UI. Never includes the token. */
export function describeClinicAPI(a: ClinicAPITarget): string {
  return a.sshHost ? `${a.baseURL} (ผ่าน ssh ${a.sshUser}@${a.sshHost})` : a.baseURL;
}

export interface APICall {
  /** HTTP status the backend returned, or 0 when the call never got that far. */
  status: number;
  body: string;
  /** Populated when ssh or curl itself failed rather than the backend answering. */
  transportError: string;
}

/**
 * callBackend issues one JSON request and reports the HTTP status separately from the
 * body, because the statuses carry meaning here: 404 means the endpoint is not in the
 * running build, 401 means the token is wrong, 503 means the lane is unconfigured. A
 * helper that collapsed those into "error" would hide the one thing worth knowing.
 */
export async function callBackend(
  api: ClinicAPITarget,
  method: "GET" | "POST",
  path: string,
  jsonBody?: unknown,
): Promise<APICall> {
  const url = `${api.baseURL.replace(/\/+$/, "")}${path}`;
  const payload = jsonBody === undefined ? "" : JSON.stringify(jsonBody);

  if (!api.sshHost) {
    // Direct call — used in development, and whenever :8090 is actually reachable.
    try {
      const headers: Record<string, string> = {
        "content-type": "application/json",
        "x-import-service-token": api.token,
      };
      if (api.appKey) headers["x-app-key"] = api.appKey;
      const res = await fetch(url, {
        method,
        headers,
        body: method === "POST" ? payload || "{}" : undefined,
        cache: "no-store",
      });
      return { status: res.status, body: await res.text(), transportError: "" };
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      return { status: 0, body: "", transportError: `ต่อ ${url} ไม่ได้: ${message}` };
    }
  }

  // curl on the far side. -sS keeps the body clean while still reporting curl's own
  // errors, and the status is appended behind a marker so it can be split off a body
  // that may itself contain newlines. Nothing is interpolated unquoted: the token and
  // the payload go through shellQuote, so a value containing $ or a quote cannot become
  // shell code.
  const marker = "\\n__HTTP_STATUS__:%{http_code}";
  const parts = [
    "curl -sS -X",
    method,
    shellQuote(url),
    "-H",
    shellQuote("content-type: application/json"),
    "-H",
    shellQuote(`X-Import-Service-Token: ${api.token}`),
    ...(api.appKey ? ["-H", shellQuote(`X-App-Key: ${api.appKey}`)] : []),
    "--max-time 60",
    "-w",
    shellQuote(marker),
  ];
  if (method === "POST") {
    // Read the body from stdin so it never appears in the process list on the far host.
    parts.push("--data-binary @-");
  }
  const r: SSHResult = await runSSH(sshArgsFor(api, parts.join(" ")), payload || "{}");

  if (r.code !== 0 && !r.stdout) {
    return {
      status: 0,
      body: "",
      transportError: (r.stderr || `ssh/curl ล้มเหลว (exit ${r.code})`).trim(),
    };
  }
  const idx = r.stdout.lastIndexOf("__HTTP_STATUS__:");
  if (idx < 0) {
    return {
      status: 0,
      body: r.stdout,
      transportError: (r.stderr || "ไม่ได้รับสถานะ HTTP กลับมาจาก curl").trim(),
    };
  }
  const status = Number.parseInt(r.stdout.slice(idx + "__HTTP_STATUS__:".length).trim(), 10);
  return {
    status: Number.isFinite(status) ? status : 0,
    body: r.stdout.slice(0, idx).replace(/\n$/, ""),
    transportError: "",
  };
}

/** sshArgsFor mirrors server-target's sshArgs but for the backend host. */
function sshArgsFor(a: ClinicAPITarget, remoteCommand: string): string[] {
  return [
    "-i", a.sshKey,
    "-p", a.sshPort,
    "-o", "BatchMode=yes",
    "-o", "StrictHostKeyChecking=accept-new",
    "-o", "ConnectTimeout=15",
    "-o", "IdentitiesOnly=yes",
    `${a.sshUser}@${a.sshHost}`,
    remoteCommand,
  ];
}

/**
 * explainFailure turns the backend's answer into the thing the operator has to fix.
 *
 * These are not generic HTTP messages. Each one corresponds to a specific, known state of
 * the deployment, and saying which it is saves an afternoon of guessing. The body matters
 * as much as the status: two different layers answer 401, and they need opposite fixes.
 */
export function explainFailure(status: number, body = ""): string {
  if (status === 401) {
    // The app-key layer sits IN FRONT of the import token, so this 401 means the request
    // never reached the import lane at all — a correct token cannot help.
    if (/unknown app/i.test(body)) {
      return "backend ปฏิเสธที่ชั้น app key — ต้องเพิ่มช่องของเครื่องมือนี้ใน APP_KEYS ของ backend แล้วตั้ง TARGET_*_APP_KEY ให้ตรง";
    }
    return "backend ปฏิเสธ import token — ตรวจว่า TARGET_*_IMPORT_TOKEN ตรงกับ IMPORT_SERVICE_TOKEN บน backend";
  }
  switch (status) {
    case 404:
      return "backend ที่รันอยู่ยังไม่มี endpoint นี้ — ต้อง deploy image ที่มี POST /v3/import/clinics ก่อน";
    case 503:
      return "backend ยังไม่ได้ตั้ง IMPORT_SERVICE_TOKEN — lane /v3/import ทั้งชุดจึงไม่ถูก mount";
    case 0:
      return "ยังไปไม่ถึง backend เลย (เน็ตเวิร์ก/ssh)";
    default:
      return "";
  }
}

/** TARGET_IDS re-exported so callers validating an id do not need two imports. */
export { TARGET_IDS };
