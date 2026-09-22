package legacyimport

// The category master has to carry the categories of BOTH kinds of thing.
//
// WHAT WENT WRONG
//
// The export splits them: `category` holds the ones a SERVICE carries, `item_types` holds
// the ones a DRUG carries ("ยา" / "อุปกรณ์" / "เวชภัณฑ์"). buildCatalogCategory wrote only
// the first, while buildItems went on passing the item_types name to categoryIDByName.
// That resolver is a subselect, and a subselect that matches nothing returns NULL rather
// than failing — so every imported drug landed with category_id = NULL and nothing said so.
// The rows were there, the names were right, and the catalogue screen had nothing to group
// a single drug by.
//
// V4 has no such split: catalog.ResolveCategoryID (internal/controller/catalog/sku.go)
// takes whatever name the caller holds, matches it case/space-insensitively, and CREATES
// the row when it is missing — for a drug from the stock page exactly as for a service.

import (
	"strings"
	"testing"
)

// twoKindsOfCategory: one service category, three item_types, and one name that appears in
// both collections with different spacing/case.
const twoKindsOfCategory = `db.getCollection("category").insert([
  { _id: ObjectId("ee0000000000000000000001"), name: "ทรีตเมนต์หน้า" },
  { _id: ObjectId("ee0000000000000000000002"), name: "Wellness" }
]);
db.getCollection("item_types").insert([
  { _id: ObjectId("ff0000000000000000000001"), name: "ยา" },
  { _id: ObjectId("ff0000000000000000000002"), name: "อุปกรณ์" },
  { _id: ObjectId("ff0000000000000000000003"), name: " wellness " }
]);
db.getCollection("items").insert([
  { _id: ObjectId("ab0000000000000000000001"), code: "ST000001", name1: "Botox", type: ObjectId("ff0000000000000000000001"), quantity: "1", quantity_total: "1", cost: "100", price: "200", active: true, stock: ObjectId("5fab83eebb59bca34e513211") }
]);`

func buildCategoryOne(t *testing.T, body string) (string, *Report) {
	t.Helper()
	mc, err := parseMongoCatalog([]byte(body))
	if err != nil {
		t.Fatalf("parseMongoCatalog: %v", err)
	}
	rep := NewReport("CCAT01")
	return buildCatalogCategory(mc, Input{ClinicCode: "CCAT01"}, rep), rep
}

func TestCategoryMasterCarriesDrugCategoriesToo(t *testing.T) {
	sql, _ := buildCategoryOne(t, twoKindsOfCategory)

	// Both collections' names, in one master.
	for _, name := range []string{"ทรีตเมนต์หน้า", "ยา", "อุปกรณ์"} {
		if !strings.Contains(sql, "'"+name+"'") {
			t.Errorf("category %q is missing — a drug whose category has no row here gets "+
				"category_id = NULL, silently, because categoryIDByName is a subselect\n%s", name, sql)
		}
	}
}

// The drug's catalog_item row must actually resolve to a category now. This is the assertion
// that would have caught the original bug: the category SQL alone looked fine.
func TestDrugCatalogRowResolvesItsCategory(t *testing.T) {
	mc, err := parseMongoCatalog([]byte(twoKindsOfCategory))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	in := Input{ClinicCode: "CCAT01", BranchMap: map[string]string{"01": "BR00001"}}
	catSQL, _ := buildCategoryOne(t, twoKindsOfCategory)
	_, drugSQL, _, _, err := buildItems(mc, in, NewReport(in.ClinicCode))
	if err != nil {
		t.Fatalf("buildItems: %v", err)
	}

	// The drug asks for "ยา"...
	if !strings.Contains(drugSQL, "lower(btrim('ยา'))") {
		t.Fatalf("the drug's catalog_item row does not look its category up by name\n%s", drugSQL)
	}
	// ...and the category master is what has to answer. Same name, same clinic.
	if !strings.Contains(catSQL, "'ยา'") {
		t.Errorf("the category the drug asks for is never inserted, so the lookup returns NULL")
	}
}

// The target index is on lower(btrim(name)), so two spellings that differ only by case or
// spacing are ONE category there. Emitting both would burn a sort_order slot and make the
// winner depend on map iteration order, which breaks the deterministic-output promise.
func TestCategoryNamesAreDedupedCaseAndSpaceInsensitively(t *testing.T) {
	sql, _ := buildCategoryOne(t, twoKindsOfCategory)

	if got := strings.Count(sql, "INSERT INTO catalog_category"); got != 4 {
		t.Errorf("category inserts = %d, want 4 (ทรีตเมนต์หน้า · Wellness · ยา · อุปกรณ์ — "+
			"' wellness ' is the same category as 'Wellness')\n%s", got, sql)
	}
}

// Determinism is part of the artifact's contract: the same dump must render the same bytes
// so two runs can be diffed.
func TestCategoryOutputIsDeterministic(t *testing.T) {
	first, _ := buildCategoryOne(t, twoKindsOfCategory)
	for i := 0; i < 5; i++ {
		again, _ := buildCategoryOne(t, twoKindsOfCategory)
		if again != first {
			t.Fatalf("run %d differs from the first — map iteration order is leaking into the "+
				"output, so two runs of the same dump cannot be diffed", i+2)
		}
	}
}

// The drug categories being folded in is a change to what the clinic sees, so it is said.
func TestDrugCategoriesAreReported(t *testing.T) {
	_, rep := buildCategoryOne(t, twoKindsOfCategory)
	if !hasText(rep.Notes, "item_types") {
		t.Errorf("folding the drug categories into the master was not reported: %v", rep.Notes)
	}
}
