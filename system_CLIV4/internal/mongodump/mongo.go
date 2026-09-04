// Package mongodump reads the .js files exported from the legacy Node/MongoDB
// service — the `dbmongo/<name>.js` half of a tenant's export — so the import
// tooling has one parser for them instead of a regex per call site.
//
// It is the Mongo sibling of internal/legacydump (which reads mysqldump .sql) and
// follows the same two rules for the same reasons:
//
//   - a hand-rolled scanner, not a regex. These files contain Thai text, commas,
//     braces and quotes inside string literals, `//` sequences inside URLs, and
//     nested documents — every one of which breaks naive splitting.
//   - NULL and "" stay distinguishable. A Mongo field that is absent, one that is
//     explicitly null, and one that is the empty string are three different facts,
//     and collapsing them silently rewrites the source data.
//
// # WHAT IT ACCEPTS
//
// The shapes mongoexport / mongosh / Studio3T actually emit:
//
//	db.patient.insert({ ... });
//	db.patient.insertOne({ ... })
//	db.patient.insertMany([{ ... }, { ... }]);
//	db.getCollection("patient").insert({ ... });
//	db['patient'].save({ ... });
//
// plus both spellings of Extended JSON — the shell constructors
// (`ObjectId("…")`, `ISODate("…")`, `NumberDecimal("…")`, `new Date(…)`) and the
// canonical `{"$oid": "…"}` / `{"$date": …}` / `{"$numberDecimal": "…"}` form.
//
// Anything else at statement level is SKIPPED rather than refused: a real dump is
// full of `use olddb`, `db.patient.createIndex(...)`, `print(...)` and comments,
// and failing the whole file because of a line nobody wanted would make the parser
// useless. A MALFORMED insert is a different matter and returns an error carrying
// the line number, because that one means data would go missing.
//
// # WHAT IT DELIBERATELY DOES NOT DO
//
// It does not map anything onto V4 tables. Which collection becomes which table is
// a decision with money and patient safety in it (see IMPORT_DATA.md §6), so this
// package stops at "here is what the file contains, faithfully typed".
package mongodump

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ObjectID is a Mongo _id, kept as its 24-hex text. It is NOT a plain string so a
// caller mapping to V4 can tell "this was an ObjectId" from "this was a string that
// happens to look like one" — the `mongo_id varchar(24)` columns in V4 want the
// former and nothing else.
type ObjectID string

// Decimal is a NumberDecimal, kept as TEXT and never parsed into a float. Every
// NumberDecimal in these dumps is money, and float64 cannot hold ฿0.1 exactly. The
// V4 side is numeric(p,s), which accepts this string directly.
type Decimal string

// Timestamp is the internal BSON timestamp (replication oplog), not a date.
type Timestamp struct {
	T uint32 `json:"t"`
	I uint32 `json:"i"`
}

// Binary is BinData / {"$binary": …} left base64-encoded.
type Binary struct {
	SubType int    `json:"sub_type"`
	Base64  string `json:"base64"`
}

// Regex is a /pattern/flags literal or {"$regularExpression": …}.
type Regex struct {
	Pattern string `json:"pattern"`
	Options string `json:"options"`
}

// MinKey and MaxKey are the BSON sentinels; they carry no value of their own.
type MinKey struct{}

// MaxKey is the upper BSON sentinel. See MinKey.
type MaxKey struct{}

// Doc is one parsed document. Values are: nil, bool, string, int64, float64,
// time.Time, ObjectID, Decimal, Timestamp, Binary, Regex, MinKey, MaxKey, Doc, or
// []any of those.
type Doc map[string]any

// Collection is every document one file inserted into one collection, in file order.
type Collection struct {
	Name string `json:"name"`
	Docs []Doc  `json:"-"`
	// Statements is how many insert calls contributed, which is how an operator
	// tells one insertMany of 5000 from 5000 separate insert lines.
	Statements int `json:"statements"`
}

// Field is one dotted path observed across a collection's documents.
type Field struct {
	// Path uses dots for nesting and a trailing [] for "inside an array", e.g.
	// "address.zip" or "items[].code".
	Path string `json:"path"`
	// Count is how many documents carried this path at all (absent ≠ null).
	Count int `json:"count"`
	// Nulls is how many of those carried it as an explicit null.
	Nulls int `json:"nulls"`
	// Kinds are the value kinds observed, sorted, e.g. ["string","null"]. More than
	// one kind on a path is the single most useful warning this package produces:
	// it is what a mixed-type legacy column looks like from the outside.
	Kinds []string `json:"kinds"`
	// Sample is the first non-empty value rendered as text, clipped, so the operator
	// can recognise the field without opening the dump.
	Sample string `json:"sample,omitempty"`
}

// insertMethods are the call names that carry documents. Anything else on a
// collection (createIndex, drop, find, updateOne, …) is skipped.
var insertMethods = map[string]bool{
	"insert":     true,
	"insertOne":  true,
	"insertMany": true,
	"save":       true,
}

