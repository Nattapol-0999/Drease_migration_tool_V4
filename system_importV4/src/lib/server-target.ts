import "server-only";

// Where the "import to server" button writes, and how it gets there.
//
// The route runs psql ON THE SERVER over ssh instead of opening a tunnel and using a
// local psql: nothing extra has to be installed here, the credentials never travel
// past the Next.js server, and the SQL is handed over on stdin so it never lands in a
// process argument or a shell history.

export interface ServerTarget {
  sshHost: string;
  sshUser: string;
  sshPort: string;
  sshKey: string;
  dbHost: string;
  dbPort: string;
  dbName: string;
  dbUser: string;
  dbPassword: string;
  /** Non-empty when Postgres runs in docker on the server. */
  dockerContainer: string;
  /** Shown next to the button so an operator can see where it points. */
  label: string;
}

/**
 * readServerTarget returns the configured target, or a list of what is missing.
 * The button stays disabled while anything is missing — a half-configured target is
 * how a run ends up somewhere nobody intended.
 */
export function readServerTarget(): { target: ServerTarget | null; missing: string[] } {
  const env = (k: string) => (process.env[k] ?? "").trim();

  const target: ServerTarget = {
    sshHost: env("IMPORT_SSH_HOST"),
    sshUser: env("IMPORT_SSH_USER") || "ubuntu",
    sshPort: env("IMPORT_SSH_PORT") || "22",
    sshKey: env("IMPORT_SSH_KEY"),
    dbHost: env("IMPORT_DB_HOST") || "localhost",
    dbPort: env("IMPORT_DB_PORT") || "5432",
    dbName: env("IMPORT_DB_NAME"),
    dbUser: env("IMPORT_DB_USER") || "postgres",
    dbPassword: env("IMPORT_DB_PASSWORD"),
    dockerContainer: env("IMPORT_DB_DOCKER_CONTAINER"),
    label: env("IMPORT_TARGET_LABEL"),
  };

  const missing: string[] = [];
  if (!target.sshHost) missing.push("IMPORT_SSH_HOST");
  if (!target.sshKey) missing.push("IMPORT_SSH_KEY");
  if (!target.dbName) missing.push("IMPORT_DB_NAME");
  if (!target.dbPassword) missing.push("IMPORT_DB_PASSWORD");

  return { target: missing.length === 0 ? target : null, missing };
}

/** A short, non-secret description of the target for the UI. */
export function describeTarget(t: ServerTarget): string {
  const where = `${t.sshUser}@${t.sshHost}`;
  const db = `${t.dbName} @ ${t.dbHost}:${t.dbPort}`;
  return t.label ? `${t.label} · ${where} · ${db}` : `${where} · ${db}`;
}

/**
 * psqlCommand builds the remote command. ON_ERROR_STOP makes psql abort on the first
 * error, and the generated script wraps everything in one transaction, so a failure
 * anywhere leaves the database untouched.
 *
 * The password goes in through PGPASSWORD on the remote environment rather than a
 * connection string, so it does not appear in the SQL or in psql's own logging.
 */
export function psqlCommand(t: ServerTarget): string {
  const flags = `-h ${t.dbHost} -p ${t.dbPort} -U ${t.dbUser} -d ${t.dbName} -v ON_ERROR_STOP=1 -f -`;
  if (t.dockerContainer) {
    // -i keeps stdin open so the script can be piped into the container.
    return `docker exec -i -e PGPASSWORD=${shellQuote(t.dbPassword)} ${t.dockerContainer} psql ${flags}`;
  }
  return `PGPASSWORD=${shellQuote(t.dbPassword)} psql ${flags}`;
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
    "-o", "StrictHostKeyChecking=accept-new",
    "-o", "ConnectTimeout=15",
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
    return "ssh ปฏิเสธ key — ถ้า key มี passphrase ต้อง `ssh-add` เข้า ssh-agent ก่อน (ดู README)";
  }
  if (/Connection timed out|Could not resolve|No route to host/i.test(text)) {
    return "ต่อ ssh ไม่ถึงเครื่องปลายทาง — ตรวจ IMPORT_SSH_HOST / พอร์ต / เน็ตเวิร์ก";
  }
  if (/password authentication failed/i.test(text)) {
    return "psql ปฏิเสธรหัสผ่าน — ตรวจ IMPORT_DB_PASSWORD";
  }
  if (/database .* does not exist/i.test(text)) {
    return "ไม่พบฐานข้อมูลชื่อนี้บนเซิร์ฟเวอร์ — ตรวจ IMPORT_DB_NAME";
  }
  if (/psql: command not found|not found/i.test(text)) {
    return "ไม่พบคำสั่ง psql บนเซิร์ฟเวอร์ — ถ้า Postgres อยู่ใน docker ให้ตั้ง IMPORT_DB_DOCKER_CONTAINER";
  }
  return "";
}
