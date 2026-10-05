package legacyimport

import (
	"strings"
	"testing"
)

// An integer column cannot hold a fraction, and Postgres does not refuse one — it rounds,
// commits, and says nothing. That is how six fractional bills.list[].amount values became
// invoice_item.qty of 0, 1, 1, 1, 2 on pinluck-clinic, with the whole clinic in production
// before anything complained. The range check could not see it: integerFits compares only
// the integer part, on purpose, so a wide legacy id is not called out of range over its
// decimals. Owner's decision 2 (5 Oct 2026): make it a violation, for every integer column
// this importer writes.

func TestFractionalPartIgnoresTrailingZeros(t *testing.T) {
	cases := []struct {
		in         string
		wantDigits string
		wantFrac   bool
		why        string
	}{
		{"0.2", "2", true, "the measured ST039 quantity"},
		{"1.44", "44", true, "the measured ST168 quantity"},
		{"0.005", "005", true, "the half-satang Postgres would round away"},
		{"2.00", "", false, "the whole number two, written long"},
		{"2.0", "", false, "same"},
		{"40", "", false, "no decimal point at all"},
		{"0", "", false, "zero"},
		{"-1.5", "5", true, "negative fractions count too"},
		{"-3.000", "", false, "negative whole number"},
		{"100.10", "10", true, "a trailing zero after a real digit still counts"},
	}
	for _, c := range cases {
		gotDigits, gotFrac := fractionalPart(c.in)
		if gotFrac != c.wantFrac || gotDigits != c.wantDigits {
			t.Errorf("fractionalPart(%q) = (%q, %v), want (%q, %v) — %s",
				c.in, gotDigits, gotFrac, c.wantDigits, c.wantFrac, c.why)
		}
	}
}

func TestFractionalQtyInAnIntegerColumnRefusesTheScript(t *testing.T) {
	// The exact statement shape renderInvoiceItems produces, with the measured quantity.
	script := `BEGIN;
INSERT INTO invoice_item
    (clinic_code, invoice_id, kind, code, name, price, qty, discount, use_course,
     sort_order, catalog_item_id, compensations, created_at)
SELECT 'CBC40B0', 1, 'supply', 'ST039', 'x', 500, 0.2, 0, false,
       0, NULL, '[]'::jsonb, now();
COMMIT;
`
	violations, st := scanScriptColumnLimits(script)
	if st.Statements != 1 {
		t.Fatalf("read %d statements, want 1 (the shape changed and this test is now blind)", st.Statements)
	}
	if len(violations) == 0 {
		t.Fatal("0.2 into invoice_item.qty was accepted — this is the bug that reached production")
	}
	var found bool
	for _, v := range violations {
		if v.table == "invoice_item" && v.column == "qty" && v.kind == kindFraction {
			found = true
			if !strings.Contains(v.got, "ปัด") || !strings.Contains(v.got, "0") {
				t.Errorf("the violation does not say what the column would end up holding: %+v", v)
			}
		}
	}
	if !found {
		t.Errorf("no integer-fraction violation on invoice_item.qty: %+v", violations)
	}

	// And the whole thing has to refuse, not warn.
	rep := &Report{}
	if err := checkScriptColumnLimits(script, rep); err == nil {
		t.Fatal("the script was handed out with a fractional quantity in it")
	} else if !strings.Contains(err.Error(), "มีเศษ") {
		t.Errorf("the refusal does not explain the problem:\n%s", err)
	}
}

func TestWholeQtyWrittenWithDecimalsIsNotAViolation(t *testing.T) {
	// moneyLit renders source digits verbatim, so a quantity of 2 can arrive as "2.00".
	// Refusing that would block every clinic over a formatting detail.
	script := `BEGIN;
INSERT INTO invoice_item
    (clinic_code, invoice_id, kind, code, name, price, qty, discount, use_course,
     sort_order, catalog_item_id, compensations, created_at)
SELECT 'CBC40B0', 1, 'supply', 'ST090', 'x', 4, 40.00, 0, false,
       0, NULL, '[]'::jsonb, now();
COMMIT;
`
	violations, _ := scanScriptColumnLimits(script)
	for _, v := range violations {
		if v.kind == kindFraction {
			t.Errorf("40.00 was called a fraction: %+v", v)
		}
	}
}

