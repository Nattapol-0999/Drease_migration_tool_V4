package legacyimport

// Tests for the course register: a course is written into `courses` too, and its head row
// points back at it, because V4's course settings screen reads that register (ListCourses)
// and its editor only opens a head with source_table='courses'. Without it an imported
// course sold fine and could never be edited (3,353 such heads on production, 24 Oct 2026).

import (
	"regexp"
	"strings"
	"testing"
)

const courseRegisterSID = "(SELECT id FROM courses WHERE clinic_code = 'C9C9611' AND code = 'CS001' AND status = 1 ORDER BY id LIMIT 1)"

// headStatement returns the catalog_item statement of the course, so assertions about the
// head cannot be satisfied by some other statement in the batch.
func headStatement(t *testing.T, courseSQL string) string {
	t.Helper()
	at := strings.Index(courseSQL, "INSERT INTO catalog_item\n")
	if at < 0 {
		t.Fatalf("no catalog_item statement in the course batch\n%s", courseSQL)
	}
	end := strings.Index(courseSQL[at:], "updated_at = now();")
	if end < 0 {
		t.Fatalf("catalog_item statement has no end\n%s", courseSQL[at:])
	}
	return courseSQL[at : at+end]
}

// The register row comes first: the head resolves its source_id from it.
func TestCourseWritesItsRegisterRowBeforeTheHead(t *testing.T) {
	courseSQL, _, _ := buildCoursesOne(t, courseSample)
	reg := strings.Index(courseSQL, "INSERT INTO courses")
	head := strings.Index(courseSQL, "INSERT INTO catalog_item\n")
	if reg < 0 {
		t.Fatalf("the course batch writes no `courses` row — the settings screen will not list it\n%s", courseSQL)
	}
	if head < 0 || reg > head {
		t.Errorf("courses is written after the head (courses at %d, head at %d) — the head cannot find it", reg, head)
	}
}

// The head carries the register as its source, the way coursesetting's Insert writes it.
func TestCourseHeadPointsAtItsRegisterRow(t *testing.T) {
	courseSQL, _, _ := buildCoursesOne(t, courseSample)
	head := headStatement(t, courseSQL)
	if !strings.Contains(head, "'courses', "+courseRegisterSID) {
		t.Errorf("head is not source_table='courses' with the register's id\n%s", head)
	}
	if strings.Contains(head, "'catalog', NULL") {
		t.Errorf("head still written as source_table='catalog' with no source_id\n%s", head)
	}
}

// A code the catalogue already holds is never given a register row: a re-run already has
// one, a head an older version wrote as 'catalog' is left as it was (5ก), and a row of
// another type must not be tangled with a register it does not belong to.
func TestCourseRegisterIsOnlyWrittenWhileTheCodeIsFree(t *testing.T) {
	courseSQL, _, _ := buildCoursesOne(t, courseSample)
	at := strings.Index(courseSQL, "INSERT INTO courses")
	end := strings.Index(courseSQL[at:], ";")
	reg := courseSQL[at : at+end]
	for _, want := range []string{
		"NOT EXISTS (SELECT 1 FROM courses\n                    WHERE clinic_code = 'C9C9611' AND code = 'CS001' AND status = 1)",
		"NOT EXISTS (SELECT 1 FROM catalog_item\n                    WHERE clinic_code = 'C9C9611' AND code = 'CS001' AND status IN (1, 2) AND NOT code_conflict)",
	} {
		if !strings.Contains(reg, want) {
			t.Errorf("register insert is missing the guard %q\n%s", want, reg)
		}
	}
	// status 1 is the register's "live" flag; whether it sells is the head's business.
	if !strings.Contains(reg, "0, false, 1,") {
		t.Errorf("register row is not written live (status 1)\n%s", reg)
	}
}

// The editor refuses a course whose head and register disagree on name or price, and the
// head is refreshed from V2 on every run — so the register row is refreshed with it.
func TestCourseRegisterMirrorsTheHead(t *testing.T) {
	courseSQL, _, _ := buildCoursesOne(t, courseSample)
	if !regexp.MustCompile(`(?s)UPDATE courses c SET\s+name_th\s+= 'Bright Set',\s+price\s+= 9000,`).MatchString(courseSQL) {
		t.Errorf("no UPDATE keeping the register's name and price equal to the head's\n%s", courseSQL)
	}
	if !strings.Contains(courseSQL, "ci.source_table = 'courses' AND ci.source_id = c.id") {
		t.Errorf("the mirror is not limited to a register row a head points at\n%s", courseSQL)
	}
	head := headStatement(t, courseSQL)
	if !strings.Contains(head, "'Bright Set'") || !strings.Contains(head, ", 9000, 0, 'courses'") {
		t.Errorf("head does not carry the same name and price\n%s", head)
	}
}

