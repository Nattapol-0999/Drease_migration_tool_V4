package legacyimport

import (
	"strings"
	"testing"
)

// The guard that stops a value from leaving this tool when it cannot fit the column it is
// bound for. It has to do two things that pull against each other: refuse what would make
// psql stop, and NOT refuse the long or large values that are perfectly legal.
//
// Three real failures shaped it, and each is a test here:
//
//	receipt.detail  varchar(255)  a 26,286-character remark   (C52AEF6, 1 Oct)
//	slot_stock.amount numeric(14,2) a lot of 99,999,999,999,999 (CBE8806, 2 Oct)
//	receipt.note    text          the SAME 26,286 characters, which must pass

func TestLimitGuardRefusesTheRemarkThatBrokeTheRealImport(t *testing.T) {
	long := strings.Repeat("ก", 26286)
	script := "BEGIN;\n" + renderReceipts([]receiptPlan{{
		no: "R1", invoiceNo: "B1", installmentNo: 1, hn: "016900242",
		patientName: "อัยยา", branchCode: "BR00001", doctorCode: "D001",
		remark: long, detail: long, // detail un-clipped: the bug, reproduced
		payDate: "NULL", createdAt: "now()", pointApply: "0",
		paidExpr: "0", discountExpr: "0",
	}}, Input{ClinicCode: "MBU1"}, &EntityResult{}) + "COMMIT;\n"

	violations, st := scanScriptColumnLimits(script)
	if st.Statements != 1 {
		t.Fatalf("read %d INSERT statements, want 1 (skipped %d) — nothing below is tested",
			st.Statements, st.Skipped)
	}
	if len(violations) != 1 {
		t.Fatalf("found %d violations, want exactly 1 (receipt.detail): %+v", len(violations), violations)
	}
	v := violations[0]
	if v.table != "receipt" || v.column != "detail" || v.kind != kindWidth {
		t.Errorf("the violation does not name receipt.detail as a width problem: %+v", v)
	}

	err := checkScriptColumnLimits(script, &Report{})
	if err == nil {
		t.Fatal("accepted a script psql would stop on")
	}
	for _, want := range []string{"receipt.detail", "varchar(255)", "26286", "clip("} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}
}

func TestLimitGuardRefusesTheStockLotThatBrokeTheRealImport(t *testing.T) {
	// CBE8806: slot_stock.amount is numeric(14,2), so anything from 10^12 up overflows.
	// The source had 99,999,999,999,999 on three lots out of 206.
	script := "INSERT INTO slot_stock\n" +
		"    (mongo_id, item_code, code, amount, remain, amount_total, clinic_code, branch_code)\n" +
		"VALUES ('6a3bf39e3a364c0bab2c7bcc', 'ST000098', 'ST000098', 99999999999999, " +
		"99999999999999, 99999999999999, 'MBU1', 'MAIN_STOCK');\n"

	violations, st := scanScriptColumnLimits(script)
	if st.Statements != 1 {
		t.Fatalf("the VALUES shape was not read: statements=%d skipped=%d", st.Statements, st.Skipped)
	}
	// amount, remain and amount_total all overflow; the message groups them per column.
	if len(violations) != 3 {
		t.Fatalf("found %d violations, want 3 (amount, remain, amount_total): %+v",
			len(violations), violations)
	}
	for _, v := range violations {
		if v.kind != kindNumeric {
			t.Errorf("%s.%s was not reported as a numeric problem: %+v", v.table, v.column, v)
		}
	}

	err := checkScriptColumnLimits(script, &Report{})
	if err == nil {
		t.Fatal("accepted a numeric overflow")
	}
	for _, want := range []string{"slot_stock.amount", "numeric(14,2)", "10^12", "99999999999999"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}
}

