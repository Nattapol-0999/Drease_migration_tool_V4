package legacyimport

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"system_cliv4/internal/legacydump"
)

// apptDocFields are the legacy columns with no V4 column that are still worth keeping.
// They go into the doc jsonb — which is NOT NULL with no default, so a value has to be
// supplied anyway, and doc is where the original document used to live. ref_status in
// particular ("LINE"/"TEL"/"facebook"/"WALKIN"/"WEBSITE") is the acquisition channel and
// would otherwise be lost.
//
// Columns that were NULL or empty in every row of the first clinic's dump
// (braces_ID, implantation_ID, medicine_*, anesthetic_*, payment_waiting,
// payment_success, Notify_id, drtime_id, appointment_source_*) are not listed; an empty
// value is skipped anyway, so listing them would only add noise.
var apptDocFields = []string{
	"clinic_name", "doctorname", "ref_status", "payment_status", "confirm_check",
	"datetime_stamp", "datetime_in", "datetime_out", "users_stamp_id", "uniqueKey",
	"line_patient_status", "send_line_notification", "notify_state", "goldCard",
	"fixed_status", "appointment_time_start", "appointment_time_end",
}

// buildAppointment maps the legacy `appointment` table onto V4 `public.appointment`.
//
// Three things about the legacy shape drive it:
//
//   - The legacy `clinic_code` column is the ROOM code, not a clinic: its values
//     ('01'..'10') and the companion `clinic_name` ('Wanaka 1', 'Doctor Room',
//     'ห้องหัตถการ') match the legacy room master. It maps to room_id; V4's clinic_code
//     is the tenant constant.
//   - The date and the two times are separate columns, combined into V4's two
//     timestamptz values.
//   - There is no status column; it is derived from confirm_check (see
//     statusFromConfirmCheck).
//
// Idempotent through migrate_source_ref = the legacy id, on the partial unique index
// uq_appointment_migrate_src. appointment has no RLS and no foreign keys, so a wrong
// hn / doctor_code / room_id would import silently — the importer runs orphan checks
// after the load and reports them.
// bookable is the set of doctor_codes V4 will actually accept on an appointment (see
// buildBookableDoctors). It is passed in rather than recomputed so the rule lives in
// one place.
// branchByPatient maps HN → legacy branch code, used because the legacy appointment rows
// do not carry a usable branch of their own (see branchByHN).
// branchOfRoom, when supplied, maps a legacy room code to the V4 branch_code that room is
// imported at. It is used ONLY to report a disagreement, never to change what is written:
// validate_appointment_assignment overwrites NEW.branch_code with the room's branch on
// every insert, so the value this builder emits for a row with a room is advisory. Without
// the count, an appointment that says one branch and lands at another is invisible.
func buildAppointment(
	rows []legacydump.Row,
	bookable map[string]bool,
	branchByPatient map[string]string,
	branchOfRoom map[string]string,
	in Input,
	rep *Report,
) (string, error) {
	e := rep.entity(EntAppointment)
	e.SourceRows = len(rows)
	tr := truncations{}
	unbookable := map[string]int{}
	br := newBranchResolver(in)
	defer br.report(rep, "appointment")
	// Appointments whose HN is not in the patient dump have nothing to derive a branch
	// from; they fall back to the default and are counted so the number is visible.
	noPatientBranch := 0
	// "branch this builder wrote → branch the trigger will put it at", per pair.
	branchOverridden := map[string]int{}
	defer func() {
		if noPatientBranch > 0 {
			rep.notef("appointment: %d แถวหาสาขาจากคนไข้ไม่ได้ (ไม่พบ HN ในไฟล์คนไข้) — ใช้สาขาเริ่มต้น", noPatientBranch)
		}
	}()

	cols := []string{
		"clinic_code", "migrate_source_ref", "hn", "patient_name", "tel", "doctor_code",
		"room_id", "branch_code", "appointment_date", "appointment_end_date",
		"case_detail", "status", "confirm_status", "transfer_receipt", "doc",
		"created_at", "updated_at",
	}
	var s sqlBuilder
	seen := map[string]bool{}
	statusCount := map[string]int{}

	for n, r := range rows {
		ref := r.Get("id")
		if ref == "" {
			return "", fmt.Errorf("appointment row %d: empty id (it is the dedup key)", n+1)
		}
		if seen[ref] {
			rep.warnf("appointment: duplicate legacy id %q in the dump", ref)
		}
		seen[ref] = true

		hn := r.Get("HN")
		if hn == "" {
			e.skip("no HN")
		}
		name := r.Get("patientname") // legacy values carry a leading space; Get trims
		if name == "" {
			e.skip("no patient_name")
		}
		room := r.Get("clinic_code") // legacy "clinic_code" is the ROOM code
		if room == "" {
			e.skip("no room")
		}
		doctor := r.Get("doctor_code")
		droppedDoctor := ""
		if doctor == "" {
			e.skip("no doctor")
		} else if !bookable[doctor] {
			// V4's validate_appointment_assignment trigger REFUSES a doctor_code that
			// cannot be walked back to an active clinic_staff row with role 'doctor' —
			// which is the case for doctors deleted from the legacy system long ago.
			//
			// The appointment itself is still real history (patient, date, room), so it
			// is kept and only the BROKEN link is cleared. The original code is preserved
			// in doc.legacy_doctor_code, so nothing from the source is lost: it moves
			// from a column V4 would reject to a field that records what the dump said.
			droppedDoctor = doctor
			doctor = ""
			unbookable[droppedDoctor]++
			e.skip("doctor not bookable in V4 (kept as doc.legacy_doctor_code)")
		}

		status, confirmStatus, known := statusFromConfirmCheck(r.Get("confirm_check"))
		if !known {
			rep.warnf("appointment id=%s: unrecognised confirm_check %q — imported as %s/%s",
				ref, r.Get("confirm_check"), status, confirmStatus)
		}
		statusCount[status]++

		startLit, startAt, okStart := combineDateTime(r.Get("appointment_date"), r.Get("appointment_time_start"))
		if !okStart {
			rep.warnf("appointment id=%s: unparseable start %q %q — imported as NULL",
				ref, r.Get("appointment_date"), r.Get("appointment_time_start"))
		}
		endLit, endAt, okEnd := combineDateTime(r.Get("appointment_date"), r.Get("appointment_time_end"))
		if !okEnd {
			rep.warnf("appointment id=%s: unparseable end %q %q — imported as NULL",
				ref, r.Get("appointment_date"), r.Get("appointment_time_end"))
		}
		if startAt != nil && endAt != nil && endAt.Before(*startAt) {
			rep.warnf("appointment id=%s: end time %s is before start %s — imported as-is",
				ref, r.Get("appointment_time_end"), r.Get("appointment_time_start"))
		}

		doc, err := apptDoc(r, droppedDoctor)
		if err != nil {
			return "", fmt.Errorf("appointment row %d (id=%s): build doc: %w", n+1, ref, err)
		}

		hnLit, cut := clip(hn, 20)
		tr.note("hn", cut)
		nameLit, cut := clip(name, 255)
		tr.note("patient_name", cut)
		telLit, cut := clip(r.Get("tel"), 32)
		tr.note("tel", cut)
		docCode, cut := clip(doctor, 64)
		tr.note("doctor_code", cut)
		roomLit, cut := clip(room, 64)
		tr.note("room_id", cut)
		receipt, cut := clip(r.Get("transfer_receipt"), 255)
		tr.note("transfer_receipt", cut)

		// The branch comes from the PATIENT, not from this row: the legacy
		// appointment.branch_code is blank in every row of the exports seen, while
		// patient.branch_code is filled for every patient and no HN appears under two
		// branches (see branchByHN). Falling back to the row's own value first keeps a
		// dump that does fill it working.
		legacyBranch := r.Get("branch_code")
		if strings.TrimSpace(legacyBranch) == "" {
			if b, ok := branchByPatient[hn]; ok {
				legacyBranch = b
			} else if hn != "" {
				noPatientBranch++
			}
		}
		branchLit := br.resolve(legacyBranch)

		// The trigger has the last word on branch_code when the row names a room, so what
		// matters here is whether the two answers agree. Counted per (from → to) pair
		// rather than as one total: 232 bookings moving Ratchada→Siam is a mapping to
		// examine, while a handful scattered across pairs is data entry.
		if room != "" {
			if roomBranch, known := branchOfRoom[room]; known && roomBranch != branchLit {
				branchOverridden[branchLit+" → "+roomBranch]++
			}
		}

		s.comment("legacy id %s · HN %s · room %s · %s %s", ref, orDash(hn), orDash(room),
			r.Get("appointment_date"), r.Get("appointment_time_start"))
		// Insert-only: a re-run skips appointments already imported for this clinic
		// (idempotent on the legacy id), rather than overwriting rows an operator may
		// have since edited.
		s.addf(`INSERT INTO appointment (%s)
VALUES (%s, %s, %s, %s, %s, %s,
        %s, %s, %s, %s,
        %s, %s, %s, %s, %s::jsonb,
        %s, now())
ON CONFLICT (clinic_code, migrate_source_ref) WHERE migrate_source_ref IS NOT NULL
DO NOTHING;

`, joinCols(cols),
			lit(in.ClinicCode), lit(ref), nullLit(hnLit), nullLit(nameLit), nullLit(telLit), nullLit(docCode),
			nullLit(roomLit), lit(branchLit), startLit, endLit,
			nullLit(r.Get("remark")), lit(status), lit(confirmStatus), nullLit(receipt), lit(doc),
			tsLit(r.Get("datetime_stamp"), "now()"))
	}

	e.Statements = s.stmts
	if sum := tr.summary(); sum != "" {
		rep.warnf("appointment: truncated %s", sum)
	}
	rep.notef("appointment: status from confirm_check — reserve=%d confirm=%d cancel=%d",
		statusCount["reserve"], statusCount["confirm"], statusCount["cancel"])

	// The room wins, and the operator is told how often that changed the answer. Kept as a
	// WARNING rather than a note: these rows are filed at a branch other than the one the
	// source recorded, and per-branch reports will read differently because of it.
	if len(branchOverridden) > 0 {
		total := 0
		pairs := make([]string, 0, len(branchOverridden))
		for pair, n := range branchOverridden {
			total += n
			pairs = append(pairs, fmt.Sprintf("%s (%d นัด)", pair, n))
		}
		sort.Strings(pairs)
		rep.warnf("appointment: %d นัด สาขาที่ได้จากคนไข้/ต้นทาง ไม่ตรงกับสาขาของห้องที่จอง — V4 ยึดสาขาของห้อง (validate_appointment_assignment เขียนทับ branch_code ทุกครั้งที่มี room_id) ฉะนั้นนัดเหล่านี้จะไปอยู่สาขาของห้อง · %s",
			total, strings.Join(pairs, ", "))
	}

	// Loudly, and per code. The appointments were kept; what changed is that a link V4
	// would have rejected is now recorded in doc.legacy_doctor_code instead of
	// doctor_code. An operator reading the report can still find every one of them:
	//   SELECT * FROM appointment WHERE doc ? 'legacy_doctor_code';
	if len(unbookable) > 0 {
		total := 0
		codes := make([]string, 0, len(unbookable))
		for code, n := range unbookable {
			total += n
			codes = append(codes, fmt.Sprintf("%s (%d นัด)", code, n))
		}
		sort.Strings(codes)
		rep.warnf("appointment: %d นัด อ้าง doctor_code ที่ V4 ปฏิเสธ (หมอถูกลบในระบบเก่า หรือไม่ผูก clinic_staff role=doctor) — เก็บนัดไว้ แต่ล้าง doctor_code และบันทึกรหัสเดิมไว้ที่ doc.legacy_doctor_code · %d รหัส: %s",
			total, len(unbookable), strings.Join(codes, ", "))
		rep.notef("appointment: ค้นรายการที่ถูกล้าง doctor_code ได้ด้วย  SELECT migrate_source_ref, doc->>'legacy_doctor_code' FROM appointment WHERE clinic_code = '%s' AND doc ? 'legacy_doctor_code';",
			in.ClinicCode)
	}
	return s.String(), nil
}

