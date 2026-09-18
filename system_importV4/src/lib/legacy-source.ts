import "server-only";

import { createHash } from "node:crypto";

import type { Client as SSHClient } from "ssh2";

import { shellQuote } from "@/lib/server-target";
import type { CliInputField } from "@/lib/types";

// The "import จากเซิร์ฟเวอร์" tab: pull a clinic's legacy dumps straight off the
// Dr.Ease V2 box, so an operator types an IP instead of hunting for export files.
//
// WHY ssh2 AND NOT THE ssh CLI that src/lib/server-target.ts uses: the legacy boxes
// take a PASSWORD and nothing else, and the ssh CLI reads a password only from a TTY —
// which is exactly what `BatchMode=yes` forbids. sshpass does not exist on Windows.
// ssh2 speaks the protocol in-process, so the password never becomes a command
// argument (world-readable through `ps`) and never needs a helper binary.
//
// The credential rule is the same one the Go backend settled on for its own pull lane
// (internal/legacypull): CREDENTIALS COME FROM THE ENVIRONMENT, THE REQUEST CARRIES
// ONLY THE HOST. A browser can therefore never point this at a host of its choosing
// *with a secret of its choosing* — and when LEGACY_HOSTS_ALLOWED is set, not even the
// host is free.

export interface LegacySource {
  sshUser: string;
  sshPassword: string;
  sshPort: number;
  /** Optional: empty means mysqldump relies on the box's own ~/.my.cnf. */
  mysqlUser: string;
  mysqlPassword: string;
  mysqlDatabase: string;
  mysqlTables: string[];
  /** Empty disables the Mongo half (the pull then returns the .sql only). */
  mongoDatabase: string;
  mongoCollections: string[];
  /**
   * Empty means the Mongo half connects with no credentials, which is what every box this
   * tab saw before one of them turned authorisation on: `listCollections requires
   * authentication`, code 13, and the catalog silently came back empty. Both halves of the
   * credential are optional together so an unauthenticated box behaves exactly as before.
   */
  mongoUser: string;
  mongoPassword: string;
  /** Where that user is defined. Mongo's own convention is `admin`. */
  mongoAuthDb: string;
  /** Empty means any syntactically valid host is accepted. */
  allowedHosts: string[];
  /** Empty means the host key is accepted and reported instead of pinned. */
  fingerprints: string[];
  /**
   * Where the page images live on the source, split so the archive's member paths come out
   * right. The CLI locates an image at nas-doctorease/mris_images/page/<url> INSIDE the
   * archive (system_CLIV4 internal/legacyimport/page.go), so the tar has to be rooted one
   * level above nas-doctorease and name the subtree explicitly. Getting this pair wrong does
   * not fail loudly: the survey reports every single image as missing.
   */
  imageBase: string;
  imageMember: string;
  connectTimeoutMs: number;
  commandTimeoutMs: number;
}

/** One pulled dump, shaped so the browser can turn it back into a File. */
export interface PulledFile {
  field: CliInputField;
  name: string;
  bytes: number;
  base64: string;
}

export interface PullResult {
  host: string;
  fingerprint: string;
  files: PulledFile[];
  /** Operator-facing trace of what ran and what came back. Never holds a secret. */
  log: string[];
  /** Things that did not stop the pull but change what the operator got. */
  warnings: string[];
}

// One dump is held in memory on the way to the browser, so it is bounded by the same
// number /api/generate accepts as an upload.
const MAX_FILE_BYTES = 64 * 1024 * 1024;
const MAX_STDERR_CHARS = 64 * 1024;

// The MySQL tables the CLI needs. `clinic` is the legacy ROOM master — the name is the
// legacy schema's, not a mistake (see system_CLIV4 README).
//
// `branch` is here even though NOTHING is imported from it. It is the tenant's branch
// registry, and it is what /api/survey-branches reads to find out how many branches the
// clinic has, which company owns each one, and how much data names it. Without it a
// multi-branch tenant pulled through this tab looks single-branch, and every patient in it
// gets filed at one branch with no error anywhere to say so. It is a handful of rows.
const DEFAULT_TABLES = "users,doctor,clinic,patient,appointment,branch";

// The catalog collections the CLI reads out of the dbmongo half. `stocks` is here
// because slot_stock (one row per lot) is built from it.
const DEFAULT_COLLECTIONS = "category,items,listheals,course_items,stocks";

