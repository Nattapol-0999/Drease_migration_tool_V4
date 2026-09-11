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
// buildUserMap already answers that by falling back to <name>_<CLINIC>@gmail.com, which means
// the bare name being taken is NORMAL — it is the trigger for the fallback, not a failure.
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
func buildUsersCollisionGuard(rows []legacydump.Row, clinic string) string {
	// Pairs, not a flat set: which form a row ends up with depends on the other one.
	type pair struct{ bare, suffixed string }
	seen := map[string]bool{}
	pairs := make([]pair, 0, len(rows))
	for _, r := range rows {
		e := strings.ToLower(strings.TrimSpace(r.Get("email")))
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		pairs = append(pairs, pair{e, strings.ToLower(suffixedEmail(e, clinic))})
	}
	if len(pairs) == 0 {
		return ""
	}

	values := make([]string, 0, len(pairs))
	for _, p := range pairs {
		values = append(values, fmt.Sprintf("        (%s, %s)", lit(p.bare), lit(p.suffixed)))
	}

	return fmt.Sprintf(`-- GUARD: refuse only when an account has NOWHERE to land.
-- users.email is UNIQUE across the whole table, not per clinic. A name another tenant holds
-- is handled by falling back to <name>_%s@gmail.com, so the bare name being taken is the
-- NORMAL case, not an error. What cannot be handled is both forms being taken: the insert
-- would be skipped and the account would never exist. That is what this refuses, by name,
-- before anything is written.
-- users.id is deliberately NOT checked: V4 issues every id now, so a legacy number colliding
-- says nothing about the account.
-- A collision with THIS clinic is a re-run and is skipped by the insert-only statements.
DO $guard$
DECLARE
    clash text;
BEGIN
    SELECT string_agg(format('%%s (รูปสำรอง %%s ก็เป็นของ clinic %%s)', w.bare, w.suffixed, w.taken_by), '; ')
      INTO clash
      FROM (
          SELECT w.bare, w.suffixed,
                 (SELECT COALESCE(NULLIF(u.clinic_code, ''), '(ไม่มี clinic_code)')
                    FROM users u
                   WHERE lower(u.email) = w.suffixed
                     AND COALESCE(u.clinic_code, '') <> %s
                   LIMIT 1) AS taken_by
            FROM (VALUES
%s
                 ) AS w(bare, suffixed)
           -- only rows that will actually need the fallback
           WHERE EXISTS (SELECT 1 FROM users u
                          WHERE lower(u.email) = w.bare
                            AND COALESCE(u.clinic_code, '') <> %s)
      ) w
     WHERE w.taken_by IS NOT NULL;

    IF clash IS NOT NULL THEN
        RAISE EXCEPTION 'users: ชื่อล็อกอินเป็นของคลินิกอื่น และรูปสำรองก็ถูกใช้ไปแล้ว จึงไม่มีที่ให้บัญชีนี้ลง — ปฏิเสธทั้งสคริปต์: %%', clash;
    END IF;
END $guard$;

`, clinic, lit(clinic), strings.Join(values, ",\n"), lit(clinic))
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
