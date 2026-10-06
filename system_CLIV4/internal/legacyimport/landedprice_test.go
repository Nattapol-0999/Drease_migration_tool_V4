package legacyimport

import (
	"math/big"
	"strings"
	"testing"

	"system_cliv4/internal/mongodump"
)

// invoice_item.price is numeric(12,2) and Postgres rounds a longer price without a word,
// exactly as the integer qty column rounds a fraction.
//
// THE MEASUREMENT THIS FILE EXISTS FOR — master-clinic (C9C89A4), 6 Oct 2026
//
// Bill 680028253, three lines:
//
//	STR000018       Benzac AC5   150           x 1   = 150
//	LH119           Doctor Fee   100           x 1   = 100
//	STY00011000001  Doxyclate    3.571428572   x 28  = 100.000000016
//	                                           Σ     = 350.000000016
//
// V2's own price_total is 350.000000016 and its receipt collected 350, so the importer's
// arithmetic agreed with the source at every step. The column then held 3.57 — 99.96 over
// 28 units — the net came to 349.96, and guard 3 refused the whole 241,181-line script for
// collecting ฿350 on a bill worth ฿349.96. By then the invoice, its items and its receipts
// were committed, because the script commits per entity.
//
// 3.571428572 is 100/28 as V2's JavaScript computed it. Over the whole export: 4,899 bill
// lines in 2,712 bills, ONE such price, and no fractional quantity at all.
//
// Owner's decision ก (6 Oct 2026): fold it, the same answer already given for quantities.

func TestPriceSurvivesTheColumnKnowsWhatNumericTwoHolds(t *testing.T) {
	cases := []struct {
		in         string
		survives   bool
		measurable bool
		why        string
	}{
		{"150", true, true, "a whole number needs no decimals"},
		{"3.57", true, true, "exactly the scale the column keeps"},
		{"3.5", true, true, "fewer decimals than the scale"},
		{"3.570", true, true, "a trailing zero is not precision"},
		{"3.00", true, true, "two written as money"},
		{"0", true, true, "zero"},
		{"-3.57", true, true, "a credit line still fits"},
		{"3.571428572", false, true, "THE measured value"},
		{"3.575", false, true, "half a satang is still beyond the column"},
		{"0.001", false, true, "a tenth of a satang"},
		{"1e-3", false, true, "the same thing spelled as an exponent"},
		{"now()", false, false, "not a number at all"},
		{"", false, false, "empty"},
		{"ราคา", false, false, "text"},
	}
	for _, c := range cases {
		survives, measurable := priceSurvivesTheColumn(c.in)
		if survives != c.survives || measurable != c.measurable {
			t.Errorf("priceSurvivesTheColumn(%q) = (%v, %v), want (%v, %v) — %s",
				c.in, survives, measurable, c.survives, c.measurable, c.why)
		}
	}
}

func TestPriceAsWrittenRoundsTheWayPostgresDoes(t *testing.T) {
	// Half away from zero, at the column's scale — not banker's rounding.
	cases := []struct{ in, want string }{
		{"3.571428572", "3.57"},
		{"3.575", "3.58"},
		{"3.565", "3.57"},
		{"-3.575", "-3.58"},
		{"150", "150.00"},
	}
	for _, c := range cases {
		got, ok := priceAsWritten(c.in)
		if !ok {
			t.Errorf("priceAsWritten(%q) could not read it", c.in)
			continue
		}
		if got.FloatString(2) != c.want {
			t.Errorf("priceAsWritten(%q) = %s, want %s", c.in, got.FloatString(2), c.want)
		}
	}
	if _, ok := priceAsWritten("now()"); ok {
		t.Error("an unreadable price was measured anyway")
	}
}

// doxyclateBill is the measured bill, as a bills document.
func doxyclateBill() mongodump.Doc {
	line := func(code, name, price string, amount any) mongodump.Doc {
		return mongodump.Doc{
			"types": "medicine", "code": code, "name": name,
			"price": mongodump.Decimal(price), "amount": amount,
		}
	}
	return mongodump.Doc{
		"_id":         mongodump.ObjectID("684b1c930546e9148e8c2267"),
		"status":      "send",
		"hn":          "016800046",
		"invoice_no":  "680028253",
		"price_total": mongodump.Decimal("350.000000016"),
		"list": []any{
			line("STR000018", "Benzac AC5", "150", int64(1)),
			line("LH119", "Doctor Fee", "100", int64(1)),
			line("STY00011000001", "Doxyclate", "3.571428572", int64(28)),
		},
	}
}

