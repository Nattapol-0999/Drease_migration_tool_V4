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
    hint: "แคตตาล็อก: category → catalog_category · items → items + catalog_item(drug) + slot_stock · listheals → บริการ · listheal_medicine → สูตรวัสดุของบริการ · course_items → คอร์ส (+ ทะเบียน courses) · page → รูปหน้าเวชระเบียน (photos)",
  },
] as const;

export type CliInputField = (typeof CLI_INPUTS)[number]["field"];

/**
 * Files that are UPLOADED the same way as the CLI inputs but are not inputs to the
 * generate step.
 *
 * `images` is the photo archive (public_img.tar.gz). It is deliberately NOT a member of
 * CLI_INPUTS: that list drives the generate form's attachment slots AND the loop in
 * /api/generate that copies each staged file to the CLI's working directory as either
 * sql.sql or mongo.js. An entry there would put a 530 MB tarball through that loop as
 * `sql.sql` and hand it to the CLI as a MySQL dump.
 *
 * It still needs the chunked upload path, because a real clinic's archive is far past
 * anything a single request can carry: measured on the first multi-branch tenant, 437
 * photos came to 520 MB while /api/import-photos accepted 64 MB in one body. Sharing the
 * staging directory is what makes that size reachable — the ceiling there is
 * MAX_UPLOAD_BYTES (4 GB of disk), not a proxy's body limit.
 */
/*
 * `logo` and `profiles` are the two SMALL trees under the source's public/img, added 18 Sep
 * 2026. They are separate staged files rather than extra members inside `images` on purpose:
 * the CLI locates every page image by the exact prefix nas-doctorease/mris_images/page/… and
 * surveyArchive stats that tree whole, so widening that archive would put unrelated files in
 * front of code that is working in production. Kept apart, the photo path cannot see them.
 *
 * Both are optional. They only exist when the operator pulled from a server; tab 1 (import
 * from files) never produces them, and a source without the folders is not an error.
 *
 * Sizes are measured, not assumed: clinic-logo is 44 KB over 2 files and profile-image
 * 556 KB over 5 — together 0.08% of the 736 MB page archive, so pulling them costs nothing.
 */
export const STAGED_ONLY_FIELDS = ["images", "logo", "profiles"] as const;

export type StagedOnlyField = (typeof STAGED_ONLY_FIELDS)[number];

/** Every field the staging directory and /api/upload-chunk accept. */
export type StagedField = CliInputField | StagedOnlyField;

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
  // Point balances (loyalty_account). This list is an ALLOWLIST used to trim the mongo dump
  // before the CLI sees it, so a collection missing here is invisible to the CLI no matter
  // what it can parse — which is exactly how the loyalty group failed on its first run:
  // the CLI refused with 'ไม่มี collection "points"' while the collection sat in the
  // uploaded file, already discarded by mongo-trim.
  "points",
  // The movement history behind those balances. NOT imported (owner's decision, 15 Sep
  // 2026) — it is carried through only so the CLI can COUNT it and state how many rows were
  // left out. Dropping it here instead would make that warning read "0 documents", which
  // says the ledger did not exist rather than that it was deliberately skipped.
  "point_logs",
  // LINE pairings (internal/legacyimport/linebind.go). line_patients is the source; pair_logs
  // is read only when a box has no line_patients, by the owner's rule of 6 Oct 2026, and a
  // box with neither simply skips the group. Both have to be on this ALLOWLIST as well as on
  // the pull list (LEGACY_MONGO_COLLECTIONS): the linebind group shipped with neither, so the
  // pull never fetched line_patients — 10.0.133.87 holds 360 — and had it been fetched, this
  // trim would have discarded it before the CLI saw it. Either gate alone hides it.
  "line_patients",
  "pair_logs",
] as const;

