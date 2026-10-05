package legacyimport

import (
	"math/big"
	"strings"
	"testing"
)

// The six lines that put pinluck-clinic into production with four invoices worth less than
// the receipts under them and two showing debt that never existed. invoice_item.qty is an
// integer column; V2 records the line quantity in bills.list[].amount, and these six carry
// a fraction. Owner's decision 1c (5 Oct 2026): refuse at generate time, because the SQL
// guards read the rows back AFTER their transaction has committed and there is nothing
// left for a RAISE to undo.

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
