package legacyimport

import (
	"slices"
	"strings"
	"system_cliv4/internal/legacydump"
	"testing"
)

// The OPD note lane carries the Anjali (C1) mapping into the tool. These tests pin the
// mapping, the vital-sign rules and the safety properties of the generated SQL; the passes
// themselves were rehearsed against the test database (BEGIN … ROLLBACK).

const opdVisitDump = "INSERT INTO `patient_visit_mater` (`id`, `visitdate`, `VN`, `HN`, `user_id`, `create_at`, `active`) VALUES\n" +
	"(1, '2026-10-30 00:00:00', '001', '016900002', 391, '2026-10-30 01:41:40', '1'),\n" +
	"(2, '2026-10-30 00:00:00', '002', '016900003', 391, '2026-10-30 02:00:00', '0'),\n" + // closed in V2
	"(3, '2026-05-12 00:00:00', '002', '016900002', 0, '2026-05-12 00:37:00', '1');\n"

const opdDoctorDump = "INSERT INTO `doctor` (`id`, `doctor_code`, `doctor_user_id`, `doctor_active`) VALUES\n" +
	"(1, '1000', 391, '1');\n"

const opdNoteDump = "INSERT INTO `patient_opd_note` (`id`, `HN`, `visitDate`, `chief_complaint`, `presen_illness`, `physical_exam`, `diagnosis_impression`, `management`, `advice`, `user_id`, `created_at`, `update_at`) VALUES\n" +
	"(10, '016900002', '2026-10-30 00:00:00', 'cc one', 'pi one', 'pe one', 'dx one', 'mg one', 'ad one', '391', NULL, NULL),\n" +
	// the same patient-day again: a second note, plus an exact copy of the first advice
	"(11, '016900002', '2026-10-30 00:00:00', 'cc two', '', NULL, NULL, NULL, 'ad one', '391', NULL, NULL),\n" +
	// blank in all six fields
	"(12, '016900002', '2026-05-12 00:00:00', '', '', '', NULL, NULL, '', '0', NULL, NULL);\n"

func opdBuild(t *testing.T, visits, notes, vitals string) (string, *Report) {
	t.Helper()
	return opdBuildKnown(t, nil, visits, notes, vitals)
}

// opdBuildKnown is opdBuild with the patient dump's HN set; nil is a run that read none.
func opdBuildKnown(t *testing.T, knownHN map[string]bool, visits, notes, vitals string) (string, *Report) {
	t.Helper()
	in := Input{ClinicCode: "CTEST01"}
	rep := NewReport(in.ClinicCode)
	vr := parseOpt(t, "patient_visit_mater", visits)
	nr := parseOpt(t, "patient_opd_note", notes)
	sr := parseOpt(t, "patient_vital_sign", vitals)
	sqlText, err := buildOpdNote(vr, nr, sr, parseTable(t, "doctor", opdDoctorDump), knownHN, in, rep)
	if err != nil {
		t.Fatalf("buildOpdNote: %v", err)
	}
	return sqlText, rep
}

func parseOpt(t *testing.T, table, dump string) []legacydump.Row {
	t.Helper()
	if dump == "" {
		return nil
	}
	return parseTable(t, table, dump)
}