// Identifiers are interpolated into a remote command, so they are checked rather than
// trusted — even though they come from env and not from a request.
const IDENT_RE = /^[A-Za-z0-9_-]+$/;

const IPV4_RE = /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/;
const HOSTNAME_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$/i;

function env(k: string): string {
  return (process.env[k] ?? "").trim();
}

function csv(v: string): string[] {
  return v
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);
}

/**
 * readLegacySource returns the configured source, or the list of what is missing.
 *
 * Only the SSH credentials are mandatory. The MySQL login is optional on purpose:
 * some legacy boxes authenticate mysqldump through the account's own ~/.my.cnf or a
 * unix socket, and demanding a password we do not need would disable the tab for them.
 */
export function readLegacySource(): { source: LegacySource | null; missing: string[] } {
  const source: LegacySource = {
    sshUser: env("LEGACY_SSH_USER"),
    sshPassword: env("LEGACY_SSH_PASSWORD"),
    sshPort: Number(env("LEGACY_SSH_PORT") || "22"),
    mysqlUser: env("LEGACY_MYSQL_USER"),
    mysqlPassword: env("LEGACY_MYSQL_PASSWORD"),
    mysqlDatabase: env("LEGACY_MYSQL_DB") || "dms_doctoreasetemporary",
    mysqlTables: csv(env("LEGACY_MYSQL_TABLES") || DEFAULT_TABLES),
    mongoDatabase: env("LEGACY_MONGO_DB"),
    mongoCollections: csv(env("LEGACY_MONGO_COLLECTIONS") || DEFAULT_COLLECTIONS),
    mongoUser: env("LEGACY_MONGO_USER"),
    mongoPassword: env("LEGACY_MONGO_PASSWORD"),
    mongoAuthDb: env("LEGACY_MONGO_AUTH_DB") || "admin",
    // Defaults measured on the source, not guessed: mris_images/page is 1.1 GB over 2,548
    // files and mris_images/thumb 22 MB over 2,522. Naming mris_images rather than the whole
    // nas-doctorease folder also leaves out oldhn-newhn.zip, which is 2.1 GB of already
    // compressed archive that holds no page image.
    imageBase: env("LEGACY_IMAGE_BASE") || "/var/www/html/doctoreasetemporary/public",
    imageMember: env("LEGACY_IMAGE_MEMBER") || "nas-doctorease/mris_images",
    allowedHosts: csv(env("LEGACY_HOSTS_ALLOWED")),
    fingerprints: csv(env("LEGACY_SSH_FINGERPRINT")),
    connectTimeoutMs: 15_000,
    commandTimeoutMs: Number(env("LEGACY_PULL_TIMEOUT_SECONDS") || "300") * 1000,
  };

  const missing: string[] = [];
  if (!source.sshUser) missing.push("LEGACY_SSH_USER");
  if (!source.sshPassword) missing.push("LEGACY_SSH_PASSWORD");
  if (!Number.isFinite(source.sshPort) || source.sshPort <= 0) missing.push("LEGACY_SSH_PORT");
  if (source.mysqlTables.length === 0) missing.push("LEGACY_MYSQL_TABLES");

  return { source: missing.length === 0 ? source : null, missing };
}

/** A short, non-secret description for the UI. */
export function describeSource(s: LegacySource): string {
  const auth = s.mongoUser ? ` · ล็อกอิน ${s.mongoUser}@${s.mongoAuthDb}` : " · ไม่ล็อกอิน";
  const mongo = s.mongoDatabase
    ? `mongo ${s.mongoDatabase} (${s.mongoCollections.length} collection)${auth}`
    : "mongo ปิดอยู่ (ไม่ได้ตั้ง LEGACY_MONGO_DB)";
  return `${s.sshUser}@<ip>:${s.sshPort} · mysql ${s.mysqlDatabase} (${s.mysqlTables.length} ตาราง) · ${mongo}`;
}

/**
 * checkHost validates the one value that DOES come from the request. It returns the
 * reason a host is refused, or null when it is acceptable.
 */
