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
    label: "MongoDB (.js)",
    hint: "แคตตาล็อก: category → catalog_category · items → items + catalog_item(drug) + slot_stock · listheals → บริการ · course_items → คอร์ส",
  },
] as const;

export type CliInputField = (typeof CLI_INPUTS)[number]["field"];

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

export interface GenerateOk {
  ok: true;
  /** Suggested download name, e.g. import_C9D4865.sql */
  filename: string;
  /** The generated SQL script, verbatim. */
  sql: string;
  /** The CLI's own stdout report: what will be written, warnings, notes. */
  report: string;
}

export interface GenerateErr {
  ok: false;
  /** Why generation failed — the CLI fails closed and explains itself on stderr. */
  error: string;
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
