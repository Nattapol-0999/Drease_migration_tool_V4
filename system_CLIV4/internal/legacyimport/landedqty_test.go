package legacyimport

import (
	"math/big"
	"strings"
	"testing"
)

// The six lines that put pinluck-clinic into production with four invoices worth less than
// the receipts under them and two showing debt that never existed. invoice_item.qty is an
// integer column; V2 records the line quantity in bills.list[].amount, and these six carry
// a fraction.
//
// TWO DECISIONS, IN ORDER, AND BOTH ARE STILL HERE
//
// 1c (5 Oct): refuse at generate time, because the SQL guards read the rows back AFTER
// their transaction has committed and there is nothing left for a RAISE to undo.
//
// ค (5 Oct, later the same day): move the fraction into the price instead — 16.25 at ฿400
// becomes 1 at ฿6,500 — so an ordinary clinical quantity does not send the clinic back to
// hand-edit V2 before every migration.
//
// So checkLandedLineValues no longer fires on a dump like this: planBillLines folds the
// fraction before a plan is built. What it tests now is the INVARIANT — no plan may reach
// the writers carrying a quantity an integer column would round — which is what proves the
// fold is complete, plus the backstop for a literal nothing can parse.

func TestQtyAsWrittenRoundsTheWayPostgresCasts(t *testing.T) {
	// numeric -> int4 is half AWAY FROM ZERO, not banker's rounding. 2.5 must be 3, or a
	// check built on this would disagree with the database it is protecting.
	cases := []struct {
		in   string
		want string
	}{
		{"0.2", "0"}, // measured: ST039 x0.2 landed as 0
		{"1.44", "1"},
		{"0.5", "1"}, // measured: ST118 x0.5 landed as 1
		{"1.5", "2"}, // measured: LH001 x1.5 landed as 2
		{"2.5", "3"}, // not banker's rounding, which would give 2
		{"3.5", "4"},
		{"0.4999", "0"},
		{"1", "1"},
		{"0", "0"},
		{"40", "40"},
		{"-0.5", "-1"}, // away from zero in both directions
		{"-1.44", "-1"},
		{"2.00", "2"},
	}
	for _, c := range cases {
		got, ok := qtyAsWritten(c.in)
		if !ok {
			t.Errorf("%q was not readable as a number", c.in)
			continue
		}
		if got.String() != c.want {
			t.Errorf("qtyAsWritten(%q) = %s, want %s", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "   ", "abc", "now()", "1 + 1", "'5'"} {
		if _, ok := qtyAsWritten(bad); ok {
			t.Errorf("%q was read as a number", bad)
		}
	}
}

func TestWholeQuantitiesSurviveTheColumnAndFractionsDoNot(t *testing.T) {
	cases := []struct {
		in       string
		survives bool
		readable bool
	}{
		{"1", true, true},
		{"40", true, true},
		{"2.00", true, true}, // the whole number two, written long
		{"0", true, true},
		{"-3", true, true},
		{"0.2", false, true},
		{"1.44", false, true},
		{"0.005", false, true}, // the half-satang Postgres would round away
		{"abc", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		survives, readable := qtySurvivesTheColumn(c.in)
		if readable != c.readable || survives != c.survives {
			t.Errorf("qtySurvivesTheColumn(%q) = (%v, %v), want (%v, %v)",
				c.in, survives, readable, c.survives, c.readable)
		}
	}
}

func TestLineValueLostIsExactDecimalNotFloat(t *testing.T) {
	// The whole point of big.Rat here: 0.1 and 0.2 cannot be held in float64, and a
	// money figure computed through one is a wrong bill.
	cases := []struct {
		price, qty string
		want       string // positive = the invoice ends up worth LESS than the source says
	}{
		{"500", "0.2", "100.00"},    // 500x0.2 = 100 real, qty 0 stored -> all of it lost
		{"1000", "1.44", "440.00"},  // 1440 real, 1000 stored
		{"2000", "0.5", "-1000.00"}, // 1000 real, 2000 stored -> worth MORE
		{"630", "1.5", "-315.00"},   // 945 real, 1260 stored
		{"4", "40", "0.00"},         // whole quantity, nothing moves
		{"0.10", "0.3", "0.03"},     // a figure float64 cannot represent
		// 333.33x1.11 = 369.9963 real; 1.11 stores as 1, so 333.33 lands and 36.6663 is
		// lost. Rounded to satang for the report, which is where the 0.0037 goes.
		{"333.33", "1.11", "36.67"},
	}
	for _, c := range cases {
		got, ok := lineValueLost(c.price, c.qty)
		if !ok {
			t.Errorf("price=%q qty=%q was not measurable", c.price, c.qty)
			continue
		}
		if got.FloatString(2) != c.want {
			t.Errorf("lineValueLost(%q, %q) = %s, want %s", c.price, c.qty, got.FloatString(2), c.want)
		}
	}
	if _, ok := lineValueLost("abc", "1"); ok {
		t.Error("an unreadable price was measured anyway")
	}
	if _, ok := lineValueLost("500", "now()"); ok {
		t.Error("an unreadable quantity was measured anyway")
	}
}

// billWithLines builds a bill plan carrying the given (code, price, qty) lines.
func billWithLines(no, hn string, lines ...[3]string) billPlan {
	p := billPlan{no: no, hn: hn}
	for i, l := range lines {
		p.lines = append(p.lines, linePlan{
			kind: "supply", code: l[0], name: "รายการ " + l[0],
			price: l[1], qty: l[2], sortOrder: i,
		})
	}
	return p
}

func TestWholeQuantitiesAreNotRefused(t *testing.T) {
	plans := []billPlan{
		billWithLines("690000001", "016900001",
			[3]string{"ST090", "4", "40"},
			[3]string{"LH001", "630", "1"}),
		billWithLines("690000002", "016900002",
			[3]string{"ST044", "360", "2.00"}), // long-written whole number
	}
	rep := &Report{}
	if err := checkLandedLineValues(plans, rep); err != nil {
		t.Fatalf("a clinic with only whole quantities was refused: %v", err)
	}
	// It has to say it looked, or "no problem" cannot be told from "not checked".
	joined := strings.Join(rep.Notes, "\n")
	if !strings.Contains(joined, "invoice_item.qty") {
		t.Errorf("the report does not record that the check ran:\n%s", joined)
	}
}

func TestTheSixMeasuredLinesAreRefusedAndAllNamedAtOnce(t *testing.T) {
	// Exactly the shape measured on pinluck-clinic, in file order.
	plans := []billPlan{
		billWithLines("670070199", "016700062", [3]string{"ST118", "2000", "0.5"}),
		billWithLines("670075362", "016700111",
			[3]string{"ST039", "500", "0.2"},
			[3]string{"ST090", "4", "40"}),
		billWithLines("690004617", "016900003",
			[3]string{"LH058", "100", "25"},
			[3]string{"LH001", "630", "1"},
			[3]string{"ST168", "1000", "1.44"}),
		billWithLines("690012615", "016900008", [3]string{"ST168", "1000", "1.44"}),
		billWithLines("690028527", "016800135", [3]string{"ST039", "500", "0.2"}),
		billWithLines("690040398", "016900048", [3]string{"LH001", "630", "1.5"}),
	}
	err := checkLandedLineValues(plans, &Report{})
	if err == nil {
		t.Fatal("six fractional quantities were accepted, which is how this reached production")
	}
	msg := err.Error()

	// One refusal, every bill in it. Stopping at the first is what turns one clinic into
	// three round trips of pull, create, generate, import, fail, delete.
	for _, want := range []string{
		"670070199", "670075362", "690004617", "690012615", "690028527", "690040398",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name invoice %s:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "6 บรรทัดใน 6 ใบ") {
		t.Errorf("the refusal does not count the lines and bills:\n%s", msg)
	}
	// The money each bill moves by, both directions.
	for _, want := range []string{
		"670075362 100.00", "690004617 440.00", "690012615 440.00", "690028527 100.00",
		"670070199 -1000.00", "690040398 -315.00",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not state the drift %q:\n%s", want, msg)
		}
	}
	// Nothing has been written when this fires, and the operator has to know that before
	// going to look at the database.
	if !strings.Contains(msg, "ยังไม่มีอะไรแตะฐานข้อมูล") {
		t.Errorf("the refusal does not say the database is untouched:\n%s", msg)
	}
	// And what to do about it.
	if !strings.Contains(msg, "จำนวน 1 ราคา 100") {
		t.Errorf("the refusal does not say how to fix the source:\n%s", msg)
	}
	// The lines that were fine must not be listed.
	if strings.Contains(msg, "ST090") || strings.Contains(msg, "LH058") {
		t.Errorf("a whole-quantity line was reported as a problem:\n%s", msg)
	}
}

func TestRefusalNamesTheDirectionOfTheDrift(t *testing.T) {
	// Rounding UP is the dangerous one: it shows a patient a debt they never had, and no
	// SQL guard looks for it. The message must not call both directions "lost".
	up := checkLandedLineValues([]billPlan{
		billWithLines("670070199", "016700062", [3]string{"ST118", "2000", "0.5"}),
	}, &Report{})
	if up == nil {
		t.Fatal("a quantity that rounds up was accepted")
	}
	if !strings.Contains(up.Error(), "ยอดบิลจะมากกว่าต้นทาง") {
		t.Errorf("a rounded-up quantity is not described as inflating the bill:\n%s", up)
	}
	down := checkLandedLineValues([]billPlan{
		billWithLines("670075362", "016700111", [3]string{"ST039", "500", "0.2"}),
	}, &Report{})
	if down == nil {
		t.Fatal("a quantity that rounds down was accepted")
	}
	if !strings.Contains(down.Error(), "ยอดบิลจะน้อยกว่าต้นทาง") {
		t.Errorf("a rounded-down quantity is not described as cutting the bill:\n%s", down)
	}
}

func TestRefusalIsTheSameOnEveryRun(t *testing.T) {
	// Map iteration order must not reach the message: two runs of one dump have to
	// produce the same refusal, or an operator cannot tell a re-run from a change.
	plans := []billPlan{
		billWithLines("690040398", "016900048", [3]string{"LH001", "630", "1.5"}),
		billWithLines("670070199", "016700062", [3]string{"ST118", "2000", "0.5"}),
		billWithLines("690004617", "016900003", [3]string{"ST168", "1000", "1.44"}),
	}
	var first string
	for i := 0; i < 5; i++ {
		err := checkLandedLineValues(plans, &Report{})
		if err == nil {
			t.Fatalf("run %d accepted fractional quantities", i)
		}
		if first == "" {
			first = err.Error()
			continue
		}
		if err.Error() != first {
			t.Fatalf("run %d produced a different refusal:\n%s\n--- vs ---\n%s", i, err, first)
		}
	}
}

func TestAnUnreadableQuantityIsLeftToTheScriptScan(t *testing.T) {
	// Guessing at a literal this cannot parse would invent a violation. The finished
	// script is scanned by checkScriptColumnLimits, which refuses a fractional value in
	// any integer column by name — so nothing is let through, it is caught one step on.
	plans := []billPlan{
		billWithLines("690000003", "016900003", [3]string{"ST001", "100", "now()"}),
	}
	if err := checkLandedLineValues(plans, &Report{}); err != nil {
		t.Fatalf("an unreadable quantity was turned into a refusal here: %v", err)
	}
}

func TestEmptyPlansAreNotARefusal(t *testing.T) {
	if err := checkLandedLineValues(nil, &Report{}); err != nil {
		t.Fatalf("a run with no bills was refused: %v", err)
	}
	if err := checkLandedLineValues([]billPlan{}, nil); err != nil {
		t.Fatalf("a nil report broke the check: %v", err)
	}
}

func TestDriftAccumulatesPerBillNotPerLine(t *testing.T) {
	// One bill with two fractional lines moves by the SUM of them, and a bill is listed
	// once. Reporting per line would make a bill with four bad lines look like four bills.
	plans := []billPlan{
		billWithLines("690099999", "016900099",
			[3]string{"ST039", "500", "0.2"},   // +100
			[3]string{"ST168", "1000", "1.44"}, // +440
			[3]string{"ST090", "4", "40"}),     // fine
	}
	err := checkLandedLineValues(plans, &Report{})
	if err == nil {
		t.Fatal("accepted")
	}
	if !strings.Contains(err.Error(), "690099999 540.00") {
		t.Errorf("the per-bill drift is not the sum of its lines:\n%s", err)
	}
	if !strings.Contains(err.Error(), "2 บรรทัดใน 1 ใบ") {
		t.Errorf("the counts do not read as two lines in one bill:\n%s", err)
	}
}

func TestLineValueLostMatchesABigRatComputedByHand(t *testing.T) {
	// A guard against the arithmetic drifting: 1000 x 1.44 is 1440 exactly, and the
	// stored quantity is 1, so 440 is lost. Done twice, two ways.
	byHand := new(big.Rat)
	byHand.SetString("1440")
	byHand.Sub(byHand, new(big.Rat).SetInt64(1000))
	got, ok := lineValueLost("1000", "1.44")
	if !ok {
		t.Fatal("not measurable")
	}
	if got.Cmp(byHand) != 0 {
		t.Errorf("lineValueLost = %s, hand-computed = %s", got.FloatString(4), byHand.FloatString(4))
	}
}

// ── moving the fraction into the price (owner's decision ค) ──────────────────────────
//
// The owner asked for the fraction to be ROUNDED UP first. Measured on the two dumps that
// hit this, that invents ฿3,835 of debt across eight invoices — 16.25 -> 17 makes a ฿6,500
// bill ฿6,800 while the receipt still says ฿6,500, so a full_paid visit turns into
// partially_paid and the patient is shown a balance they never owed. Guard 3 never sees
// it: it only looks for money ABOVE the net. Folding into the price keeps every satang.

func TestFoldFractionIntoPriceKeepsTheValueExactly(t *testing.T) {
	// Every fractional line measured on the two clinics, and what each has to become.
	cases := []struct {
		price, qty string
		want       string
		where      string
	}{
		{"400", "16.25", "6500.00", "denarin LH026 Xeomin, twice"},
		{"2000", "0.5", "1000.00", "pinluck 670070199 ST118, half a bottle"},
		{"500", "0.2", "100.00", "pinluck 670075362 and 690028527 ST039"},
		{"1000", "1.44", "1440.00", "pinluck 690004617 and 690012615 ST168"},
		{"630", "1.5", "945.00", "pinluck 690040398 LH001"},
	}
	for _, c := range cases {
		got, residue, ok := foldFractionIntoPrice(c.price, c.qty)
		if !ok {
			t.Errorf("%s: %s x %s was not foldable", c.where, c.qty, c.price)
			continue
		}
		if got != c.want {
			t.Errorf("%s: %s x %s folded to %s, want %s", c.where, c.qty, c.price, got, c.want)
		}
		if residue.Sign() != 0 {
			t.Errorf("%s: %s x %s left a residue of %s — these all divide exactly",
				c.where, c.qty, c.price, residue.FloatString(6))
		}
	}
}

func TestFoldingIsNeverRoundingUp(t *testing.T) {
	// The decision that was rejected, kept as a test so it cannot creep back in.
	// Rounding 16.25 up gives 17 x 400 = 6,800 against a ฿6,500 receipt.
	got, _, ok := foldFractionIntoPrice("400", "16.25")
	if !ok {
		t.Fatal("not foldable")
	}
	if got == "6800.00" {
		t.Fatal("the quantity was rounded up — that is the ฿300 of invented debt this avoids")
	}
	if got != "6500.00" {
		t.Fatalf("folded to %s, want the source value 6500.00", got)
	}
}

func TestFoldReportsTheHalfSatangItCannotKeep(t *testing.T) {
	// invoice_item.price is numeric(12,2). 333.33 x 1.11 is 369.9963, which cannot be
	// held, so the product is rounded and the residue has to be said out loud.
	got, residue, ok := foldFractionIntoPrice("333.33", "1.11")
	if !ok {
		t.Fatal("not foldable")
	}
	if got != "370.00" {
		t.Errorf("folded to %s, want 370.00 (half away from zero, like the database)", got)
	}
	if residue.Sign() == 0 {
		t.Error("a product that does not fit two decimals reported no residue")
	}
	if residue.FloatString(4) != "-0.0037" {
		t.Errorf("residue = %s, want -0.0037", residue.FloatString(4))
	}
}

func TestFoldRefusesNothingButLeavesUnreadableLiteralsAlone(t *testing.T) {
	for _, c := range [][2]string{{"abc", "1.5"}, {"500", "now()"}, {"", "0.2"}} {
		if _, _, ok := foldFractionIntoPrice(c[0], c[1]); ok {
			t.Errorf("price=%q qty=%q was folded on a guess", c[0], c[1])
		}
	}
}

func TestReportOfFoldedQuantitiesNamesBothNumbersAndTheSideEffect(t *testing.T) {
	rep := &Report{}
	zero := func() *big.Rat { return new(big.Rat) }
	reportFoldedQuantities([]foldedLine{
		{invoiceNo: "690011967", code: "LH026", name: "Program Botulinum Toxin 1u (Xeomin)",
			srcPrice: "400", srcQty: "16.25", newPrice: "6500.00", residue: zero()},
		{invoiceNo: "680108492", code: "LH026", name: "Program Botulinum Toxin 1u (Xeomin)",
			srcPrice: "400", srcQty: "16.25", newPrice: "6500.00", residue: zero()},
	}, rep)

	joined := strings.Join(rep.Warnings, "\n")
	for _, want := range []string{
		"2 บรรทัดใน 2 ใบ", // counted
		"680108492",       // both bills, and in number order
		"690011967",
		"16.25 × 400",          // what the source said
		"จำนวน 1 ราคา 6500.00", // what was written
		"ยอดบิลเท่าเดิมทุกสตางค์",
		"ไม่ใช่ราคาในแคตตาล็อกอีกแล้ว", // the side effect, stated not buried
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the report does not carry %q:\n%s", want, joined)
		}
	}
	// A warning, not a note: the unit price changing is something the clinic must be told.
	if len(rep.Warnings) == 0 {
		t.Error("the fold was reported as a note instead of a warning")
	}
	// Bills must be listed in a fixed order whatever order they were collected in.
	if strings.Index(joined, "680108492") > strings.Index(joined, "690011967") {
		t.Error("the bills are not in number order, so two runs could read differently")
	}
}

func TestResidueIsOnlyReportedWhenThereIsOne(t *testing.T) {
	clean := &Report{}
	reportFoldedQuantities([]foldedLine{
		{invoiceNo: "1", code: "A", name: "a", srcPrice: "400", srcQty: "16.25",
			newPrice: "6500.00", residue: new(big.Rat)},
	}, clean)
	for _, w := range clean.Warnings {
		if strings.Contains(w, "ยอดรวมเคลื่อนไป") {
			t.Errorf("reported a rounding residue that was zero: %s", w)
		}
	}

	lossy := &Report{}
	r, _ := new(big.Rat).SetString("-0.0037")
	reportFoldedQuantities([]foldedLine{
		{invoiceNo: "1", code: "A", name: "a", srcPrice: "333.33", srcQty: "1.11",
			newPrice: "370.00", residue: r},
	}, lossy)
	var said bool
	for _, w := range lossy.Warnings {
		if strings.Contains(w, "ยอดรวมเคลื่อนไป") {
			said = true
			if !strings.Contains(w, "-0.0037") {
				t.Errorf("the residue warning does not state the amount: %s", w)
			}
		}
	}
	if !said {
		t.Error("a non-zero rounding residue was not reported")
	}
}

func TestReportSaysNothingWhenNothingWasFolded(t *testing.T) {
	rep := &Report{}
	reportFoldedQuantities(nil, rep)
	if len(rep.Warnings) != 0 {
		t.Errorf("warned about a fold that did not happen: %v", rep.Warnings)
	}
	reportFoldedQuantities([]foldedLine{{invoiceNo: "1", residue: new(big.Rat)}}, nil)
}

func TestPlansAfterFoldingPassTheInvariantCheck(t *testing.T) {
	// End to end on the shape planBillLines produces: a folded line carries qty 1 and the
	// whole value in its price, so checkLandedLineValues — the invariant that no plan may
	// reach the writers with a quantity an integer column would round — finds nothing.
	newPrice, _, ok := foldFractionIntoPrice("400", "16.25")
	if !ok {
		t.Fatal("not foldable")
	}
	plans := []billPlan{billWithLines("690011967", "016800323",
		[3]string{"LH026", newPrice, "1"})}
	if err := checkLandedLineValues(plans, &Report{}); err != nil {
		t.Fatalf("a folded plan still trips the invariant check: %v", err)
	}
	// And the value is unchanged: 16.25 x 400 = 6,500 = 1 x 6,500.
	before, _ := new(big.Rat).SetString("6500")
	after, _ := new(big.Rat).SetString(newPrice)
	if before.Cmp(after) != 0 {
		t.Errorf("the bill is worth %s after folding, was %s", after.FloatString(2), before.FloatString(2))
	}
}
