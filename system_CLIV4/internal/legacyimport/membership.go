package legacyimport

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"system_cliv4/internal/legacydump"
)

// rolePresetIDByKey is a scalar subquery that resolves a built-in role_presets id by
// its key at write time. The id is NOT hardcoded: a clinic can carry custom presets
// whose ids differ, and only the built-in row (clinic_code IS NULL) is the right
// target — NULLS FIRST puts it ahead of any clinic-scoped preset that reused the key.
func rolePresetIDByKey(key string) string {
	return "(SELECT id FROM role_presets WHERE key = " + lit(key) +
		" AND is_builtin = true AND active = true ORDER BY clinic_code NULLS FIRST, id LIMIT 1)"
}

// isOwnerRow reports whether a legacy users row is the account the caller named as
// the clinic owner (matched on the legacy login name `code`, case-insensitively).
func isOwnerRow(r legacydump.Row, ownerUsername string) bool {
	return strings.EqualFold(strings.TrimSpace(r.Get("code")), strings.TrimSpace(ownerUsername))
}

// hasOwner reports whether the dump contains the named owner account.
func hasOwner(rows []legacydump.Row, ownerUsername string) bool {
	for _, r := range rows {
		if r.Get("id") == "" {
			continue
		}
		if isOwnerRow(r, ownerUsername) {
			return true
		}
	}
	return false
}

/**
 * ownerCandidates lists the codes that WOULD be accepted as owner_username.
 *
 * The refusal it feeds used to say only how many accounts the dump held: "owner_username
 * \"1096\" not found among the 112 account(s)". True, and useless — the operator's next move
 * was to open a 10 MB dump and search it by hand for a column whose name is not obvious
 * (owner_username matches legacy `users`.`code`, not a name, a username or an email).
 *
 * Filtered exactly like hasOwner, rows with no id skipped, so every code printed is one that
 * would actually have worked. Listing codes the caller would then be refused for would be
 * worse than listing none.
 *
 * Sorted so two runs over one file suggest the same thing, and numerically where the codes are
 * numbers — these are staff numbers in every export seen, and 1002 belongs before 1010.
 */
func ownerCandidates(rows []legacydump.Row) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Get("id") == "" {
			continue
		}
		code := strings.TrimSpace(r.Get("code"))
		if code == "" || seen[code] {
			continue
		}
		seen[code] = true
		out = append(out, code)
	}
	sort.Slice(out, func(i, j int) bool {
		a, aErr := strconv.Atoi(out[i])
		b, bErr := strconv.Atoi(out[j])
		if aErr == nil && bErr == nil {
			return a < b
		}
		return out[i] < out[j]
	})
	return out
}

// maxOwnerCandidatesShown bounds the refusal message. Enough to recognise the numbering scheme
// and pick one; not so many that the real sentence scrolls off the top of a job log.
const maxOwnerCandidatesShown = 20

// describeOwnerCandidates renders the list for the error, saying how many were withheld rather
// than trailing off — "…" alone leaves the reader unsure whether theirs was in the part cut.
func describeOwnerCandidates(rows []legacydump.Row) string {
	codes := ownerCandidates(rows)
	if len(codes) == 0 {
		return "ไฟล์นี้ไม่มีบัญชีที่ใช้เป็น owner ได้เลย (ทุกแถวไม่มี id หรือไม่มี code)"
	}
	shown := codes
	suffix := ""
	if len(shown) > maxOwnerCandidatesShown {
		shown = shown[:maxOwnerCandidatesShown]
		suffix = fmt.Sprintf(" … และอีก %d รหัส", len(codes)-maxOwnerCandidatesShown)
	}
	return fmt.Sprintf("code ที่ใช้ได้ในไฟล์นี้: %s%s", strings.Join(shown, ", "), suffix)
}