func planLinesOf(t *testing.T, d mongodump.Doc) ([]linePlan, []foldedLine, *Report) {
	t.Helper()
	rep := &Report{}
	eItem := &EntityResult{}
	var folded []foldedLine
	unknown := map[string]int{}
	var noCode, useCourse, coursePurchase int
	lines, _, err := planBillLines(d, d.Text("_id"), d.Text("invoice_no"), rep, eItem,
		unknown, &noCode, &useCourse, &coursePurchase, &folded)
	if err != nil {
		t.Fatalf("planBillLines: %v", err)
	}
	return lines, folded, rep
}

func TestTheMeasuredPriceIsFoldedAndTheBillKeepsItsValue(t *testing.T) {
	lines, folded, _ := planLinesOf(t, doxyclateBill())

	if len(lines) != 3 {
		t.Fatalf("wrote %d lines, want 3", len(lines))
	}

	// The offending line is now a whole quantity at a price the column can hold.
	got := lines[2]
	if got.qty != "1" {
		t.Errorf("quantity is %q, want \"1\" — the fold did not run", got.qty)
	}
	if got.price != "100.00" {
		t.Errorf("price is %q, want \"100.00\" (3.571428572 x 28)", got.price)
	}
	if fits, _ := priceSurvivesTheColumn(got.price); !fits {
		t.Errorf("the folded price %q still does not fit numeric(12,2)", got.price)
	}

	// THE POINT: what the three lines are worth, computed the way guard 3 computes it,
	// has to equal the ฿350 the receipt collected. 349.96 is the bug.
	total := new(big.Rat)
	for _, l := range lines {
		p, okP := new(big.Rat).SetString(l.price)
		q, okQ := new(big.Rat).SetString(l.qty)
		if !okP || !okQ {
			t.Fatalf("line %q x %q is not readable as numeric", l.price, l.qty)
		}
		total.Add(total, new(big.Rat).Mul(p, q))
	}
	if total.FloatString(2) != "350.00" {
		t.Errorf("the bill lands worth %s, want 350.00 — guard 3 refuses anything under the ฿350 collected",
			total.FloatString(2))
	}

	// The clean lines are untouched: folding one line must not rewrite the others.
	if lines[0].price != "150" || lines[0].qty != "1" {
		t.Errorf("a line that already fitted was rewritten: price %q qty %q", lines[0].price, lines[0].qty)
	}
	if lines[1].price != "100" || lines[1].qty != "1" {
		t.Errorf("a line that already fitted was rewritten: price %q qty %q", lines[1].price, lines[1].qty)
	}

	// Recorded once, with both numbers, and attributed to the PRICE column.
	if len(folded) != 1 {
		t.Fatalf("recorded %d folded lines, want 1: %+v", len(folded), folded)
	}
	if folded[0].why != foldForPrice {
		t.Errorf("the fold is attributed to %v, want foldForPrice — the report would name the wrong column",
			folded[0].why)
	}
	if folded[0].srcPrice != "3.571428572" || folded[0].srcQty != "28" {
		t.Errorf("the source numbers were not kept: price %q qty %q", folded[0].srcPrice, folded[0].srcQty)
	}
	if folded[0].residue.FloatString(9) != "0.000000016" {
		t.Errorf("residue is %s, want 0.000000016 (what the satang scale drops)",
			folded[0].residue.FloatString(9))
	}
}