export function checkHost(s: LegacySource, host: string): string | null {
  const h = host.trim();
  if (!h) return "กรอก IP ของเซิร์ฟเวอร์ก่อน";
  if (h.length > 253) return "IP/hostname ยาวเกินไป";
  if (!IPV4_RE.test(h) && !HOSTNAME_RE.test(h)) {
    return `"${h}" ไม่ใช่ IP หรือ hostname ที่ใช้ได้`;
  }
  if (s.allowedHosts.length > 0) {
    const allowed = s.allowedHosts.some((a) => a.toLowerCase() === h.toLowerCase());
    if (!allowed) {
      return `${h} ไม่อยู่ใน LEGACY_HOSTS_ALLOWED — เพิ่มเข้าไปใน .env.local ก่อนถ้าตั้งใจจะดึงจากเครื่องนี้`;
    }
  }
  return null;
}

function validIdent(kind: string, v: string): void {
  if (!IDENT_RE.test(v)) {
    throw new Error(`${kind} "${v}" มีอักขระที่ใช้ไม่ได้ (รับ A-Z a-z 0-9 _ - เท่านั้น)`);
  }
}

/** OpenSSH-style fingerprint, so it can be compared with `ssh-keyscan | ssh-keygen -lf -`. */
function fingerprintOf(key: Buffer): string {
  return "SHA256:" + createHash("sha256").update(key).digest("base64").replace(/=+$/, "");
}

interface RemoteResult {
  code: number;
  stdout: Buffer;
  stderr: string;
}

/**
 * connect opens one authenticated SSH connection.
 *
 * Legacy algorithms are APPENDED to ssh2's defaults (not substituted) because these
 * boxes are old enough to offer only sha1 key exchange and ssh-rsa host keys, which
 * modern defaults exclude. Appending keeps a modern box on modern crypto.
 */
async function connect(s: LegacySource, host: string): Promise<{ conn: SSHClient; fingerprint: string }> {
  const { Client } = require("ssh2") as typeof import("ssh2");
  const conn = new Client();

  let seen = "";
  let pinRejected = false;

  return new Promise<{ conn: SSHClient; fingerprint: string }>((resolve, reject) => {
    conn.on("ready", () => resolve({ conn, fingerprint: seen }));
    conn.on("error", (err: Error & { level?: string }) => {
      conn.end();
      reject(connectError(err, s, host, seen, pinRejected));
    });

    conn.connect({
      host,
      port: s.sshPort,
      username: s.sshUser,
      password: s.sshPassword,
      readyTimeout: s.connectTimeoutMs,
      // The password is sent over this channel, so an unverified host is a real risk:
      // whoever answers the port learns it. When fingerprints are configured this is a
      // hard gate; when they are not, the fingerprint is reported to the operator so
      // they can pin it (LEGACY_SSH_FINGERPRINT) after checking it once.
      hostVerifier: (key: Buffer) => {
        seen = fingerprintOf(key);
        if (s.fingerprints.length === 0) return true;
        const ok = s.fingerprints.some((f) => f.replace(/^SHA256:/, "") === seen.replace(/^SHA256:/, ""));
        pinRejected = !ok;
        return ok;
      },
      // `prepend`/`remove` are empty because the modern defaults must stay first and
      // stay present; the append lists are only a fallback for what an old sshd offers.
      // (@types/ssh2 requires all three keys — ssh2 itself treats them as optional.)
      algorithms: {
        kex: {
          append: [
            "diffie-hellman-group14-sha1",
            "diffie-hellman-group-exchange-sha1",
            "diffie-hellman-group1-sha1",
          ],
          prepend: [],
          remove: [],
        },
        serverHostKey: { append: ["ssh-rsa", "ssh-dss"], prepend: [], remove: [] },
        cipher: { append: ["aes256-cbc", "aes128-cbc", "3des-cbc"], prepend: [], remove: [] },
        hmac: { append: ["hmac-sha1"], prepend: [], remove: [] },
      },
    });
  });
}

