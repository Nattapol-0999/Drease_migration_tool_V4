package legacyimport

// The script commits at every group boundary instead of once at the end.
//
// WHY IT DOES
//
// PostgreSQL locks per RELATION — table, index, toast — and holds each lock until the
// transaction ends. One transaction over the 19 tables this script writes to, plus the 7 it
// touches with UPDATE/DELETE/ANALYZE, plus their indexes, goes past
// max_locks_per_transaction (64 by default). Measured against drease_temporary with a
// 6,439-patient export:
//
//	ERROR: out of shared memory
//	HINT:  You might need to increase max_locks_per_transaction
//	CONTEXT: INSERT INTO privacy_patient_hn_ownership … privacy_lock_patient_hn() line 49
//
// Splitting by row count does nothing for THIS problem: 500 patients hold the same relation
// locks as 6,439. Only cutting the number of TABLES per transaction helps.
//
// That sentence used to end "so row-count batching is pointless", and that was wrong. It is
// true of relation locks and false of the patient group, because privacy_lock_patient_hn
// takes a transaction-scoped ADVISORY lock per HN per row. Those accumulate with rows, so
// patient needs both cuts: this one, and the row-count one in patient_batch_test.go.
//
// WHAT THESE TESTS PROTECT
//
// The dangerous half of the change is not the COMMIT, it is the two SET LOCAL statements.
// They last exactly as long as their transaction. Miss one re-issue and nothing raises:
// app.clinic_code is what every FORCE-RLS policy matches on, so the policies match no rows
// and every INSERT after that commit writes ZERO while psql prints success for each one.
//
// So the invariant is arithmetic and it is checked as such: as many app.clinic_code settings
// as there are BEGINs, and as many COMMITs as BEGINs.

import (
	"strings"
	"testing"
)

// twoGroupInput is a run with users and patients — two groups, so at least two transactions.
func twoGroupInput() Input {
	return Input{
		ClinicCode:    "MBU1",
		DefaultBranch: "BR00001",
		OwnerUsername: "ease",
		Groups:        []string{"people", "patients"},
		UsersDump: []byte("INSERT INTO `users` (`id`, `code`, `email`, `name`) VALUES\n" +
			"(1, 'ease', 'ease', 'Ease'),\n(2, 'beer', 'beer', 'Beer');\n"),
		PatientDump: []byte("INSERT INTO `patient` (`HN`, `Firstname`, `user_insert`) VALUES\n" +
			"('0169001', 'ก', 1),\n('0169002', 'ข', 2);\n"),
	}
}

func TestEveryTransactionReissuesTheTenantSetting(t *testing.T) {
	script, _, err := BuildScript(twoGroupInput())
	if err != nil {
		t.Fatalf("BuildScript: %v", err)
	}

	begins := strings.Count(script, "\nBEGIN;")
	commits := strings.Count(script, "\nCOMMIT;")
	tenant := strings.Count(script, "SET LOCAL app.clinic_code")
	conforming := strings.Count(script, "SET LOCAL standard_conforming_strings")

	if begins < 2 {
		t.Fatalf("BEGIN count = %d, want at least 2 — a run with two groups has to split, "+
			"or the lock ceiling is still reachable", begins)
	}
	if commits != begins {
		t.Errorf("BEGIN = %d but COMMIT = %d — an unbalanced script leaves psql in a "+
			"transaction or commits one that was never opened", begins, commits)
	}
	// The whole point. A missing re-issue writes zero rows and says nothing.
	if tenant != begins {
		t.Errorf("BEGIN = %d but SET LOCAL app.clinic_code = %d — every transaction after the "+
			"first would run with the tenant GUC unset, and under the RLS-bound role every "+
			"INSERT in it writes 0 rows while psql reports success", begins, tenant)
	}
	if conforming != begins {
		t.Errorf("BEGIN = %d but SET LOCAL standard_conforming_strings = %d — legacy text is "+
			"full of backslashes and a transaction without it can mangle them", begins, conforming)
	}
}

// The map has to outlive the commits, or the six places that translate a person through it
// read a relation that was dropped underneath them.
func TestUserMapSurvivesTheGroupCommits(t *testing.T) {
	script, _, err := BuildScript(twoGroupInput())
	if err != nil {
		t.Fatalf("BuildScript: %v", err)
	}
	if !strings.Contains(script, "CREATE TEMP TABLE import_user_map") {
		t.Fatal("the map is not created at all")
	}
	if strings.Contains(script, "ON COMMIT DROP") {
		t.Error("import_user_map is ON COMMIT DROP: it disappears at the first group boundary, " +
			"and every later lookup into it fails on a missing relation")
	}
	if !strings.Contains(script, "ON COMMIT PRESERVE ROWS") {
		t.Error("import_user_map must say ON COMMIT PRESERVE ROWS — still TEMP, so it is dropped " +
			"when psql disconnects and nothing is left in the database")
	}
	// It still has to be created before anything reads it, commits or no commits.
	created := strings.Index(script, "CREATE TEMP TABLE import_user_map")
	firstRead := strings.Index(script, "FROM import_user_map")
	if firstRead >= 0 && firstRead < created {
		t.Error("something reads import_user_map before it is created")
	}
}

// The arithmetic has to hold for a small run too, and the boundary is per ENTITY rather than
// per group: "people" alone emits three of them (users, doctor, room), which is better for the
// lock ceiling than the group-level split that was asked for — each transaction touches fewer
// tables still. The count itself is not pinned, because it follows the entity list and would
// turn every future addition into a failing test for no reason. What is pinned is that the
// transactions stay balanced and every one of them carries the tenant setting.
func TestASmallRunIsStillBalanced(t *testing.T) {
	in := twoGroupInput()
	in.Groups = []string{"people"}
	in.PatientDump = nil
	script, _, err := BuildScript(in)
	if err != nil {
		t.Fatalf("BuildScript: %v", err)
	}
	begins := strings.Count(script, "\nBEGIN;")
	if begins < 1 {
		t.Fatalf("no transaction at all in:\n%s", script[:min(len(script), 400)])
	}
	if got := strings.Count(script, "\nCOMMIT;"); got != begins {
		t.Errorf("BEGIN = %d but COMMIT = %d", begins, got)
	}
	if got := strings.Count(script, "SET LOCAL app.clinic_code"); got != begins {
		t.Errorf("BEGIN = %d but SET LOCAL app.clinic_code = %d — a transaction without it "+
			"writes 0 rows under the RLS-bound role and reports success", begins, got)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