// V4 derives a course's ราคาปกติ from its children; a stored list_price is the import's
// invention. Services and drugs keep theirs.
func TestCourseListPriceIsNull(t *testing.T) {
	courseSQL, _, _ := buildCoursesOne(t, courseSample)
	head := headStatement(t, courseSQL)
	if !strings.Contains(head, "NULL, 9000, 0, 'courses'") {
		t.Errorf("course list_price is not NULL (want list_price NULL, sell_price 9000)\n%s", head)
	}
	svc := upsertCatalogItem("C9C9611", "LH001", "service", "A", "", "", "1000", "0", 1, "catalog", "NULL", "")
	if !strings.Contains(svc, "1000, 1000, 0, 'catalog'") {
		t.Errorf("a service lost its list_price\n%s", svc)
	}
}

// The guard is in the same batch and scoped to the codes this run wrote.
func TestCourseRegisterGuardIsScopedToThisRun(t *testing.T) {
	courseSQL, _, _ := buildCoursesOne(t, courseSample)
	guard := strings.Index(courseSQL, "DO $courses$")
	if guard < 0 {
		t.Fatalf("no register guard in the course batch\n%s", courseSQL)
	}
	if guard < strings.LastIndex(courseSQL, "INSERT INTO catalog_course\n") {
		t.Error("the guard runs before the last course is written")
	}
	body := courseSQL[guard:]
	if !strings.Contains(body, "ARRAY['CS001']::text[]") {
		t.Errorf("guard is not scoped to this run's codes\n%s", body)
	}
	if n := strings.Count(body, "RAISE EXCEPTION"); n != 3 {
		t.Errorf("guard refuses %d cases, want 3 (head without register, register without head, drift)", n)
	}
	if !strings.Contains(body, "ci.legacy_source_key IS DISTINCT FROM c.source_key") {
		t.Error("guard does not compare the source key the editor compares")
	}
}

// V2's switch for a course is `status`, and an inactive course arrives as a draft.
func TestInactiveCourseArrivesAsDraft(t *testing.T) {
	body := strings.Replace(courseSample, `name_th: "Bright Set",`, `name_th: "Bright Set", status: "inactive",`, 1)
	courseSQL, _, rep := buildCoursesOne(t, body)
	head := headStatement(t, courseSQL)
	if !strings.Contains(head, courseRegisterSID+", false, 2,") {
		t.Errorf("inactive course is not status 2\n%s", head)
	}
	var said bool
	for _, n := range rep.Notes {
		if strings.Contains(n, "inactive") && strings.Contains(n, "CS001") {
			said = true
		}
	}
	if !said {
		t.Errorf("the drafted course is not named in the report; notes = %v", rep.Notes)
	}

	active := strings.Replace(courseSample, `name_th: "Bright Set",`, `name_th: "Bright Set", status: "active",`, 1)
	courseSQL, _, _ = buildCoursesOne(t, active)
	if !strings.Contains(headStatement(t, courseSQL), courseRegisterSID+", false, 1,") {
		t.Error("an active, priced course with lines is no longer sellable")
	}
}

// item_type is decided at the first write and then locked, as V4 locks it. A re-run must
// not flip a SKU between drug and goods, or turn a row of one type into another.
func TestUpsertNeverOverwritesItemType(t *testing.T) {
	sql := upsertCatalogItem("C9C9611", "ST000001", "drug", "A", "", "", "10", "5", 1, "items", "NULL", "")
	update := sql[strings.Index(sql, "DO UPDATE SET"):]
	if strings.Contains(update, "item_type") {
		t.Errorf("DO UPDATE still writes item_type\n%s", update)
	}
}

// source_id is filled only into a row that names the same table: an `items` id must not
// land on a row that says 'catalog'.
func TestUpsertFillsSourceIDOnlyForTheSameSourceTable(t *testing.T) {
	sql := upsertCatalogItem("C9C9611", "CS001", "course", "A", "", "", "10", "0", 1, "courses", "NULL", "")
	want := "CASE WHEN catalog_item.source_table = EXCLUDED.source_table\n" +
		"                               THEN COALESCE(catalog_item.source_id, EXCLUDED.source_id)\n" +
		"                               ELSE catalog_item.source_id END"
	if !strings.Contains(sql, want) {
		t.Errorf("source_id fill is not limited to the same source_table\n%s", sql)
	}
}
