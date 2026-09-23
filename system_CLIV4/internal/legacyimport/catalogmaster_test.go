package legacyimport

import (
	"strings"
	"testing"
)

// Which of a code's `items` documents supplies the catalogue row.
//
// In V2 a code has one document per stock room, each with its own price, and only one of
// them can become catalog_item. The rule (owner, 23 Sep 2026) is that the BRANCH room's
// document wins unconditionally — the branch room is where the stock sits and where staff
// price it. Dump order deliberately does not decide it any more: mongoexport sorts by
// _id, an ObjectId starts with its creation time, and the central room is almost always
// the older one, which is how five SKUs the clinic sells daily arrived priced at 0.
//
// "Unconditionally" is asserted as plainly as the good case, because the owner chose it
// over the softer variants: a branch document that says 0, or says nothing at all, still
// wins. Anyone reading a 0 price and expecting a fallback should find that expectation
// failing here, not in production.

// twoRoomsPerCode: every code exists in the central warehouse and in a branch room, with
// prices chosen so the winner is unambiguous in the SQL.
//
//	CENTRAL-ONLY   only in the warehouse                     → warehouse document wins
//	BRANCH-PRICED  warehouse 0, branch 350                   → 350 (the reported bug)
//	BRANCH-ZERO    warehouse 15900, branch 0                 → 0 (owner accepted this)
//	BRANCH-BLANK   warehouse named, branch has no name       → name falls back to the code
//	UNTICKED-ROOM  warehouse 900, room of a branch not being
//	               imported says 50                          → 900, that room has no say
func twoRoomsPerCode() []byte {
	return []byte(`db.getCollection("items").insert([
  { _id: ObjectId("cc0000000000000000000001"), code: "CENTRAL-ONLY", name1: "ยาคลังกลางเท่านั้น", quantity: "5", quantity_total: "5", cost: "3", price: "120", active: true, stock: ObjectId("5fab83eebb59bca34e513211") },
  { _id: ObjectId("cc0000000000000000000002"), code: "BRANCH-PRICED", name1: "ผ้าคาดผม", quantity: "0", quantity_total: "0", cost: "0", price: "0", active: true, stock: ObjectId("5fab83eebb59bca34e513211") },
  { _id: ObjectId("cc0000000000000000000003"), code: "BRANCH-PRICED", name1: "ผ้าคาดผม", quantity: "42", quantity_total: "42", cost: "0", price: "350", active: true, stock: ObjectId("63608c6c7796354dbae69c55") },
  { _id: ObjectId("cc0000000000000000000004"), code: "BRANCH-ZERO", name1: "Skinvive", quantity: "2", quantity_total: "2", cost: "0", price: "15900", active: true, stock: ObjectId("5fab83eebb59bca34e513211") },
  { _id: ObjectId("cc0000000000000000000005"), code: "BRANCH-ZERO", name1: "Skinvive", quantity: "1", quantity_total: "1", cost: "0", price: "0", active: true, stock: ObjectId("63608c6c7796354dbae69c55") },
  { _id: ObjectId("cc0000000000000000000006"), code: "BRANCH-BLANK", name1: "Dermalogica serum", quantity: "3", quantity_total: "3", cost: "0", price: "990", active: true, stock: ObjectId("5fab83eebb59bca34e513211") },
  { _id: ObjectId("cc0000000000000000000007"), code: "BRANCH-BLANK", name1: "", quantity: "0", quantity_total: "0", cost: "0", price: "0", active: true, stock: ObjectId("63608c6c7796354dbae69c55") },
  { _id: ObjectId("cc0000000000000000000008"), code: "UNTICKED-ROOM", name1: "ยาของอีกบริษัท", quantity: "9", quantity_total: "9", cost: "0", price: "900", active: true, stock: ObjectId("5fab83eebb59bca34e513211") },
  { _id: ObjectId("cc0000000000000000000009"), code: "UNTICKED-ROOM", name1: "ยาของอีกบริษัท", quantity: "4", quantity_total: "4", cost: "0", price: "50", active: true, stock: ObjectId("64128720030d9b36ce99cd57") }
]);
`)
}

// buildCatalogFromTwoRooms hands back the items master and the catalog_item SQL, which is
// where the chosen document shows up.
func buildCatalogFromTwoRooms(t *testing.T) (itemsSQL, catalogSQL string) {
	t.Helper()
	in := Input{
		ClinicCode: "MBMASTER",
		BranchDump: branchRegistryWithStocks(),
		MongoDump:  twoRoomsPerCode(),
		BranchMap:  tickedSeven(),
	}
	mc, err := parseMongoCatalog(in.MongoDump)
	if err != nil {
		t.Fatalf("parse mongo: %v", err)
	}
	itemsSQL, catalogSQL, _, _, err = buildItems(mc, in, NewReport(in.ClinicCode))
	if err != nil {
		t.Fatalf("buildItems: %v", err)
	}
	return itemsSQL, catalogSQL
}

