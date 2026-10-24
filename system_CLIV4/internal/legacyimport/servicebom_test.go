package legacyimport

// Tests for service recipes: mris.listheal_medicine → catalog_item_child relation_kind='bom'.
// A bom edge makes checkout draw that material on every use and refuse the whole bill when
// it cannot, so these pin WHICH edges are written as much as how.

import (
	"strings"
	"testing"
)

const bomSample = `db.getCollection("listheals").insert([
  { _id: ObjectId("6a00000000000000000000d1"), code: "LH006", name_th: "Filler Voluma", cost: NumberDecimal("0"), active: true },
  { _id: ObjectId("6a00000000000000000000d2"), code: "LH103", name_th: "Filler test",   cost: NumberDecimal("10000"), active: true }
]);
db.getCollection("items").insert([
  { _id: ObjectId("6b00000000000000000000e1"), code: "ST000024", name1: "Juvederm Voluma" },
  { _id: ObjectId("6b00000000000000000000e2"), code: "ST000040", name1: "Yvoire" },
  { _id: ObjectId("6b00000000000000000000e3"), code: "ST000003", name1: "Ulthera tip" },
  { _id: ObjectId("6b00000000000000000000e4"), code: "ST000099", name1: "Gauze" }
]);
db.getCollection("listheal_medicine").insert([
  { _id: ObjectId("6c00000000000000000000f1"), listheal_id: ObjectId("6a00000000000000000000d2"),
    medicine: [
      { item_id: ObjectId("6b00000000000000000000e2"), name: "Yvoire 1 cc", unit: "cc", numoftime: 1, fixed: false },
      { item_id: ObjectId("6b00000000000000000000e3"), name: "tip", unit: "Line", numoftime: 1.5, fixed: true },
      { item_id: ObjectId("6b00000000000000000000e2"), name: "Yvoire again", unit: "cc", numoftime: 3, fixed: false },
      { item_id: ObjectId("6b0000000000000000000999"), name: "gone", unit: "cc", numoftime: 1, fixed: false },
      { item_id: ObjectId("6b00000000000000000000e4"), name: "gauze", unit: "pc", numoftime: 0, fixed: false },
      { item_id: ObjectId("6b00000000000000000000e4"), name: "gauze", unit: "pc", numoftime: 0.125, fixed: false }
    ],
    created_at: ISODate("2023-02-24T03:52:50.784Z"), updated_at: ISODate("2023-02-24T03:52:50.784Z") },
  { _id: ObjectId("6c00000000000000000000f2"), listheal_id: ObjectId("6a00000000000000000000d1"),
    medicine: [ { item_id: ObjectId("6b00000000000000000000e1"), name: "old", unit: "cc", numoftime: 1, fixed: false } ],
    created_at: ISODate("2022-11-07T07:53:27.548Z"), updated_at: ISODate("2022-11-07T07:53:27.548Z") },
  { _id: ObjectId("6c00000000000000000000f3"), listheal_id: ObjectId("6a00000000000000000000d1"),
    medicine: [ { item_id: ObjectId("6b00000000000000000000e1"), name: "new", unit: "cc", numoftime: 2, fixed: false } ],
    created_at: ISODate("2023-05-01T00:00:00.000Z"), updated_at: ISODate("2023-05-01T00:00:00.000Z") },
  { _id: ObjectId("6c00000000000000000000f4"), listheal_id: ObjectId("6a0000000000000000000999"),
    medicine: [ { item_id: ObjectId("6b00000000000000000000e1"), name: "orphan", unit: "cc", numoftime: 1, fixed: false } ],
    created_at: ISODate("2023-01-01T00:00:00.000Z"), updated_at: ISODate("2023-01-01T00:00:00.000Z") }
]);`

func buildBOMOne(t *testing.T) (string, *Report) {
	t.Helper()
	mc, err := parseMongoCatalog([]byte(bomSample))
	if err != nil {
		t.Fatalf("parseMongoCatalog: %v", err)
	}
	rep := NewReport("C9C9611")
	in := Input{ClinicCode: "C9C9611"}
	if _, err := buildCatalogServices(mc, in, rep); err != nil {
		t.Fatalf("buildCatalogServices: %v", err)
	}
	return buildServiceBOM(mc, in, rep), rep
}

// recipeStatementFor returns the recipe statement of one service.
func recipeStatementFor(t *testing.T, sql, svcCode string) string {
	t.Helper()
	at := strings.Index(sql, "AND code = '"+svcCode+"' AND item_type = 'service'")
	if at < 0 {
		t.Fatalf("no recipe statement for %s\n%s", svcCode, sql)
	}
	start := strings.LastIndex(sql[:at], "INSERT INTO catalog_item_child")
	end := strings.Index(sql[at:], ";")
	return sql[start : at+end]
}

// One statement per service, with every usable material of its recipe.
func TestServiceRecipeIsWrittenAsBomEdges(t *testing.T) {
	sql, _ := buildBOMOne(t)
	if n := strings.Count(sql, "INSERT INTO catalog_item_child"); n != 2 {
		t.Fatalf("recipe statements = %d, want 2 (LH006 and LH103; the orphan has no service)\n%s", n, sql)
	}
	lh103 := recipeStatementFor(t, sql, "LH103")
	if !strings.Contains(lh103, "VALUES ('ST000040', 1, 0), ('ST000003', 1.5, 1), ('ST000099', 0.13, 2)") {
		t.Errorf("LH103 materials are not the usable ones in order (duplicate, unknown item and qty 0 dropped; 0.125 rounded)\n%s", lh103)
	}
	if !strings.Contains(lh103, "'bom'") {
		t.Error("recipe edge is not relation_kind='bom'")
	}
}

