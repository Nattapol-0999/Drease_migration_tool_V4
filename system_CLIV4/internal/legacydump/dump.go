// Package legacydump reads the phpMyAdmin/mysqldump .sql files exported from the
// legacy Dr.Ease V2 MySQL database, so the one-off importers under cmd/ can share
// one parser instead of each carrying its own copy.
//
// It is deliberately a small hand-rolled scanner rather than a regex: the dumps
// contain Thai text, commas and parentheses inside string literals, backslash
// escapes (\' \r \n) and a mix of NULL and ” for "no value" — all of which break
// naive splitting.
package legacydump

import (
	"fmt"
	"strings"
	"unicode"
)

// Row is one parsed INSERT tuple: source column name -> value. A nil pointer is
// SQL NULL; a non-nil pointer to "" is the empty string. Keeping them distinct
// matters because the legacy dumps use BOTH (older rows are NULL, newer ones ”).
type Row map[string]*string

// Get returns the trimmed text of a column, or "" when the column is missing or
// SQL NULL. Use the map directly when NULL and ” must be told apart.
func (r Row) Get(col string) string {
	v, ok := r[col]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(*v)
}

// Parse extracts every tuple of `INSERT INTO <table> (...) VALUES (...)` from a
// dump. It returns the column list of the first INSERT (all statements in one
// dump share it) and the rows in file order.
func Parse(sql, table string) ([]string, []Row, error) {
	var cols []string
	var rows []Row

	// Keyword matching is ASCII-case-insensitive: mysqldump and phpMyAdmin both emit
	// upper case, but hand-edited files and some GUI exports do not, and a statement
	// this parser silently skips is rows that silently go missing. Tables() matches
	// the same way, so the two can never disagree about what a file contains.
	needle := "INSERT INTO `" + table + "`"
	rest := sql
	for {
		i := indexFoldASCII(rest, needle)
		if i < 0 {
			break
		}
		rest = rest[i+len(needle):]

		// column list: ( `a`, `b`, ... ) VALUES
		open := strings.IndexByte(rest, '(')
		closeIdx := indexFoldASCII(rest, ") VALUES")
		if open < 0 || closeIdx < 0 || closeIdx < open {
			return nil, nil, fmt.Errorf("malformed INSERT header for %q", table)
		}
		stmtCols := splitIdents(rest[open+1 : closeIdx])
		if cols == nil {
			cols = stmtCols
		} else if strings.Join(cols, ",") != strings.Join(stmtCols, ",") {
			return nil, nil, fmt.Errorf("INSERT statements disagree on column order")
		}

		// tuples run until the terminating semicolon of this statement.
		p := &scanner{s: rest[closeIdx+len(") VALUES"):]}
		for {
			p.skipSpaceAndCommas()
			if p.eof() || p.peek() == ';' {
				break
			}
			vals, err := p.tuple()
			if err != nil {
				return nil, nil, err
			}
			if len(vals) != len(cols) {
				return nil, nil, fmt.Errorf("tuple has %d values, header has %d columns", len(vals), len(cols))
			}
			r := make(Row, len(cols))
			for k, c := range cols {
				r[c] = vals[k]
			}
			rows = append(rows, r)
		}
		rest = rest[closeIdx+len(") VALUES")+p.i:]
	}
	if cols == nil {
		return nil, nil, fmt.Errorf("no INSERT INTO `%s` found", table)
	}
	return cols, rows, nil
}

// splitIdents turns "`HN`, `Braces_ID`" into ["HN","Braces_ID"].
func splitIdents(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.Trim(strings.TrimSpace(p), "`"))
	}
	return out
}

type scanner struct {
	s string
	i int
}

func (p *scanner) eof() bool  { return p.i >= len(p.s) }
func (p *scanner) peek() byte { return p.s[p.i] }
func (p *scanner) next() byte { b := p.s[p.i]; p.i++; return b }

func (p *scanner) skipSpace() {
	for !p.eof() && unicode.IsSpace(rune(p.peek())) {
		p.i++
	}
}

func (p *scanner) skipSpaceAndCommas() {
	for !p.eof() && (unicode.IsSpace(rune(p.peek())) || p.peek() == ',') {
		p.i++
	}
}

// tuple parses "(v1, v2, ...)" starting at the opening parenthesis.
func (p *scanner) tuple() ([]*string, error) {
	if p.eof() || p.peek() != '(' {
		return nil, fmt.Errorf("expected '(' at offset %d", p.i)
	}
	p.i++ // consume '('
	var vals []*string
	for {
		p.skipSpace()
		if p.eof() {
			return nil, fmt.Errorf("unexpected end of input inside tuple")
		}
		if p.peek() == ')' {
			p.i++
			return vals, nil
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		vals = append(vals, v)
		p.skipSpace()
		if p.eof() {
			return nil, fmt.Errorf("unexpected end of input after value")
		}
		switch p.next() {
		case ',':
			continue
		case ')':
			return vals, nil
		default:
			return nil, fmt.Errorf("expected ',' or ')' at offset %d", p.i-1)
		}
	}
}

// value parses a single NULL, quoted string, or bare literal (number).
func (p *scanner) value() (*string, error) {
	if p.peek() == '\'' {
		return p.quoted()
	}
	start := p.i
	for !p.eof() && p.peek() != ',' && p.peek() != ')' {
		p.i++
	}
	lit := strings.TrimSpace(p.s[start:p.i])
	if strings.EqualFold(lit, "NULL") {
		return nil, nil
	}
	return &lit, nil
}

// quoted parses a MySQL single-quoted string, resolving backslash escapes.
func (p *scanner) quoted() (*string, error) {
	p.i++ // consume opening quote
	var b strings.Builder
	for {
		if p.eof() {
			return nil, fmt.Errorf("unterminated string literal")
		}
		c := p.next()
		switch c {
		case '\'':
			// MySQL also allows '' as an escaped quote inside a literal.
			if !p.eof() && p.peek() == '\'' {
				p.i++
				b.WriteByte('\'')
				continue
			}
			s := b.String()
			return &s, nil
		case '\\':
			if p.eof() {
				return nil, fmt.Errorf("dangling backslash in string literal")
			}
			switch e := p.next(); e {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '0':
				b.WriteByte(0)
			case 'Z':
				b.WriteByte(26)
			default: // \' \" \\ \% \_ and anything else -> the literal char
				b.WriteByte(e)
			}
		default:
			b.WriteByte(c)
		}
	}
}
