package legacyimport

import (
	"strings"
	"testing"

	"system_cliv4/internal/legacydump"
)

// A patient row with no HN, and a file whose HN column is spelled differently, look
// identical from inside the loop: Row.Get is an exact map lookup, so both read as "".
// They need opposite outcomes, and getting that backwards is expensive either way --
// refusing a whole export over one bad row, or reporting success having written nothing.

func parsePatient(t *testing.T, dump string) ([]string, []legacydump.Row) {
	t.Helper()
	cols, rows, err := legacydump.Parse(dump, "patient")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cols, rows
}

func TestPatientRowWithoutHNIsSkippedAndTheRestStillImport(t *testing.T) {
	cols, rows := parsePatient(t, "INSERT INTO `patient` (`HN`, `Firstname`, `branch_code`) VALUES\n"+
		"('0169001', 'ก', '01'),\n"+
		"('', 'ข', '01'),\n"+ // empty string
		"(NULL, 'ค', '01'),\n"+ // SQL NULL: same thing to Get()
		"('   ', 'ง', '01'),\n"+ // whitespace only: Get() trims
		"('0169005', 'จ', '01');\n")

	rep := NewReport("MBU1")
	sqlText, err := buildPatient(cols, rows, Input{ClinicCode: "MBU1", DefaultBranch: "BR00001"}, rep)
	if err != nil {
		t.Fatalf("three unusable rows must not reject the file: %v", err)
	}

	// The two good rows are there and the three bad ones are not.
	for _, hn := range []string{"0169001", "0169005"} {
		if !strings.Contains(sqlText, "'"+hn+"'") {
			t.Errorf("HN %s was dropped along with the bad rows:\n%s", hn, head(sqlText, 400))
		}
	}
	for _, name := range []string{"'ข'", "'ค'", "'ง'"} {
		if strings.Contains(sqlText, name) {
			t.Errorf("a row with no HN was written anyway (%s): it has no identity to write", name)
		}
	}
	if n := strings.Count(sqlText, "INSERT INTO patient"); n != 2 {
		t.Errorf("expected 2 inserts, got %d", n)
	}

	// Silence would be the real bug: three patients are not in V4 and nobody said so.
	if !hasText(rep.Warnings, "ข้าม 3 แถว") {
		t.Errorf("the skipped rows were not reported: %v", rep.Warnings)
	}
}

func TestPatientFileWithNoHNAtAllIsRefusedAndNamesTheColumns(t *testing.T) {
	// `hn` differs from `HN` only by case, which is the shape a re-exported file takes.
	// Skipping every row would leave a run that "succeeded" with zero patients.
	cols, rows := parsePatient(t, "INSERT INTO `patient` (`hn`, `Firstname`, `patient_hn_old`, `Tel`) VALUES\n"+
		"('0169001', 'ก', 'X1', '081'),\n"+
		"('0169002', 'ข', 'X2', '082');\n")

	_, err := buildPatient(cols, rows, Input{ClinicCode: "MBU1", DefaultBranch: "BR00001"}, NewReport("MBU1"))
	if err == nil {
		t.Fatal("a file where no row has an HN must be refused, not imported as zero patients")
	}
	msg := err.Error()

	// The message has to carry the fix. "empty HN" sent the operator looking at their data
	// for a blank cell that was never there.
	for _, want := range []string{
		"ไม่มีแถวไหนมี HN เลย", // what happened
		"2 แถว",            // how much was affected
		"`hn`",             // the column that is probably meant
		"`patient_hn_old`", // and the other candidate, so the guess is not silently made
		"Firstname",        // the full list, to recognise the right one
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not tell the operator %q:\n%s", want, msg)
		}
	}
}

func TestAnEmptyPatientFileIsNotTreatedAsAMissingColumn(t *testing.T) {
	// No rows means nothing to say about columns. Refusing here would block a run whose
	// patient file is legitimately empty.
	cols, rows := parsePatient(t, "INSERT INTO `patient` (`HN`, `Firstname`) VALUES ('0169001', 'ก');\n")
	if _, err := buildPatient(cols, rows[:0], Input{ClinicCode: "MBU1", DefaultBranch: "BR00001"},
		NewReport("MBU1")); err != nil {
		t.Errorf("an empty row set is not a broken file: %v", err)
	}
}

