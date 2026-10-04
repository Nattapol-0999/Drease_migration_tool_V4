package legacyimport

import (
	"strings"
	"testing"
)

// A quantity wider than its column must be cut to what that column holds — per column.
//
// WHY THIS NEEDS A TEST OF ITS OWN
//
// V2 lets a quantity be typed as a field of nines. The tri-clinic export carries
// 99999999999999 units of a face mask, and psql rejected it with "numeric field overflow"
// a third of the way down the file, which left the clinic half imported because the
// script commits once per data group.
//
// The owner's rule (ก1, 2 Oct 2026): cut it to the ceiling of each column separately.
// slot_stock.amount is numeric(14,2) and stock_movement.qty_delta is numeric(10,2), so
// the SAME source number becomes two different figures and the receive ledger for that
// lot will not add up to the shelf. That is accepted, and it is why the lots are named in
// the report rather than cut quietly.
//
// Money is never cut. It is not asserted here because nothing in this file calls
// clampToColumn for a money column — it is asserted by the guard still refusing a script
// whose prices do not fit (see widths_test.go).

// wideQtyLots: one lot too wide for both columns, one that fits slot_stock but not the
// movement (the case that proves ก1 is per column), one exactly on the movement's edge,
// and a plainly ordinary control.
func wideQtyLots() []byte {
	return []byte(`db.getCollection("items").insert([
  { _id: ObjectId("cc0000000000000000000001"), code: "WIDE-BOTH", name1: "มาสก์ฝ้า", quantity: "99999999999999", quantity_total: "10000999999993", cost: "7", price: "90", active: true, stock: ObjectId("63608c6c7796354dbae69c55") },
  { _id: ObjectId("cc0000000000000000000002"), code: "WIDE-MOVE-ONLY", name1: "ปากกาเกินแค่ใบเสร็จรับเข้า", quantity: "100000000", quantity_total: "100000000", cost: "3", price: "90", active: true, stock: ObjectId("63608c6c7796354dbae69c55") },
  { _id: ObjectId("cc0000000000000000000003"), code: "EDGE-FITS", name1: "พอดีขอบ", quantity: "99999999", quantity_total: "99999999", cost: "2", price: "90", active: true, stock: ObjectId("63608c6c7796354dbae69c55") },
  { _id: ObjectId("cc0000000000000000000004"), code: "POS-CTL", name1: "ยาปกติ", quantity: "42", quantity_total: "200", cost: "7", price: "90", active: true, stock: ObjectId("63608c6c7796354dbae69c55") }
]);
`)
}

func buildWideQtyLots(t *testing.T) (slotSQL, moveSQL string, rep *Report) {
	t.Helper()
	return buildLotsAndMoves(t, Input{
		ClinicCode: "MBWIDE",
		BranchDump: branchRegistryWithStocks(),
		MongoDump:  wideQtyLots(),
		BranchMap:  tickedSeven(),
	})
}

func TestWideQuantityIsCutToEachColumnsOwnCeiling(t *testing.T) {
	slotSQL, moveSQL, _ := buildWideQtyLots(t)

	lot := statementFor(slotSQL, "WIDE-BOTH")
	if strings.Contains(lot, "99999999999999") {
		t.Errorf("the lot still carries the 14-digit quantity:\n%s", lot)
	}
	if !strings.Contains(lot, "999999999999.99") {
		t.Errorf("the lot was not cut to numeric(14,2)'s ceiling:\n%s", lot)
	}
	if strings.Contains(lot, "10000999999993") {
		t.Errorf("amount_total kept its 14-digit value:\n%s", lot)
	}

	move := movementFor(t, moveSQL, "WIDE-BOTH")
	if !strings.Contains(move, "99999999.99") {
		t.Errorf("qty_delta was not cut to numeric(10,2)'s ceiling:\n%s", move)
	}
	if strings.Contains(move, "999999999999.99") {
		t.Errorf("qty_delta got slot_stock's ceiling, which does not fit it:\n%s", move)
	}
}