// buildBookableDoctors is the set of doctor_codes an appointment may reference.
//
// It mirrors validate_appointment_assignment exactly: the doctor row must exist, must
// not be deactivated, and must reach an active clinic_staff row with role 'doctor' —
// which in this import means the doctor is linked to a legacy user that the users dump
// also contains. Anything outside this set makes the trigger raise.
func buildBookableDoctors(doctorRows, userRows []legacydump.Row) map[string]bool {
	users := map[string]bool{}
	for _, u := range userRows {
		if id := u.Get("id"); id != "" {
			users[id] = true
		}
	}

	out := map[string]bool{}
	for _, d := range doctorRows {
		code := d.Get("doctor_code")
		if code == "" {
			continue
		}
		if active := d.Get("doctor_active"); active == "0" {
			continue
		}
		uid := d.Get("doctor_user_id")
		if uid == "" || uid == "0" || !users[uid] {
			continue // staff_id would stay NULL → the trigger refuses
		}
		out[code] = true
	}
	return out
}

// apptDoc packs the legacy columns V4 has no home for into the doc jsonb.
//
// droppedDoctor, when set, is a doctor_code the source carried but V4 refuses (the
// doctor no longer exists, or is not bookable). It is recorded here so the fact is not
// lost: the appointment says who the old system had assigned, even though the column
// had to be cleared for the row to be accepted at all.
func apptDoc(r legacydump.Row, droppedDoctor string) (string, error) {
	m := map[string]any{"migrate_source": "legacy appointment dump", "legacy_id": r.Get("id")}
	for _, f := range apptDocFields {
		if v := r.Get(f); v != "" {
			m[f] = v
		}
	}
	if droppedDoctor != "" {
		m["legacy_doctor_code"] = droppedDoctor
		m["legacy_doctor_dropped_reason"] = "doctor ไม่มีในตาราง doctor หรือไม่ผูก clinic_staff role=doctor — V4 ปฏิเสธ doctor_code นี้"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
