import "server-only";

// Where the "import to server" button writes, and how it gets there.
//
// The route runs psql ON THE SERVER over ssh instead of opening a tunnel and using a
// local psql: nothing extra has to be installed here, the credentials never travel
// past the Next.js server, and the SQL is handed over on stdin so it never lands in a
// process argument or a shell history.
//
// There are two environments and they are configured separately. The browser only ever
// sends a target ID; the host, the database and the credentials are looked up here. A
// request cannot name a server that was not configured, so a tampered payload cannot
// redirect an import somewhere nobody intended.

import { TARGET_IDS, type TargetId } from "@/lib/types";

/** How psql authenticates on the far side. */
export type PsqlMode = "peer" | "password" | "docker";

export interface ServerTarget {
  id: TargetId;
  sshHost: string;
  sshUser: string;
  sshPort: string;
  sshKey: string;
  dbHost: string;
  dbPort: string;
  dbName: string;
  dbUser: string;
  dbPassword: string;
  mode: PsqlMode;
  /** Non-empty when Postgres runs in docker on the server (mode "docker"). */
  dockerContainer: string;
  /** Shown next to the button so an operator can see where it points. */
  label: string;
  /** True for the environment that holds real clinic data. Drives the warning in the UI. */
  isProduction: boolean;
}

function env(k: string): string {
  return (process.env[k] ?? "").trim();
}

/**
 * readTarget builds one environment's target, or reports what is missing.
 *
 * Connection details that are the same everywhere (the ssh account, the key, how psql
 * authenticates) are read from shared variables; only the host and database name are
 * per-environment. That is on purpose: two nearly-identical blocks of configuration is
 * how one of them ends up quietly out of date.
 */
function readTarget(id: TargetId): { target: ServerTarget | null; missing: string[] } {
  const up = id.toUpperCase();
  const mode = (env("IMPORT_PSQL_MODE") || "peer") as PsqlMode;

  const target: ServerTarget = {
    id,
    sshHost: env(`TARGET_${up}_HOST`),
    sshUser: env("IMPORT_SSH_USER") || "ubuntu",
    sshPort: env("IMPORT_SSH_PORT") || "22",
    sshKey: env("IMPORT_SSH_KEY"),
    dbHost: env(`TARGET_${up}_DB_HOST`) || "localhost",
    dbPort: env(`TARGET_${up}_DB_PORT`) || "5432",
    dbName: env(`TARGET_${up}_DB`),
    dbUser: env("IMPORT_DB_USER") || "postgres",
    dbPassword: env("IMPORT_DB_PASSWORD"),
    mode: mode === "password" || mode === "docker" ? mode : "peer",
    dockerContainer: env("IMPORT_DB_DOCKER_CONTAINER"),
    label: env(`TARGET_${up}_LABEL`) || id,
    isProduction: id === "production",
  };

  const missing: string[] = [];
  if (!target.sshHost) missing.push(`TARGET_${up}_HOST`);
  if (!target.dbName) missing.push(`TARGET_${up}_DB`);
  if (!target.sshKey) missing.push("IMPORT_SSH_KEY");
  // Only the password mode needs a password. Under peer authentication there is nothing
  // to store, which is the reason it is the default.
  if (target.mode === "password" && !target.dbPassword) missing.push("IMPORT_DB_PASSWORD");
  if (target.mode === "docker" && !target.dockerContainer) missing.push("IMPORT_DB_DOCKER_CONTAINER");

  return { target: missing.length === 0 ? target : null, missing };
}

export interface TargetsView {
  /** Only the environments that are completely configured. */
  available: ServerTarget[];
  /** Why an environment is unusable, so an operator can fix it rather than guess. */
  problems: { id: TargetId; missing: string[] }[];
}

/** readTargets returns every configured environment. */
export function readTargets(): TargetsView {
  const available: ServerTarget[] = [];
  const problems: { id: TargetId; missing: string[] }[] = [];
  for (const id of TARGET_IDS) {
    const { target, missing } = readTarget(id);
    if (target) available.push(target);
    else problems.push({ id, missing });
  }
  return { available, problems };
}

/**
 * resolveTarget maps a target ID from a request to a configured target.
 *
 * Returns null for anything that is not a configured environment. The caller must treat
 * that as a refusal: guessing a default here is how a request meant for test lands on
 * production.
 */
export function resolveTarget(id: unknown): ServerTarget | null {
  if (typeof id !== "string") return null;
  if (!(TARGET_IDS as readonly string[]).includes(id)) return null;
  const { target } = readTarget(id as TargetId);
  return target;
}

/** A short, non-secret description of the target for the UI. */
export function describeTarget(t: ServerTarget): string {
  const where = `${t.sshUser}@${t.sshHost}`;
  const db = t.mode === "peer" ? `${t.dbName} (peer)` : `${t.dbName} @ ${t.dbHost}:${t.dbPort}`;
  return `${t.label} · ${where} · ${db}`;
}

/**
 * psqlCommand builds the remote command. ON_ERROR_STOP makes psql abort on the first
 * error, and the generated script wraps everything in one transaction, so a failure
 * anywhere leaves the database untouched.
 */
