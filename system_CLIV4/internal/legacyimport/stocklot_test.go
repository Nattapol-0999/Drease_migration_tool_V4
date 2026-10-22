package legacyimport

import (
	"strings"
	"testing"
)

// Where buildItems puts each stock lot.
//
// This is the file that decides it, and the decision is invisible in a green build: a lot
// written with the wrong branch_code is accepted by the database and looks like inventory.
// Three outcomes have to hold at once, so they are asserted from ONE build rather than three
// — a rule that gets one case right by getting another wrong would otherwise pass.
//
//	stock owned by a TICKED branch    → that branch's BRxxxxx
//	stock owned by NO branch          → MAIN_STOCK (V4's central warehouse)
//	stock owned by an UNTICKED branch → not imported at all, and counted
//
// The last one is the one worth stating plainly. A branch left off the tick list is being
// imported as its own clinic — different legal entity, its own TIN — and its lots arrive
// there. Parking a copy on this clinic's default branch would put the same physical boxes in
// two clinics, and would also claim they sit on a shelf they were never on.

// lotsAcrossThreeKindsOfStock: two lots at ticked branch 01, one at ticked 04, one at
// unticked 03, one at unticked 06, and two in the warehouse stock no branch claims.
func lotsAcrossThreeKindsOfStock() []byte {
	return []byte(`db.getCollection("items").insert([
  { _id: ObjectId("aa0000000000000000000001"), code: "TICK-01-A", name1: "ยาสาขา 01 ก", quantity: "10", quantity_total: "10", cost: "5", price: "9", active: true, stock: ObjectId("63608c6c7796354dbae69c55") },
  { _id: ObjectId("aa0000000000000000000002"), code: "TICK-01-B", name1: "ยาสาขา 01 ข", quantity: "4", quantity_total: "4", cost: "5", price: "9", active: true, stock: ObjectId("63608c6c7796354dbae69c55") },
  { _id: ObjectId("aa0000000000000000000003"), code: "TICK-04-A", name1: "ยาสาขา 04", quantity: "7", quantity_total: "7", cost: "5", price: "9", active: true, stock: ObjectId("660a55b65a0fdc487be92ba2") },
  { _id: ObjectId("aa0000000000000000000004"), code: "UNTICK-03", name1: "ยาสาขา 03", quantity: "3", quantity_total: "3", cost: "5", price: "9", active: true, stock: ObjectId("64128720030d9b36ce99cd57") },
  { _id: ObjectId("aa0000000000000000000005"), code: "UNTICK-06", name1: "ยาสาขา 06", quantity: "2", quantity_total: "2", cost: "5", price: "9", active: true, stock: ObjectId("68c7c642974f3dbb950d44ab") },
  { _id: ObjectId("aa0000000000000000000006"), code: "WARE-A", name1: "ยาคลังกลาง ก", quantity: "50", quantity_total: "50", cost: "5", price: "9", active: true, stock: ObjectId("5fab83eebb59bca34e513211") },
  { _id: ObjectId("aa0000000000000000000007"), code: "WARE-B", name1: "ยาคลังกลาง ข", quantity: "60", quantity_total: "60", cost: "5", price: "9", active: true, stock: ObjectId("5fab83eebb59bca34e513211") }
]);
`)
}

// tickedSeven is the map the backend returns after creating the branches the operator ticked.
// 03 and 06 are absent because they belong to other companies.
func tickedSeven() map[string]string {
	return map[string]string{
		"00": "BR00001", "01": "BR00002", "02": "BR00003",
		"04": "BR00004", "05": "BR00005", "07": "BR00006", "08": "BR00007",
	}
}

func buildLots(t *testing.T, in Input) (string, *Report) {
	t.Helper()
	slotSQL, _, rep := buildLotsAndMoves(t, in)
	return slotSQL, rep
}

// buildLotsAndMoves also hands back the receive ledger, which is written from the same
// pass over the same documents — so "every lot got a movement" can only be asserted from
// one build, not from two.
func buildLotsAndMoves(t *testing.T, in Input) (slotSQL, moveSQL string, rep *Report) {
	t.Helper()
	mc, err := parseMongoCatalog(in.MongoDump)
	if err != nil {
		t.Fatalf("parse mongo: %v", err)
	}
	rep = NewReport(in.ClinicCode)
	_, _, slotSQL, moveSQL, err = buildItems(mc, in, rep)
	if err != nil {
		t.Fatalf("buildItems: %v", err)
	}
	return slotSQL, moveSQL, rep
}

