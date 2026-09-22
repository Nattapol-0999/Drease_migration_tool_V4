package legacyimport

import (
	"regexp"
	"strings"
	"testing"
)

// A drug's catalog status follows items.active and nothing else — settled with the clinic
// owner on 15 Sep 2026.
//
// The rule it replaced asked for a selling price AND the five label fields
// (generic_name/strength/dosage_form/indication/warning). Measured on the first real
// tenant, the export supplies none of the five for any SKU and leaves 316 of 398 prices at
// 0, so every drug landed as draft: not sellable, missing from the item picker, and no
// correct data in the dump could have changed it. Meanwhile V2's own stock screen listed
// those same SKUs as ใช้งาน.
//
// So the test pins the pair that matters: switched on in V2 goes in sellable even with no
// price, switched off stays draft even with a big one.

// drugItemsDump is a mongo `items` export in the shape the parser reads: two SKUs that are
// active (one with a price, one without) and two that are not (one of them expensive).
const drugItemsDump = `db.getCollection("items").insertMany([
{"_id":"a1","code":"ST000397","name":"Vitaran","category":"ยา","unit":"CC","price":"9900","cost":"0","active":true,"createdAt":"2026-07-02 11:24:00"},
{"_id":"a2","code":"ST000005","name":"เข็มเบอร์ 30","category":"อุปกรณ์","unit":"กล่อง","price":"0","cost":"12","active":true,"createdAt":"2022-01-05 09:00:00"},
{"_id":"a3","code":"Botox 100U","name":"Botox nabota 100u","category":"ยา","unit":"ยูนิต","price":"15900","cost":"0","active":false,"createdAt":"2023-03-01 09:00:00"},
{"_id":"a4","code":"ST000026","name":"Micropore (3M)","category":"อื่นๆ","unit":"ม้วน","price":"0","cost":"35","active":false,"createdAt":"2022-02-01 09:00:00"}
]);
`

// statusOfCatalogItem pulls the status column out of the generated INSERT for one code.
// The value sits between source_id and code_conflict in upsertCatalogItem's tuple, so it is
// read by locating the statement for the code and then the literal that precedes 'items'.
func statusOfCatalogItem(t *testing.T, sqlText, code string) string {
	t.Helper()
	for _, stmt := range strings.Split(sqlText, ";\n") {
		if !strings.Contains(stmt, "INSERT INTO catalog_item") {
			continue
		}
		if !strings.Contains(stmt, "'"+code+"'") {
			continue
		}
		// ... 'items', (SELECT id FROM items ...), false, <status>, ...
		m := regexp.MustCompile(`'items',\s*\(SELECT id FROM items[^)]*\),\s*(?:true|false),\s*(\d+)`).FindStringSubmatch(stmt)
		if m != nil {
			return m[1]
		}
		// Fall back to any bare integer after the source_table literal, so a column
		// reorder fails loudly here instead of silently matching nothing.
		m2 := regexp.MustCompile(`'items',.*?,\s*(\d+),`).FindStringSubmatch(stmt)
		if m2 != nil {
			return m2[1]
		}
		t.Fatalf("found the statement for %s but could not read its status:\n%s", code, stmt)
	}
	t.Fatalf("no catalog_item statement for %s in:\n%s", code, head(sqlText, 1500))
	return ""
}

func TestDrugStatusFollowsItemsActiveNotPrice(t *testing.T) {
	mc, err := parseMongoCatalog([]byte(drugItemsDump))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	in := Input{ClinicCode: "CTEST01", BranchMap: map[string]string{"01": "BR00001"}}
	rep := NewReport(in.ClinicCode)

	_, drugSQL, _, _, err := buildItems(mc, in, rep)
	if err != nil {
		t.Fatalf("buildItems: %v", err)
	}

	// Active in V2 → sellable, whether or not the export carried a price. The 0-price case
	// is the one the old rule got wrong: 316 of one tenant's 398 SKUs look like this and
	// the clinic sells them every day.
	for _, code := range []string{"ST000397", "ST000005"} {
		if got := statusOfCatalogItem(t, drugSQL, code); got != "1" {
			t.Errorf("%s is active in V2 but was imported with status=%s — it would be missing from the item picker", code, got)
		}
	}

	// Switched off in V2 → draft, even at 15,900. Draft is still receivable into stock and
	// visible to staff; only selling is withheld.
	for _, code := range []string{"Botox 100U", "ST000026"} {
		if got := statusOfCatalogItem(t, drugSQL, code); got != "2" {
			t.Errorf("%s is inactive in V2 but was imported with status=%s — V4 would offer something the clinic switched off", code, got)
		}
	}

	// The count has to be reported: two SKUs silently unavailable is exactly the failure
	// this whole change came out of.
	if !hasText(rep.Notes, "items.active=false") {
		t.Errorf("the drafted SKUs were not reported: %v", rep.Notes)
	}
	if !hasText(rep.Notes, "2 SKU") {
		t.Errorf("the note does not carry the count: %v", rep.Notes)
	}
}

func TestDrugStatusIgnoresTheLabelFieldsEntirely(t *testing.T) {
	// The five label fields are absent from every export seen (0 of 398 on the first real
	// tenant). If a future change reinstates them as a gate, this fails rather than quietly
	// hiding the catalogue again.
	mc, err := parseMongoCatalog([]byte(drugItemsDump))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	in := Input{ClinicCode: "CTEST01", BranchMap: map[string]string{"01": "BR00001"}}
	itemsSQL, drugSQL, _, _, err := buildItems(mc, in, NewReport(in.ClinicCode))
	if err != nil {
		t.Fatalf("buildItems: %v", err)
	}

	// items rows carry no label data — proving the status above was not reached by
	// accidentally filling it in.
	for _, col := range []string{"generic_name_th", "strength", "dosage_form", "indication", "warning"} {
		if strings.Contains(itemsSQL, col) {
			t.Errorf("items INSERT now writes %s — clinical text must not be invented from a product name", col)
		}
	}
	if got := statusOfCatalogItem(t, drugSQL, "ST000005"); got != "1" {
		t.Errorf("a SKU with no price and no label is active in V2, so it must be status=1, got %s", got)
	}
}
