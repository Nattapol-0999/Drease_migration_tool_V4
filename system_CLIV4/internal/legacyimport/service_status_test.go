package legacyimport

// A service V2 switched off (listheals.active = false) arrives as a draft — owner's decision
// ก, 24 Oct 2026. Before, only the price was consulted, so a switched-off service arrived
// sellable and went onto the storefront.

import (
	"strings"
	"testing"
)

const serviceStatusSample = `db.getCollection("listheals").insert([
  { _id: ObjectId("6a00000000000000000000c1"), code: "LH001", name_th: "On",        cost: NumberDecimal("1000"), active: true },
  { _id: ObjectId("6a00000000000000000000c2"), code: "LH002", name_th: "Off",       cost: NumberDecimal("1000"), active: false },
  { _id: ObjectId("6a00000000000000000000c3"), code: "LH003", name_th: "Unsaid",    cost: NumberDecimal("1000") },
  { _id: ObjectId("6a00000000000000000000c4"), code: "LH004", name_th: "Off free",  cost: NumberDecimal("0"),    active: false }
]);`

// serviceStatusOf reads the status a service's catalog_item statement writes.
func serviceStatusOf(t *testing.T, sql, code string) string {
	t.Helper()
	at := strings.Index(sql, "'"+code+"', 'service'")
	if at < 0 {
		t.Fatalf("no catalog_item statement for %s\n%s", code, sql)
	}
	rest := sql[at:]
	marker := "'catalog', NULL, false, "
	m := strings.Index(rest, marker)
	if m < 0 {
		t.Fatalf("cannot find the status of %s\n%s", code, rest)
	}
	return rest[m+len(marker) : m+len(marker)+1]
}

func TestSwitchedOffServiceArrivesAsDraft(t *testing.T) {
	mc, err := parseMongoCatalog([]byte(serviceStatusSample))
	if err != nil {
		t.Fatalf("parseMongoCatalog: %v", err)
	}
	rep := NewReport("C9C9611")
	sql, err := buildCatalogServices(mc, Input{ClinicCode: "C9C9611"}, rep)
	if err != nil {
		t.Fatalf("buildCatalogServices: %v", err)
	}
	for code, want := range map[string]string{
		"LH001": "1", // switched on, priced
		"LH002": "2", // switched off: draft
		"LH003": "1", // the export never said — absent is not "off"
		"LH004": "2", // no price: draft as before
	} {
		if got := serviceStatusOf(t, sql, code); got != want {
			t.Errorf("%s status = %s, want %s", code, got, want)
		}
	}
	var note string
	for _, n := range rep.Notes {
		if strings.Contains(n, "active=false") {
			note = n
		}
	}
	if !strings.Contains(note, "1 รายการ") || !strings.Contains(note, "LH002") {
		t.Errorf("the priced switched-off service is not named once in the report: %q", note)
	}
	if strings.Contains(note, "LH004") {
		t.Errorf("a service drafted for having no price is reported as switched off: %q", note)
	}
}
