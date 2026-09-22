package legacyimport

// catalog_item.sku — the code staff read out loud, and the one this import never issued.
//
// WHY THESE TESTS EXIST
//
// The failure this covers is invisible in every place an operator looks. The import
// succeeds, psql prints no warning, the catalogue lists every row, stock moves and bills
// print — and the SKU column is blank on all of them, because upsertCatalogItem's column
// list has no `sku` in it. Measured on prod 2026-09-22: C2EC1E3 2,503 rows / 0 sku,
// C9D4865 359 / 0, against 100% on the five clinics that predate the CLI.
//
// WHAT THEY PROTECT
//
//  1. That the block is emitted at all, and for a drug-only run too. The sequence bump
//     next to it deliberately ignores drugs (imported stock keeps its V2 ST code), so
//     copying that condition is the obvious mistake and it silently skips a pharmacy.
//  2. That it stays replay-safe. Dropping the `sku IS NULL` filter would renumber rows
//     whose codes are already on paper; dropping GREATEST would let an older export
//     rewind the counter and hand out numbers that are already in use.
//  3. That every status is numbered. A WHERE status = 1 here looks tidy and leaves a
//     soft-deleted row ready to collide with a live one the day somebody undeletes it —
//     uniq_catalog_item_clinic_sku covers all statuses precisely because sku is forever.
//  4. That a leftover NULL is loud. The prefix CASE has no ELSE, so an item_type nobody
//     mapped yields NULL and the row keeps a blank code. That has to raise here, not turn
//     up on a screen weeks later.

import (
	"strings"
	"testing"
)

func skuSQL() string { return buildCatalogSKUIssue("C0TEST1") }

// catalogOnlyInput imports one drug and nothing else: no service, no course. That is the
// shape that the sequence bump's condition would wrongly reject.
func drugOnlyInput() Input {
	return Input{
		ClinicCode:    "C0TEST1",
		DefaultBranch: "BR00001",
		Groups:        []string{"catalog"},
		MongoDump: []byte(`db.items.insertMany([{"_id":"i1","clinic_code":"C0TEST1",` +
			`"code":"ST000001","name":"Botox 100U","quantity":5,"price":3500,"active":true}]);` + "\n"),
	}
}

func TestSKUIsIssuedForADrugOnlyRun(t *testing.T) {
	script, _, err := BuildScript(drugOnlyInput())
	if err != nil {
		t.Fatalf("BuildScript: %v", err)
	}
	if !strings.Contains(script, "INSERT INTO catalog_item_sku_counter") {
		t.Error("a run that imported drugs and nothing else emitted no SKU block — " +
			"the gate in script.go is probably copied from buildSequenceBump, which " +
			"ignores drugs on purpose because ST codes mint no CS/LH number")
	}
	// The drug rows must already be in the database when the numbering pass runs, so the
	// block cannot come before the catalog batch.
	skuAt := strings.Index(script, "INSERT INTO catalog_item_sku_counter")
	drugAt := strings.LastIndex(script, "INSERT INTO catalog_item")
	if skuAt >= 0 && drugAt >= 0 && skuAt < drugAt {
		t.Error("the SKU pass is emitted before the last catalog_item INSERT — it reads " +
			"those rows back with row_number(), so it would number an empty set")
	}
}

func TestSKUBlockIsAbsentWhenNoCatalogWasWritten(t *testing.T) {
	script, _, err := BuildScript(twoGroupInput()) // people + patients
	if err != nil {
		t.Fatalf("BuildScript: %v", err)
	}
	if strings.Contains(script, "catalog_item_sku_counter") {
		t.Error("a people/patients run emitted the SKU block; it has no catalogue rows to " +
			"number and would take a lock on a table it does not write")
	}
}

