// Types shared between the CLI-invoking route handler and the UI.
//
// The CLI (system_CLIV4) is the single source of truth for the mapping; this app only
// collects files, runs it, and hands back what it produced.

// The inputs the CLI accepts. `room` is the legacy table literally named `clinic` —
// it is the ROOM master, not clinic data. `mongo` is the whole dbmongo export; the CLI
// picks the collections it can map out of it.
export const CLI_INPUTS = [
  {
    field: "sql",
    label: "MySQL (.sql)",
    hint: "ไฟล์ dump ทั้งฐานได้เลย — ระบบหาตาราง users · doctor · clinic(=ห้อง) · patient · appointment ให้เอง",
  },
  {
    field: "mongo",
    // `page` comes out of the SAME file even though it lives in another legacy database
    // (mris): the parser selects by collection name and ignores the rest, so appending one
    // export to the other is all it takes. One attachment, two things read out of it.
    label: "MongoDB (.js)",
    hint: "แคตตาล็อก: category → catalog_category · items → items + catalog_item(drug) + slot_stock · listheals → บริการ · course_items → คอร์ส · page → รูปหน้าเวชระเบียน (photos)",
  },
] as const;

export type CliInputField = (typeof CLI_INPUTS)[number]["field"];

/**
 * Largest total attachment the tool accepts, counted before anything is sent.
 *
 * A dump this large never travels as one request. It is sliced into CHUNK_BYTES pieces
 * (see /api/upload-chunk), so the ceilings that used to decide this number no longer
 * apply to it: Cloudflare's 100 MB body limit and nginx's `client_max_body_size 64m` now
 * only have to fit one chunk. What this value protects is disk in the staging directory.
 */
export const MAX_UPLOAD_BYTES = 4096 * 1024 * 1024;

/**
 * Size of one upload slice. Must stay comfortably under BOTH the Cloudflare body limit
 * (100 MB on Free/Pro) and nginx `client_max_body_size` (64 MB) — raising it past either
 * makes every upload fail at the proxy, before the app is involved.
 */
export const CHUNK_BYTES = 32 * 1024 * 1024;

/**
 * Largest input the CLI may be handed, measured AFTER the Mongo dump is filtered.
 *
 * This one is about memory, not transport: the CLI reads its input with os.ReadFile and
 * keeps every parsed document. Measured on the deployed box (908 MB RAM + 8 GB swap):
 * a 897.6 MB input finished in 347 seconds with a 703 MB peak RSS, 271k major page faults,
 * no OOM kill, and the web app untouched. 1 GB is that measurement with headroom — going
 * higher needs a bigger box or a streaming parser in the CLI, not a bigger number.
 */
export const MAX_CLI_INPUT_BYTES = 1024 * 1024 * 1024;

/** How often the browser asks a running job for its status. */
export const JOB_POLL_MS = 2500;

/**
 * The Mongo collections system_CLIV4 actually reads. Anything else in a dump is skipped
 * by its parser, so exporting it only costs upload budget.
 *
 * Taken from the CLI source, not from memory:
 *   internal/legacyimport/catalog.go — items · listheals · course_items, the four name
 *     lookups (category · kind · item_types · item_units), stocks (slot_stock is built
 *     from it), and the money side (bills · payment_summary · course_bills)
 *   internal/legacyimport/page.go — page (รูปหน้าเวชระเบียน → photos)
 *
 * `page` lives in the `mris` database while the catalog lives in another; both end up in
 * one attachment because the parser selects by collection name, not by database.
 */
export const MONGO_COLLECTIONS_USED = [
  "category",
  "kind",
  "item_types",
  "item_units",
  "stocks",
  "items",
  "listheals",
  "course_items",
  "bills",
  "payment_summary",
  "course_bills",
  "page",
] as const;

