package legacyimport

// The receive ledger, and what a lot is worth.
//
// Two things buildItems used to get wrong about stock, both invisible in a green build and
// both wrong in the database:
//
//  1. NO stock_movement AT ALL. V4 opens no lot without one — ReceiveLot and GuidedReceive
//     each call InsertStockReceipt in the same transaction as InsertSlotLot. An imported
//     clinic therefore held inventory that had never arrived: the stock-in report was
//     empty, stock/movements showed nothing, and on-hand could not be reconciled against
//     any history. Migration 0209 names the same failure from the other side — 44 of 213
//     lot×branch pairs in demo already disagree with slot_stock "because not every writer
//     has always gone through a movement".
//
//  2. cost_lot = cost_per_unit. `items.cost` is the cost of ONE unit (of the-gloss-cct's 42
//     priced SKUs, every one sits at or below twice its selling price and none resembles a
//     lot total), so a 50-unit lot was booked at the value of a single unit.
//
// These are asserted from ONE build because they are written in one pass: "every lot got a
// movement" is not a property two separate builds can establish.

import (
	"strings"
	"testing"
)

// lotsWithCostAndReceiveDate: one multi-unit priced lot, one zero-cost lot, and one lot in
// a branch this clinic did not create. `store` is the legacy receive date.
func lotsWithCostAndReceiveDate() []byte {
	return []byte(`db.getCollection("items").insert([
  { _id: ObjectId("bb0000000000000000000001"), code: "PRICED", name1: "Semaglutide", name2: "2.5", unit: ObjectId("dd00000000000000000000d1"), quantity: "52", quantity_total: "52", cost: "4999", price: "6900", active: true, store: "2025-03-04 08:30:00", stock: ObjectId("63608c6c7796354dbae69c55") },
  { _id: ObjectId("bb0000000000000000000002"), code: "FREEBIE", name1: "ตัวอย่างแจก", quantity: "3", quantity_total: "3", cost: "0", price: "0", active: true, store: "2025-05-06 09:00:00", stock: ObjectId("63608c6c7796354dbae69c55") },
  { _id: ObjectId("bb0000000000000000000003"), code: "OTHERBR", name1: "ของสาขาอื่น", quantity: "9", quantity_total: "9", cost: "120", price: "200", active: true, store: "2025-06-07 10:00:00", stock: ObjectId("64128720030d9b36ce99cd57") }
]);
db.getCollection("item_units").insert([
  { _id: ObjectId("dd00000000000000000000d1"), name: "ปากกา" }
]);
`)
}

func receiveInput() Input {
	return Input{
		ClinicCode:    "MBLOT1",
		BranchDump:    branchRegistryWithStocks(),
		MongoDump:     lotsWithCostAndReceiveDate(),
		BranchMap:     tickedSeven(),
		DefaultBranch: "BR00004",
	}
}

// One movement per lot, and exactly one. A lot with no receive row is stock that appeared
// out of nowhere; two would double the quantity received.
func TestEveryImportedLotGetsOneReceiveMovement(t *testing.T) {
	slotSQL, moveSQL, rep := buildLotsAndMoves(t, receiveInput())

	lots := strings.Count(slotSQL, "INSERT INTO slot_stock")
	moves := strings.Count(moveSQL, "INSERT INTO stock_movement")
	if lots != 2 {
		t.Fatalf("slot_stock inserts = %d, want 2 (the third lot's branch is not in this clinic)\n%s", lots, slotSQL)
	}
	if moves != lots {
		t.Errorf("stock_movement inserts = %d but slot_stock = %d — a lot with no receive row "+
			"is inventory with no arrival history\n%s", moves, lots, moveSQL)
	}
	// And the dropped lot must not have left a movement behind, which would credit a
	// receive into a branch that never got the goods.
	if strings.Contains(moveSQL, "OTHERBR") {
		t.Errorf("the lot of an unticked branch was not imported, but its receive was:\n%s", moveSQL)
	}

	if !hasText(rep.Notes, "reason='receive'") {
		t.Errorf("the receive ledger was not reported: %v", rep.Notes)
	}
}