func TestLimitGuardKnowsTheNarrowNumericColumns(t *testing.T) {
	// receipt.vat_rate and card_charge_percent are numeric(6,2): 9999.99 fits, 10000 does
	// not. These are the tightest money columns in the schema and the easiest to overflow
	// by copying a source value through.
	fits := "INSERT INTO receipt (clinic_code, receipt_no, vat_rate, card_charge_percent)\n" +
		"VALUES ('MBU1', 'R1', 9999.99, 0);\n"
	if v, _ := scanScriptColumnLimits(fits); len(v) != 0 {
		t.Errorf("9999.99 in numeric(6,2) was refused: %+v", v)
	}

	over := "INSERT INTO receipt (clinic_code, receipt_no, vat_rate, card_charge_percent)\n" +
		"VALUES ('MBU1', 'R1', 10000, 0);\n"
	v, _ := scanScriptColumnLimits(over)
	if len(v) != 1 || v[0].column != "vat_rate" {
		t.Errorf("10000 in numeric(6,2) was not refused as vat_rate: %+v", v)
	}
}

func TestLimitGuardChecksIntegerRanges(t *testing.T) {
	// catalog_item.status is smallint. A legacy value copied through is how it overflows.
	over := "INSERT INTO catalog_item (clinic_code, code, status)\n" +
		"VALUES ('MBU1', 'LH001', 40000);\n"
	v, _ := scanScriptColumnLimits(over)
	if len(v) != 1 || v[0].column != "status" || v[0].kind != kindInteger {
		t.Fatalf("40000 in a smallint was not refused: %+v", v)
	}
	if !strings.Contains(describeLimitViolations(v), "32767") {
		t.Errorf("the refusal does not say what a smallint holds:\n%s", describeLimitViolations(v))
	}

	fits := "INSERT INTO catalog_item (clinic_code, code, status)\n" +
		"VALUES ('MBU1', 'LH001', 1);\n"
	if v, _ := scanScriptColumnLimits(fits); len(v) != 0 {
		t.Errorf("status = 1 was refused: %+v", v)
	}
}

func TestLimitGuardLeavesTextAndJsonbAlone(t *testing.T) {
	// The fixed shape of the receipt: the whole remark in note (text), 255 characters in
	// detail. And appointment.doc is jsonb, whose payload runs to hundreds of characters.
	long := strings.Repeat("ก", 26286)
	clipped, _ := clip(long, 255)
	script := renderReceipts([]receiptPlan{{
		no: "R1", invoiceNo: "B1", installmentNo: 1, hn: "016900242",
		patientName: "อัยยา", branchCode: "BR00001", doctorCode: "D001",
		remark: long, detail: clipped,
		payDate: "NULL", createdAt: "now()", pointApply: "0",
		paidExpr: "0", discountExpr: "0",
	}}, Input{ClinicCode: "MBU1"}, &EntityResult{})

	doc := "{\"a\":\"" + strings.Repeat("x", 900) + "\"}"
	script += "INSERT INTO appointment (clinic_code, hn, patient_name, status, doc)\n" +
		"VALUES ('MBU1', '0169001', 'สมชาย ใจดี', 'reserve', '" + doc + "'::jsonb);\n"

	violations, st := scanScriptColumnLimits(script)
	if st.Statements != 2 {
		t.Fatalf("read %d statements, want 2 (skipped %d)", st.Statements, st.Skipped)
	}
	if len(violations) != 0 {
		t.Errorf("a legal text or jsonb value was refused: %+v", violations)
	}
	if st.Strings == 0 {
		t.Error("no string value was compared — the guard read the statements but measured nothing")
	}
}