func TestOpdNoteFollowsTheAnjaliMapping(t *testing.T) {
	sqlText, rep := opdBuild(t, opdVisitDump, opdNoteDump, "")

	// A visit per REAL date, the V2 VN and the author translated through the doctor table.
	if !strings.Contains(sqlText, "(1, '2026-10-30', '001', '016900002', '1000')") {
		t.Errorf("visit 1 was not staged with its date, VN and author:\n%s", head(sqlText, 1500))
	}
	if !strings.Contains(sqlText, "(3, '2026-05-12', '002', '016900002', '')") {
		t.Errorf("visit 3 (no author in V2) was not staged with a blank author:\n%s", head(sqlText, 1500))
	}
	// Decision 7: a visit V2 closed is not imported.
	if strings.Contains(sqlText, "'016900003'") {
		t.Errorf("a visit V2 closed (active=0) reached the script")
	}

	// Decision 5 + C1 boxes: CC+PI → S, PE → O, DX → A, MG → P, advice → คำแนะนำ, each with
	// its heading, and two notes of one patient-day merged in source order.
	for _, want := range []string{
		"'อาการสำคัญ\ncc one\n\ncc two\n\nประวัติปัจจุบัน\npi one'",
		"'ตรวจร่างกาย\npe one'",
		"'วินิจฉัย / การประเมิน\ndx one'",
		"'การจัดการ / แผนการรักษา\nmg one'",
		// the advice saved twice lands once
		"'คำแนะนำ\nad one'",
	} {
		if !strings.Contains(sqlText, want) {
			t.Errorf("note box missing or wrong, want %q in:\n%s", want, head(sqlText, 2500))
		}
	}
	if !strings.Contains(sqlText, ", '1000', 2)") {
		t.Errorf("the merged note does not carry its author and its count of 2 source notes")
	}
	// A note blank in all six fields still stages (its visit is opened) with NULL boxes.
	if !strings.Contains(sqlText, "(12, '2026-05-12', '016900002', NULL, NULL, NULL, NULL, NULL, '', 1)") {
		t.Errorf("the blank note was not staged with empty boxes:\n%s", head(sqlText, 2500))
	}
	if !hasText(rep.Notes, "ว่างทั้ง 6 ช่อง 1") || !hasText(rep.Notes, "รวมจากหลายใบ 1") {
		t.Errorf("the report does not count the merged and the blank note: %v", rep.Notes)
	}
	if !hasText(rep.Notes, "patient_diag") {
		t.Errorf("the report does not say the diagnosis box is left out like Anjali: %v", rep.Notes)
	}
}

const opdVitalDump = "INSERT INTO `patient_vital_sign` (`id`, `visitdate`, `VN`, `weight`, `height`, `heart_rate`, `systolic`, `diastolic`, `temperature`, `o2_sat`, `urinary`, `defection`, `pain_score`, `symptom_text`, `rr`, `spo2`, `DTX`, `visual_acuity_RE`, `visual_acuity_LE`, `tension_RE`, `tension_LE`, `autorefraction_RE`, `autorefraction_LE`, `ltrt`, `slcm`, `user_id`, `create_at`, `update_at`) VALUES\n" +
	// an older copy for visit 1 that the newer row below supersedes
	"(20, '2026-10-30 00:00:00', '001', 50, 0, 0, 0, 0, 0, 0, 0, 0, 0, 'Chief complaint:\\n\\nPresent illness:', 0, 0, 0, '', '', '', '', '', '', 0, 0, 391, '2026-10-30 01:41:40', NULL),\n" +
	// weight 60.5 kg · height 1 cm (typed garbage) · temp 36.5 · spo2 blank, o2_sat 98 · DTX 0
	"(21, '2026-10-30 00:00:00', '001', 60.5, 1, 72, 120, 80, 36.5, 98, 2, 0, 3, 'Chief complaint:test\\n\\nPresent illness:test', 0, 0, 0, '20/40', '', '', '', '', '', 0, 1.5, 391, '2026-10-30 01:41:40', NULL),\n" +
	// the empty form of visit 3: nothing typed at all
	"(22, '2026-05-12 00:00:00', '002', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 'Chief complaint:\\n\\nPresent illness:', 0, 0, 0, '', '', '', '', '', '', 0, 0, 0, '2026-05-12 00:37:00', NULL);\n"

