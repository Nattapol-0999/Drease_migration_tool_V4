package legacyimport

// Two services under one listheals code (owner's decision ค, 7 Oct 2026).
//
//	a code ending in a number   the later row moves to the next free number (LC033 → LC093)
//	a code with no number       the FIRST row keeps the code, the rest are left out and named
//
// The second rule replaced a refusal of the whole import. The shapes are the real ones from
// the clinic in the 7 Oct screenshot: "Set ทำแผล" three times at 200 / 250 / 300 and
// "Consultation" twice at 500 / 0, beside "OR1" three times, which still renumbers.

import (
	"strings"
	"testing"
)

const noTailDuplicates = `db.getCollection("listheals").insert([
  { _id: ObjectId("65f42f66042b330af9632c38"), code: "Set ทำแผล", name_th: "เวชภัณฑ์ทำแผล", cost: NumberDecimal("200") },
  { _id: ObjectId("65f430bf042b330af9632cc6"), code: "Set ทำแผล", name_th: "เวชภัณฑ์ทำแผล", cost: NumberDecimal("250") },
  { _id: ObjectId("65f430dd042b330af9632cd3"), code: "Set ทำแผล", name_th: "เวชภัณฑ์ทำแผล", cost: NumberDecimal("300") },
  { _id: ObjectId("663f4148042b330af966f820"), code: "OR1", name_th: "ผ่าตัด", cost: NumberDecimal("16000") },
  { _id: ObjectId("663f41d1042b330af966f847"), code: "OR1", name_th: "ค่าบริการพยาบาล", cost: NumberDecimal("3500") },
  { _id: ObjectId("65f41a03042b330af963290b"), code: "Consultation", name_th: "Doctor fee", cost: NumberDecimal("500") },
  { _id: ObjectId("66714d77042b330af9699623"), code: "Consultation", name_th: "Doctor fee", cost: NumberDecimal("0") }
]);
`

// catalogItemInserts splits a service batch into its catalog_item statements, so each can
// be read on its own: the catalog_service statement after it names the same code twice.
func catalogItemInserts(sql string) []string {
	return strings.Split(sql, "INSERT INTO catalog_item\n")[1:]
}

func insertsNaming(sql, code string) []string {
	var out []string
	for _, st := range catalogItemInserts(sql) {
		if strings.Contains(st, "'"+code+"'") {
			out = append(out, st)
		}
	}
	return out
}

func TestAServiceCodeWithNoNumberKeepsTheFirstAndNamesTheRest(t *testing.T) {
	mc, err := parseMongoCatalog([]byte(noTailDuplicates))
	if err != nil {
		t.Fatalf("parseMongoCatalog: %v", err)
	}
	rep := NewReport("C9C9611")
	sql, err := buildCatalogServices(mc, Input{ClinicCode: "C9C9611"}, rep)
	if err != nil {
		t.Fatalf("a duplicated code with no number refused the import again: %v", err)
	}

	// The first row of each code, at its own price — list_price, sell_price, cost.
	for code, price := range map[string]string{"Set ทำแผล": "200, 200, 0", "Consultation": "500, 500, 0"} {
		got := insertsNaming(sql, code)
		if len(got) != 1 {
			t.Fatalf("%s is written %d times, want once", code, len(got))
		}
		if !strings.Contains(got[0], price) {
			t.Errorf("%s did not keep its FIRST row (want %s):\n%s", code, price, got[0])
		}
	}
	// No code is invented for the rows left out.
	for _, invented := range []string{"Set ทำแผล2", "Set ทำแผล-2", "Consultation2"} {
		if len(insertsNaming(sql, invented)) != 0 {
			t.Errorf("a code %q was invented — decision ค leaves the row out instead", invented)
		}
	}
	// A code ending in a number still moves along, as before.
	if len(insertsNaming(sql, "OR1")) != 1 || len(insertsNaming(sql, "OR2")) != 1 {
		t.Errorf("OR1's duplicate no longer moves to OR2:\n%s", sql)
	}
	if got := len(catalogItemInserts(sql)); got != 4 {
		t.Errorf("wrote %d services, want 4 (Set ทำแผล, OR1, OR2, Consultation)", got)
	}

	// Every row left out is named with its price beside the one that kept the code.
	for _, want := range []string{
		"3 รายการใช้รหัสซ้ำ",
		"Set ทำแผล: ไม่นำเข้า เวชภัณฑ์ทำแผล ราคา 250 · เก็บ เวชภัณฑ์ทำแผล ราคา 200",
		"Set ทำแผล: ไม่นำเข้า เวชภัณฑ์ทำแผล ราคา 300 · เก็บ เวชภัณฑ์ทำแผล ราคา 200",
		"Consultation: ไม่นำเข้า Doctor fee ราคา 0 · เก็บ Doctor fee ราคา 500",
		"7 ต.ค. 2569",
	} {
		if !hasWarning(rep, want) {
			t.Errorf("the report does not say %q · warnings = %v", want, rep.Warnings)
		}
	}

	// And accounted for on the entity: 7 rows = 4 written + 3 left out.
	e := rep.Entities[EntCatalogService]
	left := 0
	for reason, n := range e.Skipped {
		if strings.Contains(reason, "ไม่มีเลขท้าย") {
			left += n
		}
	}
	if e.SourceRows != 7 || left != 3 {
		t.Errorf("source %d · left out %d, want 7 · 3 · skipped = %v", e.SourceRows, left, e.Skipped)
	}
}

// A course that names a left-out row lands on the service that kept the code, instead of
// losing the line as an unresolved heal_id. Real: the clinic's courses point at the 200 row
// only, but nothing stops a course from naming the 250 one.
func TestACourseLineNamingALeftOutServiceLandsOnTheKeptOne(t *testing.T) {
	body := noTailDuplicates + `db.getCollection("course_items").insert([
  { _id: ObjectId("6b00000000000000000000d1"),
    name_th: "ทำแผล 5 ครั้ง",
    price: NumberDecimal("1000"),
    list_item: [ { heal_id: ObjectId("65f430bf042b330af9632cc6"), number_of_time: "5" } ]
  }
]);`
	mc, err := parseMongoCatalog([]byte(body))
	if err != nil {
		t.Fatalf("parseMongoCatalog: %v", err)
	}
	rep := NewReport("C9C9611")
	in := Input{ClinicCode: "C9C9611"}
	// The order of a real run: services first, then courses resolve through healCode.
	if _, err := buildCatalogServices(mc, in, rep); err != nil {
		t.Fatalf("buildCatalogServices: %v", err)
	}
	_, lineSQL, err := buildCourses(mc, in, rep)
	if err != nil {
		t.Fatalf("buildCourses: %v", err)
	}
	if got := strings.Count(lineSQL, "INSERT INTO catalog_course_line"); got != 1 {
		t.Fatalf("course lines = %d, want 1 — the line naming the left-out row was dropped\n%s", got, lineSQL)
	}
	if !strings.Contains(lineSQL, "code = 'Set ทำแผล'") {
		t.Errorf("the course line does not resolve to the service that kept the code:\n%s", lineSQL)
	}
}