// The ledger row has to be a RECEIVE, has to point at the lot, and has to name no user.
func TestReceiveMovementShape(t *testing.T) {
	_, moveSQL, _ := buildLotsAndMoves(t, receiveInput())
	stmt := movementFor(t, moveSQL, "PRICED")

	if !strings.Contains(stmt, "'receive'") {
		t.Errorf("the movement is not reason='receive' — stock_movement defaults to "+
			"'pos_sale', so leaving it out would book the arrival as a sale\n%s", stmt)
	}
	// slot_stock.id is issued by the database one statement earlier, so the link can only
	// be a lookup — and mongo_id is the column migration 0035 declares UNIQUE.
	if !strings.Contains(stmt, "FROM slot_stock s") || !strings.Contains(stmt, "s.mongo_id =") {
		t.Errorf("slot_id is not resolved from slot_stock.mongo_id\n%s", stmt)
	}
	if !strings.Contains(stmt, "s.id") {
		t.Errorf("the movement does not carry slot_id, so 'which lot' is unanswerable\n%s", stmt)
	}
	// lot_code mirrors slot_stock.code (migration 0108), which this importer sets to the
	// mongo id.
	if !strings.Contains(stmt, "'bb0000000000000000000001'") {
		t.Errorf("lot_code does not mirror the lot's code\n%s", stmt)
	}
	// qty_delta is positive and is the lot's quantity.
	if !strings.Contains(stmt, ", 52, ") {
		t.Errorf("qty_delta is not the lot quantity 52\n%s", stmt)
	}
	// No person did this. Naming one would be a lie the ledger keeps forever.
	if strings.Contains(stmt, "user_id") {
		t.Errorf("the movement names a user_id; an import has no human receiver\n%s", stmt)
	}
	if !strings.Contains(stmt, "'migrate v2'") {
		t.Errorf("the movement is not marked as coming from the migration\n%s", stmt)
	}
}

// The goods arrived when V2 says they arrived. Stamping now() would pile years of
// deliveries onto the afternoon of the import and make every stock-in report wrong.
func TestReceiveMovementUsesTheLegacyReceiveDate(t *testing.T) {
	_, moveSQL, _ := buildLotsAndMoves(t, receiveInput())

	stmt := movementFor(t, moveSQL, "PRICED")
	if !strings.Contains(stmt, "2025-03-04 08:30:00") {
		t.Errorf("created_at is not the lot's own first_date\n%s", stmt)
	}
	// The same date the lot itself carries — the two must not disagree about when the box
	// turned up.
	slotSQL, _, _ := buildLotsAndMoves(t, receiveInput())
	if !strings.Contains(statementFor(slotSQL, "PRICED"), "2025-03-04 08:30:00") {
		t.Errorf("the lot and its movement disagree about the arrival date\n%s",
			statementFor(slotSQL, "PRICED"))
	}
}

// Re-running the generated script must not receive the same lot twice. The guard has to
// key on the reason as well: a withdrawal from the same lot is also a stock_movement row.
func TestReceiveMovementIsIdempotentPerLotAndReason(t *testing.T) {
	_, moveSQL, _ := buildLotsAndMoves(t, receiveInput())
	stmt := movementFor(t, moveSQL, "PRICED")

	if !strings.Contains(stmt, "NOT EXISTS") {
		t.Fatalf("the movement insert has no re-run guard\n%s", stmt)
	}
	for _, want := range []string{"m.slot_id", "m.reason", "m.clinic_code"} {
		if !strings.Contains(stmt, want) {
			t.Errorf("the re-run guard does not key on %s — without the reason a later "+
				"withdrawal from this lot would be mistaken for this receive\n%s", want, stmt)
		}
	}
}

// cost_lot is the whole lot, cost_per_unit is one unit. Giving both the same number
// understates the value of every multi-unit lot in the clinic.
func TestLotCostIsUnitCostTimesQuantity(t *testing.T) {
	slotSQL, _, rep := buildLotsAndMoves(t, receiveInput())
	stmt := statementFor(slotSQL, "PRICED")

	if !strings.Contains(stmt, "(4999::numeric * 52::numeric)") {
		t.Errorf("cost_lot is not unit cost × quantity — V4's own GuidedReceive writes "+
			"CostLot: UnitCost * Qty beside CostPerUnit: UnitCost\n%s", stmt)
	}
	// The multiplication belongs to Postgres: numeric × numeric is exact, and no money
	// value in this package is allowed through a float64.
	if strings.Contains(stmt, "259948") {
		t.Errorf("the product was computed in Go; money must reach the database as exact "+
			"numerics, not as a float result\n%s", stmt)
	}
	if !hasText(rep.Notes, "cost_lot") {
		t.Errorf("the change to lot value was not reported: %v", rep.Notes)
	}
}

// A lot with no cost keeps rendering the bare literal 0, so a diff between two generated
// files shows only the lots whose value actually changed.
func TestZeroCostLotStillRendersZero(t *testing.T) {
	slotSQL, _, _ := buildLotsAndMoves(t, receiveInput())
	stmt := statementFor(slotSQL, "FREEBIE")
	if strings.Contains(stmt, "::numeric *") {
		t.Errorf("a zero-cost lot should render 0, not a multiplication\n%s", stmt)
	}
}