// buildClinicUser writes the clinic_user memberships. This is the row auth needs:
// ListUserClinics reads clinic_user JOIN role_presets WHERE is_active = true, and
// sessionForUser answers 403 no_active_clinic when a user has none — a correct
// password is not enough on its own.
//
// The account whose legacy `code` matches Input.OwnerUsername becomes the owner;
// every other account becomes counter_staff. Roles are resolved by key, not id.
//
// PRIMARY KEY (user_id, clinic_code) gives a real conflict target. Insert-only
// (ON CONFLICT DO NOTHING): a re-run never rewrites a membership an operator may have
// since narrowed by hand. branch_codes stays '{}' (not restricted to branches).
func buildClinicUser(rows []legacydump.Row, in Input, rep *Report) (string, error) {
	e := rep.entity(EntClinicUser)
	e.SourceRows = len(rows)

	var s sqlBuilder
	others := 0
	for _, r := range rows {
		id := r.Get("id")
		if id == "" {
			continue
		}
		roleKey := RoleKeyStaff
		if isOwnerRow(r, in.OwnerUsername) {
			roleKey = RoleKeyOwner
		} else {
			others++
		}
		s.comment("%s / %s → %s", r.Get("code"), displayName(r), roleKey)
		s.addf(`INSERT INTO clinic_user (user_id, clinic_code, role_preset_id, is_active, branch_codes)
VALUES (%s, %s, %s, true, '{}')
ON CONFLICT (user_id, clinic_code) DO NOTHING;

`, userIDByLegacy(id), lit(in.ClinicCode), rolePresetIDByKey(roleKey))
	}
	e.Statements = s.stmts
	rep.notef("clinic_user: owner=%q, %d other account(s) as %s", in.OwnerUsername, others, RoleKeyStaff)
	return s.String(), nil
}

