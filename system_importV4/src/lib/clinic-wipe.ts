import "server-only";

import {
  hint,
  psqlCommand,
  runSSH,
  sshArgs,
  type ServerTarget,
  type SSHResult,
} from "@/lib/server-target";
import type { ClinicWipeSurvey } from "@/lib/types";

/**
 * Removing a clinic from a target database, completely.
 *
 * WHY THIS EXISTS
 *
 * A failed import leaves the tenant half-written. The generated script commits once per
 * ENTITY (see server-target.psqlCommand), so a refusal part way through leaves every group
 * before it in the database, and the only way forward is to delete the clinic and run the
 * whole thing again. Between 1 and 5 Oct 2026 that was done by hand seven times â€”
 * C57779D, CD8DAF4, C3D4A02, CBE8806, C9B7272, C9A4FBB, CAAF382 â€” each time by writing a
 * one-off shell script against the box. This is that procedure, with the four things those
 * runs taught, as a button.
 *
 * WHAT THOSE SEVEN RUNS TAUGHT, AND WHAT IS DONE ABOUT IT HERE
 *
 *  1. A scan over information_schema.columns alone INCLUDES VIEWS. catalog_public_item
 *     selects on clinic_code, so it was reported as holding rows it does not own, and
 *     DELETE against it failed with "cannot delete from view". Every scan below joins
 *     information_schema.tables and takes table_type = 'BASE TABLE'.
 *
 *  2. Triggers fight the delete. trg_privacy_hn_ownership_immutable raises on DELETE from
 *     privacy_patient_hn_ownership, trg_patient_privacy_hn_lock re-claims every HN, and the
 *     recall_capture_* triggers write a FRESH recall_change_outbox row for every row
 *     removed â€” measured on C3D4A02: clearing 29,732 outbox rows first left 29,736 to
 *     clear afterwards. The hand-written scripts dealt with this by ordering the deletes
 *     carefully and switching session_replication_role for two statements. That ordering was
 *     per-clinic and cannot be generalised, so here the whole transaction runs in
 *     'replica' and the order stops mattering at all.
 *
 *  3. Which means biz_event has to be excluded IN CODE. It is append-only audit the owner
 *     decided never to delete, and trg_biz_event_immutable is what normally enforces that â€”
 *     but 'replica' turns that trigger off too. The exclusion below is the only thing
 *     standing between this and an erased audit trail.
 *
 *  4. Some child tables carry no clinic_code and would be orphaned: measured on C3D4A02,
 *     catalog_item_image, patient_first_visit, patient_visit_fact and patient_consents.
 *     They are NOT hardcoded here â€” the list is derived from foreign keys at run time, so a
 *     table added to V4 later is covered without anyone remembering to come back.
 *
 * REHEARSE, THEN COMMIT (owner's decision â‘£, 5 Oct 2026)
 *
 * The same SQL is run twice: once ending in ROLLBACK, and only if that succeeds again
 * ending in COMMIT. It is the rehearsal that caught both the view and the outbox refill
 * above, on a real clinic, before anything was lost.
 *
 * NO BACKUP (owner's decision â‘¢, 5 Oct 2026)
 *
 * The hand runs each archived the rows to CSV first. This does not, because the case it
 * exists for is "delete it, I am importing it again" â€” the data is about to be replaced by
 * a fresh run of the same dump. Anyone who needs a copy takes it before pressing the
 * button.
 */

/** The audit table that is never deleted. See point 3 above. */
const AUDIT_TABLE_KEPT = "biz_event";

/**
 * How psql is asked for machine-readable output.
 *
 * psqlCommand fixes `-f -`, so the formatting is set with meta-commands inside the script
 * rather than with flags. Unaligned + tuples-only is one JSON document on stdout and
 * nothing else.
 */
const PSQL_PLAIN_OUTPUT = `\\pset tuples_only on\n\\pset format unaligned\n\\pset pager off\n`;

