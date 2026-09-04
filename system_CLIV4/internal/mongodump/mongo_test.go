package mongodump

import (
	"strings"
	"testing"
	"time"
)

// The shapes a real dbmongo/*.js carries, in one file: shell constructors, canonical
// Extended JSON, Thai text, an insertMany, a getCollection accessor, comments, and
// statements that are not inserts at all.
const sample = `
// exported 2026-08-26
use drease_old;

db.patient.insert({
    "_id" : ObjectId("66b0f1c2a1b2c3d4e5f60718"),
    "HN" : "016900001",
    "Firstname" : "สมชาย",
    "Lastname" : "ใจดี",
    "note" : "แพ้ยา // ไม่ใช่คอมเมนต์",
    "Tel" : null,
    "email" : "",
    "Birthday" : ISODate("1988-03-14T00:00:00.000Z"),
    "balance" : NumberDecimal("1250.75"),
    "visits" : NumberLong(42),
    "active" : true,
    "address" : { "zip" : "10110", "province" : "กรุงเทพฯ" },
    "tags" : [ "vip", "recall" ]
});

/* a block comment ) with a paren in it */
db.patient.createIndex({ "HN" : 1 }, { unique : true });

db.getCollection("patient").insertMany([
    { "_id" : { "$oid" : "66b0f1c2a1b2c3d4e5f60719" }, "HN" : "016900002",
      "Birthday" : { "$date" : 1700000000000 }, "score" : { "$numberLong" : "9007199254740993" } },
    { "_id" : ObjectId("66b0f1c2a1b2c3d4e5f6071a"), "HN" : "016900003", "score" : 3.5 }
]);

db['stock'].save({ "code" : "MED-1", "qty" : NumberInt(7), "cost" : NumberDecimal("0.10") });
`

func TestParseCollectionsAndCounts(t *testing.T) {
	cols, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cols) != 2 {
		names := make([]string, len(cols))
		for i, c := range cols {
			names[i] = c.Name
		}
		t.Fatalf("want 2 collections, got %d (%v)", len(cols), names)
	}

	// patient is first-seen and must have merged the insert with the insertMany.
	if cols[0].Name != "patient" {
		t.Fatalf("first collection = %q, want patient", cols[0].Name)
	}
	if got := len(cols[0].Docs); got != 3 {
		t.Fatalf("patient docs = %d, want 3", got)
	}
	if got := cols[0].Statements; got != 2 {
		t.Fatalf("patient statements = %d, want 2 (insert + insertMany)", got)
	}
	if cols[1].Name != "stock" || len(cols[1].Docs) != 1 {
		t.Fatalf("second collection = %q with %d docs, want stock with 1", cols[1].Name, len(cols[1].Docs))
	}
}

func TestParseTypedValues(t *testing.T) {
	cols, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	d := cols[0].Docs[0]

	if got, ok := d["_id"].(ObjectID); !ok || string(got) != "66b0f1c2a1b2c3d4e5f60718" {
		t.Errorf("_id = %#v, want ObjectID(66b0…0718)", d["_id"])
	}
	// Thai text must survive byte-for-byte.
	if got := d["Firstname"]; got != "สมชาย" {
		t.Errorf("Firstname = %#v, want สมชาย", got)
	}
	// A `//` inside a string literal is data, not the start of a comment.
	if got := d["note"]; got != "แพ้ยา // ไม่ใช่คอมเมนต์" {
		t.Errorf("note = %#v — a // inside a literal was treated as a comment", got)
	}
	// NULL and "" must stay distinguishable: this is the whole reason the package
	// keeps typed values instead of flattening to strings.
	if v, ok := d["Tel"]; !ok || v != nil {
		t.Errorf("Tel = %#v, want an explicit nil", d["Tel"])
	}
	if got := d["email"]; got != "" {
		t.Errorf("email = %#v, want the empty string", got)
	}
	if _, absent := d["nosuchfield"]; absent {
		t.Errorf("a field that is not in the document must not be present in the map")
	}

	want := time.Date(1988, 3, 14, 0, 0, 0, 0, time.UTC)
	got, ok := d["Birthday"].(time.Time)
	if !ok || !got.Equal(want) {
		t.Errorf("Birthday = %#v, want %v", d["Birthday"], want)
	}

	// Money must stay text. A float64 cannot hold 1250.75 exactly and 0.10 not at all.
	if got, ok := d["balance"].(Decimal); !ok || string(got) != "1250.75" {
		t.Errorf("balance = %#v, want Decimal(1250.75)", d["balance"])
	}
	if got, ok := d["visits"].(int64); !ok || got != 42 {
		t.Errorf("visits = %#v, want int64(42)", d["visits"])
	}
	if got, ok := d["active"].(bool); !ok || !got {
		t.Errorf("active = %#v, want true", d["active"])
	}

	addr, ok := d["address"].(Doc)
	if !ok {
		t.Fatalf("address = %#v, want a nested Doc", d["address"])
	}
	if addr["zip"] != "10110" {
		t.Errorf("address.zip = %#v", addr["zip"])
	}
	tags, ok := d["tags"].([]any)
	if !ok || len(tags) != 2 || tags[0] != "vip" {
		t.Errorf("tags = %#v", d["tags"])
	}
}