// buildClinicStaff writes the clinic_staff rows, which is what GET /settings/users
// actually lists: ListClinicStaff selects FROM clinic_staff and only LEFT JOINs
// users / clinic_user to pick up branch_codes. Importing `users` alone leaves that page
// empty.
//
// password_hash stays NULL on purpose — the real hash lives on the users row and the
// email/JWT flow reads it from there (login decision, migration 0058).
//
// The table has no unique key beyond its PK, so rows are guarded by NOT EXISTS on
// (clinic_code, user_id).
//
// tel comes from the legacy doctor dump when one was uploaded: the legacy users table
// has no phone column, but doctor.phone_number is keyed by doctor_user_id.
//
// ROLE, and why it can differ from the clinic_user role of the same person:
// clinic_staff.role_id is the STAFF DIRECTORY marker — it is what
// validate_appointment_assignment reads to decide whether a doctor_code may be booked
// (doctor → staff_id → clinic_staff → role_presets.key = 'doctor'). clinic_user
// .role_preset_id is a different question: what the person may DO in the app.
// So an account that appears in the legacy doctor table gets role `doctor` HERE while
// still holding owner/counter_staff over in clinic_user. That satisfies both rules
// without trading one away: the owner keeps administrative control and, if they also
// treat patients, their appointments still book.
func buildClinicStaff(rows []legacydump.Row, tel map[string]string, doctorUsers map[string]bool,
	extras []extraDoctorStaff, in Input, rep *Report) (string, error) {
	e := rep.entity(EntClinicStaff)
	e.SourceRows = len(rows)

	var s sqlBuilder
	withTel, asDoctor := 0, 0
	for _, r := range rows {
		id := r.Get("id")
		if id == "" {
			continue
		}
		username := r.Get("code") // the legacy login name
		email := r.Get("email")
		name := r.Get("name")
		if name == "" && StaffNameFallsBackToUsername {
			name = username
			if name == "" {
				name = email
			}
			e.skip("empty name (shown as the login name)")
		}
		phone := tel[id]
		if phone != "" {
			withTel++
		}
		// A doctor marker wins over owner/counter_staff in the DIRECTORY: without it the
		// person cannot be assigned an appointment at all. Their app permissions are
		// unaffected — those come from clinic_user.
		roleKey := RoleKeyStaff
		switch {
		case doctorUsers[id]:
			roleKey = RoleKeyDoctor
			asDoctor++
		case isOwnerRow(r, in.OwnerUsername):
			roleKey = RoleKeyOwner
		}

		nameLit, _ := clip(name, 255)
		userLit, _ := clip(username, 255)
		telLit, _ := clip(phone, 64)

		// email here is the DIRECTORY copy shown in Settings › ผู้ใช้งาน, so it is read from
		// the map instead of from this row: it has to be the value the account ENDED UP with,
		// or a staff row would show `admin` beside a login of `admin_C8B173E@gmail.com` and
		// send somebody to the wrong field. That is also why the local `email` above is only
		// used as a name fallback — the map already applied clip() and the collision rule.
		s.comment("%s / %s → %s", username, oneLine(name), roleKey)
		s.addf(`INSERT INTO clinic_staff (clinic_code, name, username, email, tel, role_id, has_login, active, status, user_id, created_at, updated_at)
SELECT %s, %s, %s, (SELECT m.email FROM import_user_map m WHERE m.legacy_id = %s), %s, %s, true, true, 1, %s, now(), now()
WHERE NOT EXISTS (
    SELECT 1 FROM clinic_staff cs WHERE cs.clinic_code = %s AND cs.user_id = %s
);

`, lit(in.ClinicCode), lit(nameLit), lit(userLit), id, lit(telLit),
			rolePresetIDByKey(roleKey), userIDByLegacy(id), lit(in.ClinicCode), userIDByLegacy(id))
	}
	// A doctor whose account already belongs to another doctor gets a staff record of
	// its own, so both stay bookable (uniq_doctor_clinic_staff allows one doctor per
	// staff row). has_login is FALSE: this is a directory entry, not a second way to log
	// in as that account. The name comes from the doctor table, so the staff list shows
	// the doctor's real name instead of the shared account's.
	for _, x := range extras {
		name := firstNonEmpty(x.NameTh, x.NameEn, x.DoctorCode)
		nameLit, _ := clip(name, 255)
		userLit, _ := clip(doctorStaffUsername(x.DoctorCode), 255)
		telLit, _ := clip(x.Tel, 64)

		// email is NOT NULL on this table and this record has no address of its own (it
		// is a directory entry, not a login), so it is written empty rather than NULL.
		s.comment("%s / %s → %s (ใบพนักงานเฉพาะของหมอ ไม่ใช้ล็อกอิน)", x.DoctorCode, oneLine(name), RoleKeyDoctor)
		s.addf(`INSERT INTO clinic_staff (clinic_code, name, username, email, tel, role_id, has_login, active, status, user_id, created_at, updated_at)
SELECT %s, %s, %s, '', %s, %s, false, true, 1, %s, now(), now()
WHERE NOT EXISTS (
    SELECT 1 FROM clinic_staff cs WHERE cs.clinic_code = %s AND cs.username = %s
);

`, lit(in.ClinicCode), lit(nameLit), lit(userLit), lit(telLit),
			rolePresetIDByKey(RoleKeyDoctor), userIDByLegacy(x.UserID), lit(in.ClinicCode), lit(userLit))
	}

	e.Statements = s.stmts
	if withTel > 0 {
		rep.notef("clinic_staff: %d phone number(s) taken from the doctor dump", withTel)
	}
	if len(extras) > 0 {
		rep.notef("clinic_staff: เพิ่มใบพนักงานเฉพาะของหมอ %d ใบ (บัญชีเดียวผูกหลายหมอ) — has_login=false", len(extras))
	}
	if asDoctor > 0 {
		rep.notef("clinic_staff: %d account(s) marked role=%s (required before their appointments can be booked); their clinic_user role is unchanged",
			asDoctor, RoleKeyDoctor)
	}
	return s.String(), nil
}

// doctorUserIDsFrom reads the set of legacy users.id that the doctor dump links to a
// doctor record (doctor.doctor_user_id). Those accounts must be filed as role `doctor`
// in clinic_staff, or V4 refuses to book appointments against their doctor_code.
func doctorUserIDsFrom(rows []legacydump.Row) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		if uid := r.Get("doctor_user_id"); uid != "" && uid != "0" {
			out[uid] = true
		}
	}
	return out
}

// telByUserID reads doctor_user_id -> phone_number from the legacy doctor dump. The
// first doctor row wins when one account is linked to several doctors.
func telByUserID(rows []legacydump.Row) map[string]string {
	out := map[string]string{}
	for _, r := range rows {
		uid := r.Get("doctor_user_id")
		phone := r.Get("phone_number")
		if uid == "" || uid == "0" || phone == "" {
			continue
		}
		if _, taken := out[uid]; !taken {
			out[uid] = phone
		}
	}
	return out
}

// displayName picks something human for a generated SQL comment.
func displayName(r legacydump.Row) string {
	if n := r.Get("name"); n != "" {
		return oneLine(n)
	}
	if n := r.Get("tha_name"); n != "" {
		return oneLine(n)
	}
	return "(no name)"
}
