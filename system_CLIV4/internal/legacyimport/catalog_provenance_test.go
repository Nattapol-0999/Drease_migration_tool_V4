package legacyimport

// An imported ยา/เวชภัณฑ์ row must carry its provenance: catalog_item.source_id and
// catalog_item.legacy_source_key are never empty. Owner rule, 22 Sep 2026.
//
// WHAT WENT WRONG
//
// The source_id subquery read `... AND code = Y AND active LIMIT 1`. One word — `active` —
// and every SKU that V2 had switched off landed with source_id = NULL. What follows is all
// silent:
//
//  1. source_id NULL
//  2. trg_catalog_capture_legacy_source_key (migrations 0296/0301) reads `items` THROUGH
//     source_id, so legacy_source_key is forced to NULL as well
//  3. nonSellableCatalogCodes (billing/helpers.go) refuses a drug when `i.id IS NULL` —
//     before it looks at a single label field
//
// So the SKU cannot be sold, and filling in the five legal label fields does not help: the
// pharmacist's work has nowhere to land. Measured on prod 2026-09-22, matching with no
// exceptions — C2EC1E3 450 of 645, C6C936C 221 of 566. Both repaired by hand with
// 22_link_items_source_id.sql.
//
// WHY TESTS AND NOT JUST THE FIX
//
// `AND active` is the reading a careful person would write: of course you want the live row.
// It is correct for the 195 codes V2 still has switched on and wrong for the other 450, and
// nothing in the import, the log, or the catalogue screen tells you which you got.

import (
	"strings"
	"testing"
)

func drugCatalogSQL(t *testing.T) string {
	t.Helper()
	mc, err := parseMongoCatalog([]byte(drugItemsDump))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	in := Input{ClinicCode: "CTEST01", BranchMap: map[string]string{"01": "BR00001"}}
	_, catalogSQL, _, _, err := buildItems(mc, in, NewReport(in.ClinicCode))
	if err != nil {
		t.Fatalf("buildItems: %v", err)
	}
	if !strings.Contains(catalogSQL, "INSERT INTO catalog_item") {
		t.Fatal("no catalog_item INSERT was produced — the fixture is wrong, not the writer")
	}
	return catalogSQL
}

func TestDrugSourceIDDoesNotSkipSwitchedOffRows(t *testing.T) {
	sql := drugCatalogSQL(t)

	// The exact shape that produced 671 unsellable SKUs across two tenants.
	if strings.Contains(sql, "AND code = 'ST000397' AND active LIMIT 1") ||
		strings.Contains(sql, "AND active LIMIT 1") {
		t.Errorf("the source_id subquery still filters on `active`. A SKU switched off in V2 "+
			"gets source_id = NULL, which nulls legacy_source_key through the trigger and "+
			"makes the row permanently unsellable:\n%s", sql)
	}
	if !strings.Contains(sql, "ORDER BY active DESC, id ASC LIMIT 1") {
		t.Errorf("the source_id subquery is not deterministic. Several items rows can share a "+
			"code, so the pick has to be stated: the row V2 still has on, else the oldest:\n%s", sql)
	}
	// It must still be scoped to the clinic — an unscoped pick would link one tenant's
	// catalogue row to another tenant's stock master.
	if !strings.Contains(sql, "SELECT id FROM items WHERE clinic_code = 'CTEST01'") {
		t.Errorf("the source_id subquery is not clinic-scoped:\n%s", sql)
	}
	if !strings.Contains(sql, "'items'") {
		t.Errorf("source_table is not 'items'; stockfefo.Classify refuses a drug/goods row "+
			"with any other provenance:\n%s", sql)
	}
}

func TestDrugBatchRefusesToFinishWithEmptyProvenance(t *testing.T) {
	sql := drugCatalogSQL(t)

	if !strings.Contains(sql, "source_id IS NULL") || !strings.Contains(sql, "RAISE EXCEPTION") {
		t.Errorf("the drug batch does not refuse on a missing source_id. A warning is not "+
			"enough: the row is unsellable forever and nothing downstream says so:\n%s", sql)
	}
	// Checked separately from source_id on purpose — legacy_source_key is written by a
	// TRIGGER, so if that trigger is dropped the source_id check passes and this one is the
	// only thing left.
	if !strings.Contains(sql, "legacy_source_key IS NULL") {
		t.Errorf("legacy_source_key is not checked. It is written by "+
			"trg_catalog_capture_legacy_source_key, not by this script, so it needs its own "+
			"assertion:\n%s", sql)
	}
	// A link to the wrong row is worse than no link: it attaches one product's drug label to
	// another product's bill, and nothing on screen shows it.
	if !strings.Contains(sql, "i.code = ci.code") {
		t.Errorf("the guard does not verify that source_id points at the SAME code:\n%s", sql)
	}
	if !strings.Contains(sql, "i.clinic_code = ci.clinic_code") {
		t.Errorf("the guard does not verify that source_id points at the same clinic:\n%s", sql)
	}
	// The guard belongs to the drug batch, so a failure rolls those rows back with it.
	guardAt := strings.Index(sql, "RAISE EXCEPTION")
	lastInsert := strings.LastIndex(sql, "INSERT INTO catalog_item")
	if guardAt >= 0 && lastInsert >= 0 && guardAt < lastInsert {
		t.Error("the guard runs before the last catalog_item INSERT, so it would judge rows " +
			"that have not been written yet")
	}
}

func TestProvenanceGuardIsScopedToOneClinic(t *testing.T) {
	sql := itemsProvenanceGuard("CTEST01")
	if strings.Count(sql, "'CTEST01'") < 3 {
		t.Errorf("the clinic literal appears fewer than three times; an unscoped guard would "+
			"refuse this import over another tenant's rows:\n%s", sql)
	}
	if !strings.Contains(sql, "item_type IN ('drug','goods')") {
		t.Errorf("the guard is not limited to drug/goods. Services and courses carry "+
			"listheal/catalog provenance and legitimately have no items row:\n%s", sql)
	}
	if strings.Contains(sql, "%!") {
		t.Errorf("the format string produced a Go verb error — a bare %% inside the PL/pgSQL "+
			"needs escaping:\n%s", sql)
	}
	// RAISE's own placeholders must survive into the SQL, otherwise the message the operator
	// reads has no numbers in it.
	if strings.Count(sql, "RAISE EXCEPTION") != 3 {
		t.Errorf("expected three separate refusals (no source_id / no legacy_source_key / "+
			"wrong target), got %d:\n%s", strings.Count(sql, "RAISE EXCEPTION"), sql)
	}
}