// Where V2 kept two recipes for one service, the one it showed last wins.
func TestNewestRecipeOfAServiceWins(t *testing.T) {
	sql, _ := buildBOMOne(t)
	lh006 := recipeStatementFor(t, sql, "LH006")
	if !strings.Contains(lh006, "VALUES ('ST000024', 2, 0)") {
		t.Errorf("LH006 does not carry the newer recipe (qty 2)\n%s", lh006)
	}
}

// A material is written only where V4's own recipe editor would accept it, and checkout
// would not refuse the bill over it: a live drug/goods SKU whose items row is active.
func TestRecipeMaterialMustBeALiveStockSKU(t *testing.T) {
	sql, _ := buildBOMOne(t)
	lh103 := recipeStatementFor(t, sql, "LH103")
	for _, want := range []string{
		"JOIN items i ON i.clinic_code = ci.clinic_code AND i.id = ci.source_id AND i.active",
		"ci.source_table = 'items'",
		"ci.item_type IN ('drug', 'goods') AND ci.status IN (1, 2) AND NOT ci.code_conflict",
		"WHERE mat.id <> svc.id",
	} {
		if !strings.Contains(lh103, want) {
			t.Errorf("recipe statement is missing %q\n%s", want, lh103)
		}
	}
}

// A recipe already set in V4 is left alone: the whole service is skipped, in the same
// statement, so a re-run cannot re-add a material the clinic removed on screen.
func TestServiceThatAlreadyHasARecipeIsLeftAlone(t *testing.T) {
	sql, _ := buildBOMOne(t)
	lh103 := recipeStatementFor(t, sql, "LH103")
	want := "AND NOT EXISTS (SELECT 1 FROM catalog_item_child b\n" +
		"                    WHERE b.clinic_code = 'C9C9611' AND b.parent_item_id = svc.id\n" +
		"                      AND b.relation_kind = 'bom')"
	if !strings.Contains(lh103, want) {
		t.Errorf("recipe is not skipped when the service already has one\n%s", lh103)
	}
}

// Every skip has a number, and the stock consequence is a warning.
func TestRecipeSkipsAreCountedAndTheStockRuleIsSaid(t *testing.T) {
	_, rep := buildBOMOne(t)
	var note, fixed, warn, round string
	for _, n := range rep.Notes {
		switch {
		case strings.Contains(n, "สูตรวัสดุของบริการจาก listheal_medicine"):
			note = n
		case strings.Contains(n, "fixed=true"):
			fixed = n
		}
	}
	for _, w := range rep.Warnings {
		switch {
		case strings.Contains(w, "stock_autofill_on_short"):
			warn = w
		case strings.Contains(w, "ทศนิยมเกิน 2"):
			round = w
		}
	}
	if !strings.Contains(note, "2 บริการ · 4 วัสดุ") ||
		!strings.Contains(note, "[ไม่ตรงบริการ: 1, item_id ไม่ตรง items: 1, จำนวนไม่ถูกต้อง: 1, วัสดุซ้ำ: 1, สูตรว่าง: 0, เอกสารเก่าที่ถูกแทน: 1]") {
		t.Errorf("recipe note does not carry the counts: %q", note)
	}
	if !strings.Contains(fixed, "1 วัสดุ") {
		t.Errorf("fixed=true is not counted: %q", fixed)
	}
	if warn == "" {
		t.Errorf("the checkout/stock consequence is not warned about; warnings = %v", rep.Warnings)
	}
	if !strings.Contains(round, "0.125 เป็น 0.13") {
		t.Errorf("the rounded quantity is not named: %q", round)
	}
}

// The recipe needs both of its ends, so it is ordered after the services and the drugs, and
// it travels with the catalog group.
func TestRecipeIsOrderedAfterServicesAndDrugs(t *testing.T) {
	pos := map[string]int{}
	for i, e := range EntityOrder() {
		pos[e] = i
	}
	bom, ok := pos[EntCatalogServiceBOM]
	if !ok {
		t.Fatal("the recipe entity is not in the script order")
	}
	if bom < pos[EntCatalogService] || bom < pos[EntCatalogDrug] {
		t.Errorf("recipe at %d runs before the services (%d) or the drugs (%d)", bom, pos[EntCatalogService], pos[EntCatalogDrug])
	}
	want, err := entitiesForGroups([]string{"catalog"})
	if err != nil || !want[EntCatalogServiceBOM] {
		t.Errorf("the catalog group does not carry the recipe (err %v)", err)
	}
}

// No listheal_medicine, no recipe batch at all.
func TestNoRecipeCollectionWritesNothing(t *testing.T) {
	mc, err := parseMongoCatalog([]byte(serviceStatusSample))
	if err != nil {
		t.Fatalf("parseMongoCatalog: %v", err)
	}
	if got := buildServiceBOM(mc, Input{ClinicCode: "C9C9611"}, NewReport("C9C9611")); got != "" {
		t.Errorf("a dump without listheal_medicine produced recipe SQL:\n%s", got)
	}
}
