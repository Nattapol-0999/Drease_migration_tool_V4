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