/**
 * sqlLiteral quotes a value for SQL. Used for the clinic code, which arrives from a
 * request â€” `format('%L')` inside plpgsql handles it server-side, but the code also
 * appears in plain statements, and one unquoted interpolation is all an injection needs.
 */
function sqlLiteral(v: string): string {
  return `'` + v.replaceAll(`'`, `''`) + `'`;
}

/**
 * CLINIC_CODE_RE is what a clinic code may look like.
 *
 * Checked before the value is used anywhere, including inside a quoted literal: belt and
 * braces, because this module builds SQL that deletes without a WHERE anyone reviews.
 */
export const CLINIC_CODE_RE = /^[A-Za-z0-9_-]{1,64}$/;

/**
 * The shared discovery block: which base tables name a clinic, and which children do not.
 *
 * Clinic-independent on purpose â€” it describes the SCHEMA, not one tenant â€” so the survey
 * and the wipe discover the same set and cannot disagree about what exists.
 */
const DISCOVERY_SQL = `
-- Every BASE TABLE carrying clinic_code. Views are excluded by table_type: a view that
-- selects on clinic_code reports rows it does not own and cannot be deleted from.
CREATE TEMP TABLE _wipe_tables AS
SELECT c.table_name::text AS t
  FROM information_schema.columns c
  JOIN information_schema.tables v
    ON v.table_schema = c.table_schema AND v.table_name = c.table_name
 WHERE c.column_name = 'clinic_code'
   AND c.table_schema = 'public'
   AND v.table_type = 'BASE TABLE';

-- Children that do NOT carry clinic_code but hang off a table that does, found through
-- their foreign keys rather than from a list someone has to maintain.
CREATE TEMP TABLE _wipe_children AS
SELECT ch.relname::text   AS child,
       att.attname::text  AS child_col,
       pa.relname::text   AS parent,
       patt.attname::text AS parent_col
  FROM pg_constraint k
  JOIN pg_class ch   ON ch.oid = k.conrelid
  JOIN pg_class pa   ON pa.oid = k.confrelid
  JOIN pg_namespace n ON n.oid = ch.relnamespace
  JOIN unnest(k.conkey)  WITH ORDINALITY AS ck(attnum, ord)  ON true
  JOIN unnest(k.confkey) WITH ORDINALITY AS pk(attnum, ord2) ON pk.ord2 = ck.ord
  JOIN pg_attribute att  ON att.attrelid = ch.oid AND att.attnum = ck.attnum
  JOIN pg_attribute patt ON patt.attrelid = pa.oid AND patt.attnum = pk.attnum
 WHERE k.contype = 'f'
   AND n.nspname = 'public'
   AND ch.relkind = 'r'
   AND pa.relkind = 'r'
   -- single-column keys only: a composite foreign key cannot be matched by one EXISTS,
   -- and every parent link in this schema is a single id.
   AND array_length(k.conkey, 1) = 1
   -- the child must not name a clinic itself, or the main loop already covers it
   AND ch.relname NOT IN (SELECT t FROM _wipe_tables)
   -- the parent must, or there is nothing to attribute the child to
   AND pa.relname IN (SELECT t FROM _wipe_tables);
`;

/**
 * buildSurveySQL asks what a wipe would remove, and changes nothing.
 *
 * Answered as one JSON document so the route parses one thing. Counting per table needs
 * dynamic SQL, which cannot return a value from a DO block, so the result is assembled
 * into a temp table and selected at the end.
 */