// Groups the CLI can import separately (`-only`). Selecting a subset is how a target
// that already holds part of this data gets only the missing pieces — writing the rest
// would overwrite rows someone may have edited by hand.
export const IMPORT_GROUPS = [
  { group: "people", label: "บุคลากร", detail: "users · clinic_user · clinic_staff · doctor" },
  { group: "rooms", label: "ห้อง", detail: "room" },
  { group: "patients", label: "คนไข้", detail: "patient" },
  { group: "appointments", label: "นัด", detail: "appointment" },
  { group: "catalog", label: "แคตตาล็อก", detail: "catalog_category · บริการ · items · ยา · คอร์ส" },
  { group: "stock", label: "สต็อก (ล็อต)", detail: "slot_stock" },
  {
    group: "money",
    label: "ประวัติการขาย/รับเงิน",
    // All five tables move together on purpose: an invoice without its items is a bill
    // for nothing, and a receipt without its payment channels is money with no source.
    detail: "invoice · invoice_item · receipt · receipt_payment (ต้องมีคนไข้และแคตตาล็อกในระบบก่อน)",
  },
] as const;

export type ImportGroup = (typeof IMPORT_GROUPS)[number]["group"];

/**
 * A long-running server task, polled by the browser.
 *
 * Generation and import both take longer than the 100 seconds Cloudflare allows a proxied
 * request to stay open, so both start a job and answer with its id.
 */
export interface JobView {
  id: string;
  kind: "generate" | "import" | "survey";
  state: "running" | "done" | "failed";
  /** What the server is doing right now, in words. */
  phase: string;
  log: string[];
  seconds: string;
  clinicCode: string;
  error?: string;
  /** generate: the artifact. The script itself is never sent here — it is downloaded. */
  filename?: string;
  sqlBytes?: number;
  report?: string;
  /** First few KB of the script, for review without transferring the whole file. */
  preview?: string;
  /** import: where it went and what psql said. */
  target?: string;
  isProduction?: boolean;
  psqlLog?: string;
  /**
   * survey: the branch survey itself, carried in the job rather than written to a file.
   * It is a few kilobytes and the browser needs all of it, unlike a generated script.
   */
  survey?: BranchSurvey;
  sqlBytesIn?: number;
  mongoBytesIn?: number;
}

export interface JobStartResponse {
  ok: boolean;
  jobId?: string;
  error?: string;
  target?: string;
}

export interface JobStatusResponse {
  ok: boolean;
  job?: JobView;
  error?: string;
}

export interface GenerateOk {
  ok: true;
  /** Suggested download name, e.g. import_C9D4865.sql */
  filename: string;
  /** The generated SQL script, verbatim. */
  sql: string;
  /** The CLI's own stdout report: what will be written, warnings, notes. */
  report: string;
  /**
   * What the server did before the CLI ran: which Mongo collections were kept, which were
   * dropped, and the sizes involved. Shown to the operator because a file that shrank from
   * 231 MB to 40 MB deserves an explanation of what left it.
   */
  log?: string[];
}

export interface GenerateErr {
  ok: false;
  /** Why generation failed — the CLI fails closed and explains itself on stderr. */
  error: string;
  /** Filtering log, when the failure happened after the dump was already processed. */
  log?: string[];
}

export type GenerateResponse = GenerateOk | GenerateErr;

// ── tab 2: ดึง dump จากเซิร์ฟเวอร์ของระบบเก่าเอง ─────────────────────────────
//
// The pulled bytes travel back to the browser and become File objects in the same slots
// a manual upload fills, so /api/generate keeps its single contract: multipart with
// `sql` / `mongo`. That is deliberate — tab 1 and tab 2 differ only in where the file
// came from, and nothing downstream has to know which was used.

export interface PulledFileInfo {
  field: CliInputField;
  /** Suggested file name, e.g. dms_doctoreasetemporary.sql */
  name: string;
  bytes: number;
  /** The dump itself, base64 — bytes survive a schema with latin1 columns intact. */
  base64: string;
}