function connectError(
  err: Error,
  s: LegacySource,
  host: string,
  seen: string,
  pinRejected: boolean,
): Error {
  const text = err.message || String(err);
  if (pinRejected) {
    return new Error(
      `host key ของ ${host} ไม่ตรงกับ LEGACY_SSH_FINGERPRINT — เครื่องปลายทางเสนอ ${seen} ` +
        `(ถ้ามั่นใจว่าเปลี่ยนเครื่องจริง ให้ตรวจด้วย ssh-keyscan แล้วแก้ค่าใน .env.local) ` +
        `ยังไม่ได้ส่งรหัสผ่านออกไป`,
    );
  }
  if (/All configured authentication methods failed/i.test(text)) {
    return new Error(
      `${host} ปฏิเสธการล็อกอินของ ${s.sshUser} — ตรวจ LEGACY_SSH_USER / LEGACY_SSH_PASSWORD`,
    );
  }
  if (/ECONNREFUSED/i.test(text)) return new Error(`${host}:${s.sshPort} ปฏิเสธการเชื่อมต่อ (ไม่มี ssh ฟังพอร์ตนี้)`);
  if (/ETIMEDOUT|Timed out while waiting/i.test(text)) {
    return new Error(`ต่อ ${host}:${s.sshPort} ไม่ทัน ${s.connectTimeoutMs / 1000}s — ตรวจ IP / firewall / VPN`);
  }
  if (/ENOTFOUND|EAI_AGAIN/i.test(text)) return new Error(`หา ${host} ไม่เจอใน DNS`);
  if (/EHOSTUNREACH|ENETUNREACH/i.test(text)) return new Error(`ไปไม่ถึง ${host} จากเครื่องนี้`);
  if (/Handshake failed|no matching/i.test(text)) {
    return new Error(`ตกลงวิธีเข้ารหัสกับ ${host} ไม่ได้: ${text}`);
  }
  return new Error(`ต่อ ssh ${host} ไม่ได้: ${text}`);
}

/**
 * execRemote runs one command, optionally feeding stdin, and captures stdout as BYTES.
 * A mysqldump is not necessarily valid UTF-8 (latin1 columns are common in these
 * schemas), so decoding it here would corrupt it — it stays a Buffer until the browser
 * turns it back into a file.
 */
function execRemote(
  conn: SSHClient,
  command: string,
  opts: { stdin?: string; timeoutMs: number },
): Promise<RemoteResult> {
  return new Promise((resolve, reject) => {
    conn.exec(command, (err, stream) => {
      if (err) {
        reject(new Error(`เปิด session บนเครื่องปลายทางไม่ได้: ${err.message}`));
        return;
      }

      const chunks: Buffer[] = [];
      let size = 0;
      let stderr = "";
      let tooBig = false;
      let timedOut = false;

      const timer = setTimeout(() => {
        timedOut = true;
        stream.close();
      }, opts.timeoutMs);

      stream.on("data", (d: Buffer) => {
        size += d.length;
        if (size > MAX_FILE_BYTES) {
          tooBig = true;
          stream.close();
          return;
        }
        chunks.push(d);
      });
      stream.stderr.on("data", (d: Buffer) => {
        if (stderr.length < MAX_STDERR_CHARS) stderr += d.toString("utf8");
      });
      stream.on("close", (code: number | null) => {
        clearTimeout(timer);
        if (timedOut) {
          reject(new Error(`คำสั่งบนเครื่องปลายทางใช้เวลาเกิน ${opts.timeoutMs / 1000}s — ยกเลิกแล้ว`));
          return;
        }
        if (tooBig) {
          reject(new Error(`ข้อมูลที่ดึงมาใหญ่เกิน ${MAX_FILE_BYTES / 1024 / 1024} MB`));
          return;
        }
        resolve({ code: code ?? -1, stdout: Buffer.concat(chunks), stderr: stderr.trim() });
      });

      stream.end(opts.stdin ?? "");
    });
  });
}

function remoteFail(what: string, r: RemoteResult): Error {
  const tail = r.stderr ? ` — ปลายทางบอกว่า: ${r.stderr.slice(0, 400)}` : "";
  return new Error(`${what} ไม่สำเร็จ (exit ${r.code})${tail}`);
}

/**
 * secretFileScript wraps a remote command so a secret can reach it without ever appearing
 * in a command line.
 *
 * The secret arrives on stdin, lands in a 0600 temp file, and the file is removed when the
 * shell exits however it exits. sshd runs the remote command through a shell, so anything
 * in the command string is visible in `ps` to every account on that box — and these boxes
 * have other accounts on them. mysqldumpScript below has always done this; the Mongo half
 * now does too.
 */
function secretFileScript(command: string): string {
  return [
    "umask 077",
    "f=$(mktemp) || exit 90",
    `trap 'rm -f "$f"' EXIT`,
    `cat > "$f" || exit 91`,
    command,
  ].join("\n");
}

