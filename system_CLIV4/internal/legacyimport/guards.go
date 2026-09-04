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

// buildUsersCollisionGuard refuses the whole import when a legacy user id or email
// already belongs to a DIFFERENT clinic.
//
// users is a GLOBAL identity table: `id` is the primary key across the whole platform
// and `email` is UNIQUE across the whole table (not per clinic). Importing over such a
// row would overwrite another tenant's login; skipping it would drop an account that
// patient.user_insert still points at. Neither is acceptable, so the run is refused and
// a human decides.
//
// A collision with the SAME clinic is a re-run and is left to the insert-only
// statements to skip.
func buildUsersCollisionGuard(rows []legacydump.Row, clinic string) string {
	ids := map[string]bool{}
	emails := map[string]bool{}
	for _, r := range rows {
		if id := r.Get("id"); id != "" {
			ids[id] = true
		}
		if e := strings.ToLower(r.Get("email")); e != "" {
			emails[e] = true
		}
	}
	if len(ids) == 0 && len(emails) == 0 {
		return ""
	}

	return fmt.Sprintf(`-- GUARD: no legacy account may collide with ANOTHER clinic's user.
-- users.id is a platform-wide primary key and users.email is UNIQUE across the whole
-- table. A collision with a different clinic would overwrite that tenant's login, so
-- the import refuses instead. A collision with THIS clinic is a re-run and is skipped
-- by the ON CONFLICT DO NOTHING statements below.
DO $guard$
DECLARE
    clash text;
BEGIN
    SELECT string_agg(format('id=%%s email=%%s → clinic %%s', u.id, u.email,
                             COALESCE(NULLIF(u.clinic_code, ''), '(ไม่มี clinic_code)')), '; ')
      INTO clash
      FROM users u
     WHERE (u.id::text = ANY (ARRAY[%s]) OR lower(u.email) = ANY (ARRAY[%s]))
       AND COALESCE(u.clinic_code, '') <> %s;

    IF clash IS NOT NULL THEN
        RAISE EXCEPTION 'users: มี id/email ที่เป็นของคลินิกอื่นอยู่แล้ว — ปฏิเสธทั้งสคริปต์: %%', clash;
    END IF;
END $guard$;

`, sqlTextArray(ids), sqlTextArray(emails), lit(clinic))
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
