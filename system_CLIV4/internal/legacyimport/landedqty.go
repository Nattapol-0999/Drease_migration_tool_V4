package legacyimport

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// A quantity that cannot survive the column it is written into, caught before psql runs.
//
// WHAT HAPPENED — measured on pinluck-clinic (CBC40B0), imported to production 5 Oct 2026
//
// invoice_item.qty is an `integer` column (migration 0076). V2 records a line quantity in
// `bills.list[].amount`, and six of that clinic's 29,337 bill lines carry a FRACTION:
//
//	invoice    code   price  amount  qty as stored   what it did to the bill
//	670070199  ST118   2000  0.5     1  (rounds up)  net ฿1,000 too HIGH
//	690040398  LH001    630  1.5     2  (rounds up)  net ฿315 too HIGH
//	670075362  ST039    500  0.2     0               net ฿100 too LOW
//	690028527  ST039    500  0.2     0               net ฿100 too LOW
//	690004617  ST168   1000  1.44    1               net ฿440 too LOW
//	690012615  ST168   1000  1.44    1               net ฿440 too LOW
//
// Postgres rounds on the cast and says nothing. The four that round DOWN left the invoice
// worth less than the receipts under it collected, which is what guard 3 shouted about —
// AFTER all of it had been committed. The two that round UP are worse, because no guard
// looks for that direction: those invoices are sitting in production as `partially_paid`
// with ฿1,000 and ฿315 of debt the patients never owed, and nothing complained.
//
// WHY THE SQL GUARDS CANNOT BE THE ANSWER HERE (owner's decision 1c, 5 Oct 2026)
//
// Guards 1-3 read the rows back after they are written, and the obvious fix — put them in
// the same transaction as the money so a RAISE rolls it back — does not exist to be done.
// script.go opens a transaction per ENTITY, not per group: measured on the generated file,
// `invoice` commits at line 108363, `invoice_item` at 211076, `receipt` at 544646,
// `receipt_payment` at 624343 and the settle UPDATE at 659870, each in its own
// transaction. No transaction ever holds the rows those guards compare, so there is
// nothing for a failure to undo. (The per-entity split is itself load-bearing: one
// transaction for the file exhausted max_locks_per_transaction — see script.go.)
//
// So the check moves to BEFORE anything is written, which is where every other refusal in
// this importer already lives: column widths, duplicate document numbers, bills whose
// patient is missing. A run that would change what a bill is worth produces no file at
// all, and the database is never touched.
//
// WHAT IS CHECKED, AND WHY IT IS NOT "DOES THE MONEY RECONCILE"
//
// The invariant is narrower and stronger than a money comparison: a bill must be worth the
// same after the import as the source says it is worth. Comparing against the money
// collected would miss the two invoices that round UP — they take less money than their
// inflated net, which looks like an ordinary unpaid balance.
//
// It is also deliberately NOT folded into overpaidInSource, the existing exemption for
// bills V2 itself over-collected on. Those two things look identical in a money
// comparison and mean opposite things: one is a fact about the clinic that must be carried
// across as-is, the other is damage this tool would be doing. Exempting the second would
// have let all six of these through quietly.
//
// WHAT THIS DOES NOT COVER
//
// A literal this cannot parse is left alone rather than guessed at — checkScriptColumnLimits
// scans the finished script and refuses a fractional value in any integer column, so an
// unreadable quantity is still caught one step later, by name and column.

// qtyAsWritten returns the value an integer column will actually hold, which is the
// literal rounded HALF AWAY FROM ZERO — what Postgres does casting numeric to int4
// (0.5 -> 1, 1.44 -> 1, 1.5 -> 2, 2.5 -> 3), not banker's rounding.
//
// ok is false for anything that is not a plain numeric literal; see the note above on why
// those are left to the script scan instead of being guessed at here.
func qtyAsWritten(lit string) (*big.Int, bool) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(lit))
	if !ok {
		return nil, false
	}
	num := new(big.Int).Abs(r.Num())
	den := new(big.Int).Set(r.Denom())
	// floor((2|n| + d) / 2d) is round-half-up on a non-negative value.
	q := new(big.Int).Lsh(num, 1)
	q.Add(q, den)
	q.Div(q, new(big.Int).Lsh(den, 1))
	if r.Sign() < 0 {
		q.Neg(q)
	}
	return q, true
}

