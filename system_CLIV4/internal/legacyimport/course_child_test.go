package legacyimport

// Tests for the second table a course's contents have to be written to.
//
// V4 keeps a course's composition in TWO places and the query files say which is which:
// catalog_course_line holds "canonical course composition identities", catalog_item_child is
// "the flattened price/allocation projection". Writing only the first is what left 523
// imported courses reading as EMPTY on screen while 545 lines sat in the database — the
// course screen (courses.sql), the course-deduction lookup (course_bought.sql), the tax
// allocation (itemtax.sql) and invoicing (billing.sql) all join catalog_item_child.
//
// So these tests are not about a column: they pin that both tables get the same edge, with
// the same numbers, in an order the database will accept.

import (
	"regexp"
	"strings"
	"testing"
)

// courseSample is one course with two services, taken from the shape mongosh exports.
const courseSample = `db.getCollection("listheals").insert([
  { _id: ObjectId("6a00000000000000000000a1"), code: "LH004", name_th: "Basic Bright", price: NumberDecimal("1000") },
  { _id: ObjectId("6a00000000000000000000a2"), code: "LH008", name_th: "Collagen Bright", price: NumberDecimal("2000") }
]);
db.getCollection("course_items").insert([
  { _id: ObjectId("6b00000000000000000000b1"),
    name_th: "Bright Set",
    price: NumberDecimal("9000"),
    list_item: [
      { heal_id: ObjectId("6a00000000000000000000a1"), number_of_time: "1" },
      { heal_id: ObjectId("6a00000000000000000000a2"), number_of_time: "10" }
    ]
  }
]);`

func buildCoursesOne(t *testing.T, body string) (courseSQL, lineSQL string, rep *Report) {
	t.Helper()
	mc, err := parseMongoCatalog([]byte(body))
	if err != nil {
		t.Fatalf("parseMongoCatalog: %v", err)
	}
	rep = NewReport("C9C9611")
	courseSQL, lineSQL, err = buildCourses(mc, Input{ClinicCode: "C9C9611"}, rep)
	if err != nil {
		t.Fatalf("buildCourses: %v", err)
	}
	return courseSQL, lineSQL, rep
}

// One edge, two tables. A course line with no matching catalog_item_child row is a course
// that exists and shows nothing.
func TestCourseLineAlsoWritesCatalogItemChild(t *testing.T) {
	_, lineSQL, _ := buildCoursesOne(t, courseSample)

	lines := strings.Count(lineSQL, "INSERT INTO catalog_course_line")
	children := strings.Count(lineSQL, "INSERT INTO catalog_item_child")
	if lines != 2 {
		t.Fatalf("catalog_course_line inserts = %d, want 2 (the course has two services)\n%s", lines, lineSQL)
	}
	if children != lines {
		t.Errorf("catalog_item_child inserts = %d, catalog_course_line = %d — every line needs its "+
			"projection row or the course reads as empty on screen\n%s", children, lines, lineSQL)
	}
}

// relation_kind must be 'composition'. 'bom' is the other value the CHECK allows and means a
// service's materials; every course parent already in the database uses composition.
func TestCourseChildUsesCompositionRelationKind(t *testing.T) {
	_, lineSQL, _ := buildCoursesOne(t, courseSample)
	if !strings.Contains(lineSQL, "'composition'") {
		t.Errorf("catalog_item_child rows do not set relation_kind='composition'\n%s", lineSQL)
	}
	if strings.Contains(lineSQL, "'bom'") {
		t.Error("a course edge must not be written as 'bom' — that is a service's material list")
	}
}

// The two tables must not disagree about how many sessions a course contains. qty 10 is the
// number the customer redeems against, so a mismatch is an entitlement bug.
func TestCourseChildCarriesTheSameQty(t *testing.T) {
	_, lineSQL, _ := buildCoursesOne(t, courseSample)

	// Read the qty by ANCHOR, not by comma position: both statements embed a
	// `(SELECT id FROM catalog_item … status IN (1,2))` subquery whose own commas make
	// field splitting meaningless.
	lineQty := reQty(lineQtyRe, lineSQL)
	childQty := reQty(childQtyRe, lineSQL)
	if len(lineQty) != len(childQty) {
		t.Fatalf("statement counts differ: %d lines vs %d children", len(lineQty), len(childQty))
	}
	for i := range lineQty {
		if lineQty[i] != childQty[i] {
			t.Errorf("edge %d: catalog_course_line qty %q but catalog_item_child qty %q",
				i, lineQty[i], childQty[i])
		}
	}
	// And the source's own numbers survived: 1 and 10, not defaults.
	joined := strings.Join(lineQty, ",")
	if !strings.Contains(joined, "1") || !strings.Contains(joined, "10") {
		t.Errorf("the source quantities (1 and 10) are not in the output: %v", lineQty)
	}
}

