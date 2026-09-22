package legacyimport

// Tests for the drugs a course contains (list_medicien → catalog_course_line 'medication').
//
// These lines used to be counted and thrown away. The reason given was that V4 requires a
// medication line to name the service it is used under (parent_service_catalog_item_id NOT
// NULL) and the export supposedly did not say which service that was. It does: every
// list_medicien entry carries heal_id, the treatment the drug is consumed under, right beside
// medicien_id, the drug itself.
//
// What the app does with them, and why BOTH tables matter:
//
//   - the EDITOR reads catalog_course_line. ListCourseMedications (query/course_lines.sql)
//     selects WHERE line_kind IN ('supply','medication') and joins catalog_item on
//     item_type IN ('drug','goods') and status IN (1,2). The importer writes every `items`
//     document as item_type='drug' with status 1 or 2 (catalogStatus), so a row written
//     there is a row that screen can see — and it is the only table that records WHICH
//     service the drug is consumed under.
//   - the SALE reads catalog_item_child. ListCourseCatalogCompositionForPurchase
//     (query/course_bought.sql) joins it ON relation_kind='composition' and never touches
//     catalog_course_line; billing/service.go then maps a 'drug'/'goods' child to
//     kind='medication' when it writes course_bought_items.
//
// So a drug line needs a row in each, for two different reasons.

import (
	"strings"
	"testing"
)

// courseMedSample is one course with one service and one drug used under that service.
const courseMedSample = `db.getCollection("listheals").insert([
  { _id: ObjectId("6a00000000000000000000a1"), code: "LH004", name_th: "Basic Bright", price: NumberDecimal("1000") }
]);
db.getCollection("items").insert([
  { _id: ObjectId("6c00000000000000000000c1"), code: "IT001", name_th: "Vitamin C 500", price: NumberDecimal("120") }
]);
db.getCollection("course_items").insert([
  { _id: ObjectId("6b00000000000000000000b1"),
    name_th: "Bright Set",
    price: NumberDecimal("9000"),
    list_item: [
      { heal_id: ObjectId("6a00000000000000000000a1"), number_of_time: "1" }
    ],
    list_medicien: [
      { heal_id: ObjectId("6a00000000000000000000a1"),
        medicien_id: ObjectId("6c00000000000000000000c1"),
        qty_use: "2" }
    ]
  }
]);`

// The drug becomes a line of its own, kind 'medication', and it is NOT dropped.
func TestCourseMedicationLineIsWritten(t *testing.T) {
	_, lineSQL, _ := buildCoursesOne(t, courseMedSample)

	// Counted by STATEMENT, not by occurrences of the word: 'medication' appears twice in
	// each statement — once in the inserted row and once in the re-run guard.
	if got := medicationStatements(lineSQL); got != 1 {
		t.Fatalf("medication statements = %d, want 1\n%s", got, lineSQL)
	}
	if got := strings.Count(lineSQL, "INSERT INTO catalog_course_line"); got != 2 {
		t.Errorf("catalog_course_line inserts = %d, want 2 (one service + one drug)\n%s", got, lineSQL)
	}
}

// parent_service is the whole reason these lines were droppable: without it the column V4
// declares NOT NULL has nothing to hold. It has to resolve the service by CODE, the same way
// a service line does, and the statement must refuse to write when it resolves to nothing.
func TestCourseMedicationNamesItsService(t *testing.T) {
	_, lineSQL, _ := buildCoursesOne(t, courseMedSample)

	med := medicationStatement(t, lineSQL)
	if !strings.Contains(med, "parent_service_catalog_item_id") {
		t.Fatalf("the medication statement does not set parent_service_catalog_item_id\n%s", med)
	}
	// LH004 is the service the drug is used under; IT001 is the drug. Both must appear as
	// code lookups in the same statement.
	for _, code := range []string{"'LH004'", "'IT001'"} {
		if !strings.Contains(med, code) {
			t.Errorf("the medication statement does not resolve %s\n%s", code, med)
		}
	}
	// Three ids must all resolve or nothing is written: course, drug, service.
	if got := strings.Count(med, "IS NOT NULL"); got < 3 {
		t.Errorf("guarded ids = %d, want at least 3 (course, drug, service) — a NULL "+
			"parent_service would be refused by the column, aborting the whole import\n%s", got, med)
	}
}