// qtySurvivesTheColumn reports whether the literal is already a whole number, so writing
// it into an integer column changes nothing.
func qtySurvivesTheColumn(lit string) (survives bool, measurable bool) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(lit))
	if !ok {
		return false, false
	}
	return r.IsInt(), true
}

// MOVING THE FRACTION INTO THE PRICE (owner's decision ค, 5 Oct 2026)
//
// Refusing was right as a first move — it stopped a wrong bill from being written — but it
// turned out to be the wrong place to stop. Two clinics in a row hit it, and the fractions
// were not data-entry slips: 16.25 units of Xeomin at ฿400, half a 2,000-baht bottle,
// 1.44 of a 1,000-baht dressing. Fractional quantities are ordinary in a clinic, so
// refusing every one of them means the clinic hand-edits V2 before every migration.
//
// The owner first asked for the fraction to be ROUNDED UP. Measured, that is the worse of
// the two failures: 16.25 -> 17 makes the bill ฿6,800 against the ฿6,500 the receipt
// collected, so a `full_paid` visit becomes `partially_paid` and the patient is shown a
// debt they never had. Across the two measured dumps it would invent ฿3,835 of debt over
// eight invoices, and guard 3 would not catch any of it — it only looks for money ABOVE
// the net, never below. That is the exact shape of the damage already sitting in
// production from the rounding this file exists to stop.
//
// So the quantity moves into the price instead: 16.25 x ฿400 becomes 1 x ฿6,500. The bill
// is worth the same to the satang, the receipt reconciles, the status stays `full_paid`,
// and invoice_item.qty gets the whole number it needs.
//
// WHAT IT COSTS, SAID PLAINLY
//
// The unit price on that line is no longer the catalogue price. A receipt prints
// "1 x ฿6,500" where the clinic sold 16.25 units at ฿400. catalog_item.price is untouched,
// so only this historical invoice line reads that way, and every converted line is named
// in the import report with both numbers. The item NAME is deliberately left alone — the
// real quantity belongs in the report, not printed onto a receipt.
//
// Rounding half a satang: price x qty can carry more than two decimals (333.33 x 1.11 is
// 369.9963) and invoice_item.price is numeric(12,2). The product is rounded here, half
// away from zero, and the same rounded figure feeds the bill total — so the tool and the
// database cannot disagree about what the bill is worth. Any residue is reported. Neither
// measured dump has one: every product divides exactly.

// foldedLine records one quantity that was moved into its price, for the report.
type foldedLine struct {
	invoiceNo string
	code      string
	name      string
	srcPrice  string
	srcQty    string
	newPrice  string
	residue   *big.Rat // what rounding to satang dropped; zero in every measured case
}

// foldFractionIntoPrice turns "16.25 at 400" into "1 at 6500".
//
// ok is false when either literal is not a plain number. Those are left exactly as they
// were: checkLandedLineValues refuses the run over them by name, which is the right
// outcome for a value nobody can read rather than a guess at what it meant.
func foldFractionIntoPrice(price, qty string) (newPrice string, residue *big.Rat, ok bool) {
	p, okP := new(big.Rat).SetString(strings.TrimSpace(price))
	q, okQ := new(big.Rat).SetString(strings.TrimSpace(qty))
	if !okP || !okQ {
		return "", nil, false
	}
	exact := new(big.Rat).Mul(p, q)
	// invoice_item.price is numeric(12,2); render at the column's scale so the literal in
	// the file is exactly what the database will hold.
	rendered := exact.FloatString(moneyScale)
	back, _ := new(big.Rat).SetString(rendered)
	return rendered, new(big.Rat).Sub(exact, back), true
}

// moneyScale is the number of decimal places every money column in V4 keeps.
const moneyScale = 2