func TestLimitGuardCountsCharactersNotBytes(t *testing.T) {
	// 255 Thai characters are 765 bytes. Counting bytes would refuse a value that fits.
	exact := strings.Repeat("ก", 255)
	if len(exact) <= 255 {
		t.Fatalf("fixture is not multi-byte: %d bytes for %d runes", len(exact), len([]rune(exact)))
	}
	mk := func(s string) string {
		return renderReceipts([]receiptPlan{{
			no: "R1", invoiceNo: "B1", installmentNo: 1, hn: "0169001",
			patientName: "x", branchCode: "BR00001", doctorCode: "D001",
			remark: s, detail: s, payDate: "NULL", createdAt: "now()",
			pointApply: "0", paidExpr: "0", discountExpr: "0",
		}}, Input{ClinicCode: "MBU1"}, &EntityResult{})
	}
	if v, _ := scanScriptColumnLimits(mk(exact)); len(v) != 0 {
		t.Errorf("exactly 255 characters (%d bytes) was refused: %+v", len(exact), v)
	}
	if v, _ := scanScriptColumnLimits(mk(strings.Repeat("ก", 256))); len(v) != 1 {
		t.Errorf("256 characters was not refused: %+v", v)
	}
}

func TestLimitGuardReportsEveryViolationNotTheFirst(t *testing.T) {
	// The whole point of B: one run has to name everything that needs fixing. Three
	// different columns in three statements must all appear.
	script := "INSERT INTO slot_stock (clinic_code, code, amount)\n" +
		"VALUES ('MBU1', 'ST1', 99999999999999);\n" +
		"INSERT INTO receipt (clinic_code, receipt_no, vat_rate)\n" +
		"VALUES ('MBU1', 'R1', 10000);\n" +
		"INSERT INTO catalog_item (clinic_code, code, status)\n" +
		"VALUES ('MBU1', '" + strings.Repeat("L", 70) + "', 1);\n"

	v, st := scanScriptColumnLimits(script)
	if st.Statements != 3 {
		t.Fatalf("read %d statements, want 3 (skipped %d)", st.Statements, st.Skipped)
	}
	if len(v) != 3 {
		t.Fatalf("found %d violations, want 3: %+v", len(v), v)
	}
	msg := describeLimitViolations(v)
	for _, want := range []string{"slot_stock.amount", "receipt.vat_rate", "catalog_item.code"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "3 ค่า") || !strings.Contains(msg, "3 คอลัมน์") {
		t.Errorf("the refusal does not say how many values and columns:\n%s", msg)
	}
}

func TestLimitGuardGroupsManyRowsOfOneColumnIntoOneEntry(t *testing.T) {
	// A bad column with many rows must read as one entry with a count, not one line per
	// row, or the message becomes unreadable exactly when it matters most.
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("INSERT INTO slot_stock (clinic_code, code, amount)\n")
		b.WriteString("VALUES ('MBU1', 'ST1', 99999999999999);\n")
	}
	v, _ := scanScriptColumnLimits(b.String())
	if len(v) != 50 {
		t.Fatalf("found %d violations, want 50", len(v))
	}
	msg := describeLimitViolations(v)
	if strings.Count(msg, "slot_stock.amount") != 1 {
		t.Errorf("slot_stock.amount is listed more than once:\n%s", msg)
	}
	if !strings.Contains(msg, "50") {
		t.Errorf("the count of 50 rows is missing:\n%s", msg)
	}
}

func TestLimitGuardDoesNotMeasureExpressions(t *testing.T) {
	// A value that is not exactly one literal — a cast, a concatenation, arithmetic, NULL,
	// a subquery — has no length or magnitude this can know. Measuring the source text
	// would invent violations out of SQL that is fine.
	script := "INSERT INTO receipt (clinic_code, detail, paid_amount, doctor_code)\n" +
		"SELECT 'MBU1', '" + strings.Repeat("a", 200) + "' || '" + strings.Repeat("b", 200) +
		"', GREATEST(99999999999999 - 99999999999999, 0), NULL\nWHERE true;\n"

	v, st := scanScriptColumnLimits(script)
	if st.Statements != 1 {
		t.Fatalf("statement not read: statements=%d skipped=%d", st.Statements, st.Skipped)
	}
	if len(v) != 0 {
		t.Errorf("an expression was measured as a literal: %+v", v)
	}
}

