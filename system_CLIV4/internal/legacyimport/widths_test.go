package legacyimport

import (
	"strings"
	"testing"
)

// The guard that stops a too-wide value from leaving this tool.
//
// It has to do two things that pull against each other: refuse the value that would make
// psql stop, and NOT refuse the long values that are perfectly legal because their column
// is `text` or `jsonb`. The receipt that broke the kvk-clinic import is both at once —
// 26,286 characters bound for receipt.note (fine) and the same 26,286 bound for
// receipt.detail (fatal) — so it is the test case that matters most.

func TestWidthGuardRefusesTheValueThatBrokeTheRealImport(t *testing.T) {
	long := strings.Repeat("ก", 26286) // the real remark was 26,286 characters
	script := "BEGIN;\n" + renderReceipts([]receiptPlan{{
		no: "R1", invoiceNo: "B1", installmentNo: 1, hn: "016900242",
		patientName: "อัยยา", branchCode: "BR00001", doctorCode: "D001",
		remark: long, detail: long, // detail un-clipped: the bug, reproduced
		payDate: "NULL", createdAt: "now()", pointApply: "0",
		paidExpr: "0", discountExpr: "0",
	}}, Input{ClinicCode: "MBU1"}, &EntityResult{}) + "COMMIT;\n"

	violations, st := scanScriptWidths(script)
	if st.Statements != 1 {
		t.Fatalf("read %d INSERT statements, want 1 (skipped %d) — the guard did not see the "+
			"statement, so nothing below is being tested", st.Statements, st.Skipped)
	}
	if len(violations) != 1 {
		t.Fatalf("found %d violations, want exactly 1 (receipt.detail): %+v", len(violations), violations)
	}
	v := violations[0]
	if v.table != "receipt" || v.column != "detail" || v.width != 255 || v.got != 26286 {
		t.Errorf("the violation does not name the real one: got %s.%s varchar(%d) len=%d",
			v.table, v.column, v.width, v.got)
	}

	// And the refusal has to say what to do, not just that something is wrong.
	err := checkScriptWidths(script, &Report{})
	if err == nil {
		t.Fatal("checkScriptWidths accepted a script psql would stop on")
	}
	for _, want := range []string{"receipt.detail", "varchar(255)", "26286", "clip("} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}
}

func TestWidthGuardLeavesTextColumnsAlone(t *testing.T) {
	// The fixed shape: the whole remark in note (text), 255 characters in detail.
	long := strings.Repeat("ก", 26286)
	clipped, _ := clip(long, 255)
	script := renderReceipts([]receiptPlan{{
		no: "R1", invoiceNo: "B1", installmentNo: 1, hn: "016900242",
		patientName: "อัยยา", branchCode: "BR00001", doctorCode: "D001",
		remark: long, detail: clipped,
		payDate: "NULL", createdAt: "now()", pointApply: "0",
		paidExpr: "0", discountExpr: "0",
	}}, Input{ClinicCode: "MBU1"}, &EntityResult{})

	violations, st := scanScriptWidths(script)
	if st.Statements != 1 {
		t.Fatalf("read %d statements, want 1 (skipped %d)", st.Statements, st.Skipped)
	}
	if len(violations) != 0 {
		t.Errorf("a 26,286-character value in receipt.note was refused — note is `text` and "+
			"has no width to exceed: %+v", violations)
	}
	// It must really have looked: a pass with zero values compared proves nothing.
	if st.Values == 0 {
		t.Error("no string value was compared against a width — the guard read the statement " +
			"but measured nothing")
	}
}

func TestWidthGuardCountsCharactersNotBytes(t *testing.T) {
	// 255 Thai characters are 765 bytes. Counting bytes would refuse a value that fits,
	// which would block an import the database would have accepted.
	exact := strings.Repeat("ก", 255)
	if len(exact) <= 255 {
		t.Fatalf("fixture is not multi-byte: %d bytes for %d runes", len(exact), len([]rune(exact)))
	}
	script := renderReceipts([]receiptPlan{{
		no: "R1", invoiceNo: "B1", installmentNo: 1, hn: "0169001",
		patientName: "x", branchCode: "BR00001", doctorCode: "D001",
		remark: exact, detail: exact,
		payDate: "NULL", createdAt: "now()", pointApply: "0",
		paidExpr: "0", discountExpr: "0",
	}}, Input{ClinicCode: "MBU1"}, &EntityResult{})

	if violations, _ := scanScriptWidths(script); len(violations) != 0 {
		t.Errorf("exactly 255 characters (%d bytes) was refused: %+v", len(exact), violations)
	}

	// One character more must be refused, so the boundary is tested from both sides.
	over := strings.Repeat("ก", 256)
	script = renderReceipts([]receiptPlan{{
		no: "R2", invoiceNo: "B1", installmentNo: 1, hn: "0169001",
		patientName: "x", branchCode: "BR00001", doctorCode: "D001",
		remark: over, detail: over,
		payDate: "NULL", createdAt: "now()", pointApply: "0",
		paidExpr: "0", discountExpr: "0",
	}}, Input{ClinicCode: "MBU1"}, &EntityResult{})
	v, _ := scanScriptWidths(script)
	if len(v) != 1 || v[0].got != 256 {
		t.Errorf("256 characters was not refused as 256: %+v", v)
	}
}

