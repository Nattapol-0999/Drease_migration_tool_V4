package legacyimport

import (
	"fmt"
	"sort"
	"strings"

	"system_cliv4/internal/legacydump"
)

// The safety rules that cannot be decided from the dump alone are emitted as SQL
// guards at the top of the generated script, inside the same transaction as the data.
// A guard that fails raises an exception, so the whole import rolls back — the same
// fail-closed outcome the API version reached in Go, expressed where this tool can
// still enforce it: in the script itself.

// buildClinicExistsGuard refuses to import into a clinic that does not exist.
//
// This is not a formality. `patient` has NO foreign key to `clinic`, so a mistyped
// clinic_code imports silently and the rows are then invisible to every tenant — the
// worst kind of failure, because it looks like success. The clinic is created through
// the normal V4 admin flow (which also gives it its subscription, settings, rules and
// counters); this tool only verifies the result.
/*
patientsPerTransaction is how many patient rows share one transaction.

WHY THE PATIENT GROUP IS THE ONLY ONE BATCHED BY ROW COUNT

`trg_patient_privacy_hn_lock` fires per row and takes a TRANSACTION-scoped advisory lock per
HN it is claiming — the current one, and old_hn when the patient has one (migration 0404,
privacy_lock_patient_hn). Advisory locks live in the same shared lock table as relation locks
and are only released at COMMIT, so they accumulate for the whole transaction.

The ratio is 2 locks per patient in the worst case and it is DATA-DEPENDENT, which is the part
that caught us out. First measurement, on agafar-clinic: 6,439 patients, 1,445 with an old_hn,
7,884 ownership rows — 1.22 per patient, and 2,000 rows was sized against that. Then doctortony
imported at exactly 2.00 (14,899 patients, 29,794 ownership rows) because every one of its
patients carries an old_hn. That put a 2,000-row batch at 4,000 advisory locks plus the group's
relation locks, against a 4,096 budget: it completed with about 1% to spare.

1,000 rows is therefore the size, not 2,000 — 2,000 locks at the worst-case ratio, half the
budget, and the margin no longer depends on how the clinic happened to renumber its patients.

This is the one thing row-count batching fixes. Splitting by rows does nothing for RELATION
locks — 500 patients lock exactly the same tables as 14,899 — which is why the group-level
split in script.go was not enough on its own.
*/
const patientsPerTransaction = 1000

/*
patientBatchBoundary closes one patient batch and opens the next.

It says which rows it is about, because a failure part way through 14,899 patients now leaves
the earlier batches committed, and "which ones made it" is the first question anyone asks.
*/
func patientBatchBoundary(done, total int, clinicCode string) string {
	return fmt.Sprintf(
		"-- ── ปิดชุดที่ %d แถวแรกของคนไข้ (จาก %d) แล้วเปิดทรานแซกชันใหม่ ─────────────\n"+
			"-- trigger ของ patient จับ advisory lock ต่อ HN ทุกแถว และปล่อยเมื่อ COMMIT เท่านั้น\n"+
			"-- ชุดละ %d แถว ≈ %d ล็อก ซึ่งอยู่ใต้เพดาน max_locks_per_transaction\n"+
			"COMMIT;\n\nBEGIN;\n%s",
		done, total, patientsPerTransaction, patientsPerTransaction*122/100, sessionSettings(clinicCode))
}

/*
sessionSettings is the pair of settings every transaction in the script has to open with.

Both are SET LOCAL, which means they last exactly as long as the transaction that issued
them. The script now commits at every group boundary (see script.go), so they are emitted
once per transaction rather than once per file.

standard_conforming_strings keeps backslashes in legacy text literal. On by default since
PostgreSQL 9.1, stated anyway so a server configured otherwise cannot quietly mangle data.

app.clinic_code is the GUC every FORCE-RLS policy reads. It is harmless when the script runs
as the table owner and REQUIRED when it runs as the RLS-bound application role — and the
failure it prevents is the silent kind: with the GUC unset the policies match nothing, so
every INSERT writes zero rows and psql still reports success.
*/
func sessionSettings(clinicCode string) string {
	return "SET LOCAL standard_conforming_strings = on;\n" +
		fmt.Sprintf("SET LOCAL app.clinic_code = %s;\n\n", lit(clinicCode))
}