/**
 * mysqlDefaultsFile is the content handed to the remote mysqldump on STDIN and written
 * to a 0600 temp file there. A password in argv shows up in `ps` for every user on the
 * box; in the environment it shows up in /proc. A defaults file shows up in neither.
 */
function mysqlDefaultsFile(s: LegacySource): string {
  const esc = (v: string) => v.replaceAll("\\", "\\\\").replaceAll('"', '\\"');
  return `[client]\nuser="${esc(s.mysqlUser)}"\npassword="${esc(s.mysqlPassword)}"\n`;
}

/**
 * mysqldumpScript exports the mapped tables as ONE file, which is exactly the shape
 * `system_cliv4 -sql` already accepts (it selects tables by name).
 *
 * --single-transaction takes a consistent snapshot without locking a live clinic out,
 * --skip-lock-tables avoids needing LOCK TABLES on a limited grant, and
 * --skip-dump-date keeps two pulls of unchanged data byte-identical so they can be
 * diffed.
 */
function mysqldumpScript(s: LegacySource, withCreds: boolean): string {
  validIdent("ชื่อฐานข้อมูล MySQL", s.mysqlDatabase);
  for (const t of s.mysqlTables) validIdent("ชื่อตาราง", t);

  const args = [s.mysqlDatabase, ...s.mysqlTables].map(shellQuote).join(" ");
  // --complete-insert is not a preference. The CLI's dump reader locates rows by searching
  // for ") VALUES", i.e. it requires the column list (internal/legacydump/dump.go), and
  // mysqldump omits that list by default. Without the flag the pull produced a dump the CLI
  // parsed as EMPTY while reporting success: has_branch_table=false, total_patients=0 and
  // branches=null on a 6.3 MB file that plainly contained 947 patients. The dumps this tab
  // was built against were exported by hand with column names, which is why nothing caught
  // it until a dump came from here.
  const flags =
    "--complete-insert --single-transaction --skip-lock-tables --no-tablespaces " +
    "--skip-dump-date --default-character-set=utf8mb4";
  const guard = `command -v mysqldump >/dev/null 2>&1 || { echo 'ไม่พบคำสั่ง mysqldump บนเครื่องนี้' >&2; exit 92; }`;

  if (!withCreds) {
    // No credentials configured: mysqldump uses whatever the account already has
    // (~/.my.cnf or socket auth). Nothing is read from stdin in this branch.
    return [guard, `mysqldump ${flags} ${args}`].join("\n");
  }

  return [
    "umask 077",
    "f=$(mktemp) || exit 90",
    `trap 'rm -f "$f"' EXIT`,
    // The credentials arrive on stdin and never touch a command line.
    `cat > "$f" || exit 91`,
    guard,
    `mysqldump --defaults-extra-file="$f" ${flags} ${args}`,
  ].join("\n");
}

async function pullMySQL(conn: SSHClient, s: LegacySource, log: string[]): Promise<Buffer> {
  const withCreds = s.mysqlUser !== "" || s.mysqlPassword !== "";
  log.push(
    `mysqldump ${s.mysqlDatabase}: ${s.mysqlTables.join(", ")}` +
      (withCreds ? ` (ล็อกอินเป็น ${s.mysqlUser})` : " (ใช้ credential ที่ตั้งไว้บนเครื่องปลายทาง)"),
  );

  const r = await execRemote(conn, mysqldumpScript(s, withCreds), {
    stdin: withCreds ? mysqlDefaultsFile(s) : undefined,
    timeoutMs: s.commandTimeoutMs,
  });
  if (r.code !== 0) throw remoteFail("mysqldump", r);
  if (r.stdout.length === 0) throw new Error("mysqldump ไม่ได้ส่งข้อมูลกลับมาเลย");

  log.push(`  ได้ ${kb(r.stdout.length)}`);
  return r.stdout;
}

/** Which of the three Mongo clients the box actually has, best first. */
async function mongoTool(conn: SSHClient, s: LegacySource): Promise<"mongoexport" | "mongosh" | "mongo" | null> {
  const r = await execRemote(
    conn,
    'for t in mongoexport mongosh mongo; do command -v "$t" >/dev/null 2>&1 && echo "$t"; done\ntrue',
    { timeoutMs: 30_000 },
  );
  const found = r.stdout.toString("utf8").split("\n").map((l) => l.trim());
  for (const t of ["mongoexport", "mongosh", "mongo"] as const) {
    if (found.includes(t)) return t;
  }
  return null;
}