// lineValueLost is price × (the quantity the source gave − the quantity that will be
// stored), as an exact decimal. Positive means the invoice will be worth LESS than the
// source says; negative means more.
func lineValueLost(price, qty string) (*big.Rat, bool) {
	p, okP := new(big.Rat).SetString(strings.TrimSpace(price))
	q, okQ := new(big.Rat).SetString(strings.TrimSpace(qty))
	if !okP || !okQ {
		return nil, false
	}
	written, ok := qtyAsWritten(qty)
	if !ok {
		return nil, false
	}
	drift := new(big.Rat).Sub(q, new(big.Rat).SetInt(written))
	return new(big.Rat).Mul(p, drift), true
}

// landedQtyProblem is one bill line whose quantity will not survive invoice_item.qty.
type landedQtyProblem struct {
	invoiceNo string
	hn        string
	code      string
	name      string
	sourceQty string
	written   string
	price     string
	lost      *big.Rat
}

func (p landedQtyProblem) String() string {
	direction := "ยอดบิลจะน้อยกว่าต้นทาง"
	lost := p.lost
	if lost.Sign() < 0 {
		direction = "ยอดบิลจะมากกว่าต้นทาง"
		lost = new(big.Rat).Neg(lost)
	}
	return fmt.Sprintf("เลขที่ %s (hn %s) รายการ %s %q: จำนวน %s จะถูกเก็บเป็น %s "+
		"— ราคา %s × จำนวนที่หายไป = %s บาท · %s",
		p.invoiceNo, p.hn, firstNonEmpty(p.code, "(ไม่มีรหัส)"), oneLine(p.name),
		p.sourceQty, p.written, p.price, lost.FloatString(2), direction)
}

// checkLandedLineValues refuses a run that would change what a bill is worth.
//
// Every offending line is collected and named in one refusal. Stopping at the first is
// what turned one clinic into three round trips of pull, create, generate, import, fail,
// delete — each attempt reporting one problem and hiding the rest.
func checkLandedLineValues(plans []billPlan, rep *Report) error {
	var problems []landedQtyProblem
	// Per-invoice totals, so the refusal can say how much each bill moves rather than
	// only how much one line does.
	drift := map[string]*big.Rat{}
	var order []string

	for _, p := range plans {
		for _, l := range p.lines {
			survives, measurable := qtySurvivesTheColumn(l.qty)
			if !measurable || survives {
				continue
			}
			written, _ := qtyAsWritten(l.qty)
			lost, ok := lineValueLost(l.price, l.qty)
			if !ok {
				continue
			}
			problems = append(problems, landedQtyProblem{
				invoiceNo: p.no, hn: p.hn, code: l.code, name: l.name,
				sourceQty: strings.TrimSpace(l.qty), written: written.String(),
				price: strings.TrimSpace(l.price), lost: lost,
			})
			if _, seen := drift[p.no]; !seen {
				drift[p.no] = new(big.Rat)
				order = append(order, p.no)
			}
			drift[p.no].Add(drift[p.no], lost)
		}
	}
	if len(problems) == 0 {
		if rep != nil {
			rep.notef("invoice_item.qty เป็นคอลัมน์จำนวนเต็ม — ตรวจแล้วว่าทุกบรรทัดมีจำนวนเต็มจำนวน " +
				"จึงไม่มีบรรทัดใดเสียมูลค่าตอนเขียนลงฐาน")
		}
		return nil
	}

	sort.Slice(problems, func(i, j int) bool {
		if problems[i].invoiceNo != problems[j].invoiceNo {
			return problems[i].invoiceNo < problems[j].invoiceNo
		}
		return problems[i].code < problems[j].code
	})
	sort.Strings(order)

	lines := make([]string, 0, len(problems))
	for _, p := range problems {
		lines = append(lines, p.String())
	}
	perBill := make([]string, 0, len(order))
	total := new(big.Rat)
	for _, no := range order {
		perBill = append(perBill, fmt.Sprintf("%s %s บาท", no, drift[no].FloatString(2)))
		total.Add(total, drift[no])
	}

	return fmt.Errorf("bills: %d บรรทัดใน %d ใบมีจำนวนเป็นเศษ ซึ่ง invoice_item.qty "+
		"(คอลัมน์จำนวนเต็ม) เก็บไม่ได้ — Postgres จะปัดให้เงียบ ๆ แล้วยอดบิลจะไม่ใช่ยอดเดิม "+
		"จึงปฏิเสธตั้งแต่ตอนสร้างไฟล์ ยังไม่มีอะไรแตะฐานข้อมูล · "+
		"ยอดที่จะเคลื่อนต่อใบ (บวก = ยอดหาย): %s · รวม %s บาท · "+
		"ทางแก้: แก้จำนวนที่ต้นทางให้เป็นจำนวนเต็มแล้วปรับราคาต่อหน่วยให้ยอดรวมเท่าเดิม "+
		"(เช่น จำนวน 0.2 ราคา 500 → จำนวน 1 ราคา 100) แล้วดึงข้อมูลใหม่ · รายการทั้งหมด:\n  %s",
		len(problems), len(order),
		strings.Join(perBill, " · "), total.FloatString(2),
		strings.Join(lines, "\n  "))
}

