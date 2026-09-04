package legacydump

import (
	"sort"
	"strings"
)

// TableInfo is what one pass over a dump can say about one table without deciding
// anything about where it should end up in V4.
type TableInfo struct {
	Name string `json:"name"`
	// Columns is the column list of the first INSERT for this table, in dump order.
	// Empty when the table was only declared (CREATE TABLE) and never populated.
	Columns []string `json:"columns"`
	// Rows is how many tuples the INSERT statements carry in total.
	Rows int `json:"rows"`
	// Statements is how many INSERT statements contributed, which distinguishes one
	// extended insert of 5000 tuples from 5000 single-row inserts.
	Statements int `json:"statements"`
	// Declared is true when a CREATE TABLE for this name appears in the file. A table
	// that is declared with Rows == 0 is an empty table, which is a different fact
	// from a table the dump never mentions at all.
	Declared bool `json:"declared"`
}

// Tables lists every table a dump touches, so a caller can show an operator what is
// in a file before asking them to map anything.
//
// Parse already reads one named table; this answers the question that comes first —
// "what is in here?" — which the importer previously could not ask, so the operator
// had to know the answer before uploading.
//
// It counts tuples with the same scanner Parse uses rather than counting '(' or
// commas: a legacy note containing "), (" would otherwise inflate the count, and a
// row count that is quietly wrong is worse than none.
func Tables(sql string) []TableInfo {
	info := map[string]*TableInfo{}
	get := func(name string) *TableInfo {
		t, ok := info[name]
		if !ok {
			t = &TableInfo{Name: name}
			info[name] = t
		}
		return t
	}

	// CREATE TABLE `x` — records existence, including for tables with no rows.
	for _, name := range identsAfter(sql, "CREATE TABLE ") {
		get(name).Declared = true
	}

	// INSERT INTO `x` (cols) VALUES (…),(…);
	const needle = "INSERT INTO `"
	rest := sql
	for {
		i := indexFoldASCII(rest, needle)
		if i < 0 {
			break
		}
		rest = rest[i+len(needle):]
		end := strings.IndexByte(rest, '`')
		if end < 0 {
			break
		}
		name := rest[:end]
		rest = rest[end+1:]

		t := get(name)
		t.Statements++

		open := strings.IndexByte(rest, '(')
		valuesAt := indexFoldASCII(rest, ") VALUES")
		if open < 0 || valuesAt < 0 || valuesAt < open {
			// A `INSERT INTO t VALUES (...)` with no column list is legal MySQL. The
			// statement is still counted; its columns are simply unknown from here.
			continue
		}
		cols := splitIdents(rest[open+1 : valuesAt])
		if len(t.Columns) == 0 {
			t.Columns = cols
		}

		// Count tuples with the real scanner, then resume after this statement.
		p := &scanner{s: rest[valuesAt+len(") VALUES"):]}
		for {
			p.skipSpaceAndCommas()
			if p.eof() || p.peek() == ';' {
				break
			}
			vals, err := p.tuple()
			if err != nil {
				// A malformed statement stops the count for this statement only. The
				// caller finds out for real when it Parses the table it selected;
				// discovery must not fail the whole listing.
				break
			}
			if len(vals) > 0 {
				t.Rows++
			}
		}
		rest = rest[valuesAt+len(") VALUES")+p.i:]
	}

	out := make([]TableInfo, 0, len(info))
	for _, t := range info {
		out = append(out, *t)
	}
	// Deterministic order: a listing that reshuffles between two identical uploads
	// is one an operator cannot compare against the previous run.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// identsAfter finds every `backticked` identifier that follows a keyword, matching
// the keyword case-insensitively (mysqldump writes upper case; hand-edited files and
// some GUI exports do not).
func identsAfter(sql, keyword string) []string {
	var out []string
	rest := sql
	for {
		i := indexFoldASCII(rest, keyword)
		if i < 0 {
			return out
		}
		rest = rest[i+len(keyword):]

		// Skip anything between the keyword and the identifier (IF NOT EXISTS).
		j := strings.IndexByte(rest, '`')
		if j < 0 {
			return out
		}
		// Refuse to jump over a statement boundary: without this, a CREATE TABLE with
		// no backticked name would adopt the next statement's identifier.
		if k := strings.IndexByte(rest, ';'); k >= 0 && k < j {
			rest = rest[k+1:]
			continue
		}
		rest = rest[j+1:]
		end := strings.IndexByte(rest, '`')
		if end < 0 {
			return out
		}
		out = append(out, rest[:end])
		rest = rest[end+1:]
	}
}

// indexFoldASCII is strings.Index with ASCII-case-insensitive matching. It is not
// strings.EqualFold-based on purpose: SQL keywords are ASCII, and Unicode folding
// would make Thai text in the dump a candidate for a keyword match.
func indexFoldASCII(s, substr string) int {
	if substr == "" {
		return 0
	}
	n := len(substr)
	first := lowerASCII(substr[0])
	for i := 0; i+n <= len(s); i++ {
		if lowerASCII(s[i]) != first {
			continue
		}
		match := true
		for j := 1; j < n; j++ {
			if lowerASCII(s[i+j]) != lowerASCII(substr[j]) {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func lowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}