func buildClinicExistsGuard(clinic string) string {
	return fmt.Sprintf(`-- GUARD: the clinic must already exist in V4.
-- patient/appointment have no FK to clinic, so a mistyped code would import rows that
-- no tenant can ever see. Create the clinic through the admin flow first.
DO $guard$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM clinic WHERE clinic_code = %s) THEN
        RAISE EXCEPTION 'clinic_code %% ไม่มีในระบบ — สร้างคลินิกในระบบก่อนแล้วรันสคริปต์นี้อีกครั้ง', %s;
    END IF;
END $guard$;

`, lit(clinic), lit(clinic))
}

// buildUsersCollisionGuard refuses the import when a login name this run would write still
// belongs to a DIFFERENT clinic after the collision rule has been applied.
//
// WHAT IT NO LONGER CHECKS
// The legacy `id` is not checked any more, because it is not written any more — V4 issues
// every users.id and import_user_map translates. That check used to refuse most runs against
// a shared database for no real reason: 36 of one export's 112 accounts landed on ids other
// clinics happened to reach first, none of which said anything about the accounts themselves.
//
// WHAT IT STILL CHECKS, AND WHY THE CONDITION IS A PAIR AND NOT AN "OR"
// email is UNIQUE across the whole table, so a name another tenant holds cannot be written.
// buildUserMap already answers that by renaming to <name>.<clinic slug>@gmail.com, and by
// walking up .2 … .9 if that form is taken too — so the bare name being taken is NORMAL: it is
// the trigger for the rename, not a failure.
//
// An earlier version listed both forms and refused if EITHER was taken. That refused exactly
// the runs the fallback exists to allow: measured against the shared test database, `admin`
// and `beer` are held by another clinic, so the whole 112-account import was rejected before
// writing anything even though both had a free suffixed form waiting.
//
// The only unwritable case is when BOTH forms are taken by other clinics. Then
// ON CONFLICT (email) DO NOTHING skips the account, new_id stays NULL, and the fill guard
// refuses — correctly, but after doing all the work. This guard says the same thing up front,
// naming the accounts, before a single row is written.
//
// A collision with the SAME clinic is a re-run and is left to the insert-only statements.
func buildUsersCollisionGuard(rows []legacydump.Row, in Input) string {
	clinic := in.ClinicCode
	// Resolved from the same Input, the same way, as buildUserMap resolves it — so this guard
	// can never ask about a form the map would not have tried. The reason for a fallback is
	// dropped here on purpose: buildUserMap reports it once, and repeating it would read like
	// two separate problems.
	suffix, _ := loginSuffix(in)

	// Rungs, not a flat set: which form a row ends up with depends on which ones above it are
	// free, so the guard has to carry the whole ladder for each account.
	type rungs struct{ bare, suffixed, prefix string }
	seen := map[string]bool{}
	list := make([]rungs, 0, len(rows))
	for _, r := range rows {
		e := strings.ToLower(strings.TrimSpace(r.Get("email")))
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		s, _ := suffixedEmail(e, suffix)
		s = strings.ToLower(s)
		list = append(list, rungs{bare: e, suffixed: s, prefix: strings.TrimSuffix(s, "@gmail.com")})
	}
	if len(list) == 0 {
		return ""
	}

	values := make([]string, 0, len(list))
	for _, p := range list {
		values = append(values, fmt.Sprintf("        (%s, %s, %s)", lit(p.bare), lit(p.suffixed), lit(p.prefix)))
	}

	return fmt.Sprintf(`-- GUARD: refuse only when an account has NOWHERE to land.
-- users.email is UNIQUE across the whole table, not per clinic. A name another tenant holds
-- is handled by renaming to <name>.%s@gmail.com, and if that form is taken as well by walking
-- up .2 … .%d — so the bare name being taken is the NORMAL case, not an error. What cannot be
-- handled is EVERY rung being taken: the insert would be skipped and the account would never
-- exist. That is what this refuses, by name, before anything is written.
-- users.id is deliberately NOT checked: V4 issues every id now, so a legacy number colliding
-- says nothing about the account.
-- A row belonging to THIS clinic is a re-import, not a clash: the ladder ignores it, lands on
-- the same login as last time, and the insert-only statements skip it.
DO $guard$
DECLARE
    clash text;
BEGIN
    SELECT string_agg(format('%%s (ทั้ง %%s และ .2-.%d ก็เป็นของคลินิกอื่นหมด)', w.bare, w.suffixed), '; ')
      INTO clash
      FROM (VALUES
%s
           ) AS w(bare, suffixed, prefix)
     WHERE NOT EXISTS (
               SELECT 1
                 FROM unnest(ARRAY[w.bare, w.suffixed]
                             || ARRAY(SELECT w.prefix || '.' || g || '@gmail.com'
                                        FROM generate_series(2, %d) AS g)) AS cand(c)
                WHERE NOT EXISTS (SELECT 1 FROM users u
                                   WHERE lower(u.email) = lower(cand.c)
                                     AND COALESCE(u.clinic_code, '') <> %s)
           );

    IF clash IS NOT NULL THEN
        RAISE EXCEPTION 'users: ชื่อล็อกอินเป็นของคลินิกอื่น และทุกรูปสำรองก็ถูกใช้ไปแล้ว จึงไม่มีที่ให้บัญชีนี้ลง — ปฏิเสธทั้งสคริปต์: %%', clash;
    END IF;
END $guard$;

`, suffix, loginRungMax, loginRungMax, strings.Join(values, ",\n"), loginRungMax, lit(clinic))
}