/**
 * collectionStatement pulls one collection and returns a statement the CLI's parser
 * accepts (internal/mongodump documents the shapes: db.getCollection(...).insert /
 * insertMany, shell constructors, and canonical Extended JSON).
 *
 * mongoexport is preferred because it emits canonical Extended JSON, which keeps
 * ObjectId, ISODate and NumberDecimal distinguishable — the CLI relies on that
 * ($numberDecimal in particular, because every decimal in these dumps is money).
 */
async function collectionStatement(
  conn: SSHClient,
  s: LegacySource,
  tool: "mongoexport" | "mongosh" | "mongo",
  collection: string,
): Promise<{ text: string; bytes: number } | null> {
  validIdent("ชื่อ collection", collection);
  validIdent("ชื่อฐานข้อมูล Mongo", s.mongoDatabase);

  // Both halves of the credential, or neither. A box with authorisation off keeps the exact
  // command it had before, so nothing changes for the sources that already worked.
  const useAuth = s.mongoUser !== "" && s.mongoPassword !== "";
  if (useAuth) validIdent("ชื่อฐานข้อมูลที่ใช้ล็อกอิน Mongo", s.mongoAuthDb);

  if (tool === "mongoexport") {
    const base =
      `mongoexport --quiet --db ${shellQuote(s.mongoDatabase)} ` +
      `--collection ${shellQuote(collection)} --jsonArray`;
    // --config is mongoexport's own answer to this (4.2+): a file it reads the password out
    // of itself, so --password never appears. JSON.stringify produces a double-quoted YAML
    // scalar, which keeps a password containing ':' or '#' from breaking the parse.
    const cmd = useAuth
      ? secretFileScript(
          `${base} --username ${shellQuote(s.mongoUser)} ` +
            `--authenticationDatabase ${shellQuote(s.mongoAuthDb)} --config "$f"`,
        )
      : base;
    const r = await execRemote(conn, cmd, {
      timeoutMs: s.commandTimeoutMs,
      stdin: useAuth ? `password: ${JSON.stringify(s.mongoPassword)}\n` : undefined,
    });
    if (r.code !== 0) throw remoteFail(`mongoexport ${collection}`, r);
    const body = r.stdout.toString("utf8").trim();
    if (body === "" || body === "[]") return null;
    return {
      text: `db.getCollection("${collection}").insertMany(${body});\n`,
      bytes: r.stdout.length,
    };
  }

  // Shell fallback. The printer emits one insert per document, which is what these
  // dbmongo/*.js exports have always looked like. mongosh has EJSON; the legacy mongo
  // shell has tojson, whose ObjectId("…")/ISODate("…") output the parser also accepts.
  const printer =
    tool === "mongosh"
      ? `db.getCollection("${collection}").find().forEach(function(d){print('db.getCollection("${collection}").insert(' + EJSON.stringify(d) + ');')})`
      : `db.getCollection("${collection}").find().forEach(function(d){print('db.getCollection("${collection}").insert(' + tojson(d) + ');')})`;

  // With credentials the script goes in through stdin as a FILE the shell then runs, because
  // the alternative is db.auth(user, password) inside --eval, and --eval is a command line.
  const cmd = useAuth
    ? secretFileScript(`${tool} --quiet ${shellQuote(s.mongoDatabase)} "$f"`)
    : `${tool} --quiet ${shellQuote(s.mongoDatabase)} --eval ${shellQuote(printer)}`;
  const r = await execRemote(conn, cmd, {
    timeoutMs: s.commandTimeoutMs,
    stdin: useAuth
      ? `db.getSiblingDB(${JSON.stringify(s.mongoAuthDb)}).auth(${JSON.stringify(s.mongoUser)}, ${JSON.stringify(s.mongoPassword)});\n${printer}\n`
      : undefined,
  });
  if (r.code !== 0) throw remoteFail(`${tool} ${collection}`, r);
  const body = r.stdout.toString("utf8").trim();
  if (body === "") return null;
  return { text: body + "\n", bytes: r.stdout.length };
}