// movementFor returns the stock_movement statement that mentions itemCode.
func movementFor(t *testing.T, moveSQL, itemCode string) string {
	t.Helper()
	for _, stmt := range strings.Split(moveSQL, ";") {
		if strings.Contains(stmt, "INSERT INTO stock_movement") && strings.Contains(stmt, "'"+itemCode+"'") {
			return stmt
		}
	}
	t.Fatalf("no stock_movement statement mentions %s in:\n%s", itemCode, moveSQL)
	return ""
}

// items.min_alert is the low-stock threshold, and the import must not invent one.
//
// The column defaults to 5 (migration 0068). Both of V4's own intake doors send 0 instead —
// Stock.CreateItem passes the form's value, 0 when untouched, and GuidedReceive hardcodes
// pgkit.Numeric(0). Relying on the default meant every imported SKU arrived with a reorder
// level nobody chose, so a freshly migrated clinic opened its stock page to low-stock
// warnings manufactured by the migration. The export carries no reorder data at all.
func TestImportedItemDoesNotInventAReorderThreshold(t *testing.T) {
	mc, err := parseMongoCatalog(lotsWithCostAndReceiveDate())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	in := receiveInput()
	itemsSQL, _, _, _, err := buildItems(mc, in, NewReport(in.ClinicCode))
	if err != nil {
		t.Fatalf("buildItems: %v", err)
	}

	if !strings.Contains(itemsSQL, "min_alert") {
		t.Fatalf("the items INSERT does not name min_alert, so the column default of 5 "+
			"applies and every imported SKU gets a reorder level the clinic never set\n%s", itemsSQL)
	}

	// Written on the way IN only. The UPDATE half of the upsert must leave it alone, or a
	// re-import resets a threshold a human has since chosen — the same rule `active` follows.
	insertPart := itemsSQL
	if i := strings.Index(itemsSQL, "UPDATE items SET"); i >= 0 {
		insertPart = itemsSQL[:i]
	} else {
		t.Fatal("the items upsert no longer has an UPDATE half; this test reads the two separately")
	}
	if !strings.Contains(insertPart, "min_alert") {
		t.Errorf("min_alert is not in the INSERT half\n%s", insertPart)
	}
	for _, stmt := range strings.Split(itemsSQL, ";") {
		if strings.Contains(stmt, "UPDATE items SET") && strings.Contains(stmt, "min_alert") {
			t.Errorf("min_alert is in the UPDATE half — a re-import would overwrite a "+
				"threshold set by hand in V4\n%s", stmt)
		}
	}
}

// End-to-end: the receive ledger has to reach the ARTIFACT, not just the builder.
//
// A new entity that is not in `order` is built, counted, reported — and then silently
// dropped by BuildScript, which skips anything with no statements and never mentions the
// ones it does not know. That failure looks exactly like success: the report says the rows
// were generated and the .sql file does not contain them.
func TestReceiveLedgerReachesTheGeneratedScript(t *testing.T) {
	in := receiveInput()
	in.Groups = []string{"catalog", "stock"}

	script, rep, err := BuildScript(in)
	if err != nil {
		t.Fatalf("BuildScript: %v", err)
	}

	if !strings.Contains(script, "INSERT INTO stock_movement") {
		t.Fatalf("the assembled script has no stock_movement statements — the entity is " +
			"built and reported but never emitted, which reads as success everywhere")
	}
	// Its own labelled section, so a reviewer reading the file top-down can see it.
	if !strings.Contains(script, "── "+EntStockMovement+" ") {
		t.Errorf("the script has no %s section header", EntStockMovement)
	}
	// And it must come AFTER the lots, because it resolves each lot's id from slot_stock.
	iLot := strings.Index(script, "INSERT INTO slot_stock")
	iMove := strings.Index(script, "INSERT INTO stock_movement")
	if iLot < 0 {
		t.Fatal("no slot_stock statements in the script")
	}
	if iMove < iLot {
		t.Error("stock_movement is emitted before slot_stock — the lot lookup would find " +
			"nothing and the receive rows would silently write zero rows")
	}
	// The planner needs to know slot_stock grew before the per-lot lookups start.
	if !strings.Contains(script, "ANALYZE slot_stock;") {
		t.Error("slot_stock is not analysed before stock_movement resolves every lot by " +
			"mongo_id — inside one transaction nothing else refreshes the statistics")
	}

	if _, ok := rep.Entities[EntStockMovement]; !ok {
		t.Errorf("the report does not carry a %s entity", EntStockMovement)
	}
}