// buildRolePresetsGuard refuses to run when the built-in role presets this import
// assigns are missing.
//
// clinic_user.role_preset_id / clinic_staff.role_id are filled by a subquery that looks
// the preset up BY KEY. If the row is absent the subquery yields NULL and Postgres
// reports a NOT NULL violation on role_preset_id — an error that says nothing about the
// real cause. Checking up front turns that into a sentence an operator can act on.
func buildRolePresetsGuard(keys ...string) string {
	quoted := make([]string, 0, len(keys))
	for _, k := range keys {
		quoted = append(quoted, lit(k))
	}
	return fmt.Sprintf(`-- GUARD: the built-in role presets must exist before accounts can be filed.
-- Without them the role lookup returns NULL and Postgres reports a confusing
-- "null value in column role_preset_id" instead of the real cause.
DO $guard$
DECLARE
    missing text;
BEGIN
    SELECT string_agg(k, ', ')
      INTO missing
      FROM (SELECT unnest(ARRAY[%s]) AS k) want
     WHERE NOT EXISTS (
         SELECT 1 FROM role_presets rp
          WHERE rp.key = want.k AND rp.is_builtin = true AND rp.active = true
     );

    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'role_presets ไม่มี built-in role: %% — ฐานข้อมูลนี้ยัง migrate ไม่ครบ', missing;
    END IF;
END $guard$;

`, strings.Join(quoted, ", "))
}

// sqlTextArray renders a sorted, quoted list for an ARRAY[...] literal. Sorted so two
// runs of the same dump produce a byte-identical script (diffable artifacts).
func sqlTextArray(set map[string]bool) string {
	if len(set) == 0 {
		return "NULL::text"
	}
	vals := make([]string, 0, len(set))
	for v := range set {
		vals = append(vals, lit(v))
	}
	sort.Strings(vals)
	return strings.Join(vals, ", ")
}