// Parse scans one .js dump and returns its collections in first-seen order.
//
// Repeated inserts into the same collection are merged into one Collection, which
// is what a dump written as thousands of single-line inserts needs.
func Parse(src []byte) ([]Collection, error) {
	p := &scanner{s: string(src)}

	var order []string
	byName := map[string]*Collection{}

	for {
		p.skipGap()
		if p.eof() {
			break
		}

		// Only an identifier can begin a collection access; everything else is
		// noise we step over one byte at a time.
		if !isIdentStart(p.peek()) {
			p.i++
			continue
		}
		start := p.i
		word := p.ident()
		if word != "db" {
			continue // some other identifier — `use`, `print`, `var`, …
		}

		name, method, ok := p.accessor()
		if !ok {
			// `db` appeared but not as a collection access we understand (e.g.
			// `db.version()`); resume scanning after the token we consumed.
			p.i = start + len(word)
			continue
		}
		if !insertMethods[method] {
			p.skipCall() // e.g. db.patient.createIndex({...})
			continue
		}

		docs, err := p.insertArgs()
		if err != nil {
			return nil, fmt.Errorf("%s: line %d: %w", name, p.line(), err)
		}

		c, seen := byName[name]
		if !seen {
			c = &Collection{Name: name}
			byName[name] = c
			order = append(order, name)
		}
		c.Docs = append(c.Docs, docs...)
		c.Statements++
	}

	out := make([]Collection, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out, nil
}

// Fields summarises a collection's shape: every dotted path, how often it appeared,
// which value kinds it held and one sample. Paths come back in first-seen document
// order, which keeps a dump's own column order intact instead of alphabetising it.
func (c Collection) Fields() []Field {
	type acc struct {
		field Field
		kinds map[string]bool
	}
	var order []string
	seen := map[string]*acc{}

	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		a, ok := seen[prefix]
		if !ok {
			a = &acc{field: Field{Path: prefix}, kinds: map[string]bool{}}
			seen[prefix] = a
			order = append(order, prefix)
		}
		a.field.Count++
		a.kinds[kindOf(v)] = true
		if v == nil {
			a.field.Nulls++
		} else if a.field.Sample == "" {
			if s := sampleOf(v); s != "" {
				a.field.Sample = s
			}
		}

		switch t := v.(type) {
		case Doc:
			for k, child := range t {
				walk(prefix+"."+k, child)
			}
		case []any:
			for _, child := range t {
				walk(prefix+"[]", child)
			}
		}
	}

	for _, d := range c.Docs {
		// Top level is walked directly so the root itself is not a path.
		for k, v := range d {
			walk(k, v)
		}
	}

	out := make([]Field, 0, len(order))
	for _, p := range order {
		a := seen[p]
		kinds := make([]string, 0, len(a.kinds))
		for k := range a.kinds {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		a.field.Kinds = kinds
		out = append(out, a.field)
	}
	// Documents iterate in map order, so first-seen is not stable across runs.
	// Sort by path to make the output deterministic — a report that reshuffles
	// between two identical requests is one nobody trusts.
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// kindOf labels a parsed value for the Kinds summary.
func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case string:
		return "string"
	case int64:
		return "int"
	case float64:
		return "double"
	case ObjectID:
		return "objectId"
	case Decimal:
		return "decimal"
	case time.Time:
		return "date"
	case Timestamp:
		return "timestamp"
	case Binary:
		return "binary"
	case Regex:
		return "regex"
	case MinKey:
		return "minKey"
	case MaxKey:
		return "maxKey"
	case Doc:
		return "object"
	case []any:
		return "array"
	default:
		return fmt.Sprintf("%T", v)
	}
}

const sampleLimit = 80

// sampleOf renders one value as short text for the inspect report. Objects and
// arrays return "" — their children are sampled on their own paths, and printing a
// nested blob here would push the real values off the screen.
func sampleOf(v any) string {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case bool:
		if t {
			s = "true"
		} else {
			s = "false"
		}
	case int64:
		s = fmt.Sprintf("%d", t)
	case float64:
		s = strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", t), "0"), ".")
	case ObjectID:
		s = string(t)
	case Decimal:
		s = string(t)
	case time.Time:
		s = t.UTC().Format(time.RFC3339)
	case Timestamp:
		s = fmt.Sprintf("%d:%d", t.T, t.I)
	case Binary:
		s = fmt.Sprintf("binary(%d bytes b64)", len(t.Base64))
	case Regex:
		s = "/" + t.Pattern + "/" + t.Options
	default:
		return ""
	}
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " "))
	if len(s) > sampleLimit {
		// Clip on a rune boundary: these dumps are full of Thai, and cutting a
		// multi-byte character mid-way produces the mojibake the legacy patient
		// "Age" column is already famous for.
		r := []rune(s)
		if len(r) > sampleLimit {
			r = r[:sampleLimit]
		}
		s = string(r) + "…"
	}
	return s
}

// Get reads a dotted path out of a document. found=false means the path is absent,
// which a caller must be able to tell from a present-but-null value.
func (d Doc) Get(path string) (value any, found bool) {
	var cur any = d
	for _, seg := range strings.Split(path, ".") {
		doc, ok := cur.(Doc)
		if !ok {
			return nil, false
		}
		cur, ok = doc[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Text reads a dotted path as text, the way legacydump.Row.Get does: "" for
// absent, null, or a container. Use Get when absent and null must differ.
func (d Doc) Text(path string) string {
	v, ok := d.Get(path)
	if !ok || v == nil {
		return ""
	}
	switch v.(type) {
	case Doc, []any:
		return ""
	}
	return sampleOfFull(v)
}

// sampleOfFull is sampleOf without the clip, for Text.
func sampleOfFull(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int64:
		return fmt.Sprintf("%d", t)
	case float64:
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", t), "0"), ".")
	case ObjectID:
		return string(t)
	case Decimal:
		return string(t)
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	default:
		return ""
	}
}