func TestLotGoesToItsOwnBranchWhenThatBranchWasCreated(t *testing.T) {
	slotSQL, _ := buildLots(t, Input{
		ClinicCode:    "MBLOT1",
		BranchDump:    branchRegistryWithStocks(),
		MongoDump:     lotsAcrossThreeKindsOfStock(),
		BranchMap:     tickedSeven(),
		DefaultBranch: "BR00004",
	})

	// Legacy 01 → BR00002 and legacy 04 → BR00004, per the map.
	for _, want := range []struct{ code, branch string }{
		{"TICK-01-A", "BR00002"},
		{"TICK-01-B", "BR00002"},
		{"TICK-04-A", "BR00004"},
	} {
		if !lotHasBranch(slotSQL, want.code, want.branch) {
			t.Errorf("lot %s is not filed at %s\n%s", want.code, want.branch,
				statementFor(slotSQL, want.code))
		}
	}
}

func TestLotInAStockNoBranchOwnsGoesToMainStockNotTheDefaultBranch(t *testing.T) {
	slotSQL, rep := buildLots(t, Input{
		ClinicCode:    "MBLOT1",
		BranchDump:    branchRegistryWithStocks(),
		MongoDump:     lotsAcrossThreeKindsOfStock(),
		BranchMap:     tickedSeven(),
		DefaultBranch: "BR00004",
	})

	for _, code := range []string{"WARE-A", "WARE-B"} {
		if !lotHasBranch(slotSQL, code, MainStockBranchCode) {
			t.Errorf("warehouse lot %s did not go to %s\n%s", code, MainStockBranchCode,
				statementFor(slotSQL, code))
		}
		// The failure this guards against: falling through to the default branch, which
		// both misplaces the boxes and inflates that branch's inventory value.
		if lotHasBranch(slotSQL, code, "BR00004") {
			t.Errorf("warehouse lot %s was parked on the default branch", code)
		}
	}

	if !strings.Contains(strings.Join(rep.Notes, " "), MainStockBranchCode) {
		t.Errorf("the report does not say the warehouse lots went to %s: %v",
			MainStockBranchCode, rep.Notes)
	}
}

func TestLotOfAnUntickedBranchIsNotImportedAndIsCounted(t *testing.T) {
	slotSQL, rep := buildLots(t, Input{
		ClinicCode:    "MBLOT1",
		BranchDump:    branchRegistryWithStocks(),
		MongoDump:     lotsAcrossThreeKindsOfStock(),
		BranchMap:     tickedSeven(),
		DefaultBranch: "BR00004",
	})

	for _, code := range []string{"UNTICK-03", "UNTICK-06"} {
		if strings.Contains(slotSQL, code) {
			t.Errorf("lot %s belongs to a branch this clinic does not have, but was imported:\n%s",
				code, statementFor(slotSQL, code))
		}
	}

	// Dropped silently is worse than not dropped at all: the second import has to be
	// checkable against this number, and those lots must appear there exactly once.
	notes := strings.Join(rep.Notes, " ")
	for _, want := range []string{"03", "06", "2 ล็อต"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the report does not mention %q — nobody can reconcile a silent drop: %v",
				want, rep.Notes)
		}
	}
}

func TestEveryLotIsAccountedForExactlyOnce(t *testing.T) {
	// Seven lots in, and every one either lands somewhere or is reported. A rule that
	// quietly loses one would still pass the three tests above.
	slotSQL, rep := buildLots(t, Input{
		ClinicCode:    "MBLOT1",
		BranchDump:    branchRegistryWithStocks(),
		MongoDump:     lotsAcrossThreeKindsOfStock(),
		BranchMap:     tickedSeven(),
		DefaultBranch: "BR00004",
	})

	inserts := strings.Count(slotSQL, "INSERT INTO slot_stock")
	if inserts != 5 {
		t.Errorf("slot_stock inserts = %d, want 5 (3 branch lots + 2 warehouse lots)", inserts)
	}
	// No lot may carry a branch this clinic never created.
	for _, forbidden := range []string{"'03'", "'06'"} {
		if strings.Contains(slotSQL, "branch_code") && strings.Contains(slotSQL, forbidden) {
			t.Errorf("a legacy branch code %s reached the SQL; codes must be translated", forbidden)
		}
	}
	if len(rep.Notes) == 0 {
		t.Error("the build reported nothing at all")
	}
}