func TestAPriceThatFitsIsNeverFolded(t *testing.T) {
	d := mongodump.Doc{
		"_id": mongodump.ObjectID("684b1c930546e9148e8c2268"), "status": "send",
		"hn": "016800046", "invoice_no": "680028254",
		"price_total": mongodump.Decimal("253.57"),
		"list": []any{
			mongodump.Doc{"types": "medicine", "code": "A", "name": "a",
				"price": mongodump.Decimal("3.57"), "amount": int64(28)},
			mongodump.Doc{"types": "medicine", "code": "B", "name": "b",
				"price": mongodump.Decimal("153.610"), "amount": int64(1)},
		},
	}
	lines, folded, _ := planLinesOf(t, d)
	if len(folded) != 0 {
		t.Errorf("a price the column can hold was folded anyway: %+v", folded)
	}
	// 3.57 x 28 keeps its quantity: the receipt still reads "28 x ฿3.57", which is what
	// the clinic sold. Folding here would cost that for nothing.
	if lines[0].qty != "28" {
		t.Errorf("quantity became %q — a line that fitted lost its count", lines[0].qty)
	}
	if lines[1].qty != "1" || lines[1].price != "153.610" {
		t.Errorf("a long-written price was rewritten: price %q qty %q", lines[1].price, lines[1].qty)
	}
}

func TestTheReportNamesThePriceColumnAndNotTheQuantity(t *testing.T) {
	// The message is the whole point of carrying `why`. "Your quantity was a fraction"
	// sends the clinic to look at a stock count; the actual problem is a pack price
	// divided by a pack size.
	rep := &Report{}
	reportFoldedQuantities([]foldedLine{{
		invoiceNo: "680028253", code: "STY00011000001", name: "Doxyclate",
		srcPrice: "3.571428572", srcQty: "28", newPrice: "100.00",
		residue: new(big.Rat), why: foldForPrice,
	}}, rep)

	joined := strings.Join(rep.Warnings, "\n")
	if !strings.Contains(joined, "invoice_item.price") {
		t.Errorf("the warning does not name the price column:\n%s", joined)
	}
	if strings.Contains(joined, "มีจำนวนเป็นเศษ") {
		t.Errorf("a price problem is reported as a fractional quantity:\n%s", joined)
	}
	for _, want := range []string{"680028253", "3.571428572", "28", "100.00", "6 ต.ค. 2569"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the warning does not carry %q:\n%s", want, joined)
		}
	}
	// The cost of the decision, stated rather than discovered.
	if !strings.Contains(joined, "ไม่ใช่ราคาในแคตตาล็อกอีกแล้ว") {
		t.Errorf("the warning does not say the unit price is no longer the catalogue price:\n%s", joined)
	}
}

func TestQuantityAndPriceAreReportedSeparately(t *testing.T) {
	rep := &Report{}
	reportFoldedQuantities([]foldedLine{
		{invoiceNo: "A1", code: "QTY", name: "q", srcPrice: "400", srcQty: "16.25",
			newPrice: "6500.00", residue: new(big.Rat), why: foldForQty},
		{invoiceNo: "B2", code: "PRICE", name: "p", srcPrice: "3.571428572", srcQty: "28",
			newPrice: "100.00", residue: new(big.Rat), why: foldForPrice},
	}, rep)

	if len(rep.Warnings) != 2 {
		t.Fatalf("got %d warnings, want one per reason:\n%s", len(rep.Warnings),
			strings.Join(rep.Warnings, "\n--\n"))
	}
	qtyMsg, priceMsg := "", ""
	for _, w := range rep.Warnings {
		if strings.Contains(w, "มีจำนวนเป็นเศษ") {
			qtyMsg = w
		}
		if strings.Contains(w, "invoice_item.price") {
			priceMsg = w
		}
	}
	if qtyMsg == "" || priceMsg == "" {
		t.Fatalf("the two reasons did not each get a warning:\n%s", strings.Join(rep.Warnings, "\n--\n"))
	}
	if strings.Contains(qtyMsg, "B2") {
		t.Errorf("the quantity warning lists a price-only line:\n%s", qtyMsg)
	}
	if strings.Contains(priceMsg, "A1") {
		t.Errorf("the price warning lists a quantity-only line:\n%s", priceMsg)
	}
}