func TestWidthGuardReadsTheVALUESShapeTooAndIgnoresJSONB(t *testing.T) {
	// appointment is written with VALUES, and its `doc` column is jsonb: the generated
	// payload runs to hundreds of characters and must not be refused. patient_name is
	// varchar(255) in the same statement, so the guard has something real to measure.
	doc := "{\"a\":\"" + strings.Repeat("x", 900) + "\"}"
	script := "INSERT INTO appointment (clinic_code, hn, patient_name, status, confirm_status, doc)\n" +
		"VALUES ('MBU1', '0169001', 'สมชาย ใจดี', 'reserve', 'pending', '" + doc + "'::jsonb);\n"

	violations, st := scanScriptWidths(script)
	if st.Statements != 1 {
		t.Fatalf("the VALUES shape was not read: statements=%d skipped=%d", st.Statements, st.Skipped)
	}
	if len(violations) != 0 {
		t.Errorf("a long jsonb payload was refused: %+v", violations)
	}
	if st.Values == 0 {
		t.Error("nothing was measured in a statement that has five varchar columns")
	}

	// Now overflow one of them and make sure it is caught in this shape as well.
	bad := strings.Replace(script, "'reserve'", "'"+strings.Repeat("r", 21)+"'", 1)
	v, _ := scanScriptWidths(bad)
	if len(v) != 1 || v[0].column != "status" || v[0].width != 20 {
		t.Errorf("a 21-character status in a VALUES statement was not refused as status "+
			"varchar(20): %+v", v)
	}
}

func TestWidthGuardIgnoresCommentsAndPlpgsqlBodies(t *testing.T) {
	// Entity headers and DO blocks contain the words "INSERT INTO". Reading them as
	// statements would measure text that is never sent as a value.
	script := "-- INSERT INTO receipt (detail) VALUES ('" + strings.Repeat("z", 400) + "')\n" +
		"DO $sku$\nBEGIN\n  INSERT INTO receipt (clinic_code, detail)\n" +
		"  VALUES ('MBU1', '" + strings.Repeat("y", 400) + "');\nEND $sku$;\n"

	violations, st := scanScriptWidths(script)
	if len(violations) != 0 {
		t.Errorf("text inside a comment or a dollar-quoted body was measured: %+v", violations)
	}
	if st.Statements != 0 {
		t.Errorf("read %d statements from a script that contains none outside a comment or "+
			"a DO block", st.Statements)
	}
}

func TestWidthGuardSkipsShapesItCannotReadInsteadOfRefusing(t *testing.T) {
	// No column list: there is no mapping from values to columns, so there is nothing to
	// check and nothing to refuse. A guard that refused here would block a real import the
	// first time a new statement shape appeared.
	script := "INSERT INTO receipt SELECT * FROM staging_receipt;\n"

	violations, st := scanScriptWidths(script)
	if len(violations) != 0 {
		t.Errorf("an unreadable shape produced violations: %+v", violations)
	}
	if st.Skipped != 1 || st.Statements != 0 {
		t.Errorf("want skipped=1 statements=0, got skipped=%d statements=%d", st.Skipped, st.Statements)
	}
	if err := checkScriptWidths(script, &Report{}); err != nil {
		t.Errorf("an unreadable shape was refused instead of skipped: %v", err)
	}
}

func TestWidthGuardDoesNotMeasureExpressions(t *testing.T) {
	// A value that is not exactly one literal — a cast, a concatenation, a subquery, NULL —
	// has no length this can know. Measuring the source text instead would invent
	// violations out of SQL that is fine.
	script := "INSERT INTO receipt (clinic_code, detail, doctor_code)\n" +
		"SELECT 'MBU1', '" + strings.Repeat("a", 200) + "' || '" + strings.Repeat("b", 200) +
		"', NULL\nWHERE true;\n"

	violations, st := scanScriptWidths(script)
	if st.Statements != 1 {
		t.Fatalf("statement not read: statements=%d skipped=%d", st.Statements, st.Skipped)
	}
	if len(violations) != 0 {
		t.Errorf("a concatenation was measured as a literal: %+v", violations)
	}
}

func TestEveryWidthIsPositiveAndTablesAreLowercase(t *testing.T) {
	// A zero or negative width would make clip() treat the column as unlimited and make
	// this guard compare against nothing. A mixed-case table name would never match the
	// identifier read out of the script.
	for table, cols := range columnWidths {
		if table != strings.ToLower(table) {
			t.Errorf("table %q is not lowercase, so it can never match a statement", table)
		}
		if len(cols) == 0 {
			t.Errorf("table %q has an empty width map — drop the entry instead", table)
		}
		for col, w := range cols {
			if w <= 0 {
				t.Errorf("%s.%s has width %d", table, col, w)
			}
		}
	}
}