func TestQuantityThatFitsTheLotButNotTheMovementIsCutOnlyThere(t *testing.T) {
	// 100000000 is nine digits: slot_stock.amount holds twelve, qty_delta only eight.
	// This is ก1 in one lot — the figure on the shelf stays exact and the ledger's does
	// not, which is the trade the owner accepted.
	slotSQL, moveSQL, _ := buildWideQtyLots(t)

	lot := statementFor(slotSQL, "WIDE-MOVE-ONLY")
	if !strings.Contains(lot, "100000000") {
		t.Errorf("the lot's quantity was cut even though it fits numeric(14,2):\n%s", lot)
	}

	move := movementFor(t, moveSQL, "WIDE-MOVE-ONLY")
	if strings.Contains(move, "100000000,") {
		t.Errorf("qty_delta kept a value numeric(10,2) cannot hold:\n%s", move)
	}
	if !strings.Contains(move, "99999999.99") {
		t.Errorf("qty_delta was not cut:\n%s", move)
	}
}

func TestQuantityThatFitsIsNotTouchedAtAll(t *testing.T) {
	// The digits are handed to Postgres verbatim (moneyLit's contract). An implementation
	// that parsed to float64 and printed it back would fail here, and so would one that
	// cut on >= instead of >.
	slotSQL, moveSQL, _ := buildWideQtyLots(t)

	edge := statementFor(slotSQL, "EDGE-FITS")
	if !strings.Contains(edge, "99999999") || strings.Contains(edge, "99999999.99") {
		t.Errorf("a quantity exactly on the edge was reformatted:\n%s", edge)
	}
	if m := movementFor(t, moveSQL, "EDGE-FITS"); !strings.Contains(m, "99999999,") {
		t.Errorf("qty_delta on the edge was cut:\n%s", m)
	}

	ctl := statementFor(slotSQL, "POS-CTL")
	for _, want := range []string{"42", "200"} {
		if !strings.Contains(ctl, want) {
			t.Errorf("the control lot lost its quantity %s:\n%s", want, ctl)
		}
	}
}

func TestTheClampedScriptPassesTheColumnGuard(t *testing.T) {
	// The point of the clamp: the file the generator hands over must be one psql will
	// finish. This is the same scan that refuses the file, run over what was just built.
	slotSQL, moveSQL, _ := buildWideQtyLots(t)

	violations, st := scanScriptColumnLimits(slotSQL + "\n" + moveSQL)
	if len(violations) != 0 {
		t.Errorf("the clamped script still has %d column violations: %+v", len(violations), violations)
	}
	if st.Numbers == 0 {
		t.Error("the scan compared no numbers at all, so it proves nothing")
	}
	if st.Skipped != 0 {
		t.Errorf("%d statements were unreadable, so the scan may have missed a violation", st.Skipped)
	}
}

func TestCostLotIsCappedOnlyWhenTheProductCouldOverflow(t *testing.T) {
	// cost_lot is multiplied by Postgres at run time, so no literal check can see the
	// result: 7 × 999999999999.99 overflows numeric(14,2) when the script runs. The cap
	// has to be in the SQL, and only where it is needed - otherwise every existing
	// generated file changes and two runs stop diffing cleanly.
	slotSQL, _, _ := buildWideQtyLots(t)

	wide := statementFor(slotSQL, "WIDE-BOTH")
	if !strings.Contains(wide, "LEAST(") {
		t.Errorf("the lot value of a clamped lot was not capped:\n%s", wide)
	}
	if !strings.Contains(wide, "999999999999.99::numeric") {
		t.Errorf("the cap is not cost_lot's own ceiling:\n%s", wide)
	}

	ctl := statementFor(slotSQL, "POS-CTL")
	if strings.Contains(ctl, "LEAST(") {
		t.Errorf("an ordinary lot had its value capped, which changes every old file:\n%s", ctl)
	}
	if !strings.Contains(ctl, "(7::numeric * 42::numeric)") {
		t.Errorf("the ordinary lot value stopped rendering as a plain product:\n%s", ctl)
	}
}