func TestALineThatBreaksBothColumnsIsNamedInBothWarnings(t *testing.T) {
	// 0.5 of something priced at 3.571428572 breaks the integer qty AND the two-decimal
	// price. Whoever reads either message has to see it.
	rep := &Report{}
	reportFoldedQuantities([]foldedLine{{
		invoiceNo: "C3", code: "BOTH", name: "b", srcPrice: "3.571428572", srcQty: "0.5",
		newPrice: "1.79", residue: new(big.Rat), why: foldForBoth,
	}}, rep)
	if len(rep.Warnings) != 2 {
		t.Fatalf("got %d warnings, want the line in both:\n%s", len(rep.Warnings),
			strings.Join(rep.Warnings, "\n--\n"))
	}
	for _, w := range rep.Warnings {
		if !strings.Contains(w, "C3") {
			t.Errorf("a line that broke both columns is missing from a warning:\n%s", w)
		}
	}
}

func TestABothColumnsLineIsFoldedOnceWithTheRightReason(t *testing.T) {
	d := mongodump.Doc{
		"_id": mongodump.ObjectID("684b1c930546e9148e8c2269"), "status": "send",
		"hn": "016800046", "invoice_no": "680028255",
		"price_total": mongodump.Decimal("1.785714286"),
		"list": []any{
			mongodump.Doc{"types": "medicine", "code": "BOTH", "name": "b",
				"price": mongodump.Decimal("3.571428572"), "amount": mongodump.Decimal("0.5")},
		},
	}
	lines, folded, _ := planLinesOf(t, d)
	if len(folded) != 1 {
		t.Fatalf("recorded %d folds, want 1", len(folded))
	}
	if folded[0].why != foldForBoth {
		t.Errorf("why = %v, want foldForBoth", folded[0].why)
	}
	if lines[0].qty != "1" || lines[0].price != "1.79" {
		t.Errorf("folded to price %q qty %q, want 1.79 x 1", lines[0].price, lines[0].qty)
	}
}

func TestAnOverPrecisePriceIsRefusedWhenItCannotBeFolded(t *testing.T) {
	// The fold needs both literals readable. An unreadable quantity next to an
	// over-precise price gets past it — and must not get past the check, because the
	// column would round the price and nothing downstream would notice a bill that grew.
	plans := []billPlan{
		billWithLines("680028253", "016800046", [3]string{"STY00011000001", "3.571428572", "now()"}),
	}
	err := checkLandedLineValues(plans, &Report{})
	if err == nil {
		t.Fatal("an over-precise price was accepted because its quantity was unreadable")
	}
	msg := err.Error()
	for _, want := range []string{
		"680028253", "3.571428572", "3.57", "invoice_item.price",
		"ยังไม่มีอะไรแตะฐานข้อมูล",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not carry %q:\n%s", want, msg)
		}
	}
	// It must not be described as a quantity problem.
	if strings.Contains(msg, "มีจำนวนเป็นเศษ") {
		t.Errorf("a price refusal is worded as a quantity refusal:\n%s", msg)
	}
}

func TestBothRefusalsAppearWhenAClinicHasBothProblems(t *testing.T) {
	plans := []billPlan{
		billWithLines("670070199", "016700062", [3]string{"ST118", "2000", "0.5"}),
		billWithLines("680028253", "016800046", [3]string{"STY0001", "3.571428572", "now()"}),
	}
	err := checkLandedLineValues(plans, &Report{})
	if err == nil {
		t.Fatal("accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "มีจำนวนเป็นเศษ") {
		t.Errorf("the quantity refusal is missing:\n%s", msg)
	}
	if !strings.Contains(msg, "invoice_item.price") {
		t.Errorf("the price refusal is missing:\n%s", msg)
	}
	// Counted separately: one line each, not two under one heading.
	if strings.Count(msg, "1 บรรทัดใน 1 ใบ") != 2 {
		t.Errorf("the two problems are not counted separately:\n%s", msg)
	}
}

func TestTheCheckSaysItLookedAtBothColumns(t *testing.T) {
	// "Nothing wrong" has to be distinguishable from "nothing checked", for the price
	// column as well as the quantity one.
	plans := []billPlan{
		billWithLines("690000001", "016900001", [3]string{"ST090", "4.00", "40"}),
	}
	rep := &Report{}
	if err := checkLandedLineValues(plans, rep); err != nil {
		t.Fatalf("a clean clinic was refused: %v", err)
	}
	joined := strings.Join(rep.Notes, "\n")
	for _, want := range []string{"invoice_item.qty", "invoice_item.price"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the note does not record that %s was checked:\n%s", want, joined)
		}
	}
}

