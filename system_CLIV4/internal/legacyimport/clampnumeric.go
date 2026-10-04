package legacyimport

import (
	"fmt"
	"strings"
)

// Cutting a stock quantity that is wider than the column it has to fit.
//
// # THE RULE (owner, 2 Oct 2026 — option ก1)
//
// A quantity whose digits do not fit its column is cut to the largest value that column
// can hold, measured PER COLUMN.
//
// # WHAT THAT COSTS, STATED PLAINLY
//
// The same source number lands as two different values, because the two columns it feeds
// are not the same width: slot_stock.amount is numeric(14,2) and holds up to
// 999999999999.99, while stock_movement.qty_delta is numeric(10,2) and stops at
// 99999999.99. So a lot of 99999999999999 units ends up holding one figure and having
// received a smaller one, and the receive ledger will not reconcile against the shelf for
// that lot. The owner chose this over cutting the whole family to the narrowest column,
// and over skipping the lot, knowing the two numbers will disagree.
//
// # WHY ONLY STOCK QUANTITIES GO THROUGH HERE
//
// Only slot_stock.amount, .remain, .amount_total and stock_movement.qty_delta are cut. No
// money column ever is: a price or a paid amount quietly reduced to fit is a wrong bill,
// and a wrong bill is worse than an import that stops. Money too wide for its column
// still reaches checkScriptColumnLimits and still refuses the file.
//
// # NOTHING IS CUT QUIETLY
//
// Every cut is recorded with the item code, the lot, the column and both numbers, and the
// report prints them. A quantity the clinic can no longer take at face value has to be
// visible to the person who ran the import.

// numericCeiling renders the largest value a numeric(p,s) column can hold —
// 999999999999.99 for numeric(14,2), 99999999 for numeric(8,0).
func numericCeiling(lim numericLimit) string {
	whole := strings.Repeat("9", lim.intDigits())
	if whole == "" {
		whole = "0"
	}
	if lim.scale <= 0 {
		return whole
	}
	return whole + "." + strings.Repeat("9", lim.scale)
}

// clampToColumn cuts one numeric literal down to what the column can hold.
//
// It gives up rather than guesses, in three cases, and each one leaves the value exactly
// as it was so checkScriptColumnLimits can still refuse it:
//
//   - the column has no numeric(p,s) entry, so there is no ceiling to cut to
//   - the value is not a single numeric literal (an expression, a cast, now())
//   - the value is negative. Quantities have already been through
//     clampNonNegativeMoney, so a negative here means something upstream changed, and
//     inventing a ceiling for it would hide that.
func clampToColumn(table, column, lit string) (string, bool) {
	cols, ok := columnNumeric[table]
	if !ok {
		return lit, false
	}
	lim, ok := cols[column]
	if !ok {
		return lit, false
	}
	if strings.HasPrefix(strings.TrimSpace(lit), "-") {
		return lit, false
	}
	digits, text, ok := numericIntegerDigits(lit)
	if !ok || digits <= lim.intDigits() {
		return lit, false
	}
	_ = text
	return numericCeiling(lim), true
}

// productFits reports whether a × b is certain to fit the column, counting digits rather
// than multiplying: the product of an m-digit and an n-digit number has at most m+n
// digits. Used by lotCostExpr, where Postgres does the multiplication at run time and the
// literal guard therefore cannot see the result.
//
// Certain is the word that matters. "Might not fit" is answered with false and the caller
// caps the expression, because the alternative is psql stopping halfway down the file.
func productFits(a, b string, lim numericLimit) bool {
	da, _, aok := numericIntegerDigits(a)
	db, _, bok := numericIntegerDigits(b)
	if !aok || !bok {
		return false
	}
	return da+db <= lim.intDigits()
}

// qtyClamp is one cut, kept so the report can name it.
type qtyClamp struct {
	itemCode string
	lot      string
	column   string
	from     string
	to       string
}

type qtyClamps []qtyClamp

// record keeps a cut. Called unconditionally with the flag clampToColumn returned, so the
// call sites read as one line per column instead of an if around each.
func (c *qtyClamps) record(itemCode, lot, column, from, to string, cut bool) {
	if !cut {
		return
	}
	*c = append(*c, qtyClamp{itemCode: itemCode, lot: lot, column: column, from: from, to: to})
}

// maxClampLinesReported bounds the report. Every cut is counted; the first twenty are
// named, which is enough to see the pattern without burying the rest of the report.
const maxClampLinesReported = 20

// report writes the cuts into the report as a warning plus one line each. A warning, not
// a note, because the stock figures of those lots are no longer the clinic's own numbers.
func (c qtyClamps) report(rep *Report) {
	if len(c) == 0 || rep == nil {
		return
	}
	lots := map[string]bool{}
	for _, x := range c {
		lots[x.itemCode+"/"+x.lot] = true
	}
	rep.warnf("slot_stock/stock_movement: ตัดจำนวนที่กว้างเกินคอลัมน์ปลายทาง %d ค่า ใน %d ล็อต "+
		"ตามมติเจ้าของคลินิก (2 ต.ค. 2569) — ยอดของล็อตเหล่านี้ไม่ใช่ตัวเลขจาก V2 อีกแล้ว "+
		"และเพราะตัดตามเพดานของแต่ละคอลัมน์ จำนวนรับเข้ากับยอดคงเหลือของล็อตเดียวกันจะไม่เท่ากัน",
		len(c), len(lots))
	for i, x := range c {
		if i >= maxClampLinesReported {
			rep.notef("  … และอีก %d ค่า", len(c)-maxClampLinesReported)
			break
		}
		rep.notef("  %s · ล็อต %s · %s: %s → %s", x.itemCode, x.lot, x.column, x.from, x.to)
	}
}

// describeClamp is the one-line form used in tests and error text.
func (x qtyClamp) String() string {
	return fmt.Sprintf("%s/%s %s %s→%s", x.itemCode, x.lot, x.column, x.from, x.to)
}