func TestEveryIntegerColumnIsCoveredNotJustQty(t *testing.T) {
	// The point of putting this in widths.go rather than only in the money planner: the
	// same silent rounding was available on thirteen other columns.
	if _, ok := columnIntegers["receipt"]["installment_no"]; !ok {
		t.Fatal("receipt.installment_no is no longer an integer column; this test is stale")
	}
	script := `BEGIN;
INSERT INTO receipt
    (clinic_code, invoice_id, receipt_no, installment_no, hn, patient_name,
     paid_amount, discount, deposit_apply, gift_card_apply, point_apply, point_spent,
     point_earned, vat_amount, vat_rate, note, detail, doctor_code, pay_date,
     branch_code, created_at)
SELECT 'CBC40B0', 1, 'R1', 1.5, '016900001', NULL,
       100, 0, 0, 0, 0, 0,
       0.25, 0, 0, NULL, NULL, NULL, now(),
       '01', now();
COMMIT;
`
	violations, _ := scanScriptColumnLimits(script)
	hit := map[string]bool{}
	for _, v := range violations {
		if v.kind == kindFraction {
			hit[v.column] = true
		}
	}
	for _, col := range []string{"installment_no", "point_earned"} {
		if !hit[col] {
			t.Errorf("a fractional value in receipt.%s was not caught: %+v", col, violations)
		}
	}
}

func TestRangeAndFractionAreReportedSeparately(t *testing.T) {
	// 3000000000.5 breaks two different rules with two different fixes. Grouping by
	// column alone printed one entry under whichever was seen first, with a count
	// covering both.
	v := []limitViolation{
		{table: "invoice_item", column: "qty", kind: kindInteger,
			limit: "integer — ช่วงที่รับได้ -2147483648 ถึง 2147483647", got: "เกินช่วง", sample: "3000000000.5"},
		{table: "invoice_item", column: "qty", kind: kindFraction,
			limit: "integer — เก็บได้แต่จำนวนเต็ม", got: "มีเศษ .5 (Postgres จะปัดให้เป็น 3000000001)", sample: "3000000000.5"},
	}
	msg := describeLimitViolations(v)
	if !strings.Contains(msg, "2 คอลัมน์") {
		t.Errorf("the two rules were merged into one entry:\n%s", msg)
	}
	for _, want := range []string{"เกินช่วง", "เก็บได้แต่จำนวนเต็ม"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not carry %q:\n%s", want, msg)
		}
	}
	// And the fraction advice is only added when a fraction is actually present.
	if !strings.Contains(msg, "จำนวน 1 ราคา 100") {
		t.Errorf("the message does not say how to fix a fractional quantity:\n%s", msg)
	}
	onlyWidth := describeLimitViolations([]limitViolation{
		{table: "receipt", column: "detail", kind: kindWidth,
			limit: "varchar(255)", got: "26286 ตัวอักษร", sample: "x"},
	})
	if strings.Contains(onlyWidth, "จำนวน 1 ราคา 100") {
		t.Errorf("the fraction advice was added to a width-only refusal:\n%s", onlyWidth)
	}
}

func TestRoundedForErrorAgreesWithTheDatabaseCast(t *testing.T) {
	cases := map[string]string{
		"0.2": "0", "1.44": "1", "0.5": "1", "1.5": "2", "2.5": "3", "-0.5": "-1",
	}
	for in, want := range cases {
		if got := roundedForError(in); got != want {
			t.Errorf("roundedForError(%q) = %s, want %s", in, got, want)
		}
	}
	if got := roundedForError("now()"); !strings.Contains(got, "อ่านค่าไม่ออก") {
		t.Errorf("an unreadable value rendered as %q instead of saying so", got)
	}
}
