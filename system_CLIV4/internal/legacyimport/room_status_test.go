package legacyimport

import (
	"strings"
	"testing"

	"system_cliv4/internal/legacydump"
)

// Three rules settled on the first multi-branch tenant, where the source had closed one
// whole branch's rooms while its appointment history still referenced them:
//
//  1. a closed room that some appointment books is imported ACTIVE, because V4's
//     validate_appointment_assignment RAISES on a room with status 0 and takes the entire
//     one-transaction import down with it;
//  2. doctor.branch_code comes from the branch map, not the legacy "01" constant;
//  3. an appointment whose branch disagrees with its room's is counted, because the
//     trigger silently overwrites branch_code with the room's.
//
// Each is a decision about clinic data, so each is pinned here rather than left to the next
// reader of the generated SQL.

func parseTable(t *testing.T, table, dump string) []legacydump.Row {
	t.Helper()
	_, rows, err := legacydump.Parse(dump, table)
	if err != nil {
		t.Fatalf("parse %s: %v", table, err)
	}
	return rows
}

// twoBranchInput mirrors the real shape: V4 owns the branch codes, the legacy dump carries
// "01"/"02", and the caller supplies the map that POST /v3/import/clinics returned.
func twoBranchInput() Input {
	return Input{
		ClinicCode: "CTEST01",
		BranchMap:  map[string]string{"01": "BR00001", "02": "BR00002"},
	}
}

const roomDump = "INSERT INTO `clinic` (`code`, `tha_name`, `branch_code`, `active`, `room_number`) VALUES\n" +
	"('01', 'treatment room1', '01', '0', '1'),\n" + // closed, BOOKED below
	"('02', 'treatment room2', '01', '0', '2'),\n" + // closed, never booked
	"('03', 'treatment room1 siam', '02', '1', '1'),\n" + // open, booked
	"('07', 'treatment room3', '01', '0', '3');\n" // closed, never booked

// One booking in room 01 (closed at branch 01) and one in room 03 (open at branch 02).
// The second row's own branch_code says 01 while its room sits at 02 — that is the
// disagreement the trigger resolves in the room's favour.
const apptDump = "INSERT INTO `appointment` (`id`, `HN`, `patientname`, `clinic_code`, `branch_code`, `appointment_date`, `appointment_time_start`, `appointment_time_end`, `confirm_check`, `datetime_stamp`) VALUES\n" +
	"('1', '016500001', 'ก ข', '01', '01', '2022-09-15', '13:30:00', '14:00:00', '10', '2022-09-15 13:13:34'),\n" +
	"('2', '016500002', 'ค ง', '03', '01', '2023-01-10', '09:00:00', '09:30:00', '10', '2023-01-10 08:00:00');\n"

func TestClosedRoomWithAppointmentsIsImportedActive(t *testing.T) {
	in := twoBranchInput()
	rooms := parseTable(t, "clinic", roomDump)
	booked := roomsWithAppointments(parseTable(t, "appointment", apptDump))

	if !booked["01"] || !booked["03"] || booked["02"] || booked["07"] {
		t.Fatalf("roomsWithAppointments read the wrong column: got %v, want just 01 and 03", booked)
	}

	rep := NewReport(in.ClinicCode)
	sqlText, err := buildRoom(rooms, booked, in, rep)
	if err != nil {
		t.Fatalf("buildRoom: %v", err)
	}

	// Room 01: closed in the source, booked, so it must go in ACTIVE. The status is the
	// 7th value of the tuple, which is easier to pin by checking the whole statement for
	// the room than by counting commas.
	stmt01 := statementForRoom(sqlText, "'01'")
	if stmt01 == "" {
		t.Fatalf("room 01 was not written at all:\n%s", head(sqlText, 600))
	}
	if !strings.Contains(stmt01, ", 1, 1, ") { // sort_order 1, status 1
		t.Errorf("room 01 is booked by an appointment but was imported closed — every appointment naming it will fail the whole import:\n%s", stmt01)
	}

	// Room 02 and 07: closed and unbooked. Nothing asked for them to be reopened, so they
	// keep the source's answer. Reopening rooms a clinic closed, for no reason, is its own
	// bug.
	for _, code := range []string{"'02'", "'07'"} {
		stmt := statementForRoom(sqlText, code)
		if stmt == "" {
			t.Fatalf("room %s was not written at all", code)
		}
		if !strings.Contains(stmt, ", 0,\n") && !strings.Contains(stmt, ", 0, ") {
			t.Errorf("room %s is closed in the source and books nothing — it must stay closed:\n%s", code, stmt)
		}
	}

	// Silence here is the failure mode: the clinic closed that room and the import
	// overrode it. The room has to be named, and the warning has to say why.
	if !hasText(rep.Warnings, "ถูกนำเข้าเป็นห้องที่ใช้งานได้") {
		t.Errorf("reopening a closed room was not reported: %v", rep.Warnings)
	}
	if !hasText(rep.Warnings, "treatment room1") {
		t.Errorf("the warning does not name the room that was reopened: %v", rep.Warnings)
	}
	// Only room 01 was reopened, so a room that stayed closed must not be in the list.
	if hasText(rep.Warnings, "treatment room2") {
		t.Errorf("a room that was left closed is reported as reopened: %v", rep.Warnings)
	}
}