async function pullMongo(
  conn: SSHClient,
  s: LegacySource,
  log: string[],
  warnings: string[],
): Promise<Buffer | null> {
  const tool = await mongoTool(conn, s);
  if (!tool) {
    warnings.push(
      "ไม่พบ mongoexport / mongosh / mongo บนเครื่องปลายทาง — ได้แต่ไฟล์ MySQL ส่วนแคตตาล็อกต้องแนบไฟล์ .js เอง",
    );
    return null;
  }
  log.push(`mongo: ใช้ ${tool} · ฐาน ${s.mongoDatabase}`);

  const parts: string[] = [`// pulled from ${s.mongoDatabase} via ${tool} by system_importV4\n`];
  let got = 0;
  for (const c of s.mongoCollections) {
    const stmt = await collectionStatement(conn, s, tool, c);
    if (!stmt) {
      log.push(`  ${c}: ว่าง/ไม่มี collection นี้ — ข้าม`);
      continue;
    }
    parts.push(stmt.text);
    got++;
    log.push(`  ${c}: ${kb(stmt.bytes)}`);
  }
  if (got === 0) {
    warnings.push(
      `ไม่มี collection ใดใน ${s.mongoDatabase} ที่ดึงได้เลย (${s.mongoCollections.join(", ")}) — ` +
        "ตรวจ LEGACY_MONGO_DB / LEGACY_MONGO_COLLECTIONS",
    );
    return null;
  }
  return Buffer.from(parts.join(""), "utf8");
}

/** listMongoDatabases is a hint for the operator when LEGACY_MONGO_DB is not set yet. */
async function listMongoDatabases(conn: SSHClient, s: LegacySource): Promise<string[]> {
  const tool = await mongoTool(conn, s);
  if (!tool || tool === "mongoexport") return [];
  const r = await execRemote(
    conn,
    `${tool} --quiet --eval ${shellQuote("db.adminCommand({listDatabases:1}).databases.forEach(function(d){print(d.name)})")}`,
    { timeoutMs: 30_000 },
  );
  if (r.code !== 0) return [];
  return r.stdout
    .toString("utf8")
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l !== "" && IDENT_RE.test(l));
}

function kb(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
}

/** What a finished image pull produced. */
export interface ImagePullResult {
  host: string;
  fingerprint: string;
  bytes: number;
  /** The tar's own member prefix, so a mismatch with the CLI is visible in the log. */
  member: string;
  log: string[];
}

/**
 * pullImagesToFile streams the page-image tree off the source and writes it STRAIGHT TO DISK
 * on this machine.
 *
 * WHY NOT execRemote LIKE EVERYTHING ELSE HERE
 *
 * execRemote buffers stdout in memory and caps it at MAX_FILE_BYTES. This archive is 1.12 GB
 * on the source that was measured, and the browser is not in the path at all: a base64 body
 * would be 1.5 GB of JSON in a tab, on a server with 908 MB of RAM. So the bytes go from the
 * ssh channel to a file descriptor and nowhere else.
 *
 * WHY THE TAR IS NOT WRITTEN ON THE SOURCE FIRST
 *
 * Because there is no room for it: that box is at 98% with 597 MB free, and the archive is
 * bigger than the free space. Writing it there would fill the root filesystem of a machine
 * running the clinic's MySQL, Mongo and web server — not a risk, an outcome. `tar -czf -`
 * sends it to stdout instead, so nothing is created on the source and nothing has to be
 * cleaned up if this fails half way.
 */
