package legacyimport

// items.source_key must be left to the column's own default, never written by the import.
//
// WHAT WENT WRONG
//
// The builder used to write 'mongo:items:' + the V2 stock code. Migration 0115 declares the
// column as
//
//	source_key varchar(32) NOT NULL DEFAULT md5(random()::text || clock_timestamp()::text)
//	CREATE UNIQUE INDEX uniq_items_source_key ON items (source_key)
//
// which is a globally unique opaque surrogate — unique across items, courses and listheal
// together, because a doctor-fee rule binds a bill line to a catalog row through it and those
// three tables reuse the same integer ids.
//
// 'ST000001' is the first SKU in every V2 database. So the first tenant to import claimed
// mongo:items:ST000001 for the whole database and the next one died on it:
//
//	ERROR: duplicate key value violates unique constraint "uniq_items_source_key"
//	DETAIL: Key (source_key)=(mongo:items:ST000001) already exists.
//
// Observed on drease_temporary: agafar-clinic imported 398 items at 03:33, and doctortony's
// import failed on the very first one at 11:07. The index was right; the writer was wrong.
//
// WHY A TEST AND NOT JUST THE FIX
//
// Putting the column back looks harmless and even helpful — it is a readable provenance
// string, and it works perfectly on an empty database and on the first tenant. It only fails
// on the SECOND clinic, which is the one case nobody re-tests.

import (
	"strings"
	"testing"
)

func TestItemsDoesNotWriteSourceKeySoASecondClinicCanImport(t *testing.T) {
	mc, err := parseMongoCatalog([]byte(drugItemsDump))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	in := Input{ClinicCode: "CTEST01", BranchMap: map[string]string{"01": "BR00001"}}

	itemsSQL, _, _, _, err := buildItems(mc, in, NewReport(in.ClinicCode))
	if err != nil {
		t.Fatalf("buildItems: %v", err)
	}
	if !strings.Contains(itemsSQL, "INSERT INTO items") {
		t.Fatal("no items INSERT was produced — the fixture is wrong, not the writer")
	}

	if strings.Contains(itemsSQL, "source_key") {
		t.Error("the items INSERT names source_key. The column is globally unique and " +
			"defaults to a random md5; writing a V2-derived value into it means only one " +
			"clinic in the database can ever hold a given V2 stock code")
	}

	// The specific value that caused it, in case a future change reintroduces the idea in a
	// different shape (a prefix change, say) while still deriving from the V2 code.
	if strings.Contains(itemsSQL, "mongo:items:") {
		t.Error("the items INSERT still carries a 'mongo:items:' key derived from the V2 code")
	}

	// Provenance must NOT have been lost in the process. items.code keeps the V2 code
	// verbatim, and that is what makes dropping source_key acceptable rather than a trade.
	// The codes are the fixture's own, not the ST000001 from the production failure.
	for _, code := range []string{"'ST000397'", "'ST000005'", "'ST000026'"} {
		if !strings.Contains(itemsSQL, code) {
			t.Errorf("items.code %s is missing — provenance is gone, and dropping source_key "+
				"was only safe because code still held the V2 code", code)
		}
	}

	// The row's real identity, which every branch of the write has to keep targeting.
	//
	// This used to assert the literal `ON CONFLICT (clinic_code, code)`. The upsert is now
	// NOT EXISTS + UPDATE (see TestItemsWriteIsIdempotentForSwitchedOffRows for why), so the
	// syntax changed while the property did not: the row is found by clinic AND code, and
	// never by source_key. If identity ever moves to source_key the cross-clinic bug comes
	// back as a silent UPDATE to another clinic's row, which is worse than the error was.
	if !strings.Contains(itemsSQL, "WHERE NOT EXISTS (SELECT 1 FROM items WHERE clinic_code = 'CTEST01' AND code =") {
		t.Errorf("the items INSERT is not guarded by a (clinic_code, code) existence check:\n%s", itemsSQL)
	}
	if !strings.Contains(itemsSQL, "UPDATE items SET") ||
		!strings.Contains(itemsSQL, "WHERE clinic_code = 'CTEST01' AND code =") {
		t.Errorf("the items UPDATE is not scoped by clinic_code AND code:\n%s", itemsSQL)
	}
	for _, forbidden := range []string{"ON CONFLICT (source_key)", "ON CONFLICT (items.source_key)"} {
		if strings.Contains(itemsSQL, forbidden) {
			t.Errorf("identity moved to source_key (%q) — a re-import would silently update "+
				"another clinic's row", forbidden)
		}
	}
}

// The items write has to be idempotent for rows V2 has SWITCHED OFF, not just for live ones.
//
// # WHAT WENT WRONG
//
// uniq_items_clinic_code is PARTIAL — `ON items (clinic_code, code) WHERE active` — so it is
// the only conflict target available and an INACTIVE row never violates it. The write used
// `ON CONFLICT (clinic_code, code) WHERE active DO UPDATE`, which means every re-run inserted
// another copy of every switched-off SKU instead of refreshing it.
//
// Measured at C2EC1E3: 645 real codes, 1,901 rows, 356 codes holding 4–6 rows each, all
// inactive. Those duplicates are what make `code` ambiguous for anything that resolves a SKU
// by code — including the source_id subquery in this same function.
//
// # WHY A TEST
//
// `ON CONFLICT ... WHERE active` reads as a careful, deliberate clause. It is the obvious
// thing to restore if someone finds NOT EXISTS + UPDATE clumsy, and it works perfectly on
// the first import of any tenant. It only breaks on the SECOND run, with no error.
func TestItemsWriteIsIdempotentForSwitchedOffRows(t *testing.T) {
	mc, err := parseMongoCatalog([]byte(drugItemsDump))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	in := Input{ClinicCode: "CTEST01", BranchMap: map[string]string{"01": "BR00001"}}
	itemsSQL, _, _, _, err := buildItems(mc, in, NewReport(in.ClinicCode))
	if err != nil {
		t.Fatalf("buildItems: %v", err)
	}

	if strings.Contains(itemsSQL, "WHERE active DO UPDATE") ||
		strings.Contains(itemsSQL, "ON CONFLICT (clinic_code, code) WHERE active") {
		t.Errorf("the write is back on the partial index as its conflict target. An inactive "+
			"row cannot violate it, so a re-run duplicates every switched-off SKU:\n%s", itemsSQL)
	}
	// The pair that makes it idempotent regardless of `active`.
	if !strings.Contains(itemsSQL, "WHERE NOT EXISTS") || !strings.Contains(itemsSQL, "UPDATE items SET") {
		t.Errorf("expected INSERT ... WHERE NOT EXISTS followed by UPDATE:\n%s", itemsSQL)
	}
	// active and source_key must stay out of the UPDATE: active is the clinic's own switch
	// (catalog_item.status is derived from it) and source_key is the immutable key a
	// doctor-fee rule may already point at.
	// One statement only. Slicing to the end of the string would sweep in the INSERT of the
	// NEXT item, whose column list legitimately names `active`.
	updatePart := itemsSQL[strings.Index(itemsSQL, "UPDATE items SET"):]
	updatePart = updatePart[:strings.Index(updatePart, ";")]
	if strings.Contains(updatePart, "active") {
		t.Error("the items UPDATE touches `active` — a re-import would flip what somebody " +
			"switched on in V4, and catalog_item.status follows it")
	}
	if strings.Contains(updatePart, "source_key") {
		t.Error("the items UPDATE touches `source_key` — re-minting it breaks any doctor-fee " +
			"rule bound to the old value")
	}
}
