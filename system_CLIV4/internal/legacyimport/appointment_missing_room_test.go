package legacyimport

import (
	"strings"
	"testing"
)

// A booking whose room code has NO ROW in the room master at all.
//
// This is not the closed-room case room_status_test.go pins — buildRoom already reopens a
// room with status 0 when something books it, because status 0 is still a row to reopen.
// Here there is nothing to reopen, and V4's validate_appointment_assignment RAISES:
//
//	psql:<stdin>:3801: ERROR:  room is not available for appointment assignment
//	CONTEXT:  PL/pgSQL function validate_appointment_assignment() line 66 at RAISE
//
// Measured on master-clinic (C9C89A4), 6 Oct 2026. The room master holds R1..R5; of its
// 2,604 appointments, 2,422 book R1, 86 R2, 49 R3, 14 R5, 1 R4, 31 book no room — and ONE
// names "01". Its own doc records the room's name at the time as "ห้องตรวจ 1" while R1 is
// now "ห้องพิเศษ 1", so the clinic renumbered and renamed its rooms and a 2023 booking kept
// the old code. One row out of 2,604 failed a 241,181-line script.
//
// Owner's decision ข (6 Oct 2026): keep the appointment, clear room_id, record the
// original in doc.legacy_room_id — exactly what already happens to a doctor_code V4
// refuses.

// A room master holding only R1, and three bookings: one in R1, one in a code that does
// not exist, one with no room at all.
const missingRoomDump = "INSERT INTO `clinic` (`code`, `tha_name`, `branch_code`, `active`, `room_number`) VALUES\n" +
	"('R1', 'ห้องพิเศษ 1', '01', '1', '1');\n"

const missingRoomApptDump = "INSERT INTO `appointment` (`id`, `HN`, `patientname`, `clinic_code`, `branch_code`, `appointment_date`, `appointment_time_start`, `appointment_time_end`, `confirm_check`, `datetime_stamp`, `clinic_name`) VALUES\n" +
	"('1', '016600001', 'เทส', '01', '01', '2023-08-17', '15:00:00', '15:30:00', '10', '2023-08-17 14:12:06', 'ห้องตรวจ 1'),\n" +
	"('2', '016600002', 'สอง', 'R1', '01', '2023-08-23', '17:30:00', '18:00:00', '10', '2023-08-23 17:00:00', 'ห้องพิเศษ 1'),\n" +
	"('3', '016600003', 'สาม', '', '01', '2023-08-24', '09:00:00', '09:30:00', '10', '2023-08-24 08:00:00', '');\n"

func TestAppointmentKeepsTheBookingAndClearsARoomThatHasNoRow(t *testing.T) {
	in := twoBranchInput()
	rooms := parseTable(t, "clinic", missingRoomDump)
	appts := parseTable(t, "appointment", missingRoomApptDump)

	rep := NewReport(in.ClinicCode)
	sqlText, err := buildAppointment(appts, map[string]bool{}, map[string]string{},
		branchByRoom(rooms, in), in, rep)
	if err != nil {
		t.Fatalf("buildAppointment: %v", err)
	}

	// Every booking is still written. Losing the row is what this avoids.
	for _, ref := range []string{"'1'", "'2'", "'3'"} {
		if !strings.Contains(sqlText, ", "+ref+", ") {
			t.Errorf("booking %s was dropped instead of having its room cleared:\n%s", ref, head(sqlText, 900))
		}
	}
	if got := strings.Count(sqlText, "INSERT INTO appointment"); got != 3 {
		t.Errorf("wrote %d appointments, want 3", got)
	}

	// Booking 1's room is gone from the column and recorded in doc.
	if strings.Contains(sqlText, "'01', 'BR00001', '2023-08-17") {
		t.Errorf("room_id '01' reached the script — the trigger would refuse it:\n%s", head(sqlText, 900))
	}
	if !strings.Contains(sqlText, `legacy_room_id`) {
		t.Errorf("the original room code was not recorded in doc:\n%s", head(sqlText, 1200))
	}
	if !strings.Contains(sqlText, `\"legacy_room_id\":\"01\"`) &&
		!strings.Contains(sqlText, `"legacy_room_id":"01"`) {
		t.Errorf("doc.legacy_room_id does not carry the code the dump said:\n%s", head(sqlText, 1200))
	}

	// Booking 2 keeps its room, because R1 is in the master.
	if !strings.Contains(sqlText, "'R1'") {
		t.Errorf("a booking in a room that DOES exist lost its room_id:\n%s", head(sqlText, 900))
	}

	// Reported, loudly, with the code and the count.
	if !hasText(rep.Warnings, "อ้างรหัสห้องที่ไม่มีอยู่ในตารางห้องของต้นทาง") {
		t.Errorf("clearing a room_id was not reported: %v", rep.Warnings)
	}
	if !hasText(rep.Warnings, "01 (1 นัด)") {
		t.Errorf("the report does not name the code and how many bookings it cost: %v", rep.Warnings)
	}
	// The consequence an operator would otherwise discover from a report that reads wrong.
	if !hasText(rep.Warnings, "branch_code = NULL") {
		t.Errorf("the report does not say these bookings end up with no branch: %v", rep.Warnings)
	}
	// And how to find them again.
	if !hasText(rep.Notes, "doc ? 'legacy_room_id'") {
		t.Errorf("the report does not say how to list the affected rows: %v", rep.Notes)
	}
}

