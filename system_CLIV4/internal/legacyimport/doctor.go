package legacyimport

import (
	"fmt"

	"system_cliv4/internal/legacydump"
)

// buildDoctor maps the legacy `doctor` table onto V4 `public.doctor`.
//
// Two decisions shape it:
//
//   - The legacy numeric doctor_code ('1000'…) is KEPT rather than re-issued in the
//     app's DR0001 format: fifteen tables reference doctor_code as a string
//     (appointment, opd_record, receipt, prescription, doctor_schedule,
//     bill_doctor_fees, patient_diag, photos, face_chart, perio_chart, followup, …),
//     so renaming it would orphan every legacy row. It is mirrored into `code` the way
//     the app's own InsertDoctor does.
//
//   - V4 carries TWO sets of name columns and the app reads BOTH: doctor_settings /
//     doctor_schedule / document_binding resolve the display name as
//     COALESCE(NULLIF(tha_name,”), NULLIF(doctor_name,”), NULLIF(eng_name,”)), while
//     doctorfee's ListActiveDoctors selects doctorname_th / doctorname_en /
//     doctornickname with NO fallback. Filling one set only leaves the other UI blank,
//     so the Thai name goes to three columns and the English name to two.
//
// doctor.id is GENERATED ALWAYS AS IDENTITY and the table has no unique key, so rows
// are guarded by NOT EXISTS on (clinic_code, doctor_code). Dropped: phone_number (V4's
// doctor has no phone column; it is used for clinic_staff.tel instead) and doctor_user
// (redundant with user_id).
// BRANCH: the legacy `doctor` table has no branch column at all — its ten columns are
// doctor_code, doctorname_th, doctorname_en, doctornickname, phone_number,
// medical_license, color, doctor_user, doctor_active, doctor_user_id. So whatever goes
// into V4's NOT NULL doctor.branch_code is a CHOICE, and it used to be the constant "01",
// which is a legacy code: correct for a tenant this tool created (buildClinic mints a
// branch row '01' to match), wrong for one whose branches came from signup as
// BR00001/BR00002 — there it names no branch that exists.
//
// Nothing in the backend filters on this column, so a wrong value raises no error. What it
// does is drop the provider out of every per-branch calendar roster: ListDoctors returns
// branch_code as "where this provider works" precisely so the calendar can bucket by it,
// and a code matching no branch lands in no bucket. Blank is not an option either —
// migration 0320 added CHECK (branch_code <> ”).
//
// It now goes through branchResolver like room, patient, appointment and money already do.
// With no source value every row takes the resolver's default, which is the first branch
// of the caller's map — the same answer syncDoctorProvider reaches at runtime through
// GetFirstActiveBranchForSetup, and what migration 0320's own backfill used
// (MIN(clinic.branch_code)). The count is reported, because "every doctor at one branch"
// is a guess and the clinic is the only one who can correct it.
func buildDoctor(rows []legacydump.Row, in Input, rep *Report) (string, error) {
	e := rep.entity(EntDoctor)
	e.SourceRows = len(rows)
	tr := truncations{}
	br := newBranchResolver(in)
	defer br.report(rep, "doctor")

	cols := []string{
		"doctor_code", "code", "tha_name", "doctor_name", "doctorname_th",
		"eng_name", "doctorname_en", "doctornickname", "medical_license",
		"color", "doctor_active", "user_id", "staff_id", "clinic_code", "branch_code",
		"created_at", "updated_at",
	}
	noStaff := 0
	staffTaken := map[string]bool{}      // legacy user_id → already linked to a doctor
	sharedStaff := map[string][]string{} // legacy user_id → doctor codes left unlinked

	var s sqlBuilder
	sharedUser := map[string][]string{}

	for n, r := range rows {
		code := r.Get("doctor_code")
		if code == "" {
			return "", fmt.Errorf("doctor row %d: empty doctor_code (it is the dedup key)", n+1)
		}
		if uid := r.Get("doctor_user_id"); uid != "" && uid != "0" {
			sharedUser[uid] = append(sharedUser[uid], code)
		}

		codeLit, cut := clip(code, 64)
		tr.note("doctor_code", cut)
		tha, cut := clip(r.Get("doctorname_th"), 255)
		tr.note("doctorname_th", cut)
		eng, cut := clip(r.Get("doctorname_en"), 255)
		tr.note("doctorname_en", cut)
		nick, cut := clip(r.Get("doctornickname"), 255)
		tr.note("doctornickname", cut)
		lic, cut := clip(r.Get("medical_license"), 32)
		tr.note("medical_license", cut)
		color, cut := clip(r.Get("color"), 32)
		tr.note("color", cut)
		active, cut := clip(r.Get("doctor_active"), 8)
		tr.note("doctor_active", cut)
		// doctor.user_id points at a V4 account, so the legacy number is translated rather
		// than copied. NULL when the source has no link, or has one that is not a number.
		userID := "NULL"
		if raw, ok := intLit(r.Get("doctor_user_id")); !ok {
			rep.warnf("doctor %s: doctor_user_id %q is not a number — imported as NULL", code, r.Get("doctor_user_id"))
		} else if raw != "NULL" && raw != "0" {
			userID = userIDByLegacy(raw)
		}
		if r.Get("doctor_user_id") == "" {
			e.skip("no user link")
		}

		// staff_id is resolved IN SQL from the clinic_staff row of the same legacy user,
		// because clinic_staff.id is generated by the database and cannot be known while
		// rendering the file. clinic_staff is inserted earlier in the same transaction,
		// so the lookup always sees it.
		//
		// This link is load-bearing: validate_appointment_assignment walks
		// doctor.staff_id → clinic_staff → role_presets.key='doctor' and refuses the
		// appointment when it breaks. A doctor with no user link therefore cannot take
		// appointments, which is why it is counted and reported rather than passed over.
		// V4 allows ONE doctor per staff record (uniq_doctor_clinic_staff), but the
		// legacy data can link several doctor rows to the same account. clinic_staff has
		// no uniqueness of its own, so the second and later doctors get a staff record of
		// their OWN (written by buildClinicStaff, carrying the doctor's real name and
		// has_login=false) and link to that instead of fighting over the account's row.
		// Every doctor therefore stays bookable.
		staffID := "NULL"
		uid := r.Get("doctor_user_id")
		switch {
		case uid == "" || uid == "0":
			noStaff++
		case staffTaken[uid]:
			sharedStaff[uid] = append(sharedStaff[uid], code)
			staffID = fmt.Sprintf(
				"(SELECT cs.id FROM clinic_staff cs WHERE cs.clinic_code = %s AND cs.username = %s LIMIT 1)",
				lit(in.ClinicCode), lit(doctorStaffUsername(code)))
		default:
			staffTaken[uid] = true
			// has_login = true pins this to the row built from the users dump, so it can
			// never accidentally resolve to one of the doctor-only rows above.
			//
			// cs.user_id holds the id V4 issued, not the legacy number, so the legacy value
			// is translated through the map first — comparing it raw would match whichever
			// clinic happens to own that id.
			staffID = fmt.Sprintf(
				"(SELECT cs.id FROM clinic_staff cs WHERE cs.clinic_code = %s AND cs.user_id = %s AND cs.has_login LIMIT 1)",
				lit(in.ClinicCode), userIDByLegacy(uid))
		}

		s.comment("%s / %s", code, oneLine(pick(tha, eng)))
		s.addf(`INSERT INTO doctor (%s)
SELECT %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, now(), now()
WHERE NOT EXISTS (
    SELECT 1 FROM doctor d WHERE d.clinic_code = %s AND d.doctor_code = %s
);

`, joinCols(cols),
			lit(codeLit), lit(codeLit),
			nullLit(tha), nullLit(tha), nullLit(tha),
			nullLit(eng), nullLit(eng),
			nullLit(nick), nullLit(lic), nullLit(color), nullLit(active),
			userID, staffID, lit(in.ClinicCode), lit(br.resolve(r.Get("branch_code"))),
			lit(in.ClinicCode), lit(codeLit))
	}

	if noStaff > 0 {
		rep.warnf("doctor: %d row(s) have no doctor_user_id, so staff_id stays NULL — V4 will REFUSE appointments booked against those doctor_code(s)", noStaff)
	}

	e.Statements = s.stmts
	if sum := tr.summary(); sum != "" {
		rep.warnf("doctor: truncated %s", sum)
	}
	for uid, codes := range sharedStaff {
		rep.warnf("doctor: บัญชี user_id %s ผูกกับหมอมากกว่าหนึ่งคน — หมอ %v ได้ใบพนักงานของตัวเอง (has_login=false, ชื่อจากตาราง doctor) เพื่อให้จองนัดได้ทั้งคู่ · คลินิกควรตรวจว่าเป็นคนละคนจริง",
			uid, codes)
	}
	for uid, codes := range sharedUser {
		if len(codes) > 1 {
			rep.warnf("doctor: user_id %s is linked to %d doctor rows (%v) — GetDoctorIDByUser/GetDoctorByUser use LIMIT 1 with no ORDER BY, so which one wins is not deterministic",
				uid, len(codes), codes)
		}
	}
	return s.String(), nil
}

// doctorStaffUsername is the username of the staff record minted for a doctor that
// cannot use its account's row (because another doctor already holds it). Derived from
// the doctor code so it is deterministic and unique within the clinic.
func doctorStaffUsername(doctorCode string) string { return "dr_" + doctorCode }

// extraDoctorStaff lists the doctors that need a staff record of their own: for each
// legacy account linked to several doctors, everyone after the first.
type extraDoctorStaff struct {
	UserID     string
	DoctorCode string
	NameTh     string
	NameEn     string
	Tel        string
}

func extraDoctorStaffRows(doctorRows []legacydump.Row) []extraDoctorStaff {
	seen := map[string]bool{}
	var out []extraDoctorStaff
	for _, r := range doctorRows {
		uid, code := r.Get("doctor_user_id"), r.Get("doctor_code")
		if uid == "" || uid == "0" || code == "" {
			continue
		}
		if !seen[uid] {
			seen[uid] = true
			continue // the first doctor uses the account's own staff row
		}
		out = append(out, extraDoctorStaff{
			UserID: uid, DoctorCode: code,
			NameTh: r.Get("doctorname_th"), NameEn: r.Get("doctorname_en"),
			Tel: r.Get("phone_number"),
		})
	}
	return out
}

func pick(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
