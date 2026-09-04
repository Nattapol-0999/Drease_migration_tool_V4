package mongodump

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// scanner walks the .js source one byte at a time. It is byte-oriented on purpose:
// every character it makes a decision about ({ } [ ] ( ) , : ' " ` / \) is ASCII, so
// multi-byte UTF-8 passes through the default branches untouched and Thai text
// inside a string literal cannot be mistaken for punctuation.
type scanner struct {
	s string
	i int
}

func (p *scanner) eof() bool  { return p.i >= len(p.s) }
func (p *scanner) peek() byte { return p.s[p.i] }

func (p *scanner) at(off int) byte {
	if p.i+off >= len(p.s) {
		return 0
	}
	return p.s[p.i+off]
}

// line is the 1-based line number of the current offset, for error messages. It
// rescans from the start, which is fine: it only runs when something has failed.
func (p *scanner) line() int {
	return 1 + strings.Count(p.s[:min(p.i, len(p.s))], "\n")
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n' || b == '\v' || b == '\f'
}

func isIdentStart(b byte) bool {
	return b == '_' || b == '$' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isIdentPart(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// skipGap consumes whitespace, `//` line comments and `/* */` block comments.
//
// It must NOT be used where a `/` could start a regex literal; value() handles that
// case itself before calling here.
func (p *scanner) skipGap() {
	for !p.eof() {
		switch {
		case isSpace(p.peek()):
			p.i++
		case p.peek() == '/' && p.at(1) == '/':
			for !p.eof() && p.peek() != '\n' {
				p.i++
			}
		case p.peek() == '/' && p.at(1) == '*':
			p.i += 2
			for !p.eof() && !(p.peek() == '*' && p.at(1) == '/') {
				p.i++
			}
			if !p.eof() {
				p.i += 2
			}
		default:
			return
		}
	}
}

// ident reads an identifier at the cursor and returns it ("" when there is none).
func (p *scanner) ident() string {
	if p.eof() || !isIdentStart(p.peek()) {
		return ""
	}
	start := p.i
	for !p.eof() && isIdentPart(p.peek()) {
		p.i++
	}
	return p.s[start:p.i]
}

// accessor parses what follows the `db` token: the collection path and the method
// being called, leaving the cursor on the method's opening parenthesis.
//
// Handles `db.coll.insert(`, `db.a.b.insert(` (dots are legal in collection names),
// `db['coll'].insert(`, `db.getCollection("coll").insert(`.
func (p *scanner) accessor() (collection, method string, ok bool) {
	var segs []string
	for {
		p.skipGap()
		if p.eof() {
			return "", "", false
		}
		switch p.peek() {
		case '.':
			p.i++
			p.skipGap()
			name := p.ident()
			if name == "" {
				return "", "", false
			}
			if name == "getCollection" {
				p.skipGap()
				if p.eof() || p.peek() != '(' {
					return "", "", false
				}
				p.i++ // '('
				p.skipGap()
				lit, err := p.stringLit()
				if err != nil {
					return "", "", false
				}
				p.skipGap()
				if p.eof() || p.peek() != ')' {
					return "", "", false
				}
				p.i++ // ')'
				segs = append(segs, lit)
				continue
			}
			segs = append(segs, name)
		case '[':
			p.i++
			p.skipGap()
			lit, err := p.stringLit()
			if err != nil {
				return "", "", false
			}
			p.skipGap()
			if p.eof() || p.peek() != ']' {
				return "", "", false
			}
			p.i++
			segs = append(segs, lit)
		case '(':
			// The last segment collected is the method, everything before it is the
			// collection. `db.insert(` (no collection) is not something we accept.
			if len(segs) < 2 {
				return "", "", false
			}
			return strings.Join(segs[:len(segs)-1], "."), segs[len(segs)-1], true
		default:
			return "", "", false
		}
	}
}

// skipCall steps over a balanced `( … )` starting at the cursor, respecting string
// literals and comments so a `)` inside Thai text or a URL cannot end it early.
func (p *scanner) skipCall() {
	p.skipGap()
	if p.eof() || p.peek() != '(' {
		return
	}
	depth := 0
	for !p.eof() {
		switch p.peek() {
		case '(':
			depth++
			p.i++
		case ')':
			depth--
			p.i++
			if depth == 0 {
				return
			}
		case '\'', '"', '`':
			if _, err := p.stringLit(); err != nil {
				p.i++ // unterminated: give up on precision, keep making progress
			}
		case '/':
			if p.at(1) == '/' || p.at(1) == '*' {
				p.skipGap()
			} else {
				p.i++
			}
		default:
			p.i++
		}
	}
}

// insertArgs parses the argument list of an insert-family call and returns the
// documents it carries. The cursor must sit on the opening parenthesis.
//
// insert/insertOne/save take one document; insertMany takes an array. Both forms are
// accepted for every method because real dumps mix them, and a second argument
// (write concern / options) is parsed and discarded.
func (p *scanner) insertArgs() ([]Doc, error) {
	p.skipGap()
	if p.eof() || p.peek() != '(' {
		return nil, fmt.Errorf("expected '(' after insert method")
	}
	p.i++ // '('

	var docs []Doc
	first := true
	for {
		p.skipGap()
		if p.eof() {
			return nil, fmt.Errorf("unexpected end of file inside insert arguments")
		}
		if p.peek() == ')' {
			p.i++
			return docs, nil
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		if first {
			switch t := v.(type) {
			case Doc:
				docs = append(docs, t)
			case []any:
				for n, el := range t {
					d, ok := el.(Doc)
					if !ok {
						return nil, fmt.Errorf("insertMany element %d is %s, expected a document", n, kindOf(el))
					}
					docs = append(docs, d)
				}
			default:
				return nil, fmt.Errorf("first insert argument is %s, expected a document or an array", kindOf(v))
			}
			first = false
		}
		// Trailing arguments (options, write concern) are parsed only so the cursor
		// lands correctly; their content is not data.
		p.skipGap()
		if p.eof() {
			return nil, fmt.Errorf("unexpected end of file after insert argument")
		}
		if p.peek() == ',' {
			p.i++
			continue
		}
	}
}

// value parses one JavaScript/Extended-JSON value.
func (p *scanner) value() (any, error) {
	p.skipGap()
	if p.eof() {
		return nil, fmt.Errorf("unexpected end of file, expected a value")
	}
	switch c := p.peek(); {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '\'' || c == '"' || c == '`':
		return p.stringLit()
	case c == '/':
		return p.regexLit()
	case c == '-' || c == '+' || isDigit(c) || (c == '.' && isDigit(p.at(1))):
		return p.number()
	case isIdentStart(c):
		return p.wordValue()
	default:
		return nil, fmt.Errorf("unexpected character %q where a value was expected", string(c))
	}
}

// object parses `{ key: value, … }`, allowing unquoted keys, single-quoted keys and
// a trailing comma — all three appear in shell-produced dumps. Canonical Extended
// JSON wrappers are collapsed into their typed value here.
func (p *scanner) object() (any, error) {
	p.i++ // '{'
	doc := Doc{}
	for {
		p.skipGap()
		if p.eof() {
			return nil, fmt.Errorf("unexpected end of file inside an object")
		}
		if p.peek() == '}' {
			p.i++
			return collapseExtJSON(doc)
		}

		var key string
		switch c := p.peek(); {
		case c == '\'' || c == '"' || c == '`':
			s, err := p.stringLit()
			if err != nil {
				return nil, err
			}
			key = s
		case isIdentStart(c):
			key = p.ident()
		case isDigit(c):
			// A numeric key, e.g. {0: …}. Read it as text; JS would too.
			start := p.i
			for !p.eof() && isIdentPart(p.peek()) {
				p.i++
			}
			key = p.s[start:p.i]
		default:
			return nil, fmt.Errorf("unexpected character %q where an object key was expected", string(c))
		}

		p.skipGap()
		if p.eof() || p.peek() != ':' {
			return nil, fmt.Errorf("expected ':' after object key %q", key)
		}
		p.i++

		v, err := p.value()
		if err != nil {
			return nil, err
		}
		doc[key] = v

		p.skipGap()
		if p.eof() {
			return nil, fmt.Errorf("unexpected end of file after value of %q", key)
		}
		switch p.peek() {
		case ',':
			p.i++
		case '}':
			p.i++
			return collapseExtJSON(doc)
		default:
			return nil, fmt.Errorf("expected ',' or '}' after value of %q", key)
		}
	}
}

func (p *scanner) array() (any, error) {
	p.i++ // '['
	out := []any{}
	for {
		p.skipGap()
		if p.eof() {
			return nil, fmt.Errorf("unexpected end of file inside an array")
		}
		if p.peek() == ']' {
			p.i++
			return out, nil
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)

		p.skipGap()
		if p.eof() {
			return nil, fmt.Errorf("unexpected end of file inside an array")
		}
		switch p.peek() {
		case ',':
			p.i++
		case ']':
			p.i++
			return out, nil
		default:
			return nil, fmt.Errorf("expected ',' or ']' inside an array")
		}
	}
}

// stringLit parses a '…', "…" or `…` literal, resolving JS escapes including \uXXXX.
func (p *scanner) stringLit() (string, error) {
	if p.eof() {
		return "", fmt.Errorf("expected a string literal")
	}
	quote := p.peek()
	if quote != '\'' && quote != '"' && quote != '`' {
		return "", fmt.Errorf("expected a string literal, found %q", string(quote))
	}
	p.i++
	var b strings.Builder
	for {
		if p.eof() {
			return "", fmt.Errorf("unterminated string literal")
		}
		c := p.s[p.i]
		p.i++
		switch c {
		case quote:
			return b.String(), nil
		case '\\':
			if p.eof() {
				return "", fmt.Errorf("dangling backslash in string literal")
			}
			e := p.s[p.i]
			p.i++
			switch e {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'v':
				b.WriteByte('\v')
			case '0':
				b.WriteByte(0)
			case 'x':
				if p.i+2 > len(p.s) {
					return "", fmt.Errorf("truncated \\x escape")
				}
				n, err := strconv.ParseUint(p.s[p.i:p.i+2], 16, 8)
				if err != nil {
					return "", fmt.Errorf("bad \\x escape: %w", err)
				}
				p.i += 2
				b.WriteByte(byte(n))
			case 'u':
				r, err := p.unicodeEscape()
				if err != nil {
					return "", err
				}
				b.WriteRune(r)
			case '\n':
				// A line continuation inside a literal contributes nothing.
			default:
				// \' \" \\ \/ and anything else is the literal character.
				b.WriteByte(e)
			}
		default:
			b.WriteByte(c)
		}
	}
}

// unicodeEscape reads the body of a \u escape (the \u is already consumed),
// including the \u{1F600} form and surrogate pairs.
func (p *scanner) unicodeEscape() (rune, error) {
	if !p.eof() && p.peek() == '{' {
		p.i++
		start := p.i
		for !p.eof() && p.peek() != '}' {
			p.i++
		}
		if p.eof() {
			return 0, fmt.Errorf("unterminated \\u{...} escape")
		}
		hex := p.s[start:p.i]
		p.i++ // '}'
		n, err := strconv.ParseUint(hex, 16, 32)
		if err != nil {
			return 0, fmt.Errorf("bad \\u{...} escape: %w", err)
		}
		return rune(n), nil
	}
	if p.i+4 > len(p.s) {
		return 0, fmt.Errorf("truncated \\u escape")
	}
	n, err := strconv.ParseUint(p.s[p.i:p.i+4], 16, 32)
	if err != nil {
		return 0, fmt.Errorf("bad \\u escape: %w", err)
	}
	p.i += 4
	r := rune(n)
	// A high surrogate must be joined with its low half or the rune is invalid.
	if r >= 0xD800 && r <= 0xDBFF && p.i+6 <= len(p.s) && p.s[p.i] == '\\' && p.s[p.i+1] == 'u' {
		lo, err := strconv.ParseUint(p.s[p.i+2:p.i+6], 16, 32)
		if err == nil && lo >= 0xDC00 && lo <= 0xDFFF {
			p.i += 6
			return ((r - 0xD800) << 10) + (rune(lo) - 0xDC00) + 0x10000, nil
		}
	}
	return r, nil
}

// regexLit parses /pattern/flags. A `/` that begins a comment never reaches here —
// value() only dispatches to it after skipGap has already eaten comments.
func (p *scanner) regexLit() (any, error) {
	p.i++ // '/'
	var pat strings.Builder
	inClass := false
	for {
		if p.eof() {
			return nil, fmt.Errorf("unterminated regular expression")
		}
		c := p.s[p.i]
		p.i++
		switch c {
		case '\\':
			if p.eof() {
				return nil, fmt.Errorf("dangling backslash in regular expression")
			}
			pat.WriteByte(c)
			pat.WriteByte(p.s[p.i])
			p.i++
		case '[':
			inClass = true
			pat.WriteByte(c)
		case ']':
			inClass = false
			pat.WriteByte(c)
		case '/':
			if inClass {
				pat.WriteByte(c)
				continue
			}
			start := p.i
			for !p.eof() && isIdentPart(p.peek()) {
				p.i++
			}
			return Regex{Pattern: pat.String(), Options: p.s[start:p.i]}, nil
		case '\n':
			return nil, fmt.Errorf("unterminated regular expression")
		default:
			pat.WriteByte(c)
		}
	}
}

// number parses an int or a double. Integers stay int64 — turning an id or a
// quantity into float64 is how large legacy ids lose their last digits.
func (p *scanner) number() (any, error) {
	start := p.i
	if p.peek() == '+' || p.peek() == '-' {
		p.i++
	}
	// Hex / octal / binary literals appear in a few hand-edited dumps.
	if !p.eof() && p.peek() == '0' && (p.at(1) == 'x' || p.at(1) == 'X') {
		p.i += 2
		hexStart := p.i
		for !p.eof() && isIdentPart(p.peek()) {
			p.i++
		}
		n, err := strconv.ParseInt(p.s[hexStart:p.i], 16, 64)
		if err != nil {
			return nil, fmt.Errorf("bad hex number %q: %w", p.s[start:p.i], err)
		}
		if p.s[start] == '-' {
			n = -n
		}
		return n, nil
	}

	isFloat := false
	for !p.eof() {
		c := p.peek()
		switch {
		case isDigit(c) || c == '_':
			p.i++
		case c == '.':
			isFloat = true
			p.i++
		case c == 'e' || c == 'E':
			isFloat = true
			p.i++
			if !p.eof() && (p.peek() == '+' || p.peek() == '-') {
				p.i++
			}
		default:
			goto done
		}
	}
done:
	lit := strings.ReplaceAll(p.s[start:p.i], "_", "")
	if lit == "" || lit == "-" || lit == "+" {
		return nil, fmt.Errorf("malformed number at line %d", p.line())
	}
	if !isFloat {
		if n, err := strconv.ParseInt(lit, 10, 64); err == nil {
			return n, nil
		}
		// Beyond int64 (a 20-digit legacy id): keep the digits as text rather than
		// silently rounding through float64.
		return lit, nil
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return nil, fmt.Errorf("bad number %q: %w", lit, err)
	}
	return f, nil
}

// wordValue handles keywords and the shell's constructor functions.
func (p *scanner) wordValue() (any, error) {
	word := p.ident()
	switch word {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null", "undefined":
		// undefined collapses to null on purpose: Postgres has one absent value and
		// inventing a second would only push the distinction one layer down.
		return nil, nil
	case "NaN":
		return nil, fmt.Errorf("NaN is not importable — fix the source value")
	case "Infinity":
		return nil, fmt.Errorf("Infinity is not importable — fix the source value")
	case "new":
		p.skipGap()
		next := p.ident()
		if next == "" {
			return nil, fmt.Errorf("expected a constructor after `new`")
		}
		return p.construct(next)
	case "MinKey":
		p.optionalEmptyCall()
		return MinKey{}, nil
	case "MaxKey":
		p.optionalEmptyCall()
		return MaxKey{}, nil
	default:
		return p.construct(word)
	}
}

// optionalEmptyCall consumes a bare `()` if present, so MinKey and MinKey() both work.
func (p *scanner) optionalEmptyCall() {
	save := p.i
	p.skipGap()
	if !p.eof() && p.peek() == '(' {
		p.skipCall()
		return
	}
	p.i = save
}

// construct parses `Name(args…)` for the Extended JSON constructors.
func (p *scanner) construct(name string) (any, error) {
	args, err := p.callArgs()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	switch name {
	case "ObjectId", "ObjectID":
		s, err := oneString(name, args)
		if err != nil {
			return nil, err
		}
		return ObjectID(s), nil

	case "ISODate", "Date":
		if len(args) == 0 {
			// `new Date()` in a dump means "whenever this file was written", which is
			// not a fact about the row. Refuse rather than stamp an import time onto
			// clinical or financial data.
			return nil, fmt.Errorf("%s() with no argument has no value to import", name)
		}
		return toTime(name, args[0])

	// Int32/Long/Double are what a modern mongosh export writes where the older shell
	// wrote NumberInt/NumberLong. Same values, different spelling — a dump from either
	// tool has to read the same way.
	case "NumberLong", "NumberInt", "Int32", "Long":
		switch v := args0(args).(type) {
		case int64:
			return v, nil
		case string:
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%s(%q): %w", name, v, err)
			}
			return n, nil
		case float64:
			return int64(v), nil
		default:
			return nil, fmt.Errorf("%s expects a number or a numeric string", name)
		}

	// Decimal128 is mongosh's spelling of NumberDecimal. Both stay TEXT: every one of
	// these in a clinic export is money, and float64 cannot hold ฿0.10 exactly.
	case "NumberDecimal", "Decimal128":
		switch v := args0(args).(type) {
		case string:
			return Decimal(strings.TrimSpace(v)), nil
		case int64:
			return Decimal(strconv.FormatInt(v, 10)), nil
		case float64:
			// The dump already lost precision here; carry the digits it has rather
			// than pretending otherwise.
			return Decimal(strconv.FormatFloat(v, 'f', -1, 64)), nil
		default:
			return nil, fmt.Errorf("%s expects a string", name)
		}

	case "UUID", "BinData":
		if name == "UUID" {
			s, err := oneString(name, args)
			if err != nil {
				return nil, err
			}
			return Binary{SubType: 4, Base64: s}, nil
		}
		if len(args) != 2 {
			return nil, fmt.Errorf("BinData expects (subtype, base64)")
		}
		sub, _ := args[0].(int64)
		b64, _ := args[1].(string)
		return Binary{SubType: int(sub), Base64: b64}, nil

	case "Timestamp":
		if len(args) != 2 {
			return nil, fmt.Errorf("Timestamp expects (t, i)")
		}
		t, _ := args[0].(int64)
		i, _ := args[1].(int64)
		return Timestamp{T: uint32(t), I: uint32(i)}, nil

	case "DBRef":
		// A cross-collection pointer. Kept as a document so the mapping layer can
		// decide what it means; this package does not resolve references.
		d := Doc{}
		if len(args) > 0 {
			d["$ref"] = args[0]
		}
		if len(args) > 1 {
			d["$id"] = args[1]
		}
		return d, nil

	default:
		return nil, fmt.Errorf("unsupported constructor %s(…) — add it to mongodump if the dump needs it", name)
	}
}

func args0(args []any) any {
	if len(args) == 0 {
		return nil
	}
	return args[0]
}

func oneString(name string, args []any) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("%s expects exactly one argument", name)
	}
	s, ok := args[0].(string)
	if !ok {
		return "", fmt.Errorf("%s expects a string", name)
	}
	return s, nil
}

