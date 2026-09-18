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

	itemsSQL, _, _, err := buildItems(mc, in, NewReport(in.ClinicCode))
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

	// The row's real identity, which the ON CONFLICT has to keep targeting. If this ever
	// becomes ON CONFLICT (source_key) the cross-clinic bug returns as an UPDATE to another
	// clinic's row, which is worse than the error: it would write silently.
	if !strings.Contains(itemsSQL, "ON CONFLICT (clinic_code, code)") {
		t.Error("the items upsert no longer keys on (clinic_code, code)")
	}
}
