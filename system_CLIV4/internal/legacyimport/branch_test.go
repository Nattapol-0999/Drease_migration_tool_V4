package legacyimport

import (
	"strings"
	"testing"

	"system_cliv4/internal/legacydump"
)

// Branch resolution has to be right for a reason that is not visible in a green build: a
// row written with the wrong branch_code is accepted by the database and looks like data.
// The codes below are the real ones from the export that forced this work ("01".."08"),
// mapped onto the codes V4 allocates ("BR00001"…).

func anjaliMap() map[string]string {
	return map[string]string{
		"01": "BR00001", "02": "BR00002", "03": "BR00003", "04": "BR00004",
		"05": "BR00005", "06": "BR00006", "00": "BR00007", "07": "BR00008",
		"08": "BR00009",
	}
}

func TestBranchResolverWithNoMapKeepsTheOldSingleBranchBehaviour(t *testing.T) {
	// Every import before multi-branch support stamped BranchCode on everything. That has
	// to keep happening when no map is supplied, or re-running an old import would change
	// what it produces.
	br := newBranchResolver(Input{})
	for _, legacy := range []string{"", "01", "04", "99"} {
		if got := br.resolve(legacy); got != BranchCode {
			t.Errorf("resolve(%q) = %q, want %q (the single-branch constant)", legacy, got, BranchCode)
		}
	}
}

func TestBranchResolverTranslatesEveryMappedCode(t *testing.T) {
	br := newBranchResolver(Input{BranchMap: anjaliMap(), DefaultBranch: "BR00001"})
	for legacy, want := range anjaliMap() {
		if got := br.resolve(legacy); got != want {
			t.Errorf("resolve(%q) = %q, want %q", legacy, got, want)
		}
	}
	if len(br.unknown) != 0 || br.blank != 0 {
		t.Errorf("clean input reported problems: unknown=%v blank=%d", br.unknown, br.blank)
	}
}

func TestBranchResolverReportsCodesItCouldNotMap(t *testing.T) {
	// The real export carries three rows whose branch_code is not in the registry
	// ("09", "68", "al" — data-entry damage). They must not be dropped: branch_code is
	// NOT NULL on every table that has it, so losing patient history over a typo is worse
	// than filing the row at the default and saying so.
	br := newBranchResolver(Input{BranchMap: anjaliMap(), DefaultBranch: "BR00001"})
	for _, bad := range []string{"09", "68", "al", "09"} {
		if got := br.resolve(bad); got != "BR00001" {
			t.Errorf("resolve(%q) = %q, want the default BR00001", bad, got)
		}
	}
	if br.unknown["09"] != 2 || br.unknown["68"] != 1 || br.unknown["al"] != 1 {
		t.Errorf("unknown counts = %v, want 09:2 68:1 al:1", br.unknown)
	}

	rep := NewReport("BRTEST")
	br.report(rep, "patient")
	joined := strings.Join(rep.Warnings, " ")
	for _, want := range []string{"09", "68", "al", "BR00001"} {
		if !strings.Contains(joined, want) {
			t.Errorf("report does not mention %q — an operator cannot act on what is not said: %q", want, joined)
		}
	}
}

func TestBranchResolverCountsRowsWithNoBranch(t *testing.T) {
	// 20,972 of 20,972 appointment rows in the export have a blank branch_code. The
	// number has to reach the report, or a silent default looks like real data.
	br := newBranchResolver(Input{BranchMap: anjaliMap(), DefaultBranch: "BR00004"})
	for i := 0; i < 5; i++ {
		if got := br.resolve(""); got != "BR00004" {
			t.Fatalf("blank resolve = %q, want BR00004", got)
		}
	}
	if br.blank != 5 {
		t.Errorf("blank = %d, want 5", br.blank)
	}
	rep := NewReport("BRTEST")
	br.report(rep, "appointment")
	if !strings.Contains(strings.Join(rep.Notes, " "), "5") {
		t.Errorf("report does not carry the blank count: %v", rep.Notes)
	}
}

func TestBranchResolverDefaultsToTheFirstMappedBranch(t *testing.T) {
	// With a map but no explicit default, the first entry in sorted order is used. Sorted
	// so the choice is the same on every run — an unstable default would move rows
	// between branches depending on map iteration order.
	br := newBranchResolver(Input{BranchMap: anjaliMap()})
	if got := br.resolve(""); got != "BR00007" {
		// "00" sorts before "01", and it maps to BR00007.
		t.Errorf("default = %q, want BR00007 (the entry for the lowest legacy code)", got)
	}
}

func TestResolveOrKeepDoesNotResolveAnAlreadyResolvedCode(t *testing.T) {
	// A receipt falls back to its bill's branch, and that value has been through
	// resolve() already. Resolving twice would look up "BR00004" in a map keyed by "04"
	// and quietly land on the default — the money would move to another branch.
	br := newBranchResolver(Input{BranchMap: anjaliMap(), DefaultBranch: "BR00001"})
	if got := resolveOrKeep(br, "", "BR00004"); got != "BR00004" {
		t.Errorf("resolveOrKeep with a resolved fallback = %q, want BR00004 unchanged", got)
	}
	if got := resolveOrKeep(br, "03", "BR00004"); got != "BR00003" {
		t.Errorf("resolveOrKeep with a legacy value = %q, want BR00003 (the legacy code wins)", got)
	}
	if got := resolveOrKeep(br, "", ""); got != "BR00001" {
		t.Errorf("resolveOrKeep with nothing = %q, want the default", got)
	}
}

func TestBranchByHNIndexesThePatientsBranch(t *testing.T) {
	// Appointments take their branch from the patient because the legacy appointment rows
	// do not carry one. This is the lookup that makes that exact rather than a guess.
	dump := "INSERT INTO `patient` (`HN`, `branch_code`, `name`) VALUES\n" +
		"('016900066', '04', 'ก'),\n" +
		"('016900453', '01', 'ข'),\n" +
		"('016900999', '', 'ค'),\n" +
		"('016900066', '02', 'ง');\n"
	_, rows, err := legacydump.Parse(dump, "patient")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	got := branchByHN(rows)
	if got["016900066"] != "04" {
		// First wins. A duplicate HN under a different branch is ambiguous, and
		// SurveyBranches reports that case before an import is attempted.
		t.Errorf("HN 016900066 = %q, want 04 (the first occurrence)", got["016900066"])
	}
	if got["016900453"] != "01" {
		t.Errorf("HN 016900453 = %q, want 01", got["016900453"])
	}
	if _, exists := got["016900999"]; exists {
		t.Error("a patient with a blank branch must not be indexed — the caller has to see it as missing")
	}
	if len(got) != 2 {
		t.Errorf("indexed %d HNs, want 2", len(got))
	}
}