// Groups the CLI can import separately (`-only`). Selecting a subset is how a target
// that already holds part of this data gets only the missing pieces — writing the rest
// would overwrite rows someone may have edited by hand.
export const IMPORT_GROUPS = [
  { group: "people", label: "บุคลากร", detail: "users · clinic_user · clinic_staff · doctor" },
  { group: "rooms", label: "ห้อง", detail: "room" },
  { group: "patients", label: "คนไข้", detail: "patient" },
  { group: "appointments", label: "นัด", detail: "appointment" },
  {
    group: "catalog",
    label: "แคตตาล็อก",
    detail: "catalog_category · บริการ + สูตรวัสดุ · items · ยา · คอร์ส (+ ทะเบียน courses)",
  },
  { group: "stock", label: "สต็อก (ล็อต)", detail: "slot_stock" },
  {
    group: "money",
    label: "ประวัติการขาย/รับเงิน",
    // All five tables move together on purpose: an invoice without its items is a bill
    // for nothing, and a receipt without its payment channels is money with no source.
    detail: "invoice · invoice_item · receipt · receipt_payment (ต้องมีคนไข้และแคตตาล็อกในระบบก่อน)",
  },
  {
    group: "loyalty",
    label: "แต้มสะสม",
    // The balance only. point_logs is NOT imported (owner's decision, 15 Sep 2026), so
    // loyalty_txn stays empty and no policy row is written — the points arrive but cannot
    // be redeemed until the clinic sets the rate on screen. Said here because a box
    // labelled "แต้มสะสม" would otherwise promise a working scheme.
    detail:
      "loyalty_account (ยอดแต้มต่อคนไข้ · ไม่รวมประวัติการได้/ใช้แต้ม และยังแลกไม่ได้จนตั้งอัตราในจอ)",
  },
  {
    group: "opdnote",
    label: "OPD note",
    // The Anjali rules (C1, 22 Oct 2026). The diagnosis box of V2's OPD page (patient_diag)
    // is NOT part of it, said here so the label does not promise the whole page.
    detail:
      "visit ตามวันจริง · OPD note → S/O/A/P + คำแนะนำ · สัญญาณชีพ (ต้องมีคนไข้ในระบบก่อน · ไม่รวมกล่องการวินิจฉัย)",
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
  kind: "generate" | "import" | "survey" | "pull";
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
   * import: the photo step that ran inside the same job, when an archive was attached.
   *
   * SEPARATE FROM `error` on purpose. The SQL commits before the images copy, so "data in,
   * images missing" is a real and recoverable state: the operator re-runs the photo step
   * alone. Reporting it through `error` would say the whole import failed, which would send
   * them to delete the tenant and start over for nothing.
   */
  photo?: {
    /** false when the SQL failed first, so the photos were never attempted. */
    ran: boolean;
    ok: boolean;
    message?: string;
    planned?: number;
    matched?: number;
    filesSent?: number;
    thumbsSent?: number;
    bytesSent?: number;
    rows?: number;
    seconds?: string;
    uploads?: string;
    log?: string;
    /**
     * The clinic logo and patient profile photos that rode along, when the session had them.
     * Counted separately from `filesSent` because those are medical-record images and these
     * are not: adding them into one total would make the number that matters unverifiable.
     */
    logoFound?: boolean;
    profilesSent?: number;
    profilesCandidates?: number;
    profilesSkipped?: number;
    /** Set when the extras failed. The import itself still succeeded — see photo-run.ts. */
    extrasError?: string;
  };
  /**
   * survey: the branch survey itself, carried in the job rather than written to a file.
   * It is a few kilobytes and the browser needs all of it, unlike a generated script.
   */
  survey?: BranchSurvey;
  sqlBytesIn?: number;
  mongoBytesIn?: number;
  /**
   * pull: what the finished pull produced — the staging session and what landed in it.
   * Present only when the job has finished successfully.
   */
  pull?: PullDone;
  /**
   * pull: bytes of the image archive on disk SO FAR, read from the file while it grows.
   *
   * There is no total to divide it by — the source streams a tar and never says how big it
   * will be — so this is a count, not a percentage. It exists because the alternative during
   * a 50-minute transfer is a modal that says the same sentence for an hour, which is
   * indistinguishable from being stuck.
   */
  pullBytes?: number;
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
  /*
   * NO base64 ANY MORE.
   *
   * This used to carry the dump itself, which meant the bytes were held in memory on the
   * server, base64'd (×1.33), stringified into JSON, sent to the browser, turned back into a
   * File and uploaded again into a second staging session. The same bytes crossed the wire
   * twice and existed in memory several times over.
   *
   * On 18 Sep 2026 that killed the process: a source with bills 43.6 MB + payment_summary
   * 30.5 MB in Mongo went past the 467 MB V8 heap on a 908 MB box, and the browser was handed
   * nginx's 502 HTML page, which surfaced as "Unexpected token '<' … is not valid JSON".
   *
   * The dumps now stream from the source straight into the staging session named by
   * PullOk.sessionId, and this record only describes them. Anything that needs the bytes reads
   * them from staging, which is where /api/generate has always read them from anyway.
   */
}

/**
 * PullOk is the reply to STARTING a pull, not its result.
 *
 * The pull became a job on 22 Sep 2026. It used to answer once, at the end, which could not
 * work: Cloudflare closes a proxied request after 100 seconds and a 15 GB image archive takes
 * 50 minutes at the measured 4.5 MB/s. The browser was handed the proxy's HTML error page
 * ("Unexpected token '<'"), the progress modal closed, and the transfer went on running with
 * nobody able to see it — and when it finished, the sessionId that names the files went
 * nowhere, so the only way to use them was to go onto the box by hand.
 *
 * Now every request is short and the browser polls /api/job, exactly as the generate, import
 * and survey steps already did. The result arrives as JobView.pull.
 */
export interface PullOk {
  ok: true;
  /** Poll /api/job?jobId=… with this. The result is JobView.pull when it finishes. */
  jobId: string;
}

/** Everything a finished pull produced. Carried on the job, never in the POST reply. */
export interface PullDone {
  host: string;
  /** SHA256 host key fingerprint of the box that answered. */
  fingerprint: string;
  /**
   * The staging session everything this pull produced now lives in: both dumps, the image
   * archive, the logo and the profile photos. Top level rather than inside `images`, because
   * the dumps are there even when the archive failed.
   */
  sessionId: string;
  files: PulledFileInfo[];
  /**
   * The page-image archive, when it was pulled too.
   *
   * It is NOT in `files`, and it has no base64: the measured archives are 736 MB and 1.12 GB,
   * so the bytes go from the legacy box straight into the staging directory on this server
   * and never enter the browser. What comes back is the session they landed in, which is the
   * same session /api/import-photos reads — that is what lets the photo step run without
   * anyone choosing a file.
   *
   * null when the archive could not be pulled. The reason is in `warnings`: the two dumps are
   * independently useful and are not thrown away because the images failed.
   */
  images: PulledImageInfo | null;
  /**
   * The clinic logo and the patient profile photos, when the source had them. Empty array is
   * a normal outcome, not a failure: these folders do not exist on every V2 box. Anything
   * that went wrong per tree is in `warnings`.
   */
  extras: PulledExtraInfo[];
  /** What ran on the far side and what came back. */
  log: string[];
  /** Did not stop the pull, but changes what the operator got. */
  warnings: string[];
}

/** The pulled image archive, described rather than carried. */
export interface PulledImageInfo {
  sessionId: string;
  name: string;
  bytes: number;
  /** The tar's member prefix, so a mismatch with what the CLI expects is visible. */
  member: string;
}

/**
 * The two small trees pulled alongside the page archive: the clinic logo and the patients'
 * profile photos. Reported separately from PulledImageInfo because either can be absent
 * without the pull having failed — a source with no such folder is normal, and saying "0 KB"
 * next to the archive that did arrive is how an operator sees which of the three came back.
 */
/**
 * What became of the clinic logo on the way to the uploads host.
 *
 * `available` is carried even on success: the source folder holds more than one candidate (two
 * were measured, 01.png and T-STAR-logo.png) and nothing in the V2 dump names which is the
 * clinic's, so the choice is configuration. Printing what was in the archive is what lets an
 * operator correct LEGACY_LOGO_FILE without going onto the source box to look.
 */
export interface ExtrasLogoResult {
  wanted: string;
  found: boolean;
  bytes: number;
  available: string[];
}

/**
 * What became of the patient profile photos.
 *
 * Four numbers rather than one, because "sent 3 of 5" has two different causes that need
 * different fixes: a file whose name is not an HN at all (bad export) and an HN with no patient
 * row (that patient was not imported). Collapsing them would hide which one happened.
 */
export interface ExtrasProfilesResult {
  candidates: number;
  sent: number;
  skippedNoPatient: number;
  skippedBadName: number;
  bytes: number;
  skippedSample: string[];
}

/** The result of copying the two decorative trees. No database rows are written for either. */
export interface ExtrasResult {
  logo: ExtrasLogoResult | null;
  profiles: ExtrasProfilesResult | null;
  filesSent: number;
  bytesSent: number;
  log: string;
}

export interface PulledExtraInfo {
  /** "logo" | "profiles" — the staged field it was written to. */
  field: StagedOnlyField;
  /** What was archived on the source, so a wrong LEGACY_* path is visible in the log. */
  member: string;
  bytes: number;
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

/**
 * What /api/vpn answers. The status fields are optional because a rejected REQUEST (a body
 * that is not "up" or "down") is refused before anything is read, so there is no status to
 * report at that point.
 *
 * `up` is the fact the operator needs and it comes from the tunnel interface existing, not
 * from the systemd unit being active — the unit is active as soon as the process starts,
 * which is before openvpn has authenticated.
 */
export interface VpnResponse {
  ok: boolean;
  error?: string;
  /** False when this host has no VPN plumbing installed at all. */
  configured?: boolean;
  up?: boolean;
  /** The network the tunnel routes, e.g. 10.0.128.0/20. */
  lan?: string;
  /** The root helper's own words, for the log panel. */
  detail?: string;
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
  /**
   * The legacy users.code values -owner would accept, in the CLI's order. Absent when the
   * dump has no users table. The form picks owner_username from it (src/lib/owner-pick.ts).
   */
  owner_candidates?: string[];
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
  /**
   * Images that ARE imported but that POST /v3/opd-record/:hn/photo would refuse today:
   * over 3 MB, or a format other than JPEG/PNG. Reported because an image a doctor already
   * attached belongs in the record and the read path serves it regardless — but somebody
   * will eventually ask why they cannot re-upload one like it.
   */
  wouldReject: PhotoWouldReject[];
  wouldRejectCount: number;
  oversizeCount: number;
  unsupportedCount: number;
  /** file_path ends .jpg while the bytes are PNG, or the reverse. Renders; header is wrong. */
  extMismatchCount: number;
  /**
   * How many matched images found V2's own thumbnail beside them in the archive. Those get
   * a thumb_path; the rest stay NULL and the app falls back to the full image.
   */
  thumbFound: number;
  thumbBytes: number;
  /** How many INSERT statements the script holds (one per matched image). */
  rows: number;
  /** The CLI's own stdout: counts, skipped revisions, warnings. */
  report: string;
  /**
   * What the file names were actually derived from — the clinic code unless the operator
   * overrode it for a clinic that was renamed after an earlier import. Echoed back rather
   * than assumed, because it is the value that decides whether this run recognises the
   * rows already at the target or imports a second copy of all of them.
   */
  photoNameSalt: string;
  /** True when that value is not the clinic code, so the screen can say so. */
  photoNameSaltOverridden: boolean;
}

/** One image the live upload endpoint would turn away, with the reason in Thai. */
export interface PhotoWouldReject {
  hn: string;
  target_path: string;
  bytes: number;
  /** Sniffed from the file's own first bytes, never from its name. */
  mime: string;
  reason: string;
}

export type PhotoImportResponse =
  | (PhotoImportBase & { ok: true; mode: "survey"; sql: string })
  | (PhotoImportBase & {
      ok: true;
      mode: "apply";
      filesSent: number;
      /** Of filesSent, how many were V2 thumbnails rather than full images. */
      thumbsSent: number;
      bytesSent: number;
      seconds: string;
      log: string;
      /**
       * The clinic logo and the patient profile photos, nested exactly as the run produced
       * them. The combined-import path flattens these into JobView.photo instead, because a
       * job record is polled and kept small; this endpoint spreads the run result whole.
       */
      extras?: ExtrasResult | null;
      extrasError?: string | null;
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

// ── ลบคลินิกออกจากปลายทาง (POST /api/clinic-wipe) ─────────────────────────────
//
// WHY THE UI IS TWO STEPS AND NOT ONE (owner's decision ②, 5 Oct 2026)
//
// A clinic code is seven characters of hex and nothing about it is readable. Measured on
// the seven clinics removed by hand this week, what sits behind one of those codes ranges
// from 8 rows (a shell whose import never ran) to 55,042 (C3D4A02, a tenant with its
// patients and catalogue already in). Typing a code and pressing delete cannot tell those
// apart, so the survey below is shown first and the delete button only exists after it:
// the operator confirms a clinic NAME and a row count, not a string they typed.

/** One base table holding rows for the clinic, and how many. */
export interface ClinicWipeTableCount {
  table: string;
  rows: number;
}

/**
 * A child table with no clinic_code of its own, reached through a foreign key.
 *
 * `via` spells the path out ("patient_consents.patient_id → patient.id") because these are
 * rows the operator cannot find by searching for the clinic code, and a count with no
 * explanation next to a delete button is not something anyone can check.
 */
export interface ClinicWipeOrphan {
  table: string;
  via: string;
  rows: number;
}

/** What a wipe would remove. Produced by a read-only pass over the target. */
export interface ClinicWipeSurvey {
  /** False when no clinic identity row carries this code — "nothing to delete". */
  found: boolean;
  clinicCode: string;
  engName: string;
  thaName: string;
  createdAt: string;
  /** The branch rows under this clinic (clinic.code <> clinic.clinic_code). */
  branches: { code: string; branchCode: string; thaName: string }[];
  tables: ClinicWipeTableCount[];
  orphans: ClinicWipeOrphan[];
  /** Rows that would go. Excludes biz_event, which never does. */
  totalRows: number;
  baseTablesScanned: number;
  /** Money on this clinic's receipts, as digits. A wipe removes the records of it. */
  receiptMoney: string;
  receipts: number;
  /** Logins that will stop working the moment the rows go. */
  liveSessions: number;
  /** Append-only audit rows that are deliberately LEFT BEHIND. */
  bizEventRows: number;
}

/** POST /api/clinic-wipe with action "survey". */
export type ClinicWipeSurveyResponse =
  | { ok: true; target: string; isProduction: boolean; survey: ClinicWipeSurvey; seconds: string }
  | { ok: false; error: string };

/** POST /api/clinic-wipe with action "wipe". */
export type ClinicWipeRunResponse =
  | {
      ok: true;
      target: string;
      isProduction: boolean;
      /** What the clinic looked like immediately before the delete. */
      before: ClinicWipeSurvey;
      /** Confirmed after the commit, from a fresh survey: every count must be zero. */
      after: ClinicWipeSurvey;
      deletedRows: number;
      bizEventKept: number;
      rehearsal: string;
      applied: string;
      seconds: string;
    }
  | {
      ok: false;
      error: string;
      /** Which run failed. "rehearsal" means nothing was changed. */
      stage?: "rehearsal" | "commit";
      rehearsal?: string;
    };