func TestRoomStatusIsUntouchedWhenNoAppointmentsAreImported(t *testing.T) {
	in := twoBranchInput()
	rooms := parseTable(t, "clinic", roomDump)

	rep := NewReport(in.ClinicCode)
	sqlText, err := buildRoom(rooms, map[string]bool{}, in, rep)
	if err != nil {
		t.Fatalf("buildRoom: %v", err)
	}
	// With no appointment dump there is nothing to protect, so all three closed rooms stay
	// closed — a rooms-only run must not quietly change the clinic's configuration.
	if n := strings.Count(sqlText, ", 0,"); n < 3 {
		t.Errorf("expected the 3 closed rooms to stay closed, found %d status-0 values:\n%s", n, head(sqlText, 900))
	}
	if hasText(rep.Warnings, "ถูกนำเข้าเป็นห้องที่ใช้งานได้") {
		t.Errorf("nothing was reopened, so nothing should be reported: %v", rep.Warnings)
	}
}

func TestDoctorBranchComesFromTheBranchMapNotTheLegacyConstant(t *testing.T) {
	in := twoBranchInput()
	// The legacy doctor table has no branch column at all — these are its real ten.
	doctors := parseTable(t, "doctor", "INSERT INTO `doctor` (`doctor_code`, `doctorname_th`, `doctorname_en`, `doctornickname`, `phone_number`, `medical_license`, `color`, `doctor_user`, `doctor_active`, `doctor_user_id`) VALUES\n"+
		"('1005', 'หมอ ก', 'Dr A', '', '081', '', '#3c8dbc', 'a', '1', '392');\n")

	rep := NewReport(in.ClinicCode)
	sqlText, err := buildDoctor(doctors, in, rep)
	if err != nil {
		t.Fatalf("buildDoctor: %v", err)
	}

	// '01' is a V2 code. In a tenant whose branches are BR00001/BR00002 it names no branch
	// that exists, and doctor.branch_code is what the calendar buckets providers by, so the
	// provider lands in no branch's roster.
	if strings.Contains(sqlText, "'CTEST01', '01', now()") {
		t.Errorf("doctor.branch_code still carries the legacy code '01':\n%s", head(sqlText, 700))
	}
	if !strings.Contains(sqlText, "'CTEST01', 'BR00001', now()") {
		t.Errorf("doctor.branch_code did not resolve through the branch map:\n%s", head(sqlText, 700))
	}
	// The source says nothing about where a doctor works, so the default is a guess and the
	// operator has to be told it was made.
	if !hasText(rep.Notes, "ไม่มีรหัสสาขาในต้นทาง") {
		t.Errorf("filling in a branch the source never stated was not reported: %v", rep.Notes)
	}
}

func TestAppointmentReportsBranchesTheRoomWillOverwrite(t *testing.T) {
	in := twoBranchInput()
	rooms := parseTable(t, "clinic", roomDump)
	appts := parseTable(t, "appointment", apptDump)

	rep := NewReport(in.ClinicCode)
	_, err := buildAppointment(appts, map[string]bool{}, map[string]string{},
		branchByRoom(rooms, in), in, rep)
	if err != nil {
		t.Fatalf("buildAppointment: %v", err)
	}

	// Booking 2 says branch 01 and books room 03, which is at BR00002. The trigger will
	// file it at BR00002 whatever this builder writes, so the count has to surface.
	if !hasText(rep.Warnings, "BR00001 → BR00002") {
		t.Errorf("an appointment whose branch the trigger will overwrite was not reported: %v", rep.Warnings)
	}
	// Booking 1 agrees with its room (both branch 01), so it must not be counted.
	if hasText(rep.Warnings, "2 นัด สาขาที่ได้") {
		t.Errorf("a booking that agrees with its room was counted as overridden: %v", rep.Warnings)
	}
}

func TestAppointmentSaysNothingWhenNoRoomBranchIsKnown(t *testing.T) {
	in := twoBranchInput()
	appts := parseTable(t, "appointment", apptDump)

	rep := NewReport(in.ClinicCode)
	// No room dump in this run: the builder cannot know where the rooms are, and guessing
	// would produce a warning about a disagreement nobody has established.
	if _, err := buildAppointment(appts, map[string]bool{}, map[string]string{}, nil, in, rep); err != nil {
		t.Fatalf("buildAppointment: %v", err)
	}
	if hasText(rep.Warnings, "ไม่ตรงกับสาขาของห้องที่จอง") {
		t.Errorf("with no room branches known, no disagreement can be claimed: %v", rep.Warnings)
	}
}

// statementForRoom returns the single INSERT statement that writes the given room code.
func statementForRoom(sqlText, quotedCode string) string {
	for _, stmt := range strings.Split(sqlText, ";\n") {
		if strings.Contains(stmt, "INSERT INTO room") &&
			strings.Contains(stmt, "'CTEST01', 'BR000") &&
			strings.Contains(stmt, ", "+quotedCode+", ") {
			return stmt
		}
	}
	return ""
}
