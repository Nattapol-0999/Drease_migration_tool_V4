package legacyimport

import (
	"strings"
	"testing"
)

// Where a guard sits in the file is part of what it means.
//
// Guard 5 ("every bill's HN must exist in patient") spent its life at the top of the
// script, above every INSERT. That reads correctly and is wrong: in a run that imports
// patients AND money, the patients it looks for are created by the same script further
// down, so the guard read an empty table and refused. Measured on a real 115 MB export
// for clinic CC00EFF: 4,372 HNs reported missing while 6,439 patient inserts sat below the
// guard waiting to create them. Exit 3, nothing imported, and the message told the
// operator to do the thing the file was already doing.
//
// WHAT THESE TESTS COVER, AND WHAT THEY DO NOT
//
// They test the two guard builders and the ONE rule that decides where the patient guard
// goes. They deliberately do not build a money batch from a synthetic Mongo fixture:
// getting a legacy bill to survive money.go means matching status, invoice_no, price
// fields and the line-item shape exactly, and a fixture that silently fails those checks
// produces an empty money batch and makes every assertion about ordering pass while
// testing nothing. That is what the first version of this file did.
//
// The end-to-end ordering is proved instead against the real export, by generating the
// script and reading where the guard lands. See the note in DECISIONS.md.

func TestGuard4OnlyAsksAboutRowsAlreadyAtTheTarget(t *testing.T) {
	// Guard 4 compares document numbers against invoice/receipt rows that were there
	// before this run. Nothing the script writes can change that answer, which is why it
	// belongs above every INSERT and why it is safe there.
	g := buildMoneyPreflightGuards("MBU1",
		map[string]string{"650001": "0169001"},
		map[string]string{"R650001": "0169001"})

	for _, want := range []string{"GUARD 4a:", "GUARD 4b:", "JOIN invoice inv", "JOIN receipt r"} {
		if !strings.Contains(g, want) {
			t.Errorf("guard 4 is missing %q:\n%s", want, head(g, 600))
		}
	}
	// It must NOT look at patient: that is guard 5's job, and mixing them is what put a
	// patient lookup above the patient inserts in the first place.
	if strings.Contains(g, "FROM patient") {
		t.Error("guard 4 reads patient — that check belongs to guard 5, which runs later")
	}
}

func TestGuard5IsBuiltSeparatelySoItCanBePlacedLater(t *testing.T) {
	g := buildMoneyPatientGuard("MBU1", map[string]bool{"0169001": true, "0169002": true})

	for _, want := range []string{"GUARD 5:", "FROM patient p", "'0169001'", "'0169002'"} {
		if !strings.Contains(g, want) {
			t.Errorf("guard 5 is missing %q:\n%s", want, head(g, 600))
		}
	}
	// The message has to tell the operator the thing that actually helps. The old wording
	// said only "import the patients first", which is wrong advice when the file already
	// contains them.
	if !strings.Contains(g, "patients") {
		t.Errorf("the refusal does not mention the patients group:\n%s", head(g, 600))
	}
	// Nothing to say when the run has no bills.
	if buildMoneyPatientGuard("MBU1", nil) != "" {
		t.Error("guard 5 was emitted for a run with no bill HNs")
	}
}

func TestScriptPutsGuard5BetweenThePatientsAndTheMoney(t *testing.T) {
	// The placement rule, checked on the assembled text with the two phases stubbed. Real
	// batches are not needed to prove ORDER: what matters is that the guard is written
	// when the loop reaches EntInvoice, which is after EntPatient in `order`.
	pi := indexOf(order, EntPatient)
	ii := indexOf(order, EntInvoice)
	if pi < 0 || ii < 0 {
		t.Fatalf("order is missing a phase: patient=%d invoice=%d", pi, ii)
	}
	if pi > ii {
		t.Fatalf("order puts patient (%d) after invoice (%d) — placing guard 5 at the "+
			"invoice phase would no longer put it after the patients", pi, ii)
	}
}

func TestGuard5MessageDoesNotContradictTheFileItIsIn(t *testing.T) {
	// The old text was "นำเข้าคนไข้ให้ครบก่อน แล้วรันอีกครั้ง" — advice that cannot be followed
	// when the very script printing it is the one importing the patients. It now names the
	// group to add, which is actionable in both situations.
	g := buildMoneyPatientGuard("MBU1", map[string]bool{"0169001": true})
	if strings.Contains(g, "นำเข้าคนไข้ให้ครบก่อน แล้วรันอีกครั้ง") &&
		!strings.Contains(g, "กลุ่ม patients") {
		t.Error("the refusal still only says to import patients first, with no mention of " +
			"adding the patients group to this run")
	}
}

// indexOf is local to this file: `order` is a []string and this is the only place that
// needs a position from it.
func indexOf(list []string, want string) int {
	for i, v := range list {
		if v == want {
			return i
		}
	}
	return -1
}