func TestEveryCutIsNamedInTheReport(t *testing.T) {
	_, _, rep := buildWideQtyLots(t)

	var warn string
	for _, w := range rep.Warnings {
		if strings.Contains(w, "slot_stock/stock_movement") {
			warn = w
		}
	}
	if warn == "" {
		t.Fatalf("no warning about cut quantities; warnings were: %v", rep.Warnings)
	}
	// Four values across two lots: WIDE-BOTH loses amount, amount_total and qty_delta,
	// WIDE-MOVE-ONLY loses only qty_delta. Counting values and lots separately is the
	// point — one lot with three cut columns is not three damaged lots.
	for _, want := range []string{"4 ค่า", "2 ล็อต", "ไม่เท่ากัน"} {
		if !strings.Contains(warn, want) {
			t.Errorf("the warning does not mention %q: %s", want, warn)
		}
	}

	notes := strings.Join(rep.Notes, "\n")
	for _, want := range []string{
		"WIDE-BOTH", "WIDE-MOVE-ONLY",
		"slot_stock.amount/remain", "stock_movement.qty_delta",
		"99999999999999 → 999999999999.99",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("the report does not name %q", want)
		}
	}
	if strings.Contains(notes, "EDGE-FITS") || strings.Contains(notes, "POS-CTL") {
		t.Error("a lot that was never cut appears in the cut list")
	}
}

func TestNumericCeilingMatchesThePrecision(t *testing.T) {
	cases := []struct {
		lim  numericLimit
		want string
	}{
		{numericLimit{14, 2}, "999999999999.99"},
		{numericLimit{10, 2}, "99999999.99"},
		{numericLimit{6, 2}, "9999.99"},
		{numericLimit{12, 4}, "99999999.9999"},
		{numericLimit{8, 0}, "99999999"},
	}
	for _, c := range cases {
		if got := numericCeiling(c.lim); got != c.want {
			t.Errorf("numeric(%d,%d) ceiling = %s, want %s", c.lim.precision, c.lim.scale, got, c.want)
		}
	}
}

func TestClampGivesUpInsteadOfGuessing(t *testing.T) {
	// Each of these leaves the value alone, so checkScriptColumnLimits still gets to
	// refuse it. Cutting something this does not understand would hide a real problem.
	cases := []struct {
		name          string
		table, column string
		in            string
	}{
		{"unknown table", "not_a_table", "amount", "99999999999999"},
		{"unknown column", "slot_stock", "not_a_column", "99999999999999"},
		{"not a literal", "slot_stock", "amount", "(7::numeric * 9::numeric)"},
		{"negative", "slot_stock", "amount", "-99999999999999"},
		{"empty", "slot_stock", "amount", ""},
		{"a money column is cut by nobody here but the function is generic",
			"receipt", "vat_rate", "1"},
	}
	for _, c := range cases {
		got, cut := clampToColumn(c.table, c.column, c.in)
		if cut || got != c.in {
			t.Errorf("%s: clampToColumn returned (%q, %v), want the value untouched", c.name, got, cut)
		}
	}
}

func TestProductFitsCountsDigitsRatherThanMultiplying(t *testing.T) {
	lim := numericLimit{14, 2} // twelve integer digits
	cases := []struct {
		a, b string
		want bool
	}{
		{"7", "42", true},
		{"1", "10000000", true},
		{"999999", "999999", true},      // 6+6 = 12, the edge
		{"9999999", "999999", false},    // 7+6 = 13
		{"7", "999999999999.99", false}, // the clamped case
		{"(1*2)", "3", false},           // not a literal: not certain, so false
	}
	for _, c := range cases {
		if got := productFits(c.a, c.b, lim); got != c.want {
			t.Errorf("productFits(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