func TestDuplicateHNStillRejectsTheWholeFile(t *testing.T) {
	// Unchanged on purpose. A missing HN is one row that cannot be written; two rows
	// claiming ONE HN is two records claiming one person, and picking either merges two
	// people's history. That is not something an importer may decide.
	cols, rows := parsePatient(t, "INSERT INTO `patient` (`HN`, `Firstname`) VALUES\n"+
		"('0169001', 'ก'),\n('0169001', 'ข');\n")

	_, err := buildPatient(cols, rows, Input{ClinicCode: "MBU1", DefaultBranch: "BR00001"}, NewReport("MBU1"))
	if err == nil || !strings.Contains(err.Error(), "duplicate HN") {
		t.Errorf("a duplicate HN must still reject the file, got: %v", err)
	}
}

func hasText(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// A patient dump carries legacy users.id in user_insert, and those are translated through
// import_user_map. The map is only emitted when a users dump was parsed -- so a run that
// imports patients WITHOUT one produces thousands of lookups into a table that never
// exists. psql only finds out after the whole file has been uploaded and started.

func TestPatientsWithoutAUsersDumpIsRefusedRatherThanEmittingABrokenScript(t *testing.T) {
	in := Input{
		ClinicCode:    "MBU1",
		DefaultBranch: "BR00001",
		Groups:        []string{"patients"},
		PatientDump: []byte("INSERT INTO `patient` (`HN`, `Firstname`, `user_insert`) VALUES\n" +
			"('0169001', 'ก', 391),\n('0169002', 'ข', 392);\n"),
	}
	_, _, err := BuildScript(in)
	if err == nil {
		t.Fatal("a patients run with no users dump must be refused: the script it would " +
			"produce reads import_user_map without creating it")
	}
	// The message has to name the fix. "relation does not exist" from psql does not.
	for _, want := range []string{"-users", "2 จุด"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not tell the operator %q: %v", want, err)
		}
	}
}

func TestPatientsWithAUsersDumpStillWorksWithoutImportingAccounts(t *testing.T) {
	// The documented path: pass the users dump so the map can be built, but do not import
	// the accounts. A clinic whose staff already exist is a normal thing to migrate into.
	in := Input{
		ClinicCode:    "MBU1",
		OwnerUsername: "admin",
		DefaultBranch: "BR00001",
		Groups:        []string{"patients"},
		UsersDump:     usersFixture(),
		PatientDump: []byte("INSERT INTO `patient` (`HN`, `Firstname`, `user_insert`) VALUES\n" +
			"('0169001', 'ก', 391);\n"),
	}
	script, _, err := BuildScript(in)
	if err != nil {
		t.Fatalf("patients-only with a users dump must build: %v", err)
	}
	if !strings.Contains(script, "CREATE TEMP TABLE import_user_map") {
		t.Error("the map was not emitted, so the lookups have nothing to read")
	}
	if strings.Contains(script, "INSERT INTO users (") {
		t.Error("accounts were written by a run that only asked for patients")
	}
	// Not strict: this run did not write the accounts, so an unresolved person is a legacy
	// reference to somebody who was never migrated, not a failure.
	if strings.Contains(script, "RAISE EXCEPTION 'users: มีบัญชีที่หา id ใหม่ไม่ได้") {
		t.Error("a patients-only run must not be blocked over staff it never imported")
	}
}

func TestTheInvariantCatchesUseBeforeCreate(t *testing.T) {
	// Direct on the checker, because assembling a script with the phases in the wrong order
	// is not something BuildScript can be talked into -- and the check has to keep holding
	// if some future phase is inserted in the wrong place.
	if err := checkUserMapIsCreatedBeforeUse("SELECT 1;\n"); err != nil {
		t.Errorf("a script that never touches the map must pass: %v", err)
	}
	if err := checkUserMapIsCreatedBeforeUse(
		"SELECT m.new_id FROM import_user_map m;\nCREATE TEMP TABLE import_user_map (x int);\n"); err == nil {
		t.Error("a lookup before the CREATE must be refused: every one above it would fail")
	}
	if err := checkUserMapIsCreatedBeforeUse(
		"CREATE TEMP TABLE import_user_map (x int);\nSELECT m.new_id FROM import_user_map m;\n"); err != nil {
		t.Errorf("the correct order must pass: %v", err)
	}
}
