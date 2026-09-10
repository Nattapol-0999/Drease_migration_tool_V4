package legacyimport

import (
	"strings"
	"testing"
)

// A refusal in one group must not take down a group nobody asked about.
//
// This is a regression test for a failure found on a real clinic, not a hypothetical. The
// export's `users` table holds eight rows whose email column contains a person's name
// instead of an address, and those names collide. users.email is UNIQUE across the whole
// table in V4, so buildUsers refuses — correctly, and by an explicit decision: the numbers
// have to be fixed at the source rather than invented here.
//
// The bug was that BuildAll built EVERY entity and let the group filter drop the unwanted
// ones afterwards, so that refusal also killed `-only catalog,stock`, which does not write a
// single user. On the deployed box it did so after a 79-second parse of a 719 MB dump.
//
// The dumps below are the smallest thing that reproduces it: a users table with a duplicate
// email, and a room table that is perfectly fine.

const usersDumpWithDuplicateEmails = "" +
	"INSERT INTO `users` (`id`, `code`, `name`, `email`, `password`) VALUES\n" +
	"(410, 'chayuda', 'Chayuda A', 'Chayuda', '$2y$10$abc'),\n" +
	"(400, 'chayuda2', 'Chayuda B', 'Chayuda', '$2y$10$def');\n"

const roomDumpFine = "" +
	"INSERT INTO `clinic` (`code`, `tha_name`, `eng_name`, `branch_code`, `active`, `room_number`) VALUES\n" +
	"('01', 'ห้องตรวจ 1', 'Room 1', '01', '1', '1'),\n" +
	"('02', 'ห้องหัตถการ', 'Procedure', '02', '1', '2');\n"

func TestUnselectedGroupCannotFailTheRun(t *testing.T) {
	in := Input{
		ClinicCode: "C0TEST1",
		Groups:     []string{"rooms"},
		UsersDump:  []byte(usersDumpWithDuplicateEmails),
		RoomDump:   []byte(roomDumpFine),
		BranchMap:  map[string]string{"01": "BR00001", "02": "BR00002"},
	}
	rep := NewReport(in.ClinicCode)
	batches, _, _, err := BuildAll(in, rep)
	if err != nil {
		t.Fatalf("asked for rooms only, but the users table failed the run: %v", err)
	}
	if strings.TrimSpace(batches[EntRoom]) == "" {
		t.Error("rooms were selected but produced no SQL")
	}
	if _, built := batches[EntUsers]; built {
		t.Error("users were not selected but were built anyway")
	}
	if !strings.Contains(batches[EntRoom], "BR00001") {
		t.Error("the branch map was not applied to the rooms that were imported")
	}
}

func TestSelectingThatGroupStillRefuses(t *testing.T) {
	// The refusal itself is the behaviour the clinic owner chose. Narrowing its blast radius
	// must not have turned it off.
	in := Input{
		ClinicCode:    "C0TEST1",
		Groups:        []string{"people"},
		OwnerUsername: "chayuda",
		UsersDump:     []byte(usersDumpWithDuplicateEmails),
	}
	rep := NewReport(in.ClinicCode)
	if _, _, _, err := BuildAll(in, rep); err == nil {
		t.Fatal("people was selected and the emails collide, so this had to be refused")
	} else if !strings.Contains(err.Error(), "duplicate email") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

func TestOwnerIsOnlyRequiredWhenPeopleAreImported(t *testing.T) {
	// Naming the clinic's owner to import a price list is a question with no answer, and it
	// used to be mandatory for any SQL attachment.
	in := Input{
		ClinicCode: "C0TEST1",
		Groups:     []string{"rooms"},
		UsersDump:  []byte(usersDumpWithDuplicateEmails),
		RoomDump:   []byte(roomDumpFine),
	}
	rep := NewReport(in.ClinicCode)
	if _, _, _, err := BuildAll(in, rep); err != nil {
		t.Fatalf("rooms without an owner_username should be fine: %v", err)
	}

	// ...and still demanded when it is the one thing that cannot be guessed.
	in.Groups = []string{"people"}
	in.OwnerUsername = ""
	rep = NewReport(in.ClinicCode)
	if _, _, _, err := BuildAll(in, rep); err == nil {
		t.Error("importing people with no owner_username must be refused")
	}
}

func TestAppointmentsStillReadUsersAndPatientsTheyDoNotWrite(t *testing.T) {
	// The filter must not become "parse only what you write". An appointment needs the users
	// and doctor tables to know which doctor_code V4 will accept, and the patient table to
	// know each HN's branch. Skipping those parses would silently strip every doctor from
	// every booking and file them at the wrong branch — output that looks like data.
	patients := "INSERT INTO `patient` (`HN`, `branch_code`, `name`) VALUES\n" +
		"('016500001', '02', 'คนไข้ ก');\n"
	appts := "INSERT INTO `appointment` (`id`, `HN`, `patientname`, `clinic_code`, `doctor_code`, " +
		"`appointment_date`, `appointment_time_start`, `appointment_time_end`, `confirm_check`) VALUES\n" +
		"('1', '016500001', 'คนไข้ ก', '01', '1010', '2026-01-02', '10:00', '11:00', '1');\n"
	doctors := "INSERT INTO `doctor` (`doctor_code`, `doctor_name`, `doctor_user_id`, `doctor_active`) VALUES\n" +
		"('1010', 'หมอ ก', '410', '1');\n"

	in := Input{
		ClinicCode:      "C0TEST1",
		Groups:          []string{"appointments"},
		UsersDump:       []byte(usersDumpWithDuplicateEmails),
		DoctorDump:      []byte(doctors),
		PatientDump:     []byte(patients),
		AppointmentDump: []byte(appts),
		BranchMap:       map[string]string{"01": "BR00001", "02": "BR00002"},
		DefaultBranch:   "BR00001",
	}
	rep := NewReport(in.ClinicCode)
	batches, _, _, err := BuildAll(in, rep)
	if err != nil {
		t.Fatalf("appointments alone should build even though the users table is broken: %v", err)
	}
	sql := batches[EntAppointment]
	if strings.TrimSpace(sql) == "" {
		t.Fatal("appointments were selected but produced no SQL")
	}
	// The doctor reaches an active user, so V4 accepts the code and it must survive.
	if !strings.Contains(sql, "'1010'") {
		t.Error("doctor_code was dropped, which means the users/doctor tables were not read")
	}
	// Neither patients nor users are written by this selection.
	for _, ent := range []string{EntPatient, EntUsers} {
		if _, built := batches[ent]; built {
			t.Errorf("%s was built but not selected", ent)
		}
	}
}

func TestUnknownGroupIsRefusedBeforeAnythingIsParsed(t *testing.T) {
	in := Input{
		ClinicCode: "C0TEST1",
		Groups:     []string{"rooms", "definitely_not_a_group"},
		RoomDump:   []byte(roomDumpFine),
	}
	rep := NewReport(in.ClinicCode)
	if _, _, _, err := BuildAll(in, rep); err == nil {
		t.Fatal("an unknown group name must be refused, not silently ignored")
	}
}