export function buildSurveySQL(code: string): string {
  const lit = sqlLiteral(code);
  return `${PSQL_PLAIN_OUTPUT}
${DISCOVERY_SQL}
CREATE TEMP TABLE _wipe_survey(payload jsonb);

DO $survey$
DECLARE
  r record;
  n bigint;
  hit int := 0;
  scanned int := 0;
  total bigint := 0;
  tabs jsonb := '[]'::jsonb;
  orphans jsonb := '[]'::jsonb;
  branches jsonb := '[]'::jsonb;
  eng text := '';
  tha text := '';
  made text := '';
  money numeric := 0;
  receipts bigint := 0;
  sessions bigint := 0;
  audit bigint := 0;
BEGIN
  -- Counted, not read INTO a boolean.
  --
  -- This was a bug: SELECT true, ... INTO found, ... leaves every target NULL when no
  -- row matches, and IF NOT found on a NULL is NULL, which is not true, so the early
  -- return never ran and a code that does not exist came back as found with no name and
  -- no rows. Measured against the deployed route on 6 Oct 2026: survey ZZZZZZZ answered
  -- found:true. count(*) cannot be NULL.
  SELECT count(*) INTO hit FROM clinic WHERE clinic_code = ${lit} AND code = ${lit};
  IF hit <> 1 THEN
    -- A code with no identity row is not an error: it is the answer "nothing to delete",
    -- and the UI must be able to say so instead of offering a button.
    INSERT INTO _wipe_survey VALUES (jsonb_build_object('found', false, 'clinicCode', ${lit}));
    RETURN;
  END IF;

  SELECT coalesce(eng_name,''), coalesce(tha_name,''), created_at::text
    INTO eng, tha, made
    FROM clinic WHERE clinic_code = ${lit} AND code = ${lit};

  SELECT coalesce(jsonb_agg(jsonb_build_object(
           'code', code, 'branchCode', coalesce(branch_code,''), 'thaName', coalesce(tha_name,'')
         ) ORDER BY code), '[]'::jsonb)
    INTO branches
    FROM clinic WHERE clinic_code = ${lit} AND code <> ${lit};

  FOR r IN SELECT t FROM _wipe_tables ORDER BY t LOOP
    scanned := scanned + 1;
    EXECUTE format('SELECT count(*) FROM public.%I WHERE clinic_code = %L', r.t, ${lit}) INTO n;
    IF n = 0 THEN CONTINUE; END IF;
    IF r.t = ${sqlLiteral(AUDIT_TABLE_KEPT)} THEN
      audit := n;
      CONTINUE;  -- counted, never deleted, and kept out of the total that will go
    END IF;
    total := total + n;
    tabs := tabs || jsonb_build_object('table', r.t, 'rows', n);
  END LOOP;

  FOR r IN SELECT child, child_col, parent, parent_col FROM _wipe_children
            ORDER BY child, child_col LOOP
    EXECUTE format(
      'SELECT count(*) FROM public.%I c WHERE EXISTS (SELECT 1 FROM public.%I p WHERE p.%I = c.%I AND p.clinic_code = %L)',
      r.child, r.parent, r.parent_col, r.child_col, ${lit}) INTO n;
    IF n = 0 THEN CONTINUE; END IF;
    total := total + n;
    orphans := orphans || jsonb_build_object(
      'table', r.child, 'via', format('%s.%s â†’ %s.%s', r.child, r.child_col, r.parent, r.parent_col), 'rows', n);
  END LOOP;

  SELECT coalesce(sum(paid_amount), 0), count(*) INTO money, receipts
    FROM receipt WHERE clinic_code = ${lit};
  SELECT count(*) INTO sessions FROM refresh_tokens WHERE clinic_code = ${lit};

  INSERT INTO _wipe_survey VALUES (jsonb_build_object(
    'found', true,
    'clinicCode', ${lit},
    'engName', eng,
    'thaName', tha,
    'createdAt', made,
    'branches', branches,
    'tables', tabs,
    'orphans', orphans,
    'totalRows', total,
    'baseTablesScanned', scanned,
    'receiptMoney', money::text,
    'receipts', receipts,
    'liveSessions', sessions,
    'bizEventRows', audit
  ));
END
$survey$;

SELECT payload::text FROM _wipe_survey;
`;
}

/**
 * buildWipeSQL removes the clinic. The caller appends ROLLBACK or COMMIT.
 *
 * Deliberately NOT ending itself: the rehearsal and the real run must be the same text,
 * because a rehearsal of something slightly different proves nothing.
 */
