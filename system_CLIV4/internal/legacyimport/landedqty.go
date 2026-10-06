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

// THE SAME FAILURE ON THE OTHER COLUMN, MEASURED ON master-clinic 6 Oct 2026
//
// invoice_item.price is numeric(12,2). A price with more than two decimals is rounded by
// Postgres on the way in — silently, exactly as the integer qty column rounds a fraction —
// and the bill stops being worth what the source says.
//
// Bill 680028253 of that clinic, three lines:
//
//	STR000018       Benzac AC5   150           x 1   = 150
//	LH119           Doctor Fee   100           x 1   = 100
//	STY00011000001  Doxyclate    3.571428572   x 28  = 100.000000016
//	                                           Σ     = 350.000000016
//
// V2's own price_total is 350.000000016 and its receipt collected 350, so every step of
// this importer's arithmetic agreed with the source. Then the column held 3.57, which is
// 99.96 over 28 units, the net came to 349.96, and guard 3 refused the whole 241,181-line
// script for taking ฿350 against a bill worth ฿349.96.
//
// 3.571428572 is 100/28 as V2's JavaScript computed it: a unit price derived by dividing a
// pack price by the pack size. Any clinic that sells by the tablet can produce one.
//
// Measured over the whole export: 4,899 bill lines across 2,712 bills, ONE price with more
// than two decimals, no fractional quantity at all. One line in a clinic, and it cost the
// clinic its entire import.

// priceSurvivesTheColumn reports whether the literal fits a money column's scale, so
// writing it changes nothing.
//
// Measured against the SCALE, not the text: 3.570 survives because the trailing zero is
// not precision, and 1e-3 does not because it is a tenth of a satang however it is
// spelled. measurable is false for anything that is not a number, which is left to
// checkScriptColumnLimits to name rather than guessed at here.
func priceSurvivesTheColumn(lit string) (survives bool, measurable bool) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(lit))
	if !ok {
		return false, false
	}
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(moneyScale), nil)
	return new(big.Rat).Mul(r, new(big.Rat).SetInt(pow)).IsInt(), true
}

// whyFolded is which column forced a line's value into its price.
//
// Carried per line rather than inferred later because the two are different facts about
// the source with different fixes — a quantity nobody can store as a whole number, versus
// a unit price carrying more decimals than money has — and one report naming the other
// sends the clinic looking in the wrong place.
type whyFolded int

const (
	foldForQty   whyFolded = iota // a fractional quantity, invoice_item.qty is integer
	foldForPrice                  // a price beyond numeric(12,2)
	foldForBoth                   // both at once
)

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
// AND THE SAME ANSWER FOR AN OVER-PRECISE PRICE (owner's decision ก, 6 Oct 2026)
//
// A price with more decimals than numeric(12,2) keeps is the same failure on the other
// column, so it gets the same treatment: 28 x ฿3.571428572 becomes 1 x ฿100.00. The owner
// was shown the alternatives — splitting the line into 27 x ฿3.57 + 1 x ฿3.61, refusing
// until the clinic fixes the division at the source, or widening the V4 column — and chose
// this one, for the reason it was chosen for quantities: the bill total is what has to
// survive, and it is the number the patient already paid.
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

// foldedLine records one line whose value was moved into its price, for the report.
type foldedLine struct {
	invoiceNo string
	code      string
	name      string
	srcPrice  string
	srcQty    string
	newPrice  string
	residue   *big.Rat  // what rounding to satang dropped; zero in every measured case
	why       whyFolded // which column forced it, so the report can say which
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

// priceAsWritten returns the value a numeric(12,2) column will actually hold: the literal
// rounded to the scale, half away from zero, which is what Postgres does.
func priceAsWritten(lit string) (*big.Rat, bool) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(lit))
	if !ok {
		return nil, false
	}
	// FloatString rounds half away from zero and renders at the scale, so the round trip
	// through it is exactly the value the column keeps.
	back, _ := new(big.Rat).SetString(r.FloatString(moneyScale))
	return back, true
}