func TestAppointmentSaysNothingWhenEveryRoomExists(t *testing.T) {
	in := twoBranchInput()
	rooms := parseTable(t, "clinic", roomDump) // holds 01, 02, 03, 07
	appts := parseTable(t, "appointment", apptDump)

	rep := NewReport(in.ClinicCode)
	if _, err := buildAppointment(appts, map[string]bool{}, map[string]string{},
		branchByRoom(rooms, in), in, rep); err != nil {
		t.Fatalf("buildAppointment: %v", err)
	}
	if hasText(rep.Warnings, "อ้างรหัสห้องที่ไม่มีอยู่") {
		t.Errorf("reported a missing room when every code is in the master: %v", rep.Warnings)
	}
}

// THE NIL CASE, WHICH IS THE ONE THAT COULD DO REAL DAMAGE.
//
// branchOfRoom is nil when this run never read the room dump — an appointments-only
// import. "I cannot tell which rooms exist" must not clear every room_id in the file, the
// same distinction the orphan-HN rule makes between a nil set and an empty one.
func TestAppointmentKeepsEveryRoomWhenTheRoomMasterWasNotRead(t *testing.T) {
	in := twoBranchInput()
	appts := parseTable(t, "appointment", missingRoomApptDump)

	rep := NewReport(in.ClinicCode)
	sqlText, err := buildAppointment(appts, map[string]bool{}, map[string]string{}, nil, in, rep)
	if err != nil {
		t.Fatalf("buildAppointment: %v", err)
	}
	if hasText(rep.Warnings, "อ้างรหัสห้องที่ไม่มีอยู่") {
		t.Errorf("claimed a room is missing without having read the room master: %v", rep.Warnings)
	}
	if strings.Contains(sqlText, "legacy_room_id") {
		t.Errorf("cleared a room_id with no room master to check it against:\n%s", head(sqlText, 900))
	}
	// Both real codes survive untouched.
	for _, want := range []string{"'01'", "'R1'"} {
		if !strings.Contains(sqlText, want) {
			t.Errorf("room_id %s was not written through:\n%s", want, head(sqlText, 900))
		}
	}
}

// An EMPTY room master is not the same as no room master: it means the dump was read and
// holds no rooms, so every room code in the appointments is unbacked.
func TestAppointmentClearsEveryRoomWhenTheMasterIsEmpty(t *testing.T) {
	in := twoBranchInput()
	appts := parseTable(t, "appointment", missingRoomApptDump)

	rep := NewReport(in.ClinicCode)
	sqlText, err := buildAppointment(appts, map[string]bool{}, map[string]string{},
		map[string]string{}, in, rep)
	if err != nil {
		t.Fatalf("buildAppointment: %v", err)
	}
	if !hasText(rep.Warnings, "2 นัด อ้างรหัสห้องที่ไม่มีอยู่") {
		t.Errorf("an empty room master should leave both booked rooms unbacked: %v", rep.Warnings)
	}
	if !strings.Contains(sqlText, "legacy_room_id") {
		t.Errorf("no room code was recorded in doc:\n%s", head(sqlText, 900))
	}
	// Still three appointments: none of them is dropped.
	if got := strings.Count(sqlText, "INSERT INTO appointment"); got != 3 {
		t.Errorf("wrote %d appointments, want 3", got)
	}
}