// THE BACKSTOP. planBillLines folds what it can read; this catches everything else, and
// every other numeric column in the schema.
func TestTheScriptScanRefusesAPriceBeyondTheColumnScale(t *testing.T) {
	script := `BEGIN;
INSERT INTO invoice_item
    (clinic_code, invoice_id, kind, code, name, price, qty, discount, use_course,
     sort_order, catalog_item_id, compensations, created_at)
SELECT 'C9C89A4', 1, 'supply', 'STY00011000001', 'Doxyclate', 3.571428572, 28, 0, false,
       0, NULL, '[]'::jsonb, now();
COMMIT;
`
	violations, st := scanScriptColumnLimits(script)
	if st.Statements != 1 {
		t.Fatalf("read %d statements, want 1 (the shape changed and this test is now blind)", st.Statements)
	}
	var found bool
	for _, v := range violations {
		if v.kind == kindScale && v.column == "price" {
			found = true
			if !strings.Contains(v.got, "3.57") {
				t.Errorf("the violation does not say what the column would hold: %+v", v)
			}
		}
	}
	if !found {
		t.Fatalf("3.571428572 into numeric(12,2) was accepted — this is the bug that refused a whole import: %+v",
			violations)
	}
	msg := describeLimitViolations(violations)
	if !strings.Contains(msg, "มีทศนิยมเกิน") {
		t.Errorf("the refusal does not explain the scale problem:\n%s", msg)
	}
}

func TestTheScriptScanLeavesPricesThatFitAlone(t *testing.T) {
	// Including a price written long. 3.570 is the number the column holds.
	script := `BEGIN;
INSERT INTO invoice_item
    (clinic_code, invoice_id, kind, code, name, price, qty, discount, use_course,
     sort_order, catalog_item_id, compensations, created_at)
SELECT 'C9C89A4', 1, 'supply', 'A', 'a', 3.570, 28, 0, false,
       0, NULL, '[]'::jsonb, now();
INSERT INTO invoice_item
    (clinic_code, invoice_id, kind, code, name, price, qty, discount, use_course,
     sort_order, catalog_item_id, compensations, created_at)
SELECT 'C9C89A4', 1, 'supply', 'B', 'b', 100.00, 1, 0, false,
       1, NULL, '[]'::jsonb, now();
COMMIT;
`
	violations, _ := scanScriptColumnLimits(script)
	for _, v := range violations {
		if v.kind == kindScale {
			t.Errorf("a price the column holds exactly was refused: %+v", v)
		}
	}
}

func TestAColumnWithAWiderScaleKeepsItsDecimals(t *testing.T) {
	// loyalty_policy.baht_per_point is numeric(12,4): four decimals are not a violation
	// there, and a check that used one global scale would refuse them.
	script := `BEGIN;
INSERT INTO loyalty_policy (clinic_code, baht_per_point) VALUES ('C9C89A4', 0.0025);
COMMIT;
`
	violations, _ := scanScriptColumnLimits(script)
	for _, v := range violations {
		if v.kind == kindScale {
			t.Errorf("numeric(12,4) was held to two decimals: %+v", v)
		}
	}

	// And five decimals in that same column IS a violation.
	tooMany := strings.Replace(script, "0.0025", "0.00255", 1)
	violations, _ = scanScriptColumnLimits(tooMany)
	var found bool
	for _, v := range violations {
		if v.kind == kindScale {
			found = true
		}
	}
	if !found {
		t.Errorf("five decimals into numeric(12,4) was accepted: %+v", violations)
	}
}

func TestDecimalsBeyondScaleIgnoresTrailingZeros(t *testing.T) {
	cases := []struct {
		in    string
		scale int
		over  bool
	}{
		{"3.57", 2, false},
		{"3.570", 2, false},
		{"3.5700000", 2, false},
		{"3.571", 2, true},
		{"3.571428572", 2, true},
		{"150", 2, false},
		{"0.0025", 4, false},
		{"0.00255", 4, true},
	}
	for _, c := range cases {
		if _, over := decimalsBeyondScale(c.in, c.scale); over != c.over {
			t.Errorf("decimalsBeyondScale(%q, %d) over = %v, want %v", c.in, c.scale, over, c.over)
		}
	}
}