func TestBranchRoomSuppliesThePrice(t *testing.T) {
	itemsSQL, catalogSQL := buildCatalogFromTwoRooms(t)

	// The bug this rule exists for: priced in the branch room, zero in the central one.
	for _, sql := range []string{itemsSQL, catalogSQL} {
		stmt := statementFor(sql, "BRANCH-PRICED")
		if stmt == "" {
			t.Fatal("BRANCH-PRICED has no master row at all")
		}
		if !strings.Contains(stmt, "350") {
			t.Errorf("BRANCH-PRICED did not take the branch room's price 350:\n%s", stmt)
		}
	}
}

func TestOneDocumentMeansOneRowWhicheverRoomItIsIn(t *testing.T) {
	itemsSQL, catalogSQL := buildCatalogFromTwoRooms(t)

	// A code in only the central room must be untouched by the rule — 350 of 817 codes on
	// the real export are exactly this, and 8 of them carry the only price they have.
	for _, sql := range []string{itemsSQL, catalogSQL} {
		stmt := statementFor(sql, "CENTRAL-ONLY")
		if !strings.Contains(stmt, "120") {
			t.Errorf("CENTRAL-ONLY lost its price:\n%s", stmt)
		}
	}

	// Exactly one master row per code, no matter how many documents fed it.
	for _, code := range []string{"BRANCH-PRICED", "BRANCH-ZERO", "CENTRAL-ONLY", "UNTICKED-ROOM"} {
		if got := strings.Count(itemsSQL, "'"+code+"'"); got == 0 {
			t.Errorf("%s never reached the items master", code)
		}
		if got := strings.Count(catalogSQL, "INSERT INTO catalog_item"); got != 5 {
			t.Errorf("catalog_item inserts = %d, want 5 (one per code)", got)
			break
		}
	}
}

func TestBranchRoomWinsEvenWhenItSaysZero(t *testing.T) {
	// The owner accepted this consequence explicitly (option 3ก): Skinvive is 15,900 in the
	// central room and 0 in the branch room, and it arrives at 0. If someone later decides
	// that is wrong, this is the test that has to change with the rule.
	_, catalogSQL := buildCatalogFromTwoRooms(t)

	stmt := statementFor(catalogSQL, "BRANCH-ZERO")
	if stmt == "" {
		t.Fatal("BRANCH-ZERO has no catalog row")
	}
	if strings.Contains(stmt, "15900") {
		t.Errorf("BRANCH-ZERO kept the central room's price — the branch room must win unconditionally:\n%s", stmt)
	}
}

func TestBranchRoomWinsEvenWhenItIsBlank(t *testing.T) {
	// Same choice, different field: the branch document has no name, and the row still comes
	// from it. The name is not left empty though — it falls back to the code, which is what
	// keeps the SKU usable on screen.
	_, catalogSQL := buildCatalogFromTwoRooms(t)

	stmt := statementFor(catalogSQL, "BRANCH-BLANK")
	if stmt == "" {
		t.Fatal("BRANCH-BLANK has no catalog row")
	}
	if strings.Contains(stmt, "Dermalogica serum") {
		t.Errorf("BRANCH-BLANK took the central room's name — the branch room must win:\n%s", stmt)
	}
	if !strings.Contains(stmt, "'BRANCH-BLANK'") {
		t.Errorf("BRANCH-BLANK has neither a name nor its code as a fallback:\n%s", stmt)
	}
}

func TestARoomOfABranchWeAreNotImportingHasNoSay(t *testing.T) {
	// Branch 03 is not in the ticked map: its lots are dropped because they arrive with that
	// branch's own clinic. Letting its document price this clinic's catalogue would price one
	// company's shelf from another's.
	_, catalogSQL := buildCatalogFromTwoRooms(t)

	stmt := statementFor(catalogSQL, "UNTICKED-ROOM")
	if stmt == "" {
		t.Fatal("UNTICKED-ROOM has no catalog row")
	}
	if !strings.Contains(stmt, "900") {
		t.Errorf("UNTICKED-ROOM did not keep the central room's price 900:\n%s", stmt)
	}
	if strings.Contains(stmt, "'50'") || strings.Contains(stmt, ", 50,") {
		t.Errorf("an unimported branch's room set the price:\n%s", stmt)
	}
}

func TestTheChoiceIsStableAcrossRuns(t *testing.T) {
	// Two builds of the same dump must pick the same document. A map iteration leaking into
	// the choice would make the same export produce different catalogues on different days.
	_, first := buildCatalogFromTwoRooms(t)
	for i := 0; i < 5; i++ {
		_, again := buildCatalogFromTwoRooms(t)
		if first != again {
			t.Fatalf("the catalogue SQL changed between two builds of the same dump (run %d)", i+2)
		}
	}
}