// reportFoldedQuantities names every line whose quantity was moved into its price.
//
// One warning, not one per line, and a WARNING rather than a note: the unit price on these
// lines is no longer the catalogue price, and a clinic reading its own invoice has to be
// told that rather than discovering it.
func reportFoldedQuantities(folded []foldedLine, rep *Report) {
	if len(folded) == 0 || rep == nil {
		return
	}
	sorted := make([]foldedLine, len(folded))
	copy(sorted, folded)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].invoiceNo != sorted[j].invoiceNo {
			return sorted[i].invoiceNo < sorted[j].invoiceNo
		}
		return sorted[i].code < sorted[j].code
	})

	bills := map[string]bool{}
	residue := new(big.Rat)
	lines := make([]string, 0, len(sorted))
	for _, f := range sorted {
		bills[f.invoiceNo] = true
		residue.Add(residue, f.residue)
		lines = append(lines, fmt.Sprintf("เลขที่ %s รายการ %s %q: %s × %s บาท → จำนวน 1 ราคา %s บาท",
			f.invoiceNo, firstNonEmpty(f.code, "(ไม่มีรหัส)"), oneLine(f.name),
			f.srcQty, f.srcPrice, f.newPrice))
	}

	msg := fmt.Sprintf("invoice_item: %d บรรทัดใน %d ใบมีจำนวนเป็นเศษ ซึ่ง invoice_item.qty "+
		"(คอลัมน์จำนวนเต็ม) เก็บไม่ได้ จึงย้ายเศษไปไว้ที่ราคาตามมติเจ้าของคลินิก (5 ต.ค. 2569) — "+
		"**ยอดบิลเท่าเดิมทุกสตางค์** ใบเสร็จยังกระทบยอดตรง และสถานะไม่เปลี่ยน · "+
		"**ผลข้างเคียงที่ต้องรู้: ราคาต่อหน่วยในบรรทัดเหล่านี้ไม่ใช่ราคาในแคตตาล็อกอีกแล้ว** "+
		"(ใบเสร็จจะพิมพ์ว่า 1 × ยอดรวม) · ราคาในแคตตาล็อก (catalog_item.price) ไม่ถูกแตะ "+
		"และชื่อรายการไม่ถูกต่อท้าย จำนวนจริงอยู่ในรายการข้างล่างนี้ · %s",
		len(sorted), len(bills), strings.Join(lines, " · "))
	rep.warnf("%s", msg)

	// Half a satang lost to the column's scale. Zero in both measured dumps; said out
	// loud when it is not, because a bill total that moved at all is the operator's
	// business and not the tool's to absorb quietly.
	if residue.Sign() != 0 {
		rep.warnf("invoice_item: การย้ายเศษไปที่ราคาทำให้ยอดรวมเคลื่อนไป %s บาท "+
			"เพราะ invoice_item.price เก็บได้ %d ตำแหน่งทศนิยม และผลคูณราคา × จำนวนมีทศนิยมมากกว่านั้น "+
			"— ปัดครึ่งขึ้นตามที่ Postgres ทำ",
			residue.FloatString(4), moneyScale)
	}
}