// priceValueLost is (the price that will be stored − the price the source gave) × qty, as
// an exact decimal. Negative means the invoice will be worth LESS than the source says;
// positive means more. Same sign convention as lineValueLost reversed, so both read as
// "what the bill moves by".
func priceValueLost(price, qty string) (*big.Rat, bool) {
	p, okP := new(big.Rat).SetString(strings.TrimSpace(price))
	q, okQ := new(big.Rat).SetString(strings.TrimSpace(qty))
	if !okP || !okQ {
		return nil, false
	}
	written, ok := priceAsWritten(price)
	if !ok {
		return nil, false
	}
	return new(big.Rat).Mul(new(big.Rat).Sub(p, written), q), true
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

// landedPriceProblem is one bill line whose PRICE will not survive invoice_item.price.
//
// Only reachable when the fold did not run — it needs both literals readable, so a line
// with an over-precise price and an unreadable quantity gets here. Kept as a refusal
// rather than left to the script scan because the money it moves can be stated exactly,
// and a number is worth more to whoever has to fix the source than "does not fit".
type landedPriceProblem struct {
	invoiceNo string
	hn        string
	code      string
	name      string
	sourcePx  string
	written   string
	qty       string
	lost      *big.Rat
}

func (p landedPriceProblem) String() string {
	head := fmt.Sprintf("เลขที่ %s (hn %s) รายการ %s %q: ราคา %s จะถูกเก็บเป็น %s",
		p.invoiceNo, p.hn, firstNonEmpty(p.code, "(ไม่มีรหัส)"), oneLine(p.name),
		p.sourcePx, p.written)
	if p.lost == nil {
		return head + fmt.Sprintf(" — บอกยอดที่เคลื่อนไม่ได้เพราะอ่านจำนวน %q ไม่ออก", oneLine(p.qty))
	}
	direction := "ยอดบิลจะน้อยกว่าต้นทาง"
	lost := p.lost
	if lost.Sign() < 0 {
		direction = "ยอดบิลจะมากกว่าต้นทาง"
		lost = new(big.Rat).Neg(lost)
	}
	return head + fmt.Sprintf(" — ส่วนต่าง × จำนวน %s = %s บาท · %s",
		p.qty, lost.FloatString(moneyScale), direction)
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
	var priceProblems []landedPriceProblem
	// Per-invoice totals, so the refusal can say how much each bill moves rather than
	// only how much one line does.
	drift := map[string]*big.Rat{}
	var order []string
	priceDrift := map[string]*big.Rat{}
	var priceOrder []string
	// Bills holding at least one line whose drift could not be computed, so the per-bill
	// figure is reported as a floor rather than as the whole of it.
	priceUnknown := map[string]bool{}

	for _, p := range plans {
		for _, l := range p.lines {
			// THE PRICE COLUMN, checked first because it is the newer of the two and the
			// one a reader is less likely to expect. A line can only arrive here with an
			// over-precise price when planBillLines could not fold it, which needs both
			// literals readable — so in practice this fires on a line whose quantity is
			// unreadable and whose price carries satang the column will not keep.
			if fits, measurable := priceSurvivesTheColumn(l.price); measurable && !fits {
				if written, ok := priceAsWritten(l.price); ok {
					// lost is nil when the QUANTITY is unreadable, which is the usual way
					// a line gets this far. The money it moves cannot be stated then —
					// but the line is still named, because "I could not price the damage"
					// must not come out as "there is no damage". That is precisely the
					// silence this check exists to break.
					lost, _ := priceValueLost(l.price, l.qty)
					priceProblems = append(priceProblems, landedPriceProblem{
						invoiceNo: p.no, hn: p.hn, code: l.code, name: l.name,
						sourcePx: strings.TrimSpace(l.price),
						written:  written.FloatString(moneyScale),
						qty:      strings.TrimSpace(l.qty), lost: lost,
					})
					if _, seen := priceDrift[p.no]; !seen {
						priceDrift[p.no] = new(big.Rat)
						priceOrder = append(priceOrder, p.no)
					}
					if lost != nil {
						priceDrift[p.no].Add(priceDrift[p.no], lost)
					} else {
						priceUnknown[p.no] = true
					}
				}
			}

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
	if len(problems) == 0 && len(priceProblems) == 0 {
		if rep != nil {
			rep.notef("invoice_item.qty เป็นคอลัมน์จำนวนเต็ม และ invoice_item.price เก็บทศนิยม %d ตำแหน่ง — "+
				"ตรวจแล้วว่าทุกบรรทัดมีจำนวนเต็มจำนวนและราคาไม่เกินทศนิยมที่คอลัมน์เก็บได้ "+
				"จึงไม่มีบรรทัดใดเสียมูลค่าตอนเขียนลงฐาน", moneyScale)
		}
		return nil
	}
	if len(problems) == 0 {
		return fmt.Errorf("%s", describeLandedPriceProblems(priceProblems, priceDrift, priceOrder, priceUnknown))
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

	msg := fmt.Sprintf("bills: %d บรรทัดใน %d ใบมีจำนวนเป็นเศษ ซึ่ง invoice_item.qty "+
		"(คอลัมน์จำนวนเต็ม) เก็บไม่ได้ — Postgres จะปัดให้เงียบ ๆ แล้วยอดบิลจะไม่ใช่ยอดเดิม "+
		"จึงปฏิเสธตั้งแต่ตอนสร้างไฟล์ ยังไม่มีอะไรแตะฐานข้อมูล · "+
		"ยอดที่จะเคลื่อนต่อใบ (บวก = ยอดหาย): %s · รวม %s บาท · "+
		"ทางแก้: แก้จำนวนที่ต้นทางให้เป็นจำนวนเต็มแล้วปรับราคาต่อหน่วยให้ยอดรวมเท่าเดิม "+
		"(เช่น จำนวน 0.2 ราคา 500 → จำนวน 1 ราคา 100) แล้วดึงข้อมูลใหม่ · รายการทั้งหมด:\n  %s",
		len(problems), len(order),
		strings.Join(perBill, " · "), total.FloatString(2),
		strings.Join(lines, "\n  "))

	// Both at once gets both refusals, kept apart. They are different problems at the
	// source with different fixes, and merging them under one count would describe
	// neither.
	if len(priceProblems) > 0 {
		msg += "\n\n" + describeLandedPriceProblems(priceProblems, priceDrift, priceOrder, priceUnknown)
	}
	return fmt.Errorf("%s", msg)
}

// describeLandedPriceProblems is the price column's refusal, in the same shape as the
// quantity one: every line named at once, the money per bill, and what to fix.
func describeLandedPriceProblems(problems []landedPriceProblem,
	drift map[string]*big.Rat, order []string, unknown map[string]bool) string {

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
		// "อย่างน้อย" on a bill where one line could not be priced: the figure is a floor,
		// and presenting a floor as the total is how a number stops being trustworthy.
		at := ""
		if unknown[no] {
			at = " (อย่างน้อย — มีบรรทัดที่คิดไม่ได้)"
		}
		perBill = append(perBill, fmt.Sprintf("%s %s บาท%s", no, drift[no].FloatString(moneyScale), at))
		total.Add(total, drift[no])
	}

	return fmt.Sprintf("bills: %d บรรทัดใน %d ใบมีราคาต่อหน่วยที่มีทศนิยมเกิน %d ตำแหน่ง ซึ่ง "+
		"invoice_item.price (numeric(12,%d)) เก็บไม่ได้ — Postgres จะปัดให้เงียบ ๆ แล้วยอดบิล "+
		"จะไม่ใช่ยอดเดิม จึงปฏิเสธตั้งแต่ตอนสร้างไฟล์ ยังไม่มีอะไรแตะฐานข้อมูล · "+
		"ปกติเครื่องมือจะย้ายมูลค่าทั้งบรรทัดไปไว้ที่ราคาให้เอง (จำนวน × ราคา → จำนวน 1 ราคารวม) "+
		"บรรทัดเหล่านี้ทำไม่ได้เพราะอ่านค่าจำนวนไม่ออก · "+
		"ยอดที่จะเคลื่อนต่อใบ (บวก = ยอดหาย): %s · รวม %s บาท · "+
		"ทางแก้: แก้ที่ต้นทางให้ราคาต่อหน่วยลงตัวในทศนิยม %d ตำแหน่ง แล้วดึงข้อมูลใหม่ "+
		"(เช่น 28 × 3.571428572 → 28 × 3.57 แล้วเติมส่วนต่างที่บรรทัดใดบรรทัดหนึ่ง) · รายการทั้งหมด:\n  %s",
		len(problems), len(order), moneyScale, moneyScale,
		strings.Join(perBill, " · "), total.FloatString(moneyScale), moneyScale,
		strings.Join(lines, "\n  "))
}

// reportFoldedQuantities names every line whose value was moved into its price.
//
// One warning per REASON, not one per line, and a WARNING rather than a note: the unit
// price on these lines is no longer the catalogue price, and a clinic reading its own
// invoice has to be told that rather than discovering it.
//
// Split by reason because the two say different things about the source. "Your quantity
// was 16.25" sends someone to look at a stock count; "your unit price was 3.571428572"
// sends them to look at a pack price divided by a pack size. One message covering both
// would send everyone to the wrong place.
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

	residue := new(big.Rat)
	for _, f := range sorted {
		residue.Add(residue, f.residue)
	}

	// foldForBoth belongs in both lists: the line really did break both columns, and a
	// reader of either message needs to see it.
	for _, group := range []struct {
		why   whyFolded
		cause string
		fix   string
	}{
		{
			why: foldForQty,
			cause: "มีจำนวนเป็นเศษ ซึ่ง invoice_item.qty (คอลัมน์จำนวนเต็ม) เก็บไม่ได้ " +
				"จึงย้ายเศษไปไว้ที่ราคาตามมติเจ้าของคลินิก (5 ต.ค. 2569)",
			fix: "ถ้าอยากให้จำนวนจริงอยู่บนใบเสร็จ ต้องแก้ที่ต้นทางให้จำนวนเป็นจำนวนเต็ม",
		},
		{
			why: foldForPrice,
			cause: fmt.Sprintf("มีราคาต่อหน่วยที่มีทศนิยมเกิน %d ตำแหน่ง ซึ่ง invoice_item.price "+
				"(numeric(12,%d)) เก็บไม่ได้ จึงย้ายมูลค่าทั้งบรรทัดไปไว้ที่ราคาตามมติเจ้าของคลินิก "+
				"(6 ต.ค. 2569)", moneyScale, moneyScale),
			fix: fmt.Sprintf("มักเกิดจากต้นทางเอาราคาแพ็กหารจำนวนเม็ด (100 ÷ 28 = 3.571428572) "+
				"ถ้าอยากให้ราคาต่อหน่วยและจำนวนจริงอยู่บนใบเสร็จ ต้องแก้ที่ต้นทางให้ราคาลงตัว "+
				"ในทศนิยม %d ตำแหน่ง", moneyScale),
		},
	} {
		bills := map[string]bool{}
		lines := make([]string, 0, len(sorted))
		for _, f := range sorted {
			if f.why != group.why && f.why != foldForBoth {
				continue
			}
			bills[f.invoiceNo] = true
			lines = append(lines, fmt.Sprintf("เลขที่ %s รายการ %s %q: %s × %s บาท → จำนวน 1 ราคา %s บาท",
				f.invoiceNo, firstNonEmpty(f.code, "(ไม่มีรหัส)"), oneLine(f.name),
				f.srcQty, f.srcPrice, f.newPrice))
		}
		if len(lines) == 0 {
			continue
		}
		rep.warnf("invoice_item: %d บรรทัดใน %d ใบ%s — "+
			"**ยอดบิลเท่าเดิมทุกสตางค์** ใบเสร็จยังกระทบยอดตรง และสถานะไม่เปลี่ยน · "+
			"**ผลข้างเคียงที่ต้องรู้: ราคาต่อหน่วยในบรรทัดเหล่านี้ไม่ใช่ราคาในแคตตาล็อกอีกแล้ว** "+
			"(ใบเสร็จจะพิมพ์ว่า 1 × ยอดรวม) · ราคาในแคตตาล็อก (catalog_item.price) ไม่ถูกแตะ "+
			"และชื่อรายการไม่ถูกต่อท้าย จำนวนจริงอยู่ในรายการข้างล่างนี้ · %s · %s",
			len(lines), len(bills), group.cause, group.fix, strings.Join(lines, " · "))
	}

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