export async function pullImagesToFile(
  s: LegacySource,
  host: string,
  destPath: string,
): Promise<ImagePullResult> {
  const { createWriteStream } = await import("node:fs");
  const { conn, fingerprint } = await connect(s, host);
  const log: string[] = [];

  try {
    log.push(`ssh ${s.sshUser}@${host}:${s.sshPort} — host key ${fingerprint}`);

    // -C then the member name, in that order, is what puts "nas-doctorease/mris_images/…" at
    // the front of every path in the archive. The CLI looks for exactly that prefix.
    const cmd =
      `cd ${shellQuote(s.imageBase)} && ` +
      `test -d ${shellQuote(s.imageMember)} && ` +
      `tar -czf - -C ${shellQuote(s.imageBase)} ${shellQuote(s.imageMember)}`;
    log.push(`tar -czf - -C ${s.imageBase} ${s.imageMember} (สตรีมออกมา ไม่เขียนไฟล์บนเครื่องต้นทาง)`);

    const bytes = await new Promise<number>((resolve, reject) => {
      conn.exec(cmd, (err, stream) => {
        if (err) {
          reject(new Error(`เปิด session บนเครื่องปลายทางไม่ได้: ${err.message}`));
          return;
        }
        const file = createWriteStream(destPath);
        let written = 0;
        let stderr = "";
        let failed: Error | null = null;

        file.on("error", (e) => {
          failed = new Error(`เขียนไฟล์ที่ ${destPath} ไม่ได้: ${e.message}`);
          stream.close();
        });
        stream.on("data", (d: Buffer) => {
          written += d.length;
          // Backpressure is honoured rather than ignored: without it a fast tunnel outruns
          // the disk and the difference is held in memory on a 908 MB box.
          if (!file.write(d)) {
            stream.pause();
            file.once("drain", () => stream.resume());
          }
        });
        stream.stderr.on("data", (d: Buffer) => {
          if (stderr.length < MAX_STDERR_CHARS) stderr += d.toString("utf8");
        });
        stream.on("close", (code: number | null) => {
          file.end(() => {
            if (failed) {
              reject(failed);
              return;
            }
            if (code !== 0) {
              // tar exits non-zero for a missing directory and for a read error alike, and
              // the stderr is the only thing that tells them apart.
              reject(
                new Error(
                  `ดึงไฟล์รูปไม่สำเร็จ (exit ${code})` +
                    (stderr.trim() ? ` — ปลายทางบอกว่า: ${stderr.trim().slice(0, 400)}` : ""),
                ),
              );
              return;
            }
            if (written === 0) {
              reject(new Error("ดึงไฟล์รูปได้ 0 ไบต์ — ตรวจ LEGACY_IMAGE_BASE / LEGACY_IMAGE_MEMBER"));
              return;
            }
            resolve(written);
          });
        });
        stream.end("");
      });
    });

    log.push(`ได้ไฟล์รูป ${kb(bytes)} → ${destPath}`);
    return { host, fingerprint, bytes, member: s.imageMember, log };
  } finally {
    conn.end();
  }
}

/**
 * pullFromHost is the whole job: connect once, run mysqldump, run the Mongo export,
 * hand back both as files. The connection is closed whatever happens.
 *
 * A Mongo failure is a WARNING, not an error: the MySQL half is independently useful
 * (people · rooms · patients · appointments) and refusing to return it because a
 * catalog export failed would make the operator re-pull the big file for nothing.
 */
export async function pullFromHost(s: LegacySource, host: string): Promise<PullResult> {
  const { conn, fingerprint } = await connect(s, host);
  const log: string[] = [];
  const warnings: string[] = [];

  try {
    log.push(`ssh ${s.sshUser}@${host}:${s.sshPort} — host key ${fingerprint}`);
    if (s.fingerprints.length === 0) {
      warnings.push(
        `host key ของ ${host} ไม่ได้ pin ไว้ — ถ้าจะดึงจากเครื่องนี้อีก ให้ใส่ ${fingerprint} ` +
          "ใน LEGACY_SSH_FINGERPRINT เพื่อกันการสวมเครื่อง (รหัสผ่านถูกส่งผ่านช่องนี้)",
      );
    }

    const files: PulledFile[] = [];

    const sql = await pullMySQL(conn, s, log);
    files.push({
      field: "sql",
      name: `${s.mysqlDatabase}.sql`,
      bytes: sql.length,
      base64: sql.toString("base64"),
    });

    if (!s.mongoDatabase) {
      const candidates = await listMongoDatabases(conn, s);
      warnings.push(
        "ยังไม่ได้ตั้ง LEGACY_MONGO_DB — ข้ามแคตตาล็อก (Mongo)" +
          (candidates.length > 0 ? ` · ฐานที่เจอบนเครื่องนี้: ${candidates.join(", ")}` : ""),
      );
    } else {
      try {
        const mongo = await pullMongo(conn, s, log, warnings);
        if (mongo) {
          files.push({
            field: "mongo",
            name: `${s.mongoDatabase}.js`,
            bytes: mongo.length,
            base64: mongo.toString("base64"),
          });
        }
      } catch (e) {
        warnings.push(`ดึง Mongo ไม่สำเร็จ: ${e instanceof Error ? e.message : String(e)}`);
      }
    }

    return { host, fingerprint, files, log, warnings };
  } finally {
    conn.end();
  }
}