export interface PullOk {
  ok: true;
  host: string;
  /** SHA256 host key fingerprint of the box that answered. */
  fingerprint: string;
  files: PulledFileInfo[];
  /** What ran on the far side and what came back. */
  log: string[];
  /** Did not stop the pull, but changes what the operator got. */
  warnings: string[];
}

export interface PullErr {
  ok: false;
  error: string;
}

export type PullResponse = PullOk | PullErr;

/** Whether the server-pull tab is usable, and what it points at. Never holds a secret. */
export interface LegacySourceStatus {
  configured: boolean;
  description?: string;
  missing?: string[];
  /** Empty/absent means any host may be entered. */
  hostsAllowed?: string[];
}

// ── ปลายทาง: test / production ────────────────────────────────────────────────
//
// The browser sends an ID from this list and nothing else. Every host, database name and
// credential lives on the server, so a tampered request cannot point an import at a
// server that was never configured.

export const TARGET_IDS = ["test", "production"] as const;
export type TargetId = (typeof TARGET_IDS)[number];

/** One environment, as the UI is allowed to see it. Never carries a key path or password. */
export interface TargetInfo {
  id: TargetId;
  /** Short name for the dropdown, e.g. "test". */
  label: string;
  /** Where it points, safe to display: "test · ubuntu@1.2.3.4 · drease_temporary (peer)". */
  description: string;
  /** Drives the red warning. True for the environment holding real clinic data. */
  isProduction: boolean;
}

/** GET /api/target */
export interface TargetsResponse {
  targets: TargetInfo[];
  /** Environments that are configured incompletely, and what they are missing. */
  problems?: { id: TargetId; missing: string[] }[];
}

// ── สร้างคลินิกที่ปลายทาง (เรียก POST /v3/import/clinics ของ backend) ─────────

/** Whether creating a clinic is possible on one environment right now, and why not. */
export interface ClinicAPIStatus {
  id: TargetId;
  available: boolean;
  /** What to fix. Present only when available is false. */
  reason?: string;
  missing?: string[];
  /** Where the request goes. Never carries the token. */
  api?: string;
  probeStatus?: number;
}

/** GET /api/clinic-api-status */
export interface ClinicAPIStatusResponse {
  targets: ClinicAPIStatus[];
}

/** The owner account, as the backend reports it. Mirrors admin.OwnerResult. */
export interface ProvisionedOwner {
  user_id: number;
  email: string;
  name: string;
  /** "created" | "existed" */
  user: string;
  /** "created" | "existed" — the clinic_user membership carrying the owner role. */
  membership: string;
  /** "created" | "existed" — the clinic_staff row shown on Settings › ผู้ใช้งาน. */
  staff: string;
  /** Present ONCE, and only when the server generated it. Not stored in readable form. */
  generated_password?: string;
  must_change_password: boolean;
  /** True when the clinic already had somebody holding the owner role. */
  clinic_already_had_owner: boolean;
}

/** What the backend reports back after provisioning. Mirrors admin.ProvisionResult. */
export interface ProvisionedClinic {
  clinic_code: string;
  generated: boolean;
  /** "created" | "existed" */
  identity: string;
  /** "created" | "existed" */
  branch: string;
  branch_code?: string;
  branch_count: number;
  /**
   * Present ONLY when the request supplied a branch list — every branch asked for, with
   * the code V4 assigned. Its absence is the signal that the target backend predates
   * multi-branch provisioning and silently dropped the list (Gin ignores unknown JSON
   * fields), which is why /api/create-clinic refuses instead of carrying on.
   */
  branches?: ProvisionedBranch[];
  /** "written" | "kept" | "skipped" — whether the tax/receipt profile was seeded. */
  settings?: string;
  /** "created" | "existed" | "skipped" */
  subscription: string;
  /** "stamped" | "kept" */
  features: string;
  tha_name: string;
  eng_name: string;
  active: boolean;
  /**
   * Whether the clinic can stop working on a date. Reported separately from
   * `subscription` so "existed" can never hide a row that is still counting down.
   */
  never_expires: boolean;
  /** Set only when the subscription DOES have an end date. */
  expires_on?: string;
  subscription_status?: string;
  owner?: ProvisionedOwner;
}

