package legacyimport

import (
	"encoding/json"
	"strings"
	"testing"

	"system_cliv4/internal/legacydump"
)

// The branch survey reports which legacy users.code values -owner would accept, so the
// web form can fill owner_username as soon as the branches are read (owner's request,
// 7 Oct 2026). The list is ownerCandidates — the exact list the import's own refusal
// prints — so a code offered here can never be one the import then turns down.

func surveyUsers() []byte {
	return []byte("INSERT INTO `users` (`id`, `code`, `name`, `email`) VALUES\n" +
		"(3, 'sorawit', 'Sorawit', 'sorawit@x.co'),\n" +
		"(1, 'Admin', 'administrator', 'admin@x.co'),\n" +
		"(7, '1010', 'Staff ten', 's10@x.co'),\n" +
		"(8, '1002', 'Staff two', 's2@x.co'),\n" +
		"(9, '', 'No code', 'nc@x.co'),\n" +
		"(NULL, 'ghost', 'No id', 'g@x.co'),\n" +
		"(10, 'Admin', 'duplicate code', 'dup@x.co');\n")
}

func TestSurveyListsTheOwnerCandidatesTheImportWouldAccept(t *testing.T) {
	s, err := SurveyBranches(Input{BranchDump: branchRegistry(), UsersDump: surveyUsers()})
	if err != nil {
		t.Fatalf("SurveyBranches: %v", err)
	}
	// Numbers numerically, then text; no blank code, no row without an id, no repeat.
	want := []string{"1002", "1010", "Admin", "sorawit"}
	if strings.Join(s.OwnerCandidates, ",") != strings.Join(want, ",") {
		t.Fatalf("owner_candidates = %v, want %v", s.OwnerCandidates, want)
	}
	// The case is the dump's own: the form shows what the file says, and -owner matches
	// without regard to case anyway.
	for _, c := range s.OwnerCandidates {
		if c == "ghost" {
			t.Error("a row with no id was offered — the import would refuse it as owner")
		}
	}
	// Every offered code really is one -owner accepts.
	_, rows, err := legacydump.Parse(string(surveyUsers()), "users")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.OwnerCandidates {
		if !hasOwner(rows, c) {
			t.Errorf("%q was offered but hasOwner rejects it", c)
		}
	}
}

func TestSurveyWithoutAUsersTableOffersNoOwner(t *testing.T) {
	s, err := SurveyBranches(Input{BranchDump: branchRegistry()})
	if err != nil {
		t.Fatalf("SurveyBranches: %v", err)
	}
	if len(s.OwnerCandidates) != 0 {
		t.Errorf("no users table, yet candidates were reported: %v", s.OwnerCandidates)
	}
	// And the JSON the web reads leaves the field out rather than sending an empty list,
	// so "not read" and "read, nobody usable" stay distinguishable.
	body, _ := json.Marshal(s)
	if strings.Contains(string(body), "owner_candidates") {
		t.Errorf("owner_candidates is in the JSON although no users table was given: %s", body)
	}
}

func TestSurveyJSONCarriesTheCandidatesUnderTheNameTheWebReads(t *testing.T) {
	s, err := SurveyBranches(Input{BranchDump: branchRegistry(), UsersDump: surveyUsers()})
	if err != nil {
		t.Fatalf("SurveyBranches: %v", err)
	}
	body, _ := json.Marshal(s)
	if !strings.Contains(string(body), `"owner_candidates":["1002","1010","Admin","sorawit"]`) {
		t.Errorf("the web reads survey.owner_candidates; the JSON says: %s", body)
	}
}
