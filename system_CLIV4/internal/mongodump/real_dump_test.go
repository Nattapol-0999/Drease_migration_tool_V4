package mongodump

import (
	"os"
	"testing"
)

// A real clinic export is the only thing that proves this parser works: it is 26 MB of
// mongosh output using Int32/Decimal128/ObjectId/ISODate tens of thousands of times,
// and a single unrecognised constructor fails the whole file.
//
// Set MRIS_DUMP to run it. Skipped when unset so the suite still passes on a machine
// that does not have the export.
func TestParseRealClinicExport(t *testing.T) {
	path := os.Getenv("MRIS_DUMP")
	if path == "" {
		t.Skip("set MRIS_DUMP to a real mongosh export to run this")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	cols, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	byName := map[string]Collection{}
	for _, c := range cols {
		byName[c.Name] = c
	}

	// The collections this import depends on, with the counts observed in the export.
	for _, want := range []struct {
		name string
		docs int
	}{
		{"items", 90},
		{"listheals", 184},
		{"course_items", 96},
		{"stocks", 2},
		{"item_types", 9},
		{"item_units", 16},
		{"category", 20},
		{"kind", 3},
		{"listheal_medicine", 36},
	} {
		c, ok := byName[want.name]
		if !ok {
			t.Errorf("collection %q missing from the parse", want.name)
			continue
		}
		if len(c.Docs) != want.docs {
			t.Errorf("%s: parsed %d docs, want %d", want.name, len(c.Docs), want.docs)
		}
	}

	// Money must arrive as Decimal (text), never float64. course_items.price is
	// Decimal128 in the source.
	course, ok := byName["course_items"]
	if !ok || len(course.Docs) == 0 {
		t.Fatal("course_items missing")
	}
	if _, isDecimal := course.Docs[0]["price"].(Decimal); !isDecimal {
		t.Errorf("course_items.price = %T, want Decimal (money must not become a float)", course.Docs[0]["price"])
	}

	// Int32 must arrive as int64, not a string or float.
	items, ok := byName["items"]
	if !ok || len(items.Docs) == 0 {
		t.Fatal("items missing")
	}
	if _, isInt := items.Docs[0]["quantity_total"].(int64); !isInt {
		t.Errorf("items.quantity_total = %T, want int64", items.Docs[0]["quantity_total"])
	}
	if _, isOID := items.Docs[0]["_id"].(ObjectID); !isOID {
		t.Errorf("items._id = %T, want ObjectID", items.Docs[0]["_id"])
	}
}