/** POST /api/create-clinic */
export type CreateClinicResponse =
  | { ok: true; target: string; seconds: string; clinic: ProvisionedClinic }
  | { ok: false; error: string; status?: number; target?: string; missing?: string[]; raw?: string };

// ── สาขา: อ่านจาก dump ก่อน แล้วค่อยสร้างคลินิก ───────────────────────────────
//
// WHY THE BRANCHES HAVE TO BE READ BEFORE THE CLINIC EXISTS
//
// V4 owns branch identity. A branch_code comes out of a sequence and carries a branch_no
// that every HN issued at that branch keeps for life, so the legacy "01" cannot be written
// through. That leaves exactly one workable order:
//
//   read the legacy branch table  →  operator picks which branches are THIS clinic
//     →  create them all in one provision call  →  it answers source_code → branch_code
//     →  hand that map to the CLI as -branch-map
//
// Getting it wrong is not a cosmetic problem: one export held nine branches and 14,264
// patients, and importing it under a single branch files every one of them at the wrong
// place, with the bills and rooms to match.

/** One row of the legacy `branch` table, plus how much data names it. Mirrors legacyimport.BranchInfo. */
export interface BranchSurveyRow {
  /** The legacy code the dump's own rows carry ("01".."08"). Not a V4 identity. */
  code: string;
  name: string;
  /** The company on the receipt. Branches under different companies are different clinics in V4. */
  legal_name: string;
  name_en: string;
  tax_id: string;
  license_no: string;
  address: string;
  phone: string;
  email: string;
  active: boolean;
  created_at: string;
  /** How much of the export points here. This is what tells an operator a branch is real. */
  patients: number;
  appointments: number;
  rooms: number;
  bills: number;
  receipts: number;
  courses_bought: number;
  stock_lots: number;
}

/** Branch codes grouped by the company that owns them. Mirrors legacyimport.LegalEntity. */
export interface LegalEntity {
  tax_id: string;
  name: string;
  /** Legacy branch codes belonging to this company. */
  branches: string[];
}

/** The CLI's `-survey-branches -json` output. Mirrors legacyimport.BranchSurvey. */
export interface BranchSurvey {
  branches: BranchSurveyRow[];
  legal_entities: LegalEntity[];
  /** Branch codes that appear in the DATA but not in the branch table — typos, usually few. */
  unknown_in_data?: Record<string, number>;
  patients_no_branch: number;
  appointments_no_branch: number;
  /** HNs found under more than one branch. Empty means the tenant splits cleanly. */
  cross_branch_hn?: string[];
  duplicate_doc_nos?: string[];
  duplicate_doc_no_count: number;
  branch_doc_nos_unique: boolean;
  bills_missing_doc_no: number;
  total_bills: number;
  total_patients: number;
  total_appointments: number;
  /** False when the dump has no `branch` table at all — nothing to map, single branch. */
  has_branch_table: boolean;
}

/**
 * POST /api/survey-branches
 *
 * `seconds` is reported because this runs inline rather than as a job: measured at 1.37 s
 * on a 48 MB whole-database dump, which leaves the 100 s proxy ceiling far away. If a
 * future dump ever gets close, that number is the warning.
 */
export type SurveyBranchesResponse =
  | { ok: true; survey: BranchSurvey; seconds: string; sqlBytes: number }
  | { ok: false; error: string };

/**
 * One branch as the UI asks for it. `source_code` is the legacy code and is NOT written
 * anywhere — it comes back paired with the branch_code V4 assigned, which is the only way
 * to tell the CLI which V4 branch a legacy row belongs to.
 */
