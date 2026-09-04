package legacydump

import "testing"

// A dump shaped like the real ones: an extended insert, a second statement for the
// same table, a table that is declared but never populated, Thai text containing the
// "), (" sequence that a naive tuple count would trip over, and a lower-case keyword.
const tablesSample = "" +
	"CREATE TABLE `patient` (\n  `HN` varchar(20) NOT NULL,\n  `Firstname` varchar(255)\n) ENGINE=InnoDB;\n" +
	"CREATE TABLE IF NOT EXISTS `empty_table` (`id` int);\n" +
	"INSERT INTO `patient` (`HN`, `Firstname`, `note`) VALUES\n" +
	"('016900001', 'สมชาย', 'ปวดเข่า), (ซ้าย'),\n" +
	"('016900002', 'สมหญิง', NULL);\n" +
	"insert into `patient` (`HN`, `Firstname`, `note`) values ('016900003', '', 'ok');\n" +
	"INSERT INTO `appointment` (`id`, `hn`) VALUES (1, '016900001');\n"

func TestTablesFindsNamesColumnsAndRowCounts(t *testing.T) {
	got := Tables(tablesSample)

	byName := map[string]TableInfo{}
	for _, ti := range got {
		byName[ti.Name] = ti
	}

	p, ok := byName["patient"]
	if !ok {
		t.Fatalf("patient not found: %#v", byName)
	}
	// Three rows across two statements — and the ), ( inside the Thai note must not
	// have been counted as a tuple boundary.
	if p.Rows != 3 {
		t.Errorf("patient rows = %d, want 3 (a '), (' inside a string literal is data)", p.Rows)
	}
	if p.Statements != 2 {
		t.Errorf("patient statements = %d, want 2", p.Statements)
	}
	if len(p.Columns) != 3 || p.Columns[0] != "HN" || p.Columns[2] != "note" {
		t.Errorf("patient columns = %v, want [HN Firstname note]", p.Columns)
	}
	if !p.Declared {
		t.Errorf("patient has a CREATE TABLE and must be marked declared")
	}

	a, ok := byName["appointment"]
	if !ok || a.Rows != 1 {
		t.Errorf("appointment = %#v, want 1 row", a)
	}
	if a.Declared {
		t.Errorf("appointment has no CREATE TABLE in this dump and must not be marked declared")
	}

	// A declared-but-empty table is a different fact from a table the dump never
	// mentions; the listing has to show it so an operator does not go looking.
	e, ok := byName["empty_table"]
	if !ok {
		t.Fatalf("empty_table missing — a declared table with no rows must still be listed")
	}
	if e.Rows != 0 || e.Statements != 0 || !e.Declared {
		t.Errorf("empty_table = %#v, want 0 rows / 0 statements / declared", e)
	}
}

func TestTablesIsDeterministic(t *testing.T) {
	first := Tables(tablesSample)
	for i := 0; i < 5; i++ {
		again := Tables(tablesSample)
		if len(again) != len(first) {
			t.Fatalf("listing length changed between runs")
		}
		for n := range first {
			if again[n].Name != first[n].Name {
				t.Fatalf("listing order changed between runs: %v vs %v", first[n].Name, again[n].Name)
			}
		}
	}
}

func TestTablesOnFileWithNoInserts(t *testing.T) {
	got := Tables("SET NAMES utf8mb4;\n-- nothing here\n")
	if len(got) != 0 {
		t.Fatalf("want no tables, got %#v", got)
	}
}

// Tables and Parse must agree: whatever the listing says is in the file is what the
// parser can read out of it. Disagreement here is how an operator ends up mapping a
// table that then imports a different number of rows.
func TestTablesAgreesWithParse(t *testing.T) {
	for _, ti := range Tables(tablesSample) {
		if ti.Rows == 0 {
			continue
		}
		cols, rows, err := Parse(tablesSample, ti.Name)
		if err != nil {
			t.Fatalf("Parse(%s): %v", ti.Name, err)
		}
		if len(rows) != ti.Rows {
			t.Errorf("%s: Tables says %d rows, Parse returns %d", ti.Name, ti.Rows, len(rows))
		}
		if len(cols) != len(ti.Columns) {
			t.Errorf("%s: Tables says %d columns, Parse returns %d", ti.Name, len(ti.Columns), len(cols))
		}
	}
}
