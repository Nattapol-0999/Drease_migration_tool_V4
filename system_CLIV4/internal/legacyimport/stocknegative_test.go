package legacyimport

import (
	"regexp"
	"strings"
	"testing"
)

// A negative quantity from V2 must reach V4 as 0.
//
// WHY THIS NEEDS A TEST OF ITS OWN
//
// V2's stock rooms run with `isNegative_qty: true`, so a withdrawal may take a lot below
// zero and it stays there. The pb-plasticsurgery export carries −88,243 units in the branch
// room against +25,471 in MainStock. Postgres accepts every one of those figures — slot_stock
// has no CHECK on `remain` — so a negative lot imports cleanly, reads as inventory on the
// stock page, and shows up in the lot value on the report. Nothing fails, which is exactly
// why it has to be asserted here.
//
// The owner's rule (23 Sep 2026): a negative quantity arrives as 0, and it applies to the
// whole quantity family of the lot — `amount`, `remain`, `amount_total`, the `cost_lot`
// derived from it, and the `qty_delta` of the receive movement. A lot that received −5 and
// holds 0 would be a new kind of wrong, so they are clamped together or not at all.
//
// The lot is still WRITTEN when the quantity clamps to 0 (owner's choice ข1): the SKU exists
// in V2 and staff expect to find it, they just have none of it on the shelf.

// negQtyLots: one lot negative on hand, one negative on both counters, one plainly positive
// (the control), and one where only the received total is negative.
func negQtyLots() []byte {
	return []byte(`db.getCollection("items").insert([
  { _id: ObjectId("bb0000000000000000000001"), code: "NEG-ONHAND", name1: "ยาติดลบคงเหลือ", quantity: "-5", quantity_total: "20", cost: "7", price: "90", active: true, stock: ObjectId("63608c6c7796354dbae69c55") },
  { _id: ObjectId("bb0000000000000000000002"), code: "NEG-BOTH", name1: "ยาติดลบทั้งคู่", quantity: "-1200.50", quantity_total: "-3000", cost: "7", price: "90", active: true, stock: ObjectId("63608c6c7796354dbae69c55") },
  { _id: ObjectId("bb0000000000000000000003"), code: "POS-CONTROL", name1: "ยาปกติ", quantity: "42", quantity_total: "200", cost: "7", price: "90", active: true, stock: ObjectId("63608c6c7796354dbae69c55") },
  { _id: ObjectId("bb0000000000000000000004"), code: "NEG-TOTAL-ONLY", name1: "ยารับเข้าติดลบ", quantity: "8", quantity_total: "-9", cost: "7", price: "90", active: true, stock: ObjectId("5fab83eebb59bca34e513211") }
]);
`)
}

// negativeNumberInValues finds a negative numeric literal in a statement's VALUES — that is,
// a `-` that follows a comma or an opening bracket rather than one inside a date or an id.
var negativeNumberInValues = regexp.MustCompile(`[,(]\s*-\d`)

func buildNegQtyLots(t *testing.T) (slotSQL, moveSQL string) {
	t.Helper()
	slotSQL, moveSQL, _ = buildLotsAndMoves(t, Input{
		ClinicCode: "MBNEG1",
		BranchDump: branchRegistryWithStocks(),
		MongoDump:  negQtyLots(),
		BranchMap:  tickedSeven(),
	})
	return slotSQL, moveSQL
}

func TestNegativeQuantityBecomesZeroInTheLot(t *testing.T) {
	slotSQL, _ := buildNegQtyLots(t)

	for _, code := range []string{"NEG-ONHAND", "NEG-BOTH", "NEG-TOTAL-ONLY"} {
		stmt := statementFor(slotSQL, code)
		if stmt == "" {
			t.Errorf("lot %s was not written at all — a clamped quantity must still produce the lot", code)
			continue
		}
		if negativeNumberInValues.MatchString(stmt) {
			t.Errorf("lot %s reached the database with a negative number:\n%s", code, stmt)
		}
	}
}