export function buildWipeSQL(code: string): string {
  const lit = sqlLiteral(code);
  return `${PSQL_PLAIN_OUTPUT}
BEGIN;

-- Triggers and foreign keys OFF for this transaction only.
--
-- This is what makes one procedure work for every clinic. With them on, the deletes have
-- to be ordered by hand against the FK graph, privacy_patient_hn_ownership refuses to be
-- deleted from at all, and the recall_capture_* triggers refill recall_change_outbox as
-- fast as it is emptied. SET LOCAL ends with the transaction, committed or not.
SET LOCAL session_replication_role = 'replica';

${DISCOVERY_SQL}
CREATE TEMP TABLE _wipe_done(t text, rows bigint);

DO $guard$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n FROM clinic WHERE clinic_code = ${lit} AND code = ${lit};
  IF n <> 1 THEN
    RAISE EXCEPTION 'à¹„à¸¡à¹ˆà¸žà¸šà¸„à¸¥à¸´à¸™à¸´à¸ % (clinic.clinic_code = clinic.code) â€” à¹„à¸¡à¹ˆà¸¥à¸šà¸­à¸°à¹„à¸£à¹€à¸¥à¸¢', ${lit};
  END IF;
END
$guard$;

DO $wipe$
DECLARE r record; n bigint;
BEGIN
  -- Children first only as a courtesy to anyone reading the log; with replica role the
  -- order cannot matter.
  FOR r IN SELECT child, child_col, parent, parent_col FROM _wipe_children
            ORDER BY child, child_col LOOP
    EXECUTE format(
      'DELETE FROM public.%I c WHERE EXISTS (SELECT 1 FROM public.%I p WHERE p.%I = c.%I AND p.clinic_code = %L)',
      r.child, r.parent, r.parent_col, r.child_col, ${lit});
    GET DIAGNOSTICS n = ROW_COUNT;
    IF n > 0 THEN INSERT INTO _wipe_done VALUES (r.child || ' (à¸œà¹ˆà¸²à¸™ ' || r.parent || ')', n); END IF;
  END LOOP;

  FOR r IN SELECT t FROM _wipe_tables ORDER BY t LOOP
    -- The audit trail is append-only by the owner's decision. trg_biz_event_immutable
    -- would normally stop this, but replica role has that trigger switched off â€” so this
    -- line is the enforcement, not a convenience.
    IF r.t = ${sqlLiteral(AUDIT_TABLE_KEPT)} THEN CONTINUE; END IF;
    EXECUTE format('DELETE FROM public.%I WHERE clinic_code = %L', r.t, ${lit});
    GET DIAGNOSTICS n = ROW_COUNT;
    IF n > 0 THEN INSERT INTO _wipe_done VALUES (r.t, n); END IF;
  END LOOP;
END
$wipe$;

-- Nothing may be left anywhere, except the audit rows that were never touched.
DO $verify$
DECLARE r record; n bigint; bad int := 0; audit bigint := 0;
BEGIN
  FOR r IN SELECT t FROM _wipe_tables ORDER BY t LOOP
    EXECUTE format('SELECT count(*) FROM public.%I WHERE clinic_code = %L', r.t, ${lit}) INTO n;
    IF n = 0 THEN CONTINUE; END IF;
    IF r.t = ${sqlLiteral(AUDIT_TABLE_KEPT)} THEN audit := n; CONTINUE; END IF;
    RAISE WARNING 'à¸¢à¸±à¸‡à¹€à¸«à¸¥à¸·à¸­à¸­à¸¢à¸¹à¹ˆ: % = %', r.t, n;
    bad := bad + 1;
  END LOOP;
  FOR r IN SELECT child, child_col, parent, parent_col FROM _wipe_children LOOP
    EXECUTE format(
      'SELECT count(*) FROM public.%I c WHERE EXISTS (SELECT 1 FROM public.%I p WHERE p.%I = c.%I AND p.clinic_code = %L)',
      r.child, r.parent, r.parent_col, r.child_col, ${lit}) INTO n;
    IF n > 0 THEN RAISE WARNING 'à¸¢à¸±à¸‡à¹€à¸«à¸¥à¸·à¸­à¸­à¸¢à¸¹à¹ˆ (à¸¥à¸¹à¸): % = %', r.child, n; bad := bad + 1; END IF;
  END LOOP;
  IF bad <> 0 THEN
    RAISE EXCEPTION 'à¸¥à¸šà¹„à¸¡à¹ˆà¸«à¸¡à¸”: à¸¢à¸±à¸‡à¸¡à¸µà¸‚à¹‰à¸­à¸¡à¸¹à¸¥à¸‚à¸­à¸‡ % à¸­à¸¢à¸¹à¹ˆà¹ƒà¸™ % à¸•à¸²à¸£à¸²à¸‡ â€” à¸›à¸à¸´à¹€à¸ªà¸˜à¸—à¸±à¹‰à¸‡à¸£à¸²à¸¢à¸à¸²à¸£', ${lit}, bad;
  END IF;
  RAISE NOTICE 'WIPE_OK biz_event_kept=%', audit;
END
$verify$;

SELECT 'WIPE_ROW ' || t || ' ' || rows FROM _wipe_done ORDER BY t;
`;
}