func TestOpdVitalKeepsWhatAnInstrumentCouldHaveRead(t *testing.T) {
	sqlText, rep := opdBuild(t, opdVisitDump, "", opdVitalDump)

	// One row per visit, named by its newest source row.
	if strings.Contains(sqlText, "  (20, ") {
		t.Errorf("the superseded older vital row was staged on its own")
	}
	// weight 60.5 kg → 60500 g · height 1 cm dropped · HR 72 · BP 120/80 · 36.5 °C → 3650
	// · SpO2 from o2_sat 98 · RR blank · pain 3 · DTX 0 → NULL; then the text columns:
	// va_re 20/40, urinary 2, sl_cm 1.5.
	want := "(21, '2026-10-30', '001', 60500, NULL, 72, 120, 80, 3650, 98, NULL, 3, NULL, " +
		"NULL, '20/40', NULL, NULL, NULL, NULL, '2', NULL, NULL, '1.5', true, " +
		"'อาการเบื้องต้น (จากแบบสัญญาณชีพเดิม)\nChief complaint:test\n\nPresent illness:test')"
	if !strings.Contains(sqlText, want) {
		t.Errorf("vital row mapped wrongly,\nwant %s\nin:\n%s", want, head(sqlText, 3000))
	}
	// The untouched form of visit 3 writes nothing at all.
	if strings.Contains(sqlText, "  (22, ") {
		t.Errorf("a vital form with nothing typed into it was staged")
	}
	if !hasText(rep.Notes, "height 1") {
		t.Errorf("the dropped out-of-range height is not reported: %v", rep.Notes)
	}
}

func TestOpdVitalOfAnAmbiguousVisitIsDropped(t *testing.T) {
	// V2 names a vital by (date, VN) only. When two active visits share that pair, the
	// vital could belong to either patient, so it is not written to either.
	visits := "INSERT INTO `patient_visit_mater` (`id`, `visitdate`, `VN`, `HN`, `user_id`, `create_at`, `active`) VALUES\n" +
		"(1, '2026-10-30 00:00:00', '001', '016900002', 0, '2026-10-30 01:00:00', '1'),\n" +
		"(2, '2026-10-30 00:00:00', '001', '016900009', 0, '2026-10-30 02:00:00', '1');\n"
	sqlText, rep := opdBuild(t, visits, "", opdVitalDump)
	if strings.Contains(sqlText, "  (21, ") || strings.Contains(sqlText, "  (20, ") {
		t.Errorf("a vital whose visit names two patients was staged")
	}
	e := rep.Entities[EntOpdNote]
	if e == nil || e.Skipped["vital: วันที่+VN ตรงกับคนไข้มากกว่า 1 คน — ไม่รู้ว่าเป็นของใคร"] != 2 {
		t.Errorf("the ambiguous vitals were not counted as skipped: %+v", e)
	}
}

func TestOpdNoteSQLOnlyWritesIntoVisitsItOpened(t *testing.T) {
	sqlText, _ := opdBuild(t, opdVisitDump, opdNoteDump, opdVitalDump)

	for _, want := range []string{
		// the three refusals of C1, plus the branch the visit needs
		"ไม่มีในทะเบียนคนไข้ของคลินิกนี้",
		"ไม่มีสาขา จึงเปิด visit ให้ไม่ได้",
		"ตัวออกเลข VN เลือกเลขที่ V4 ใช้ไปแล้ว",
		"note ผูก visit ไม่ได้",
		// a note binds only to a visit this lane opened, never to a signed one
		"AND t.title = 'นำเข้าจากระบบเดิม'",
		"AND t.signed_at IS NULL",
		"AND t.created_at = (n.d::timestamp AT TIME ZONE 'Asia/Bangkok')",
		// a deleted visit's VN still counts as used
		"FROM opd_visit_deletion x",
		// numbers past 999 are not cut by lpad
		"WHEN fr.n < 1000 THEN lpad(fr.n::text, 3, '0') ELSE fr.n::text END",
		// insert-only on every unique key
		`ON CONFLICT (clinic_code, visit_date, "VN") DO NOTHING`,
		"ON CONFLICT (clinic_code, visit_id) DO NOTHING",
		// the symptom text touches only a record this run created
		"WHERE opd_record.id IN (SELECT id FROM opd_ins)",
	} {
		if !strings.Contains(sqlText, want) {
			t.Errorf("generated SQL lacks %q", want)
		}
	}
	if strings.Contains(sqlText, "{{") {
		t.Errorf("a template placeholder was left in the SQL")
	}
	if strings.Contains(sqlText, "UPDATE visit_master") || strings.Contains(sqlText, "DELETE FROM visit_master") ||
		strings.Contains(sqlText, "DELETE FROM opd_record") {
		t.Errorf("the lane must never rewrite or delete an existing visit or note")
	}
}