// qty comes from qty_use. A course that says 2 doses and imports as 1 shorts the patient.
func TestCourseMedicationKeepsQtyUse(t *testing.T) {
	_, lineSQL, _ := buildCoursesOne(t, courseMedSample)
	med := medicationStatement(t, lineSQL)
	if !strings.Contains(med, "'medication', ") {
		t.Fatalf("unexpected medication statement shape\n%s", med)
	}
	// … 'medication', <service id subquery>, 2, 1000, now(), now()
	if !strings.Contains(med, ", 2, 1000, now(), now()") {
		t.Errorf("qty 2 from qty_use is not in the statement\n%s", med)
	}
}

// Re-running the import must not double a course's drugs.
func TestCourseMedicationIsIdempotent(t *testing.T) {
	_, lineSQL, _ := buildCoursesOne(t, courseMedSample)
	med := medicationStatement(t, lineSQL)
	if !strings.Contains(med, "NOT EXISTS") || !strings.Contains(med, "l.line_kind = 'medication'") {
		t.Errorf("the medication insert has no re-run guard keyed on line_kind\n%s", med)
	}
}

// A drug in a course gets a catalog_item_child edge too — the decision this file used to
// pin the other way (owner, 22 Sep 2026).
//
// The old test asserted exactly one child row, "the service only, not the drug", and its
// reasoning was about the course EDITOR: the edge is flat, it cannot say "consumed under
// service X", and ListCourseMedications reads catalog_course_line. All still true.
//
// What it missed is the SALE. billing/service.go calls
// ListCourseCatalogCompositionForPurchase, which joins catalog_item_child ON
// relation_kind='composition' and never looks at catalog_course_line, then maps a child of
// item_type 'drug'/'goods' to kind='medication' for course_bought_items. So with no child
// row the customer pays, gets the treatments, and gets none of the drugs — and a course
// whose only contents are drugs fails the entire payment on len(children)==0.
func TestCourseMedicationAlsoWritesChildEdge(t *testing.T) {
	_, lineSQL, _ := buildCoursesOne(t, courseMedSample)
	if got := strings.Count(lineSQL, "INSERT INTO catalog_item_child"); got != 2 {
		t.Fatalf("catalog_item_child inserts = %d, want 2 (the service AND the drug)\n%s",
			got, lineSQL)
	}

	// The drug's own edge, found by the drug's code rather than by position.
	edge := childEdgeFor(t, lineSQL, "'IT001'")
	if !strings.Contains(edge, "'composition'") {
		t.Errorf("the drug edge is not relation_kind='composition' — the sale-time query "+
			"filters on it, so any other value is invisible to billing\n%s", edge)
	}
	// qty_use 2 must survive into the projection as well, or the two tables disagree about
	// how much of the drug the course contains.
	if !strings.Contains(edge, ", 2, 1000, 'composition', now()") {
		t.Errorf("the drug edge does not carry qty 2 from qty_use\n%s", edge)
	}
	// catalog_item_child_self_chk refuses parent = child, and a 23514 here aborts the whole
	// import — so the statement has to rule it out itself.
	if !strings.Contains(edge, "<>") {
		t.Errorf("the drug edge has no parent<>child guard; catalog_item_child_self_chk "+
			"would abort the entire import on a code collision\n%s", edge)
	}
	// Re-running must not double a course's drugs.
	if !strings.Contains(edge, "NOT EXISTS") {
		t.Errorf("the drug edge has no re-run guard\n%s", edge)
	}
}

// The count is reported: a drug that reaches catalog_course_line but not catalog_item_child
// is paid for and never granted, which is invisible on every screen.
func TestCourseMedicationChildEdgeIsReported(t *testing.T) {
	_, _, rep := buildCoursesOne(t, courseMedSample)
	var found string
	for _, n := range rep.Notes {
		if strings.Contains(n, "catalog_item_child") && strings.Contains(n, "ยาในคอร์ส") {
			found = n
		}
	}
	if found == "" {
		t.Fatalf("no note about the drug's catalog_item_child edge; notes = %v", rep.Notes)
	}
	if !strings.Contains(found, "1 แถว") {
		t.Errorf("the note should count the 1 edge written: %q", found)
	}
}