export function psqlCommand(t: ServerTarget): string {
  const flags = `-d ${shellQuote(t.dbName)} -v ON_ERROR_STOP=1 -f -`;

  if (t.mode === "peer") {
    // Peer authentication: the postgres OS account maps to the postgres database role,
    // so no password exists to store or leak. `cd /tmp` first because psql running as
    // another user cannot read the invoking user's home directory and would print a
    // "could not change directory" warning into the operator's log for no reason.
    return `cd /tmp && sudo -u postgres psql ${flags}`;
  }

  const conn = `-h ${t.dbHost} -p ${t.dbPort} -U ${t.dbUser}`;
  if (t.mode === "docker") {
    // -i keeps stdin open so the script can be piped into the container.
    return `docker exec -i -e PGPASSWORD=${shellQuote(t.dbPassword)} ${t.dockerContainer} psql ${conn} ${flags}`;
  }
  // The password goes in through PGPASSWORD on the remote environment rather than a
  // connection string, so it does not appear in the SQL or in psql's own logging.
  return `PGPASSWORD=${shellQuote(t.dbPassword)} psql ${conn} ${flags}`;
}

/**
 * shellQuote wraps a value for the REMOTE shell (ssh runs the command through it).
 * Single quotes with the embedded-quote escape is the only form POSIX shells treat as
 * fully literal, so a password containing $, backtick or space cannot become code.
 */
export function shellQuote(v: string): string {
  return `'` + v.replaceAll(`'`, `'\\''`) + `'`;
}

/** sshArgs builds the argv for a remote command. Never a shell string locally. */
export function sshArgs(t: ServerTarget, remoteCommand: string): string[] {
  return [
    "-i", t.sshKey,
    "-p", t.sshPort,
    // Fail instead of prompting: stdin carries SQL, not a keyboard, so a prompt would
    // hang the request forever.
    "-o", "BatchMode=yes",
    // The host keys were verified and written to known_hosts when the tool was set up.
    // accept-new keeps a rebuilt host from blocking an import, without ever accepting a
    // CHANGED key for a host already known.
    "-o", "StrictHostKeyChecking=accept-new",
    "-o", "ConnectTimeout=15",
    // Offer only the configured key, so an unrelated agent identity cannot be tried
    // first and exhaust the server's auth attempts.
    "-o", "IdentitiesOnly=yes",
    `${t.sshUser}@${t.sshHost}`,
    remoteCommand,
  ];
}

export interface SSHResult {
  code: number;
  stdout: string;
  stderr: string;
}

/**
 * runSSH pipes stdin into ssh and collects both streams. spawn takes an argv array, so
 * nothing is interpreted by a local shell.
 */
export function runSSH(args: string[], stdin: string): Promise<SSHResult> {
  // Imported lazily so this module stays usable from code paths that never shell out.
  const { spawn } = require("node:child_process") as typeof import("node:child_process");

  return new Promise((resolve) => {
    const child = spawn("ssh", args, { windowsHide: true });

    let stdout = "";
    let stderr = "";
    const cap = 2 * 1024 * 1024; // bounded so a runaway remote cannot fill memory
    child.stdout.on("data", (d) => {
      if (stdout.length < cap) stdout += String(d);
    });
    child.stderr.on("data", (d) => {
      if (stderr.length < cap) stderr += String(d);
    });

    child.on("error", (err) => {
      resolve({ code: -1, stdout, stderr: `${stderr}\nรัน ssh ไม่ได้: ${err.message}`.trim() });
    });
    child.on("close", (code) => resolve({ code: code ?? -1, stdout, stderr }));

    child.stdin.on("error", () => {
      /* remote closed early; the exit code and stderr carry the reason */
    });
    child.stdin.end(stdin);
  });
}

/** hint turns common ssh/psql failures into the thing the operator actually has to fix. */
export function hint(r: SSHResult): string {
  const text = `${r.stderr}\n${r.stdout}`;
  if (/Permission denied|no such identity|Load key/i.test(text)) {
    return "ssh ปฏิเสธ key — ตรวจว่า IMPORT_SSH_KEY ชี้ไฟล์ที่มีอยู่ และ public key ถูกใส่ใน authorized_keys ของปลายทางแล้ว";
  }
  if (/Connection timed out|Could not resolve|No route to host/i.test(text)) {
    return "ต่อ ssh ไม่ถึงเครื่องปลายทาง — ตรวจ TARGET_*_HOST / พอร์ต / เน็ตเวิร์ก";
  }
  if (/sudo:.*(no tty|password is required|not allowed)/i.test(text)) {
    return "sudo บนเครื่องปลายทางขอรหัสผ่าน — โหมด peer ต้องให้บัญชี ssh ใช้ sudo ได้โดยไม่ถามรหัส";
  }
  if (/password authentication failed/i.test(text)) {
    return "psql ปฏิเสธรหัสผ่าน — ตรวจ IMPORT_DB_PASSWORD (หรือเปลี่ยนไปใช้ IMPORT_PSQL_MODE=peer)";
  }
  if (/database .* does not exist/i.test(text)) {
    return "ไม่พบฐานข้อมูลชื่อนี้บนเซิร์ฟเวอร์ — ตรวจ TARGET_*_DB";
  }
  if (/psql: command not found|psql: not found/i.test(text)) {
    return "ไม่พบคำสั่ง psql บนเซิร์ฟเวอร์ — ถ้า Postgres อยู่ใน docker ให้ตั้ง IMPORT_PSQL_MODE=docker และ IMPORT_DB_DOCKER_CONTAINER";
  }
  return "";
}
