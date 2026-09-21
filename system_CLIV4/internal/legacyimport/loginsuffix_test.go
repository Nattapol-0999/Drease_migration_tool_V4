package legacyimport

import (
	"strings"
	"testing"
)

// What a renamed login is made of.
//
// A login name is the one thing in this importer a human has to TYPE, so the value is not
// allowed to be a surprise: it comes from the clinic's own name in the export, the same way on
// every run and every site of the same tenant. The tests below pin the parts of that which
// cannot be seen from the generated SQL.

// branchFixture is the shape the real export has, written OUT OF ORDER on purpose: the rows
// arrive in id order, so a branch added later can sit above an older one after an edit, and
// "first branch" has to mean the lowest branch_code rather than the first line in the file.
func branchFixture() []byte {
	return []byte("INSERT INTO `branch` (`id`, `branch_code`, `branch_name`, `branch_name_th`, `stock_id`) VALUES\n" +
		"(2, '02', 'The Gloss Clinic. Sriracha', 'เดอะกลอส ศรีราชา', 'stock2'),\n" +
		"(1, '01', 'The Gloss Clinic. CCO', 'เดอะกลอส ฉะเชิงเทรา', 'stock1');\n")
}

func TestFirstBranchNameIsTheLowestBranchCodeNotTheFirstLine(t *testing.T) {
	got, err := firstBranchName(Input{ClinicCode: "MBU1", BranchDump: branchFixture()})
	if err != nil {
		t.Fatalf("firstBranchName: %v", err)
	}
	// Branch 02 is written first in the file. Taking it would give the two sites of one tenant
	// different logins for the same person, which is the whole thing this ordering prevents.
	if got != "The Gloss Clinic. CCO" {
		t.Errorf("firstBranchName = %q, want the branch_code=01 name", got)
	}
}

func TestFirstBranchNameIsEmptyRatherThanAnErrorWhenThereIsNoBranchTable(t *testing.T) {
	// An export without the branch table is a normal export — the table is read for the branch
	// survey and now for this name, and neither is worth refusing an import over.
	got, err := firstBranchName(Input{ClinicCode: "MBU1"})
	if err != nil {
		t.Fatalf("a missing branch table must not be an error: %v", err)
	}
	if got != "" {
		t.Errorf("firstBranchName = %q, want empty", got)
	}
}

func TestSlugForLogin(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		// The measured name. The dot and the spaces collapse into single hyphens.
		{"The Gloss Clinic. CCO", "the-gloss-clinic-cco"},
		{"  Wanaka   Clinic  ", "wanaka-clinic"},
		// Thai-only yields nothing on purpose: a made-up romanisation would be a login nobody
		// was told about. The caller falls back to the clinic code.
		{"คลินิกความงาม", ""},
		{"", ""},
		// A name that is only punctuation is the same case as Thai-only.
		{"...", ""},
		// Leading symbols must not become a leading hyphen.
		{"@Gloss Clinic", "gloss-clinic"},
		// Mixed script keeps the part an email can hold.
		{"Gloss คลินิก 2", "gloss-2"},
		// Capped at 24 characters. The cut lands wherever 24 lands, mid-word included: a hard
		// ceiling is what keeps every rung of the ladder inside varchar(255), and a rule that
		// backed up to the previous word would sometimes give back almost nothing
		// ("ab-cdefghijklmnopqrstuvwxyz" would become "ab").
		{"abcdefghij klmnopqrstu vwxyz", "abcdefghij-klmnopqrstu-v"},
		// What IS re-trimmed is a hyphen the cut created, so a login cannot end on one.
		{"abcdefghijklmnopqrstuvw xyz", "abcdefghijklmnopqrstuvw"},
	} {
		if got := slugForLogin(c.in); got != c.want {
			t.Errorf("slugForLogin(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := slugForLogin(strings.Repeat("a", 40)); len(got) > 24 {
		t.Errorf("slugForLogin did not cap the length: %q", got)
	}
}

func TestLoginSuffixComesFromTheBranchNameAndSaysNothingWhenItWorked(t *testing.T) {
	suffix, why := loginSuffix(Input{ClinicCode: "MBU1", BranchDump: branchFixture()})
	if suffix != "the-gloss-clinic-cco" {
		t.Errorf("suffix = %q, want the slug of the first branch name", suffix)
	}
	// `why` is for fallbacks only. A note on the happy path would put a line in every report
	// saying nothing happened.
	if why != "" {
		t.Errorf("suffix came from the branch name but a reason was reported anyway: %q", why)
	}
}

func TestLoginSuffixFallsBackToTheClinicCodeAndSaysWhy(t *testing.T) {
	for _, c := range []struct {
		name string
		in   Input
	}{
		{"no branch table at all", Input{ClinicCode: "MBU1"}},
		{"branch table with no usable name", Input{ClinicCode: "MBU1",
			BranchDump: []byte("INSERT INTO `branch` (`branch_code`, `branch_name`) VALUES\n('01', '');\n")}},
		{"a Thai-only clinic name", Input{ClinicCode: "MBU1",
			BranchDump: []byte("INSERT INTO `branch` (`branch_code`, `branch_name`) VALUES\n('01', 'คลินิกความงาม');\n")}},
	} {
		suffix, why := loginSuffix(c.in)
		if suffix != "MBU1" {
			t.Errorf("%s: suffix = %q, want the clinic code", c.name, suffix)
		}
		// A silent fallback is a login nobody can explain later, so the reason is a return value
		// the caller has to report — not something swallowed here.
		if why == "" {
			t.Errorf("%s: fell back to the clinic code without saying why", c.name)
		}
	}
}

func TestUserMapUsesTheBranchNameAndWalksUpWhenThatFormIsTakenToo(t *testing.T) {
	mapSQL, _, rep := buildUsersSQL(t, Input{
		ClinicCode: "MBU1",
		UsersDump:  usersFixture(),
		BranchDump: branchFixture(),
	})

	// The renamed form carries the clinic's own name, not its generated code.
	if !strings.Contains(mapSQL, "'admin.the-gloss-clinic-cco@gmail.com'") {
		t.Errorf("the renamed login does not carry the branch name:\n%s", head(mapSQL, 900))
	}
	// Rung 0 is still the name as it was: nobody's login changes unless it had to.
	if !strings.Contains(mapSQL, "SELECT 0 AS n, 'admin'::text AS c") {
		t.Errorf("the original name is no longer the first choice:\n%s", head(mapSQL, 900))
	}
	// And the rungs above it are generated rather than assumed to be unnecessary.
	if !strings.Contains(mapSQL, "generate_series(2, 9)") {
		t.Errorf("the ladder above the renamed form is missing:\n%s", head(mapSQL, 900))
	}
	// Scoped to OTHER clinics on every rung. Without this a re-import would climb the ladder
	// and mint a second account for the same person on every run.
	if !strings.Contains(mapSQL, "COALESCE(u.clinic_code, '') <> 'MBU1'") {
		t.Errorf("the ladder is not scoped to other clinics:\n%s", head(mapSQL, 900))
	}

	// The report has to state the shape people will be asked to type, with the real suffix in
	// it — a report that says "<name>.<clinic>" teaches nobody anything.
	var said bool
	for _, n := range rep.Notes {
		if strings.Contains(n, "<ชื่อ>.the-gloss-clinic-cco@gmail.com") {
			said = true
		}
	}
	if !said {
		t.Errorf("the report does not state the renamed shape with the real suffix: %v", rep.Notes)
	}
}