// childEdgeFor returns the catalog_item_child statement whose child lookup names code.
// Matching inside one statement matters: the file holds two and a whole-file substring
// search would pass on the service's.
func childEdgeFor(t *testing.T, lineSQL, code string) string {
	t.Helper()
	for _, stmt := range strings.Split(lineSQL, ";") {
		if !strings.Contains(stmt, "INSERT INTO catalog_item_child") {
			continue
		}
		if strings.Contains(stmt, code) {
			return stmt
		}
	}
	t.Fatalf("no catalog_item_child statement mentions %s in:\n%s", code, lineSQL)
	return ""
}

// Every reason a drug line is skipped has to be counted, or a course quietly loses contents.
func TestCourseMedicationSkipsAreCounted(t *testing.T) {
	cases := []struct {
		name    string
		replace [2]string
		want    string
	}{
		{
			name: "heal_id ที่ไม่มีใน listheals",
			replace: [2]string{`{ heal_id: ObjectId("6a00000000000000000000a1"),
        medicien_id`, `{ heal_id: ObjectId("6a00000000000000000000ff"),
        medicien_id`},
			want: "heal_id ไม่ตรงบริการ: 1",
		},
		{
			name:    "medicien_id ที่ไม่มีใน items",
			replace: [2]string{`medicien_id: ObjectId("6c00000000000000000000c1")`, `medicien_id: ObjectId("6c00000000000000000000ff")`},
			want:    "medicien_id ไม่ตรง items: 1",
		},
		{
			name:    "qty_use เป็น 0",
			replace: [2]string{`qty_use: "2"`, `qty_use: "0"`},
			want:    "qty<=0: 1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(courseMedSample, tc.replace[0], tc.replace[1], 1)
			if body == courseMedSample {
				t.Fatalf("the test sample did not change — %q was not found", tc.replace[0])
			}
			_, lineSQL, rep := buildCoursesOne(t, body)
			if got := medicationStatements(lineSQL); got != 0 {
				t.Errorf("medication statements = %d, want 0 for this case\n%s", got, lineSQL)
			}
			note := medicationNote(t, rep)
			if !strings.Contains(note, tc.want) {
				t.Errorf("the note does not report %q: %s", tc.want, note)
			}
		})
	}
}

// The happy path is reported too, with the number that proves the lines were written.
func TestCourseMedicationCountIsReported(t *testing.T) {
	_, _, rep := buildCoursesOne(t, courseMedSample)
	note := medicationNote(t, rep)
	if !strings.Contains(note, "1 บรรทัด") {
		t.Errorf("the note should count the 1 drug line written: %s", note)
	}
	if !strings.Contains(note, "heal_id") {
		t.Errorf("the note should say where parent_service came from: %s", note)
	}
}

// medicationStatements counts the statements that write a medication line.
func medicationStatements(lineSQL string) int {
	n := 0
	for _, stmt := range strings.Split(lineSQL, ";") {
		if strings.Contains(stmt, "INSERT INTO catalog_course_line") &&
			strings.Contains(stmt, "'medication'") {
			n++
		}
	}
	return n
}

// medicationStatement returns the single statement that writes the medication line.
func medicationStatement(t *testing.T, lineSQL string) string {
	t.Helper()
	for _, stmt := range strings.Split(lineSQL, ";") {
		if strings.Contains(stmt, "'medication'") {
			return stmt
		}
	}
	t.Fatalf("no medication statement in:\n%s", lineSQL)
	return ""
}

// medicationNote returns the report line about medication lines.
func medicationNote(t *testing.T, rep *Report) string {
	t.Helper()
	for _, n := range rep.Notes {
		if strings.Contains(n, "line_kind='medication'") {
			return n
		}
	}
	t.Fatalf("no note about medication lines; notes = %v", rep.Notes)
	return ""
}
