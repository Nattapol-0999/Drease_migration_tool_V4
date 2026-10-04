package legacyimport

import (
	"strings"
	"testing"

	"system_cliv4/internal/legacydump"
	"system_cliv4/internal/mongodump"
)

// A bill naming an HN that is not a patient.
//
// WHY THIS NEEDS ITS OWN TEST
//
// invoice.hn is text with no foreign key, so such a bill imports cleanly and then appears
// in nobody's history. GUARD 5 refuses it at the database, which is correct and also means
// one bad HN stops a whole clinic. Measured on the identity-medical export: 20 HN on 45
// bills, present in neither the HN nor the old_hn column of all 8,630 patient rows, worth
// ฿252,390 with ฿251,990 already collected — 1.6% of the clinic's bill value, and the
// bills carry no name or phone to rebuild a patient from.
//
// The owner's rule (4 Oct 2026): if the HN is not among the patients, skip that bill.
//
// The checks below rely on the order inside planBills: the HN is tested BEFORE the lines
// are planned, so a bill can be dropped for its HN without a catalogue fixture.

// billHN builds a bills document with a chosen HN. The shared bill() helper hardcodes one.
func billHN(id, status, no, total, hn string, lines int) mongodump.Doc {
	d := bill(id, status, no, total, lines)
	d["hn"] = hn
	return d
}

func planWithPatients(t *testing.T, bills []mongodump.Doc, knownHN map[string]bool) (*Report, *EntityResult) {
	t.Helper()
	in := Input{ClinicCode: "MBORPH", DefaultBranch: "BR00001"}
	rep := NewReport(in.ClinicCode)
	eInv, eItem := rep.entity(EntInvoice), rep.entity(EntInvoiceItem)
	mc := &mongoCatalog{bills: bills}
	if _, err := planBills(mc, newBranchResolver(in), knownHN, rep, eInv, eItem); err != nil {
		t.Fatalf("planBills: %v", err)
	}
	return rep, eInv
}

const orphanSkipReason = "HN ในบิลไม่มีอยู่ในไฟล์คนไข้ — ข้ามบิลนี้ตามมติเจ้าของคลินิก"

func TestBillWhosePatientIsNotInTheFileIsSkipped(t *testing.T) {
	known := map[string]bool{"016400480": true}
	rep, eInv := planWithPatients(t, []mongodump.Doc{
		billHN("aa00000000000000000000a1", "full_paid", "650000001", "1500", "016400480", 0),
		billHN("aa00000000000000000000a2", "full_paid", "650000002", "2500", "016999999", 0),
	}, known)

	if got := eInv.Skipped[orphanSkipReason]; got != 1 {
		t.Errorf("bills skipped for a missing patient = %d, want 1 (skips: %v)", got, eInv.Skipped)
	}
	// The known-HN bill must NOT be skipped for its HN. It is skipped later for having no
	// lines, which is a different rule and a different message.
	joined := strings.Join(rep.Warnings, "\n")
	for _, want := range []string{"016999999", "2500.00", "ไฟล์คนไข้"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the report does not mention %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "016400480") {
		t.Errorf("the patient that DOES exist was reported as missing:\n%s", joined)
	}
}

func TestTheReportAddsUpTheMoneyLeftBehind(t *testing.T) {
	// Three orphan bills, two patients. The total is what an operator needs to decide
	// whether to chase it, so it has to be the sum and not a count.
	rep, _ := planWithPatients(t, []mongodump.Doc{
		billHN("bb00000000000000000000b1", "full_paid", "650000001", "1000.50", "016999001", 0),
		billHN("bb00000000000000000000b2", "full_paid", "650000002", "2000.25", "016999001", 0),
		billHN("bb00000000000000000000b3", "send", "650000003", "500", "016999002", 0),
	}, map[string]bool{"016400480": true})

	joined := strings.Join(rep.Warnings, "\n")
	for _, want := range []string{"ข้าม 3 ใบ", "2 HN", "3500.75", "016999001", "016999002"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the report does not mention %q:\n%s", want, joined)
		}
	}
}

func TestBillsAreNotDroppedWhenTheRunHasNoPatientFile(t *testing.T) {
	// knownHN is nil when no patient dump was read — a `-only money` run into a clinic
	// whose patients are already in the database. Dropping every bill there would be
	// catastrophic, so nil must mean "cannot tell" and the SQL guard does the checking.
	rep, eInv := planWithPatients(t, []mongodump.Doc{
		billHN("cc00000000000000000000c1", "full_paid", "650000001", "1500", "016999999", 0),
	}, nil)

	if got := eInv.Skipped[orphanSkipReason]; got != 0 {
		t.Errorf("a bill was dropped for its HN with no patient file to compare against (%d)", got)
	}
	if j := strings.Join(rep.Warnings, "\n"); strings.Contains(j, "ไฟล์คนไข้") {
		t.Errorf("reported missing patients without a patient file:\n%s", j)
	}
}

func TestAnEmptyPatientFileDropsEveryBillRatherThanNone(t *testing.T) {
	// Empty and nil are different: an empty set means the file was read and has nobody in
	// it, so every bill really is an orphan. Conflating the two would import bills with no
	// patients at all and leave GUARD 5 to refuse the script after the fact.
	_, eInv := planWithPatients(t, []mongodump.Doc{
		billHN("dd00000000000000000000d1", "full_paid", "650000001", "1500", "016999999", 0),
	}, map[string]bool{})

	if got := eInv.Skipped[orphanSkipReason]; got != 1 {
		t.Errorf("an empty patient file did not drop the bill (%d)", got)
	}
}

func TestPatientHNSetKeepsPatientsWithNoBranchCode(t *testing.T) {
	// branchByHN skips a patient whose branch_code is blank, because it is answering a
	// different question. Reusing its key set would have dropped the bills of every such
	// patient — which is why this has its own function.
	rows := []legacydump.Row{
		hnRow("016400001", "01"),
		hnRow("016400002", ""),
		hnRow("", "01"),
		hnRow("  016400003  ", "02"),
	}

	set := patientHNSet(rows)
	for _, hn := range []string{"016400001", "016400002", "016400003"} {
		if !set[hn] {
			t.Errorf("patientHNSet dropped %s", hn)
		}
	}
	if len(set) != 3 {
		t.Errorf("patientHNSet returned %d entries, want 3: %v", len(set), set)
	}
	// And the comparison it exists for: branchByHN loses the branchless patient.
	if _, ok := branchByHN(rows)["016400002"]; ok {
		t.Error("branchByHN now keeps branchless patients; this test's premise is stale")
	}
}

// hnRow builds the two columns patientHNSet and branchByHN read. legacydump.Row is
// map[string]*string so it can tell SQL NULL from an empty string.
func hnRow(hn, branch string) legacydump.Row {
	return legacydump.Row{"HN": &hn, "branch_code": &branch}
}
