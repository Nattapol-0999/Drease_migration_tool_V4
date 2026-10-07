package legacyimport

// A SKU unit cost with more decimal places than items.cost / catalog_item.cost keep
// (owner's decision ก, 7 Oct 2026): rounded the way Postgres would round it, half away from
// zero, and named in the report — instead of the whole file being refused. The cost only:
// a selling price with three places is still refused, and the lot keeps the source figure.
//
// The four SKUs are the ones that stopped wellmed-clinic's import on 7 Oct 2026, tablets
// costed to three places in V2.

import (
	"strings"
	"testing"
)

const threePlaceCosts = `db.getCollection("items").insert([
  { _id: ObjectId("ab0000000000000000000184"), code: "ST000184", name1: "Multivitamin (MTV)", quantity: "690", quantity_total: "690", cost: 0.285, price: 4, active: true, stock: ObjectId("64c753d9b9255d50a3cc315b") },
  { _id: ObjectId("ab0000000000000000000271"), code: "ST000271", name1: "Pseudoephedrine HCL 60 mg", quantity: "3048", quantity_total: "3048", cost: 0.625, price: 20, active: true, stock: ObjectId("64c753d9b9255d50a3cc315b") },
  { _id: ObjectId("ab0000000000000000000474"), code: "ST000474", name1: "Nitrofurantoin 100 MG", quantity: "364", quantity_total: "364", cost: 1.498, price: 20, active: true, stock: ObjectId("64c753d9b9255d50a3cc315b") },
  { _id: ObjectId("ab0000000000000000000542"), code: "ST000542", name1: "Pseudoephedrine HCL 30 mg", quantity: "1200", quantity_total: "1200", cost: 0.485, price: 15, active: true, stock: ObjectId("64c753d9b9255d50a3cc315b") },
  { _id: ObjectId("ab0000000000000000000001"), code: "ST000001", name1: "Gauze", quantity: "10", quantity_total: "10", cost: 12.50, price: 30, active: true, stock: ObjectId("64c753d9b9255d50a3cc315b") }
]);
`

// costStatementFor returns the first statement of a batch that names code and contains lead.
func costStatementFor(sql, lead, code string) string {
	for _, st := range strings.Split(sql, ";\n") {
		if strings.Contains(st, lead) && strings.Contains(st, "'"+code+"'") {
			return st
		}
	}
	return ""
}

func TestASKUCostWithThreePlacesIsRoundedTheWayPostgresWould(t *testing.T) {
	mc, err := parseMongoCatalog([]byte(threePlaceCosts))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	in := Input{ClinicCode: "C01B42B"}
	rep := NewReport(in.ClinicCode)
	itemsSQL, catalogSQL, slotSQL, _, err := buildItems(mc, in, rep)
	if err != nil {
		t.Fatalf("buildItems: %v", err)
	}

	// Half away from zero, like numeric(12,2) on insert. 0.285 and 0.625 are the two a
	// banker's rounding would get wrong (0.28 and 0.62).
	for code, c := range map[string]struct{ src, cost, price string }{
		"ST000184": {"0.285", "0.29", "4"},
		"ST000271": {"0.625", "0.63", "20"},
		"ST000474": {"1.498", "1.50", "20"},
		"ST000542": {"0.485", "0.49", "15"},
	} {
		ins := costStatementFor(itemsSQL, "INSERT INTO items", code)
		upd := costStatementFor(itemsSQL, "UPDATE items", code)
		if !strings.Contains(ins, c.cost+", "+c.price) || !strings.Contains(upd, "cost       = "+c.cost) {
			t.Errorf("%s: items.cost is not %s (from %s)\n%s\n%s", code, c.cost, c.src, ins, upd)
		}
		cat := costStatementFor(catalogSQL, "INSERT INTO catalog_item", code)
		if !strings.Contains(cat, c.price+", "+c.price+", "+c.cost) {
			t.Errorf("%s: catalog_item cost is not %s:\n%s", code, c.cost, cat)
		}
		// The lot keeps the source figure: slot_stock.cost_per_unit is numeric(14,4).
		lot := costStatementFor(slotSQL, "INSERT INTO slot_stock", code)
		if !strings.Contains(lot, ", "+c.src+",") {
			t.Errorf("%s: the lot lost its source unit cost %s:\n%s", code, c.src, lot)
		}
		if !hasWarning(rep, code) {
			t.Errorf("%s is not named in the report · %v", code, rep.Warnings)
		}
	}

	// A cost that fits is written as it is and not reported. The dump's bare 12.50 is a
	// number, which the parser reads back as 12.5: the same value, not a rounding.
	if !strings.Contains(costStatementFor(itemsSQL, "INSERT INTO items", "ST000001"), "12.5, 30") {
		t.Error("a cost that already fits was changed")
	}
	if hasWarning(rep, "ST000001") {
		t.Error("a cost that already fits is reported as rounded")
	}

	for _, want := range []string{
		"ต้นทุนต่อหน่วยของ 4 SKU",
		"ST000184 Multivitamin (MTV) 0.285 → 0.29",
		"ST000271 Pseudoephedrine HCL 60 mg 0.625 → 0.63",
		"7 ต.ค. 2569",
	} {
		if !hasWarning(rep, want) {
			t.Errorf("the report does not say %q · %v", want, rep.Warnings)
		}
	}
}

// The whole build used to stop at the width guard. It must now pass — and only for cost.
func TestTheWidthGuardLetsARoundedCostThroughButStillStopsAThreePlacePrice(t *testing.T) {
	_, _, err := BuildScript(Input{
		ClinicCode:    "C01B42B",
		DefaultBranch: "BR00001",
		Groups:        []string{"catalog"},
		MongoDump:     []byte(threePlaceCosts),
	})
	if err != nil {
		t.Fatalf("a three-place cost still refuses the file: %v", err)
	}

	price := strings.Replace(threePlaceCosts, "price: 30", "price: 30.125", 1)
	_, _, err = BuildScript(Input{
		ClinicCode:    "C01B42B",
		DefaultBranch: "BR00001",
		Groups:        []string{"catalog"},
		MongoDump:     []byte(price),
	})
	if err == nil {
		t.Fatal("a three-place SELLING price went through — decision ก covers the cost only")
	}
	if !strings.Contains(err.Error(), "price") {
		t.Errorf("the refusal does not name the price column: %v", err)
	}
}
