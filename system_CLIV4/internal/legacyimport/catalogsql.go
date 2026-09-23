package legacyimport

import (
	"fmt"
	"strconv"
	"strings"

	"system_cliv4/internal/mongodump"
)

// SQL literal helpers used by the catalog mapping. They exist next to it rather than in
// sql.go because they answer questions only the Mongo side asks: a value that arrived
// as Decimal128 text must reach numeric columns without ever passing through float64.

// moneyLit renders a money value for a numeric column. The input is the source text
// (Decimal128 or Int32 rendered as text), so the digits are handed to Postgres verbatim
// — no float64 in the path, because ฿0.10 has no exact float representation.
func moneyLit(s string) string {
	t := strings.TrimSpace(s)
	if t == "" {
		return "0"
	}
	if !validNumeric(t) {
		// Refuse to guess: a malformed amount becomes 0 and is visible in the report
		// rather than silently becoming a wrong price.
		return "0"
	}
	return t
}

// clampNonNegativeMoney renders a quantity the same way moneyLit does, except that a
// negative number becomes 0.
//
// # WHY STOCK QUANTITIES ARE CLAMPED AND PRICES ARE NOT
//
// V2's stock rooms are configured with `isNegative_qty: true`, so a withdrawal is allowed
// to take a lot below zero and the number stays negative for good. Measured on the
// pb-plasticsurgery export: the branch room's quantities add up to −88,243 against
// MainStock's +25,471. V4 has no CHECK forbidding a negative `remain` either, so those
// figures import cleanly and then read as inventory the clinic does not have — a lot of
// −5 boxes is not a debt, it is a counting error that V2 never made anyone resolve.
//
// The owner's rule (23 Sep 2026): a negative quantity arrives as 0. Applied to the whole
// quantity family of one lot — `remain`, `amount`, `amount_total` and the `qty_delta` of
// its receive movement — because they are the same measurement seen from four angles, and
// clamping one while leaving another negative would produce a lot that received −5 and
// holds 0. `cost_lot` follows for the same reason: it is unit cost × quantity, so an
// unclamped quantity would give the lot a negative value on the stock report.
//
// Prices are deliberately NOT clamped: a negative price would be a different decision
// (about money, not counting) and none appears in any export seen so far.
func clampNonNegativeMoney(s string) string {
	lit := moneyLit(s)
	// moneyLit has already rejected anything non-numeric, so a parse failure here can only
	// mean validNumeric accepts a shape ParseFloat does not. Treat that as 0 rather than
	// letting an unparsed string through to a numeric column.
	f, err := strconv.ParseFloat(lit, 64) // comparison only, the digits are what is stored
	if err != nil || f < 0 {
		return "0"
	}
	return lit
}

// positiveMoney reports whether the source text is a number greater than zero. Used to
// decide draft vs active without parsing money into a float.
func positiveMoney(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" || !validNumeric(t) {
		return false
	}
	f, err := strconv.ParseFloat(t, 64) // comparison only, never stored
	return err == nil && f > 0
}

// intFromMoney reads a whole-number field (loyalty points) from source text.
func intFromMoney(s string) int {
	t := strings.TrimSpace(s)
	if t == "" || !validNumeric(t) {
		return 0
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || f < 0 {
		return 0
	}
	return int(f)
}

// intLitDefault renders an integer column from source text, falling back when absent.
func intLitDefault(s string, def int) string {
	t := strings.TrimSpace(s)
	if t == "" || !validNumeric(t) {
		return strconv.Itoa(def)
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return strconv.Itoa(def)
	}
	return strconv.Itoa(int(f))
}

func validNumeric(s string) bool {
	if s == "" {
		return false
	}
	dot := false
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case (r == '-' || r == '+') && i == 0:
		case r == '.' && !dot:
			dot = true
		default:
			return false
		}
	}
	return true
}

func boolLit(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// defaultLit uses the column's own default when the source has nothing to say.
func defaultLit(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return lit(def)
	}
	return lit(v)
}

// tsLitNullable is tsLit for a timestamp column that may legitimately be empty.
func tsLitNullable(s string) string {
	if strings.TrimSpace(s) == "" {
		return "NULL"
	}
	return tsLit(s, "NULL")
}

// Get2Bool reads a boolean field, defaulting to false for absent/other types.
func docBool(d mongodump.Doc, path string) bool {
	v, ok := d.Get(path)
	if !ok {
		return false
	}
	b, isBool := v.(bool)
	return isBool && b
}

var _ = fmt.Sprintf