// The quantity sits between two fixed literals in each statement, which is the only part of
// the SELECT list that cannot be confused with the commas inside a subquery.
//
//	catalog_course_line:  … 'service', NULL, <qty>, <sort>, now(), now()
//	catalog_item_child:   … <qty>, <sort>, 'composition', now()
var (
	lineQtyRe  = regexp.MustCompile(`'service', NULL, (\d+), (\d+), now\(\), now\(\)`)
	childQtyRe = regexp.MustCompile(`, (\d+), (\d+), 'composition', now\(\)`)
)

// reQty returns the qty captured by re, in statement order.
func reQty(re *regexp.Regexp, sql string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(sql, -1) {
		out = append(out, m[1])
	}
	return out
}

// The child insert must come AFTER the line insert for the same edge. Both resolve
// catalog_item by code in a subquery so neither depends on the other, but the file is read
// top-down by a reviewer and the canonical identity belongs first.
func TestCourseChildFollowsItsLine(t *testing.T) {
	_, lineSQL, _ := buildCoursesOne(t, courseSample)
	firstLine := strings.Index(lineSQL, "INSERT INTO catalog_course_line")
	firstChild := strings.Index(lineSQL, "INSERT INTO catalog_item_child")
	if firstLine < 0 || firstChild < 0 {
		t.Fatalf("expected both inserts\n%s", lineSQL)
	}
	if firstChild < firstLine {
		t.Error("catalog_item_child is written before catalog_course_line — the canonical row belongs first")
	}
}

// Re-running the script must not double the contents of a course.
func TestCourseChildIsIdempotent(t *testing.T) {
	_, lineSQL, _ := buildCoursesOne(t, courseSample)
	if !strings.Contains(lineSQL, "FROM catalog_item_child ch") {
		t.Errorf("the catalog_item_child insert has no NOT EXISTS guard\n%s", lineSQL)
	}
	guards := strings.Count(lineSQL, "SELECT 1 FROM catalog_item_child ch")
	inserts := strings.Count(lineSQL, "INSERT INTO catalog_item_child")
	if guards != inserts {
		t.Errorf("%d catalog_item_child inserts but %d guards", inserts, guards)
	}
}

// A line dropped for qty 0 must not leave a projection row behind — that would be a course
// containing something the clinic never sold.
func TestCourseZeroQtyWritesNeitherTable(t *testing.T) {
	body := strings.Replace(courseSample, `number_of_time: "10"`, `number_of_time: "0"`, 1)
	_, lineSQL, _ := buildCoursesOne(t, body)
	if got := strings.Count(lineSQL, "INSERT INTO catalog_course_line"); got != 1 {
		t.Fatalf("catalog_course_line inserts = %d, want 1 (the 0-qty line is dropped)", got)
	}
	if got := strings.Count(lineSQL, "INSERT INTO catalog_item_child"); got != 1 {
		t.Errorf("catalog_item_child inserts = %d, want 1 — the dropped line must not leave a "+
			"projection row for a session nobody sold", got)
	}
}

// The count is reported, because a course whose contents are invisible looks like data loss
// and this number is what shows the projection was written.
func TestCourseChildCountIsReported(t *testing.T) {
	_, _, rep := buildCoursesOne(t, courseSample)
	var found string
	for _, n := range rep.Notes {
		if strings.Contains(n, "catalog_item_child") {
			found = n
		}
	}
	if found == "" {
		t.Fatalf("no note about catalog_item_child; notes = %v", rep.Notes)
	}
	if !strings.Contains(found, "2 แถว") {
		t.Errorf("the note should count the 2 edges written: %q", found)
	}
}
