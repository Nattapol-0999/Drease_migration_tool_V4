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
// WHAT IT STILL CHECKS, AND WHY IT HAS TO
// email is UNIQUE across the whole table. buildUserMap already folds the clinic code into any
// name another tenant holds, so the values reaching here are the SUFFIXED ones — and if even
// those are taken, ON CONFLICT (email) DO NOTHING would skip the account while the fill guard
// resolved it to the stranger who owns that address. Refusing is the only safe outcome.
//
// A collision with the SAME clinic is a re-run and is left to the insert-only statements.
func buildUsersCollisionGuard(rows []legacydump.Row, clinic string) string {
	emails := map[string]bool{}
	for _, r := range rows {
		// Both forms, because which one a row ends up with is decided at load time: the bare
		// name when it is free, the suffixed one when it is not. Either could be the value
		// that finally collides, so both are offered to the check.
		if e := strings.ToLower(strings.TrimSpace(r.Get("email"))); e != "" {
			emails[e] = true
			emails[strings.ToLower(suffixedEmail(e, clinic))] = true
		}
	}
	if len(emails) == 0 {
		return ""
	}

	return fmt.Sprintf(`-- GUARD: no login name this run writes may still belong to ANOTHER clinic.
-- users.email is UNIQUE across the whole table, not per clinic. The map has already folded
-- the clinic code into any name another tenant holds, so what is checked here is BOTH forms:
-- if even the suffixed one is taken, ON CONFLICT (email) DO NOTHING would skip the account
-- and the fill would then resolve it to the stranger who owns that address.
-- users.id is deliberately NOT checked: V4 issues every id now, so a legacy number colliding
-- says nothing about the account.
-- A collision with THIS clinic is a re-run and is skipped by the insert-only statements.
DO $guard$
DECLARE
    clash text;
BEGIN
    SELECT string_agg(format('email=%%s → clinic %%s', u.email,
                             COALESCE(NULLIF(u.clinic_code, ''), '(ไม่มี clinic_code)')), '; ')
      INTO clash
      FROM users u
     WHERE lower(u.email) = ANY (ARRAY[%s])
       AND COALESCE(u.clinic_code, '') <> %s;

    IF clash IS NOT NULL THEN
        RAISE EXCEPTION 'users: ชื่อล็อกอินเป็นของคลินิกอื่นอยู่แล้วแม้เติมรหัสคลินิกต่อท้ายแล้ว — ปฏิเสธทั้งสคริปต์: %%', clash;
    END IF;
END $guard$;

`, sqlTextArray(emails), lit(clinic))
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