func TestWithNoBranchMapEveryLotStillLandsOnTheSingleBranch(t *testing.T) {
	// Imports that predate branch mapping supply no map and expect every row on the one
	// branch that exists. This must not start dropping lots or inventing MAIN_STOCK.
	slotSQL, _ := buildLots(t, Input{
		ClinicCode: "MBLOT1",
		BranchDump: branchRegistryWithStocks(),
		MongoDump:  lotsAcrossThreeKindsOfStock(),
	})

	inserts := strings.Count(slotSQL, "INSERT INTO slot_stock")
	// The two warehouse lots still go to MAIN_STOCK — that is about the stock they sit in,
	// not about the map — but nothing is dropped, so all seven are written.
	if inserts != 7 {
		t.Errorf("slot_stock inserts = %d, want all 7 kept when no branch map is supplied", inserts)
	}
	for _, code := range []string{"UNTICK-03", "UNTICK-06"} {
		if !strings.Contains(slotSQL, code) {
			t.Errorf("lot %s was dropped even though no branch map was supplied", code)
		}
	}
}

func TestWithNoBranchTableTheBuildRefusesInsteadOfSendingEveryLotToMainStock(t *testing.T) {
	// The failure this guards is silent, which is the whole reason it needs a test of its
	// own. With no branch table stockOwners returns an empty map, every lot falls through to
	// "no branch claims this stock", the clinic's entire inventory lands in MAIN_STOCK, and
	// the run reports success. Measured on C6C936C: 3,504 lots sent to the central warehouse
	// with nothing but a note saying 0 branches had a stock bound.
	//
	// TestStockOwnersWithNoBranchTableCannotTellRatherThanGuessing already pins the helper.
	// This pins the CALLER, which is where the damage was done.
	mc, err := parseMongoCatalog(lotsAcrossThreeKindsOfStock())
	if err != nil {
		t.Fatalf("parse mongo: %v", err)
	}
	_, _, slotSQL, _, err := buildItems(mc, Input{
		ClinicCode: "MBLOT1",
		BranchMap:  tickedSeven(),
	}, NewReport("MBLOT1"))

	if err == nil {
		t.Fatalf("buildItems accepted an export with no branch table and wrote %d lot(s) instead of refusing",
			strings.Count(slotSQL, "INSERT INTO slot_stock"))
	}
	// The operator has to be able to act on it, so the message names the table, the column
	// that carries the answer, and where the lots would otherwise have gone.
	for _, want := range []string{"branch", "stock_id", MainStockBranchCode} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so nobody can tell what to fix: %v", want, err)
		}
	}
	// A caller that logs the error but uses the strings anyway must get nothing to write.
	if slotSQL != "" {
		t.Errorf("SQL was returned alongside the refusal:\n%s", slotSQL)
	}
}

func TestALotWithNoStockReferenceIsDroppedRowByRowNotRefusedWholesale(t *testing.T) {
	// The refusal above is about an export whose lots DO name a location while the registry
	// that resolves it is missing. A document with no stock reference at all is a different
	// case the loop drops one row at a time, and it must not take the whole import down -
	// otherwise one malformed item would block a clinic that has no stock worth importing.
	mc, err := parseMongoCatalog([]byte(`db.getCollection("items").insert([
  { _id: ObjectId("aa0000000000000000000009"), code: "NO-STOCK", name1: "ยาที่ไม่ได้ระบุคลัง", quantity: "1", quantity_total: "1", cost: "5", price: "9", active: true }
]);
`))
	if err != nil {
		t.Fatalf("parse mongo: %v", err)
	}
	_, _, slotSQL, _, err := buildItems(mc, Input{ClinicCode: "MBLOT1"}, NewReport("MBLOT1"))
	if err != nil {
		t.Fatalf("an item with no stock reference must be dropped, not refused: %v", err)
	}
	if strings.Contains(slotSQL, "NO-STOCK") {
		t.Errorf("a lot with no stock reference was written anyway:\n%s", slotSQL)
	}
}

// lotHasBranch reports whether the statement that mentions lotCode also carries branchCode.
// Matching within the one statement matters: the file holds several, and a substring search
// across the whole script would pass on another lot's branch.
func lotHasBranch(slotSQL, lotCode, branchCode string) bool {
	return strings.Contains(statementFor(slotSQL, lotCode), "'"+branchCode+"'")
}

func statementFor(slotSQL, lotCode string) string {
	for _, stmt := range strings.Split(slotSQL, "ON CONFLICT (mongo_id) DO NOTHING;") {
		if strings.Contains(stmt, lotCode) {
			return strings.TrimSpace(stmt)
		}
	}
	return "(no statement mentions " + lotCode + ")"
}