func TestNegativeQuantityStillWritesTheLotAndItsMovement(t *testing.T) {
	slotSQL, moveSQL := buildNegQtyLots(t)

	if got := strings.Count(slotSQL, "INSERT INTO slot_stock"); got != 4 {
		t.Errorf("slot_stock inserts = %d, want 4 — every SKU keeps its lot even at zero", got)
	}
	if got := strings.Count(moveSQL, "INSERT INTO stock_movement"); got != 4 {
		t.Errorf("stock_movement inserts = %d, want 4 — V4 opens no lot without a receive row", got)
	}
	for _, code := range []string{"NEG-ONHAND", "NEG-BOTH"} {
		stmt := statementFor(moveSQL, code)
		if stmt == "" {
			t.Errorf("no receive movement for %s", code)
			continue
		}
		if negativeNumberInValues.MatchString(stmt) {
			t.Errorf("the receive movement for %s carries a negative qty_delta:\n%s", code, stmt)
		}
	}
}

func TestPositiveQuantitiesAreUntouched(t *testing.T) {
	// The clamp must not round, reformat or otherwise touch a number that was fine. The
	// digits are handed to Postgres verbatim on purpose (moneyLit's contract), so an
	// implementation that parsed to float64 and printed it back would break exactness here.
	slotSQL, moveSQL := buildNegQtyLots(t)

	stmt := statementFor(slotSQL, "POS-CONTROL")
	if stmt == "" {
		t.Fatal("the control lot was not written")
	}
	for _, want := range []string{"42", "200"} {
		if !strings.Contains(stmt, want) {
			t.Errorf("the control lot lost its quantity %s:\n%s", want, stmt)
		}
	}
	if move := statementFor(moveSQL, "POS-CONTROL"); !strings.Contains(move, "42") {
		t.Errorf("the control movement lost its qty_delta:\n%s", move)
	}
}

func TestClampedQuantityAlsoZeroesTheLotValue(t *testing.T) {
	// cost_lot is unit cost × quantity. Multiplying a real cost by a negative quantity
	// would hand the stock report a negative valuation, so the clamped quantity has to be
	// the one that reaches the multiplication.
	slotSQL, _ := buildNegQtyLots(t)

	stmt := statementFor(slotSQL, "NEG-ONHAND")
	if strings.Contains(stmt, "-5") {
		t.Errorf("the source quantity -5 survived into the lot value:\n%s", stmt)
	}
	// lotCostExpr short-circuits to the literal 0 when either side is zero, so the clamped
	// lot must not carry a multiplication at all.
	if strings.Contains(stmt, "::numeric * ") {
		t.Errorf("a zero-quantity lot still multiplies its cost:\n%s", stmt)
	}
}

func TestClampIsOnlyAboutQuantityNotPrice(t *testing.T) {
	// Prices are a different decision (money, not counting) and no export seen carries a
	// negative one. If that ever changes it must be decided, not absorbed silently here —
	// so this records that the clamp does not reach the price columns.
	if got := clampNonNegativeMoney("-5"); got != "0" {
		t.Errorf("clampNonNegativeMoney(-5) = %q, want 0", got)
	}
	if got := clampNonNegativeMoney("-0.01"); got != "0" {
		t.Errorf("clampNonNegativeMoney(-0.01) = %q, want 0", got)
	}
	if got := clampNonNegativeMoney("1200.50"); got != "1200.50" {
		t.Errorf("clampNonNegativeMoney kept %q, want the digits verbatim", got)
	}
	if got := clampNonNegativeMoney(""); got != "0" {
		t.Errorf("clampNonNegativeMoney(empty) = %q, want 0", got)
	}
	if got := clampNonNegativeMoney("not a number"); got != "0" {
		t.Errorf("clampNonNegativeMoney(garbage) = %q, want 0", got)
	}
	if got := moneyLit("-5"); got != "-5" {
		t.Errorf("moneyLit must stay unclamped for prices, got %q", got)
	}
}
