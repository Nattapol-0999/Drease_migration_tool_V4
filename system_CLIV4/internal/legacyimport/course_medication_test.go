package legacyimport

// Tests for the drugs a course contains (list_medicien → catalog_course_line 'medication').
//
// These lines used to be counted and thrown away. The reason given was that V4 requires a
// medication line to name the service it is used under (parent_service_catalog_item_id NOT
// NULL) and the export supposedly did not say which service that was. It does: every
// list_medicien entry carries heal_id, the treatment the drug is consumed under, right beside
// medicien_id, the drug itself.
//
// What the app does with them, and why the table matters: ListCourseMedications
// (query/course_lines.sql) reads catalog_course_line WHERE line_kind IN ('supply','medication')
// and joins catalog_item on item_type IN ('drug','goods') and status IN (1,2). The importer
// writes every `items` document as item_type='drug' with status 1 or 2 (catalogStatus), so a
// row written here is a row that screen can see.

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

// A drug used under a service is NOT a direct child of the course. catalog_item_child is the
// flattened projection whose parent→child edge cannot express "consumed under service X", and
// the screen that lists a course's drugs (ListCourseMedications) reads catalog_course_line
// instead. Pinned so that adding a child row later is a decision, not an accident.
func TestCourseMedicationWritesNoChildEdge(t *testing.T) {
	_, lineSQL, _ := buildCoursesOne(t, courseMedSample)
	if got := strings.Count(lineSQL, "INSERT INTO catalog_item_child"); got != 1 {
		t.Errorf("catalog_item_child inserts = %d, want 1 (the service only, not the drug)\n%s",
			got, lineSQL)
	}
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