/** runSQL sends one script to a target over the same ssh+psql path an import uses. */
async function runSQL(target: ServerTarget, sql: string): Promise<SSHResult> {
  return runSSH(sshArgs(target, psqlCommand(target)), sql);
}

export interface SurveyOutcome {
  survey: ClinicWipeSurvey;
  log: string;
}

/**
 * surveyClinic reads what a wipe would remove.
 *
 * Read only: the script creates temp tables and selects. Nothing it does survives the
 * session.
 */
export async function surveyClinic(
  target: ServerTarget,
  code: string,
): Promise<SurveyOutcome> {
  const r = await runSQL(target, buildSurveySQL(code));
  if (r.code !== 0) {
    throw new Error(`${hint(r)}\n${tail(r)}`.trim());
  }
  const payload = r.stdout
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l.startsWith("{"))
    .pop();
  if (!payload) {
    throw new Error(`à¸­à¹ˆà¸²à¸™à¸œà¸¥à¸ªà¸³à¸£à¸§à¸ˆà¹„à¸¡à¹ˆà¹„à¸”à¹‰ â€” psql à¹„à¸¡à¹ˆà¹„à¸”à¹‰à¸„à¸·à¸™ JSON à¸à¸¥à¸±à¸šà¸¡à¸²\n${tail(r)}`.trim());
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(payload);
  } catch {
    throw new Error(`à¸œà¸¥à¸ªà¸³à¸£à¸§à¸ˆà¹„à¸¡à¹ˆà¹ƒà¸Šà¹ˆ JSON à¸—à¸µà¹ˆà¸­à¹ˆà¸²à¸™à¹„à¸”à¹‰\n${payload.slice(0, 400)}`);
  }
  return { survey: normaliseSurvey(parsed, code), log: tail(r) };
}

export interface WipeOutcome {
  /** psql's own words from the ROLLBACK run, kept whether or not it passed. */
  rehearsal: string;
  /** psql's own words from the COMMIT run. */
  applied: string;
  /** Rows removed per table, as the transaction counted them. */
  deletedRows: number;
  bizEventKept: number;
}

/**
 * wipeClinic rehearses the delete, then does it.
 *
 * Two runs of the SAME text. If the rehearsal fails, nothing is attempted â€” and because
 * it ends in ROLLBACK, a failure there has changed nothing either.
 */