export interface ProvisionBranchInput {
  source_code: string;
  name: string;
  name_en: string;
  address: string;
  phone: string;
  /** Exactly one true. It is also the default for rows whose branch cannot be resolved. */
  is_main: boolean;
}

/**
 * The legal header a receipt prints. One TIN per clinic, deliberately — a tenant whose
 * branches belong to different companies is several clinics in V4, one per company.
 *
 * Taken from the MAIN branch's row in the legacy branch table. `tax_id` is left empty on
 * purpose (decided 9 Sep 2026): the export's values are not reliable enough to print on a
 * tax invoice, and the backend writes the profile only when none is saved, so a human can
 * fill the number in afterwards without this ever overwriting it.
 */
export interface TaxProfileInput {
  legal_name: string;
  tax_id: string;
  tax_branch_type: string;
  tax_branch_code: string;
  tax_address: string;
  phone: string;
  email: string;
  website: string;
  receipt_footer: string;
  logo_url: string;
}

/** One branch as it now exists at the target. Mirrors admin.ProvisionBranchResult. */
export interface ProvisionedBranch {
  /** Echoed back from the request. This pairing IS the map the CLI needs. */
  source_code?: string;
  branch_code: string;
  name: string;
  /** "created" | "existed" */
  status: string;
  is_main: boolean;
}

/**
 * Turns the provision answer into the CLI's -branch-map argument: "01=BR00001,02=BR00002".
 *
 * A branch the backend answered without a source_code is skipped rather than guessed at:
 * an unlabelled pair cannot be attributed to a legacy code, and a wrong pairing files a
 * whole branch's patients under someone else.
 */
export function branchMapArg(branches: ProvisionedBranch[]): string {
  return branches
    .filter((b) => (b.source_code ?? "").trim() !== "" && b.branch_code.trim() !== "")
    .map((b) => `${b.source_code!.trim()}=${b.branch_code.trim()}`)
    .join(",");
}

// ── รูปหน้าเวชระเบียนจากระบบเก่า (mris.page → photos) ─────────────────────────
//
// Two machines are involved and the UI has to show both: the image BYTES go to the
// backend host's uploads dir, the ROWS go to the database host. A survey that named only
// one of them would let an operator confirm a write they cannot see the whole of.

/** A manifest entry with no file in the archive. It is reported and never imported. */
export interface PhotoMissing {
  hn: string;
  source_url: string;
  expected_path: string;
}

/** Shared by the survey and the apply response. */
interface PhotoImportBase {
  /** Where the rows go: "test · ubuntu@1.2.3.4 · drease_temporary (peer)". */
  target: string;
  /** Where the bytes go: "ubuntu@1.2.3.4:/var/www/.../storage/uploads". */
  uploads: string;
  isProduction: boolean;
  /** Images the CLI planned from the dump (latest revision of each page). */
  planned: number;
  /** Of those, how many were found in the archive. Only these can be imported. */
  matched: number;
  missing: PhotoMissing[];
  /** Files in the archive nothing asked for. Harmless; usually other modules' images. */
  extraCount: number;
  extraSample: string[];
  totalBytes: number;
  /** How many INSERT statements the script holds (one per matched image). */
  rows: number;
  /** The CLI's own stdout: counts, skipped revisions, warnings. */
  report: string;
}

export type PhotoImportResponse =
  | (PhotoImportBase & { ok: true; mode: "survey"; sql: string })
  | (PhotoImportBase & {
      ok: true;
      mode: "apply";
      filesSent: number;
      bytesSent: number;
      seconds: string;
      log: string;
    })
  | (Partial<PhotoImportBase> & {
      ok: false;
      error: string;
      missing?: PhotoMissing[];
      filesSent?: number;
      bytesSent?: number;
      seconds?: string;
      log?: string;
    });
