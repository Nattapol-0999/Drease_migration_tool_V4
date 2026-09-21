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

func TestCollidingLoginsAreRenamedRatherThanRefusing(t *testing.T) {
	// THIS TEST REPLACED TestSelectingThatGroupStillRefuses.
	//
	// The old behaviour was to refuse the whole clinic, citing users_email_key. The owner
	// reversed that on 18 Sep 2026 after a real source turned up with
	//
	//	id 405  email "Nuch"   status 1
	//	id 423  email "nuch"   status 0
	//
	// the same person twice on a utf8_bin column. Two reasons the refusal was wrong: V4's
	// users_email_key is a plain UNIQUE (email) under C.UTF-8, so it does not fold case and had
	// no objection to that pair at all; and refusing costs the clinic its entire import over two
	// staff rows. Skipping the second row was rejected too — a switched-off account can still be
	// the recorded author of a note or a bill.
	//
	// The fixture's two rows share an email EXACTLY, which is the case the old index message was
	// actually right about, so this covers both shapes.
	in := Input{
		ClinicCode:    "C0TEST1",
		Groups:        []string{"people"},
		OwnerUsername: "chayuda",
		UsersDump:     []byte(usersDumpWithDuplicateEmails),
	}
	rep := NewReport(in.ClinicCode)
	batches, _, _, err := BuildAll(in, rep)
	if err != nil {
		t.Fatalf("colliding logins must be renamed, not refused: %v", err)
	}

	// Both people survive. Losing one silently is the outcome this whole change exists to avoid.
	if n := strings.Count(batches[EntUsers], "INSERT INTO users "); n != 2 {
		t.Errorf("wrote %d user INSERTs, want 2 — both accounts have to be imported",
			strings.Count(batches[EntUsers], "INSERT INTO users "))
	}

	// The names themselves are decided in import_user_map, which Script() writes and BuildAll
	// does not return, so the decision is checked where it is made.
	mapSQL, _, mapRep := buildUsersSQL(t, in)

	// First row in the dump keeps the name. That is dump order, which is how the account that
	// has been logging in with it goes on doing so.
	if !strings.Contains(mapSQL, "'Chayuda'") {
		t.Errorf("the first claimant lost its login name — the earlier account should keep it:\n%s", mapSQL)
	}
	// The second is disambiguated by its legacy id, not by a counter: the id is unique in the
	// dump by definition and it is what an operator can look up in the old system.
	if !strings.Contains(mapSQL, "'Chayuda_400@gmail.com'") {
		t.Errorf("the second claimant was not renamed to its id-suffixed form:\n%s", mapSQL)
	}

	// And it must be SAID. A login name changed; somebody has to be told which.
	if !hasText(mapRep.Warnings, "Chayuda_400@gmail.com") {
		t.Errorf("the rename was not reported, so nobody could tell that person: %v", mapRep.Warnings)
	}
	if !hasText(mapRep.Warnings, "id=400") {
		t.Errorf("the report does not name the row that changed: %v", mapRep.Warnings)
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