func TestOpdNoteIsAGroupOfItsOwnAfterPatients(t *testing.T) {
	ents, err := entitiesForGroups([]string{"opdnote"})
	if err != nil || !ents[EntOpdNote] || len(ents) != 1 {
		t.Fatalf("group opdnote resolves to %v, %v", ents, err)
	}
	if !slices.Contains(GroupNames(), "opdnote") {
		t.Errorf("opdnote is not listed among the selectable groups")
	}
	if slices.Index(order, EntOpdNote) < slices.Index(order, EntPatient) {
		t.Errorf("opd_note is ordered before patient; its visits need the patient's branch")
	}

	in := Input{
		ClinicCode:  "CTEST01",
		Groups:      []string{"opdnote"},
		VisitDump:   []byte(opdVisitDump),
		OpdNoteDump: []byte(opdNoteDump),
		DoctorDump:  []byte(opdDoctorDump),
	}
	script, _, err := BuildScript(in)
	if err != nil {
		t.Fatalf("BuildScript: %v", err)
	}
	// One entity, so one transaction: the passes share temporary tables that drop at commit.
	if got := strings.Count(script, "\nCOMMIT;"); got != 1 {
		t.Errorf("an opdnote-only script commits %d times, want 1", got)
	}
	if !strings.Contains(script, "-- ── opd_note") {
		t.Errorf("the script has no opd_note section")
	}
}

// 016500001 is a real case (soel, 23 Oct 2026): a V2 visit whose HN V2's own patient table
// does not carry. It is skipped and named instead of stopping the whole lane at the HN
// guard; the patient who does exist keeps every record.
const opdOrphanVisitDump = "INSERT INTO `patient_visit_mater` (`id`, `visitdate`, `VN`, `HN`, `user_id`, `create_at`, `active`) VALUES\n" +
	"(1, '2026-10-30 00:00:00', '001', '016900002', 391, '2026-10-30 01:41:40', '1'),\n" +
	"(4, '2026-10-30 00:00:00', '003', '016500001', 391, '2026-10-30 03:00:00', '1');\n"

const opdOrphanNoteDump = "INSERT INTO `patient_opd_note` (`id`, `HN`, `visitDate`, `chief_complaint`, `presen_illness`, `physical_exam`, `diagnosis_impression`, `management`, `advice`, `user_id`, `created_at`, `update_at`) VALUES\n" +
	"(10, '016900002', '2026-10-30 00:00:00', 'cc one', '', '', '', '', '', '391', NULL, NULL),\n" +
	"(13, '016500001', '2026-10-30 00:00:00', 'cc orphan', '', '', '', '', '', '391', NULL, NULL);\n"

const opdOrphanVitalDump = "INSERT INTO `patient_vital_sign` (`id`, `visitdate`, `VN`, `weight`, `heart_rate`) VALUES\n" +
	"(21, '2026-10-30 00:00:00', '001', 60, 72),\n" +
	"(30, '2026-10-30 00:00:00', '003', 55, 80);\n"