func TestParseCanonicalExtendedJSON(t *testing.T) {
	cols, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	d := cols[0].Docs[1]

	if got, ok := d["_id"].(ObjectID); !ok || string(got) != "66b0f1c2a1b2c3d4e5f60719" {
		t.Errorf(`{"$oid": …} did not collapse: %#v`, d["_id"])
	}
	if got, ok := d["Birthday"].(time.Time); !ok || got.UnixMilli() != 1700000000000 {
		t.Errorf(`{"$date": ms} did not collapse: %#v`, d["Birthday"])
	}
	// 9007199254740993 is the classic float64 casualty: as a double it becomes
	// …992. It must come back as int64 with every digit intact.
	if got, ok := d["score"].(int64); !ok || got != 9007199254740993 {
		t.Errorf("score = %#v, want int64(9007199254740993) exactly", d["score"])
	}
}

func TestParseSkipsNonInsertStatements(t *testing.T) {
	// createIndex carries a `)` inside a block comment above it and must not
	// contribute a document or derail the scan of what follows.
	cols, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, c := range cols {
		for _, d := range c.Docs {
			if _, isIndex := d["unique"]; isIndex {
				t.Fatalf("createIndex options leaked in as a document: %#v", d)
			}
		}
	}
}

func TestParseIgnoresFilesWithNoInserts(t *testing.T) {
	cols, err := Parse([]byte("use x;\ndb.foo.drop();\nprint('done');\n"))
	if err != nil {
		t.Fatalf("a dump with no inserts must not be an error: %v", err)
	}
	if len(cols) != 0 {
		t.Fatalf("want no collections, got %d", len(cols))
	}
}

func TestParseMalformedInsertReportsLine(t *testing.T) {
	src := "db.a.insert({ x : 1 });\ndb.a.insert({ y : });\n"
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("a malformed insert must be an error — silently dropping rows loses data")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error should name the line, got: %v", err)
	}
}

func TestParseRejectsUnimportableValues(t *testing.T) {
	for _, src := range []string{
		`db.a.insert({ x : NaN });`,
		`db.a.insert({ x : Infinity });`,
		`db.a.insert({ x : new Date() });`,
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("want an error for %s", src)
		}
	}
}

func TestFieldsSummarisesShapeAndMixedTypes(t *testing.T) {
	cols, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	byPath := map[string]Field{}
	for _, f := range cols[0].Fields() {
		byPath[f.Path] = f
	}

	hn, ok := byPath["HN"]
	if !ok {
		t.Fatalf("HN missing from Fields(): %#v", byPath)
	}
	if hn.Count != 3 {
		t.Errorf("HN count = %d, want 3", hn.Count)
	}
	if len(hn.Kinds) != 1 || hn.Kinds[0] != "string" {
		t.Errorf("HN kinds = %v, want [string]", hn.Kinds)
	}

	// score is int64 in one doc and float64 in another — a mixed-type legacy field,
	// which is exactly what this report exists to surface.
	score, ok := byPath["score"]
	if !ok {
		t.Fatalf("score missing from Fields()")
	}
	if len(score.Kinds) != 2 {
		t.Errorf("score kinds = %v, want two kinds (int + double)", score.Kinds)
	}

	// Explicit nulls are counted apart from absence.
	tel, ok := byPath["Tel"]
	if !ok || tel.Nulls != 1 || tel.Count != 1 {
		t.Errorf("Tel = %#v, want count 1 / nulls 1", tel)
	}

	if _, ok := byPath["address.zip"]; !ok {
		t.Errorf("nested path address.zip missing: %#v", byPath)
	}
	if _, ok := byPath["tags[]"]; !ok {
		t.Errorf("array path tags[] missing: %#v", byPath)
	}
}

func TestDocGetAndText(t *testing.T) {
	cols, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	d := cols[0].Docs[0]

	if got := d.Text("address.province"); got != "กรุงเทพฯ" {
		t.Errorf("Text(address.province) = %q", got)
	}
	if got := d.Text("Tel"); got != "" {
		t.Errorf("Text of an explicit null = %q, want empty", got)
	}
	if _, found := d.Get("Tel"); !found {
		t.Error("Get must report an explicit null as FOUND — absent and null are different facts")
	}
	if _, found := d.Get("nope.deeper"); found {
		t.Error("Get of an absent path must report found=false")
	}
	if got := d.Text("balance"); got != "1250.75" {
		t.Errorf("Text(balance) = %q, want the exact decimal text", got)
	}
}
