package legacyimport

import (
	"strings"
	"testing"
)

/*
What the stock collection becomes in the catalogue.

V2 keeps ONE stock list. V4 has two kinds of sellable stock and they are not interchangeable at
the till: nonSellableCatalogCodes (billing/helpers.go) refuses an item_type='drug' row whose five
legal label fields are not all filled in, and a V2 export carries none of them. So the choice
between 'drug' and 'goods' decides whether an imported clinic can sell out of its own stock room
at all — measured at the-gloss: 365 SKUs with stock on hand, 0 sellable, repaired afterwards by
hand.

These tests pin the two things that must not drift: the DEFAULT (drug, so no existing command
line changes meaning and nobody gets a sellable Botox by accident) and the REFUSAL of anything
that is neither (a typo must stop the run, not fall back to a default the operator did not pick).
*/

func stockFixture() Input {
	return Input{
		ClinicCode:    "MBU1",
		OwnerUsername: "admin",
		// Only the two groups this rule lives in. Without it BuildAll also builds loyalty,
		// which refuses a dump with no `points` collection — a failure about a different
		// group entirely, which would make these tests report the wrong thing.
		Groups:    []string{"catalog", "stock"},
		UsersDump: usersFixture(),
		// One SKU with one lot is enough: the type is one value for the whole collection.
		MongoDump: []byte(`db.items.insertMany([
{"_id":{"$oid":"66ee9cdf568636062d46e001"},"code":"ST000001","name":"Botox - Nabota 100U","category":"ยา","unit":"vial","cost":"1000","price":"1500","quantity":"5","quantity_total":"5","active":true,"stock":{"$oid":"635f2af6a9707232e3e300bf"}}
]);
`),
		BranchDump: []byte("INSERT INTO `branch` (`branch_code`, `branch_name`, `stock_id`) VALUES\n" +
			"('01', 'The Gloss Clinic. CCO', '635f2af6a9707232e3e300bf');\n"),
	}
}

func buildStock(t *testing.T, in Input) (string, *Report) {
	t.Helper()
	rep := NewReport(in.ClinicCode)
	batches, _, _, err := BuildAll(in, rep)
	if err != nil {
		t.Fatalf("BuildAll: %v", err)
	}
	return batches[EntCatalogDrug], rep
}

func TestStockDefaultsToDrugSoNoExistingCommandLineChangesMeaning(t *testing.T) {
	// StockItemType left empty, which is what every caller before 22 Sep 2026 passed.
	sql, _ := buildStock(t, stockFixture())
	// Matched on the VALUES row itself, not on the word anywhere in the batch: the guards in
	// the same batch legitimately name both types when they explain what they check.
	if !strings.Contains(sql, "'ST000001', 'drug'") {
		t.Errorf("an unset StockItemType must still import stock as drug:\n%s", head(sql, 700))
	}
	if strings.Contains(sql, "'ST000001', 'goods'") {
		t.Errorf("nothing asked for goods, yet the row was written as goods:\n%s", head(sql, 700))
	}
}

func TestStockAsGoodsIsWrittenAndSaidOutLoud(t *testing.T) {
	in := stockFixture()
	in.StockItemType = "goods"
	sql, rep := buildStock(t, in)

	if !strings.Contains(sql, "'ST000001', 'goods'") {
		t.Errorf("-stock-item-type=goods did not reach the catalogue row:\n%s", head(sql, 700))
	}
	if strings.Contains(sql, "'ST000001', 'drug'") {
		t.Errorf("goods was asked for but the row was written as drug:\n%s", head(sql, 700))
	}

	// A WARNING, not a note: it changes what the till accepts for every SKU in the stock room,
	// and the operator has to be able to see that they asked for it.
	var said bool
	for _, w := range rep.Warnings {
		if strings.Contains(w, "goods") && strings.Contains(w, "ฉลาก") {
			said = true
		}
	}
	if !said {
		t.Errorf("importing stock as goods was not reported as a warning: %v", rep.Warnings)
	}
}

func TestStockItemTypeRefusesAnythingElse(t *testing.T) {
	for _, bad := range []string{"Drug", "DRUG", "service", "course", "ยา", "goods "} {
		in := stockFixture()
		in.StockItemType = bad
		// " goods " with a space is trimmed and therefore valid; the trailing-space case is
		// here to prove the trim happens rather than the value being rejected for whitespace.
		_, _, _, err := BuildAll(in, NewReport(in.ClinicCode))
		if bad == "goods " {
			if err != nil {
				t.Errorf("a trailing space must be trimmed, not refused: %v", err)
			}
			continue
		}
		if err == nil {
			t.Errorf("StockItemType %q was accepted; an unknown type must stop the run rather "+
				"than silently fall back", bad)
			continue
		}
		if !strings.Contains(err.Error(), "drug") || !strings.Contains(err.Error(), "goods") {
			t.Errorf("the refusal for %q does not say which values are allowed: %v", bad, err)
		}
	}
}