// callArgs parses `( v, v, … )` at the cursor.
func (p *scanner) callArgs() ([]any, error) {
	p.skipGap()
	if p.eof() || p.peek() != '(' {
		return nil, fmt.Errorf("expected '('")
	}
	p.i++
	var out []any
	for {
		p.skipGap()
		if p.eof() {
			return nil, fmt.Errorf("unexpected end of file inside arguments")
		}
		if p.peek() == ')' {
			p.i++
			return out, nil
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.skipGap()
		if p.eof() {
			return nil, fmt.Errorf("unexpected end of file inside arguments")
		}
		switch p.peek() {
		case ',':
			p.i++
		case ')':
			p.i++
			return out, nil
		default:
			return nil, fmt.Errorf("expected ',' or ')' inside arguments")
		}
	}
}

// dateLayouts are tried in order for an ISODate string. The legacy Node service
// wrote whichever its driver produced, so all of these occur.
var dateLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999Z0700",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// toTime converts a constructor argument to a time.Time.
func toTime(name string, arg any) (any, error) {
	switch v := arg.(type) {
	case string:
		s := strings.TrimSpace(v)
		for _, layout := range dateLayouts {
			if t, err := time.Parse(layout, s); err == nil {
				return t, nil
			}
		}
		return nil, fmt.Errorf("%s(%q): unrecognised date format", name, s)
	case int64:
		// Milliseconds since epoch, which is what the shell prints.
		return time.UnixMilli(v).UTC(), nil
	case float64:
		return time.UnixMilli(int64(v)).UTC(), nil
	default:
		return nil, fmt.Errorf("%s expects a date string or epoch milliseconds", name)
	}
}

// collapseExtJSON turns a canonical Extended JSON wrapper into its typed value. A
// document with exactly one key that starts with '$' (or the two-key $binary /
// $regularExpression forms) is a wrapper, not data.
func collapseExtJSON(d Doc) (any, error) {
	if len(d) == 0 || len(d) > 2 {
		return d, nil
	}

	if len(d) == 1 {
		for k, v := range d {
			switch k {
			case "$oid":
				s, ok := v.(string)
				if !ok {
					return nil, fmt.Errorf(`{"$oid": …} expects a string`)
				}
				return ObjectID(s), nil

			case "$date":
				// Either a string, epoch ms, or a nested {"$numberLong": "…"}.
				if nested, ok := v.(int64); ok {
					return time.UnixMilli(nested).UTC(), nil
				}
				return toTime("$date", v)

			case "$numberLong", "$numberInt":
				switch n := v.(type) {
				case int64:
					return n, nil
				case string:
					parsed, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
					if err != nil {
						return nil, fmt.Errorf(`{"%s": %q}: %w`, k, n, err)
					}
					return parsed, nil
				default:
					return nil, fmt.Errorf(`{"%s": …} expects a number or numeric string`, k)
				}

			case "$numberDouble":
				switch n := v.(type) {
				case float64:
					return n, nil
				case int64:
					return float64(n), nil
				case string:
					parsed, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
					if err != nil {
						return nil, fmt.Errorf(`{"$numberDouble": %q}: %w`, n, err)
					}
					return parsed, nil
				}

			case "$numberDecimal":
				switch n := v.(type) {
				case string:
					return Decimal(strings.TrimSpace(n)), nil
				case int64:
					return Decimal(strconv.FormatInt(n, 10)), nil
				case float64:
					return Decimal(strconv.FormatFloat(n, 'f', -1, 64)), nil
				}

			case "$minKey":
				return MinKey{}, nil
			case "$maxKey":
				return MaxKey{}, nil

			case "$timestamp":
				inner, ok := v.(Doc)
				if !ok {
					return nil, fmt.Errorf(`{"$timestamp": …} expects an object`)
				}
				t, _ := inner["t"].(int64)
				i, _ := inner["i"].(int64)
				return Timestamp{T: uint32(t), I: uint32(i)}, nil

			case "$undefined":
				return nil, nil
			}
		}
		return d, nil
	}

	// Two-key wrappers.
	if bin, ok := d["$binary"]; ok {
		if inner, ok := bin.(Doc); ok {
			b64, _ := inner["base64"].(string)
			subHex, _ := inner["subType"].(string)
			sub, _ := strconv.ParseInt(subHex, 16, 32)
			return Binary{SubType: int(sub), Base64: b64}, nil
		}
		// Legacy shape: {"$binary": "<b64>", "$type": "00"}
		b64, _ := bin.(string)
		subHex, _ := d["$type"].(string)
		sub, _ := strconv.ParseInt(subHex, 16, 32)
		if _, err := base64.StdEncoding.DecodeString(b64); err != nil {
			return nil, fmt.Errorf(`{"$binary": …} is not valid base64: %w`, err)
		}
		return Binary{SubType: int(sub), Base64: b64}, nil
	}
	if re, ok := d["$regex"]; ok {
		pat, _ := re.(string)
		opts, _ := d["$options"].(string)
		return Regex{Pattern: pat, Options: opts}, nil
	}
	if re, ok := d["$regularExpression"]; ok {
		if inner, ok := re.(Doc); ok {
			pat, _ := inner["pattern"].(string)
			opts, _ := inner["options"].(string)
			return Regex{Pattern: pat, Options: opts}, nil
		}
	}
	return d, nil
}