export async function wipeClinic(
  target: ServerTarget,
  code: string,
): Promise<WipeOutcome> {
  const body = buildWipeSQL(code);

  const dry = await runSQL(target, `${body}\nROLLBACK;\n`);
  if (dry.code !== 0) {
    const err = new Error(`à¸‹à¹‰à¸­à¸¡à¹à¸«à¹‰à¸‡à¹„à¸¡à¹ˆà¸œà¹ˆà¸²à¸™ à¸ˆà¸¶à¸‡à¹„à¸¡à¹ˆà¸¥à¸šà¸­à¸°à¹„à¸£à¹€à¸¥à¸¢\n${hint(dry)}\n${tail(dry)}`.trim());
    (err as Error & { stage?: string; rehearsal?: string }).stage = "rehearsal";
    (err as Error & { stage?: string; rehearsal?: string }).rehearsal = tail(dry);
    throw err;
  }

  const real = await runSQL(target, `${body}\nCOMMIT;\n`);
  if (real.code !== 0) {
    const err = new Error(`à¸‹à¹‰à¸­à¸¡à¹à¸«à¹‰à¸‡à¸œà¹ˆà¸²à¸™à¹à¸•à¹ˆà¸£à¸­à¸šà¸ˆà¸£à¸´à¸‡à¸¥à¹‰à¸¡\n${hint(real)}\n${tail(real)}`.trim());
    (err as Error & { stage?: string; rehearsal?: string }).stage = "commit";
    (err as Error & { stage?: string; rehearsal?: string }).rehearsal = tail(dry);
    throw err;
  }

  return {
    rehearsal: tail(dry),
    applied: tail(real),
    deletedRows: countDeleted(real.stdout),
    bizEventKept: readAuditKept(`${real.stdout}\n${real.stderr}`),
  };
}

/** countDeleted adds up the WIPE_ROW lines the transaction printed. */
function countDeleted(stdout: string): number {
  let total = 0;
  for (const line of stdout.split("\n")) {
    const m = /^WIPE_ROW\s+.+\s+(\d+)\s*$/.exec(line.trim());
    if (m) total += Number(m[1]);
  }
  return total;
}

/** readAuditKept pulls the biz_event count out of the verifier's NOTICE. */
function readAuditKept(text: string): number {
  const m = /WIPE_OK biz_event_kept=(\d+)/.exec(text);
  return m ? Number(m[1]) : 0;
}

/** How much of psql's output is put in front of the operator. */
const LOG_TAIL_CHARS = 2000;

function tail(r: SSHResult): string {
  const text = `${r.stdout}\n${r.stderr}`.trim();
  return text.length > LOG_TAIL_CHARS ? `â€¦\n${text.slice(-LOG_TAIL_CHARS)}` : text;
}

/**
 * normaliseSurvey turns the database's JSON into the shape the UI is typed against.
 *
 * Every field is defaulted rather than trusted: the payload crosses a shell, ssh and psql
 * before it gets here, and a missing key must read as zero instead of rendering
 * "undefined rows" next to a delete button.
 */
function normaliseSurvey(raw: unknown, code: string): ClinicWipeSurvey {
  const o = (raw ?? {}) as Record<string, unknown>;
  const str = (v: unknown, d = ""): string => (typeof v === "string" ? v : d);
  const num = (v: unknown): number => {
    const n = typeof v === "number" ? v : Number(v);
    return Number.isFinite(n) ? n : 0;
  };
  const arr = (v: unknown): unknown[] => (Array.isArray(v) ? v : []);

  return {
    found: o.found === true,
    clinicCode: str(o.clinicCode, code),
    engName: str(o.engName),
    thaName: str(o.thaName),
    createdAt: str(o.createdAt),
    branches: arr(o.branches).map((b) => {
      const x = (b ?? {}) as Record<string, unknown>;
      return {
        code: str(x.code),
        branchCode: str(x.branchCode),
        thaName: str(x.thaName),
      };
    }),
    tables: arr(o.tables).map((t) => {
      const x = (t ?? {}) as Record<string, unknown>;
      return { table: str(x.table), rows: num(x.rows) };
    }),
    orphans: arr(o.orphans).map((t) => {
      const x = (t ?? {}) as Record<string, unknown>;
      return { table: str(x.table), via: str(x.via), rows: num(x.rows) };
    }),
    totalRows: num(o.totalRows),
    baseTablesScanned: num(o.baseTablesScanned),
    receiptMoney: str(o.receiptMoney, "0"),
    receipts: num(o.receipts),
    liveSessions: num(o.liveSessions),
    bizEventRows: num(o.bizEventRows),
  };
}