func TestOpdNoteSkipsAnHNThatV2HasNoPatientFor(t *testing.T) {
	known := map[string]bool{"016900002": true}
	sqlText, rep := opdBuildKnown(t, known, opdOrphanVisitDump, opdOrphanNoteDump, opdOrphanVitalDump)

	if strings.Contains(sqlText, "'016500001'") || strings.Contains(sqlText, "cc orphan") || strings.Contains(sqlText, "  (30, ") {
		t.Errorf("a record of an HN with no V2 patient reached the script:\n%s", head(sqlText, 3000))
	}
	for _, want := range []string{"(1, '2026-10-30', '001', '016900002', '1000')", "cc one", "  (21, "} {
		if !strings.Contains(sqlText, want) {
			t.Errorf("the patient who exists lost a record, want %q in:\n%s", want, head(sqlText, 3000))
		}
	}
	e := rep.Entities[EntOpdNote]
	for reason, want := range map[string]int{
		"visit: HN ไม่มีในทะเบียนคนไข้ของ V2":                   1,
		"note: HN ไม่มีในทะเบียนคนไข้ของ V2":                    1,
		"vital: เป็นของ visit ที่ HN ไม่มีในทะเบียนคนไข้ของ V2": 1,
	} {
		if e == nil || e.Skipped[reason] != want {
			t.Errorf("skip %q counted %d, want %d: %+v", reason, e.Skipped[reason], want, e)
		}
	}
	if !hasText(rep.Warnings, "HN 1 ราย") || !hasText(rep.Warnings, "016500001") {
		t.Errorf("the report does not name the skipped HN: %v", rep.Warnings)
	}
	// The SQL guard stays: an HN V2 does carry but V4 does not is still refused at run time.
	if !strings.Contains(sqlText, "ไม่มีในทะเบียนคนไข้ของคลินิกนี้") {
		t.Errorf("the HN guard was dropped from the script")
	}

	// A vital's (date, VN) shared with an orphan's visit still names two patients.
	shared := strings.Replace(opdOrphanVisitDump, "(4, '2026-10-30 00:00:00', '003'", "(4, '2026-10-30 00:00:00', '001'", 1)
	sqlText, rep = opdBuildKnown(t, known, shared, "", opdOrphanVitalDump)
	if strings.Contains(sqlText, "  (21, ") {
		t.Errorf("a vital whose (date, VN) is shared with an orphan's visit was given to the other patient")
	}
	if e := rep.Entities[EntOpdNote]; e == nil || e.Skipped["vital: วันที่+VN ตรงกับคนไข้มากกว่า 1 คน — ไม่รู้ว่าเป็นของใคร"] != 1 {
		t.Errorf("the shared vital was not counted as ambiguous: %+v", e)
	}
}

func TestOpdNoteWithNoPatientDumpLeavesTheHNToTheGuard(t *testing.T) {
	// nil = this run read no patient file and cannot tell an orphan from a patient, so
	// nothing is skipped and the script's HN guard decides against V4's patient table.
	sqlText, rep := opdBuildKnown(t, nil, opdOrphanVisitDump, opdOrphanNoteDump, opdOrphanVitalDump)
	for _, want := range []string{"'016500001'", "cc orphan", "  (30, "} {
		if !strings.Contains(sqlText, want) {
			t.Errorf("want %q staged when no patient file was read", want)
		}
	}
	if hasText(rep.Warnings, "016500001") {
		t.Errorf("an HN was reported skipped although no patient file was read: %v", rep.Warnings)
	}
	if !hasText(rep.Notes, "ไม่ได้อ่านไฟล์คนไข้") {
		t.Errorf("the report does not say the guard decides: %v", rep.Notes)
	}
}

func TestOpdNoteBuildScriptSkipsOrphansFromThePatientDump(t *testing.T) {
	in := Input{
		ClinicCode:    "CTEST01",
		Groups:        []string{"opdnote"},
		PatientDump:   []byte("INSERT INTO `patient` (`HN`, `branch_code`) VALUES ('016900002', 'B1');\n"),
		VisitDump:     []byte(opdOrphanVisitDump),
		OpdNoteDump:   []byte(opdOrphanNoteDump),
		VitalSignDump: []byte(opdOrphanVitalDump),
		DoctorDump:    []byte(opdDoctorDump),
	}
	script, rep, err := BuildScript(in)
	if err != nil {
		t.Fatalf("BuildScript: %v", err)
	}
	// Quoted: the bare HN is expected in the header, which repeats the report's warnings.
	if strings.Contains(script, "'016500001'") || strings.Contains(script, "cc orphan") {
		t.Errorf("BuildScript did not hand the patient dump's HNs to the OPD lane")
	}
	if !strings.Contains(script, "'016900002'") {
		t.Errorf("the patient in the dump lost its visit")
	}
	if !hasText(rep.Warnings, "016500001") {
		t.Errorf("the skipped HN is not in the report: %v", rep.Warnings)
	}
}