func TestSKUIsOnlyIssuedToRowsThatHaveNone(t *testing.T) {
	sql := skuSQL()
	if !strings.Contains(sql, "ci.sku IS NULL") {
		t.Errorf("the numbering pass is not restricted to rows without a sku — a re-run "+
			"would renumber codes that are already printed and quoted:\n%s", sql)
	}
	if !strings.Contains(sql, "GREATEST(catalog_item_sku_counter.counter, EXCLUDED.counter)") {
		t.Errorf("the counter is not forward-only; replaying an older export could rewind "+
			"it and start handing out numbers already in use:\n%s", sql)
	}
	// start_at must clear BOTH the counter row and the highest sku present, so a code
	// minted through the app between two runs cannot be duplicated.
	if !strings.Contains(sql, "GREATEST(") || !strings.Contains(sql, "max(c.counter)") ||
		!strings.Contains(sql, "max(substring(ci.sku from 5)::bigint)") {
		t.Errorf("start_at does not clear both the counter and the highest existing sku:\n%s", sql)
	}
}

func TestSKUCoversEveryStatusIncludingDeletedAndDraft(t *testing.T) {
	sql := skuSQL()
	if strings.Contains(sql, "status = 1") || strings.Contains(sql, "status IN (1") {
		t.Errorf("the pass filters on status. uniq_catalog_item_clinic_sku covers every "+
			"status because a sku may never be reused, so a soft-deleted row left without "+
			"one is a collision waiting for an undelete:\n%s", sql)
	}
}

func TestSKUPrefixesMatchTheDatabaseCheck(t *testing.T) {
	// The CASE arms are column-aligned in the emitted SQL, so the pairs are matched on
	// whitespace-collapsed text: this test is about which prefix goes with which type, and
	// it must not fail the day someone re-indents the block.
	sql := strings.Join(strings.Fields(skuSQL()), " ")
	// ck_catalog_item_sku_prefix_matches_type in migration 0367. A swapped pair here is
	// caught by the database, but only on the row that happens to hit it first.
	for itemType, prefix := range map[string]string{
		"service": "SVC", "goods": "PRD", "drug": "MED", "course": "CRS", "gift_card": "VCH",
	} {
		want := "WHEN '" + itemType + "' THEN '" + prefix + "'"
		if !strings.Contains(sql, want) {
			t.Errorf("missing or wrong prefix mapping %q:\n%s", want, skuSQL())
		}
	}
}

func TestSKUPaddingMatchesTheFormatCheck(t *testing.T) {
	sql := skuSQL()
	// Six zero-padded digits up to 999999, then the number as-is. 'SVC-0000001' is the
	// signature of a padding bug and ck_catalog_item_sku_format rejects it.
	if !strings.Contains(sql, "lpad(numbered.n::text, 6, '0')") {
		t.Errorf("the six-digit padding is not the one ck_catalog_item_sku_format expects:\n%s", sql)
	}
	if !strings.Contains(sql, "numbered.n <= 999999") {
		t.Errorf("no rollover branch — past 999999 the code must print every digit "+
			"rather than being padded to a shape the CHECK refuses:\n%s", sql)
	}
}

func TestSKULeftoverNullsRaiseInsteadOfShipping(t *testing.T) {
	sql := skuSQL()
	if !strings.Contains(sql, "RAISE EXCEPTION") {
		t.Errorf("a row left without a sku does not raise. The prefix CASE has no ELSE, so "+
			"an unmapped item_type yields NULL and the import would finish reporting "+
			"success with blank codes on screen:\n%s", sql)
	}
	if !strings.Contains(sql, "item_type") || !strings.Contains(sql, "string_agg") {
		t.Errorf("the refusal does not name the item_type that was not mapped, which is "+
			"the only thing that makes it actionable:\n%s", sql)
	}
}

func TestSKUBlockIsScopedToOneClinic(t *testing.T) {
	sql := buildCatalogSKUIssue("C0TEST1")
	if strings.Count(sql, "'C0TEST1'") < 4 {
		t.Errorf("the clinic literal appears fewer than four times; an unscoped statement "+
			"here would renumber another tenant's catalogue:\n%s", sql)
	}
	if strings.Contains(sql, "%!") {
		t.Errorf("the format string produced a Go verb error — a bare %% in the SQL needs "+
			"escaping:\n%s", sql)
	}
}