func TestLimitGuardIgnoresCommentsAndPlpgsqlBodies(t *testing.T) {
	script := "-- INSERT INTO slot_stock (amount) VALUES (99999999999999)\n" +
		"DO $sku$\nBEGIN\n  INSERT INTO slot_stock (clinic_code, amount)\n" +
		"  VALUES ('MBU1', 99999999999999);\nEND $sku$;\n"

	v, st := scanScriptColumnLimits(script)
	if len(v) != 0 {
		t.Errorf("text inside a comment or a dollar-quoted body was measured: %+v", v)
	}
	if st.Statements != 0 {
		t.Errorf("read %d statements from a script that has none outside a comment or a DO block",
			st.Statements)
	}
}

func TestLimitGuardSkipsShapesItCannotReadInsteadOfRefusing(t *testing.T) {
	script := "INSERT INTO slot_stock SELECT * FROM staging;\n"
	v, st := scanScriptColumnLimits(script)
	if len(v) != 0 {
		t.Errorf("an unreadable shape produced violations: %+v", v)
	}
	if st.Skipped != 1 || st.Statements != 0 {
		t.Errorf("want skipped=1 statements=0, got skipped=%d statements=%d", st.Skipped, st.Statements)
	}
	if err := checkScriptColumnLimits(script, &Report{}); err != nil {
		t.Errorf("an unreadable shape was refused instead of skipped: %v", err)
	}
}

func TestNumericIntegerDigitsReadsOnlyBareNumbers(t *testing.T) {
	cases := []struct {
		in     string
		digits int
		ok     bool
	}{
		{"0", 0, true}, // no significant integer digit
		{"1", 1, true},
		{"99999999999999", 14, true},
		{"-99999999999999", 14, true},
		{"1234.56", 4, true},
		{"0001234", 4, true}, // leading zeros are not significant
		{".50", 0, true},
		{"now()", 0, false},
		{"'0'", 0, false}, // a quoted string is not a number
		{"NULL", 0, false},
		{"GREATEST(0 - 0, 0)", 0, false},
		{"1e9", 0, false}, // exponent form is not read; skipped, not guessed at
		{"", 0, false},
	}
	for _, c := range cases {
		d, _, ok := numericIntegerDigits(c.in)
		if ok != c.ok || (ok && d != c.digits) {
			t.Errorf("numericIntegerDigits(%q) = (%d, %v), want (%d, %v)", c.in, d, ok, c.digits, c.ok)
		}
	}
}

func TestEveryDeclaredLimitIsSane(t *testing.T) {
	for table, cols := range columnWidths {
		if table != strings.ToLower(table) {
			t.Errorf("table %q is not lowercase, so it can never match a statement", table)
		}
		for col, w := range cols {
			if w <= 0 {
				t.Errorf("%s.%s has width %d", table, col, w)
			}
		}
	}
	for table, cols := range columnNumeric {
		for col, lim := range cols {
			if lim.precision <= 0 || lim.scale < 0 || lim.scale > lim.precision {
				t.Errorf("%s.%s has numeric(%d,%d)", table, col, lim.precision, lim.scale)
			}
			if lim.intDigits() <= 0 {
				t.Errorf("%s.%s leaves no room left of the decimal point", table, col)
			}
		}
	}
	for table, cols := range columnIntegers {
		for col, kind := range cols {
			if _, ok := intBounds[kind]; !ok {
				t.Errorf("%s.%s declares kind %q, which has no bounds", table, col, kind)
			}
		}
	}
	// A column must not be declared in two kinds at once: the scan checks width first and
	// would silently never reach the other.
	for table, cols := range columnWidths {
		for col := range cols {
			if _, dup := columnNumeric[table][col]; dup {
				t.Errorf("%s.%s is declared as both varchar and numeric", table, col)
			}
			if _, dup := columnIntegers[table][col]; dup {
				t.Errorf("%s.%s is declared as both varchar and integer", table, col)
			}
		}
	}
}
