package legacyimport

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"system_cliv4/internal/mongodump"
)

// Money import: the legacy sales history onto V4's document spine.
//
//	bills            → invoice + invoice_item
//	payment_summary  → receipt + receipt_payment
//
// WHY THIS IS SQL AND NOT AN API CALL
//
// The obvious alternative was to replay each bill through /v3/checkout and inherit its
// invariants for free. It cannot work here: checkout MINTS new document numbers (the
// clinic must keep the numbers its patients hold receipts for), it has no way to record
// a bill that was issued and never paid, it writes doctor-fee and loyalty ledgers the
// clinic did not ask to backfill, and 1,267 separate HTTP calls are 1,267 separate
// transactions — a failure halfway leaves half a year of history imported.
//
// So the invariants checkout would have given us are written out as SQL guards instead
// (see money_guards.go), inside the same transaction as the rows.
//
// WHAT THE NUMBERS ARE DERIVED FROM
//
// invoice.paid_amount and invoice.bill_discount are NOT copied from the bill. They are
// recomputed from the receipts that were actually imported, using V4's own formula
// (AddInvoicePayment in internal/db/query/invoice.sql). That makes "the invoice says it
// was paid ฿X" and "these receipts add up to ฿X" the same statement instead of two
// numbers that can drift. The outstanding balance then falls out of the arithmetic and
// matches the legacy price_total_balance — which is checked, per bill, before anything
// is written.
//
// COURSE BALANCES ARE NOT WRITTEN. A course PURCHASE line lands as
// invoice_item(kind='course'), because the money has to add up, but no course_bought
// row is created: that ledger is a separate decision and the target already holds rows
// for it.

// Money entity names, in write order. The settle pass is last because it reads the
// items and receipts the two passes before it created.
const (
	EntInvoice        = "invoice"
	EntInvoiceItem    = "invoice_item"
	EntReceipt        = "receipt"
	EntReceiptPayment = "receipt_payment"
	EntInvoiceSettle  = "invoice(settle)"
)

var moneyOrder = []string{
	EntInvoice,
	EntInvoiceItem,
	EntReceipt,
	EntReceiptPayment,
	EntInvoiceSettle,
}

// moneyBatches is the rendered SQL for the money block, one field per entity.
type moneyBatches struct {
	invoice        string
	invoiceItem    string
	receipt        string
	receiptPayment string
	settle         string

	// invoiceHN / receiptHN are the document numbers this run will write, each mapped
	// to the HN it belongs to. Guard 4 needs the pair, not just the number: a number
	// that already exists for the SAME patient is a re-run, while the same number
	// against a DIFFERENT patient is a real collision.
	invoiceHN map[string]string
	receiptHN map[string]string
	// hns is every patient the money is filed against, for guard 5.
	hns map[string]bool

	// courseSale is course_bills _id → the invoice line that sold it, for the entitlement
	// pass. Only bills that survived every check are in here, so an entitlement can
	// never point at a line this script did not write.
	courseSale map[string]courseSaleRef
	// invoiceNoByBillID is bills _id → invoice no, for entitlements that know only their
	// bill.
	invoiceNoByBillID map[string]string
}

// keySet reduces a no→hn map to the set of document numbers.
func keySet(m map[string]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// billPlan is one legacy bill that passed validation, with everything the four writers
// need already resolved. Building this list first means a bill that fails a money check
// is dropped BEFORE any of its four tables get a statement — there is no way to emit an
// invoice whose items were rejected.
type billPlan struct {
	mongoID     string
	no          string // invoice.no — the legacy running number (B6: carried over)
	hn          string
	patientName string
	branchCode  string
	createdAt   string // SQL literal
	visitDate   string // SQL literal (date)
	lines       []linePlan

	// totals, kept as source text so nothing passes through float64 on its way to a
	// numeric column. The float copies are for CHECKING only.
	priceTotal string
	balance    string

	// discountExpr is the bill-level discount, rendered as SQL ARITHMETIC over the
	// source digits — e.g. (1200 - (1020) - 0) — so Postgres computes it in `numeric`
	// and the generated file shows where the number came from. Doing the subtraction in
	// Go would put ฿0.10 through float64, which cannot represent it.
	discountExpr string

	fPriceTotal float64
	fBalance    float64
	fLineTotal  float64
}

// linePlan is one bills.list[] entry mapped onto invoice_item.
type linePlan struct {
	kind  string
	code  string
	name  string
	price string
	qty   string
	// sortOrder is assigned 0..n-1 per bill, NOT copied from the legacy `indexof`,
	// which repeats within a bill. See the renumbering in planBillLines.
	sortOrder   int
	legacyIndex int // legacy `indexof`, used only to preserve display order
	filePos     int // position in list[], the tie-break that keeps output deterministic
	useCourse   bool

	// courseBillID is the course_bills entitlement this line SOLD (legacy
	// `course_bill`). It is how an entitlement finds the exact bill line it was bought
	// on, which is the only honest way to fill course_bought.invoice_item_id.
	courseBillID string
}

// courseSaleRef locates the invoice line that sold one entitlement. The invoice_item id
// is generated by the database, so the line is addressed by (invoice no, sort_order) —
// a pair that is unique because sort_order is renumbered per bill.
type courseSaleRef struct {
	invoiceNo string
	sortOrder int
}

// receiptPlan is one payment_summary row mapped onto receipt + receipt_payment.
type receiptPlan struct {
	no            string
	invoiceNo     string
	installmentNo int
	legacyOrder   int
	hn            string
	patientName   string
	branchCode    string
	doctorCode    string
	remark        string
	payDate       string // SQL literal (date)
	createdAt     string // SQL literal
	pointApply    string
	insurer       string
	approveCode   string
	channels      []channelPlan

	// paidExpr is the MONEY that actually came in: the channel amounts added up as SQL.
	// discountExpr is what this receipt closed WITHOUT money — the difference between the
	// bill value it settled (payment_type.price_total) and the money.
	//
	// 218 receipts in this export closed a bill with no money at all: vouchers, birthday
	// gifts, review comps, service recovery. Reading price_total as "money received"
	// would book every one of them as cash in the till.
	paidExpr     string
	discountExpr string
	// amountTexts are the individual channel amounts, so the BILL-level discount can be
	// rendered as arithmetic over the same digits.
	amountTexts []string

	fPaid   float64 // = Σ channels
	fClosed float64 // = payment_type.price_total
}

// channelPlan is one non-zero payment channel inside a receipt.
type channelPlan struct {
	index  int
	method string
	amount string
}

// buildMoney renders the whole money block. It returns empty batches when the export
// carries no bills, so a catalog-only import is unaffected.
func buildMoney(mc *mongoCatalog, in Input, rep *Report) (moneyBatches, error) {
	var out moneyBatches
	if len(mc.bills) == 0 {
		return out, nil
	}

	eInv := rep.entity(EntInvoice)
	eItem := rep.entity(EntInvoiceItem)
	eRec := rep.entity(EntReceipt)
	ePay := rep.entity(EntReceiptPayment)
	eInv.SourceRows = len(mc.bills)
	eRec.SourceRows = len(mc.payments)

	// ── pass 1: which bills are in, and do their own numbers add up ───────────
	plans, err := planBills(mc, rep, eInv, eItem)
	if err != nil {
		return out, err
	}
	if len(plans) == 0 {
		rep.warnf("bills: ไม่มีบิลที่นำเข้าได้เลยจาก %d แถว — ตรวจสถานะบิลในไฟล์ต้นทาง", len(mc.bills))
		return out, nil
	}

	byBillID := map[string]*billPlan{}
	for i := range plans {
		byBillID[plans[i].mongoID] = &plans[i]
	}

	// ── pass 2: the receipts, attached to the bills that survived pass 1 ──────
	receipts, err := planReceipts(mc, byBillID, rep, eRec, ePay)
	if err != nil {
		return out, err
	}

	// A bill's discount is whatever the receipts did not collect and the patient does
	// not still owe, so it can only be worked out once the receipts are known.
	plans, receipts = balanceBills(plans, receipts, rep, eInv)
	if len(plans) == 0 {
		rep.warnf("bills: ทุกบิลตกด่านตรวจยอด — ไม่มีอะไรถูกนำเข้า")
		return out, nil
	}

	out.invoiceHN = map[string]string{}
	out.hns = map[string]bool{}
	for _, p := range plans {
		out.invoiceHN[p.no] = p.hn
		if p.hn != "" {
			out.hns[p.hn] = true
		}
	}
	out.receiptHN = map[string]string{}
	for _, r := range receipts {
		out.receiptHN[r.no] = r.hn
	}

	// Where each entitlement was sold. Built from the bills that SURVIVED every check,
	// so the entitlement pass cannot point at an invoice line that was never written.
	out.courseSale = map[string]courseSaleRef{}
	out.invoiceNoByBillID = map[string]string{}
	for _, p := range plans {
		out.invoiceNoByBillID[p.mongoID] = p.no
		for _, l := range p.lines {
			if l.courseBillID != "" {
				out.courseSale[l.courseBillID] = courseSaleRef{invoiceNo: p.no, sortOrder: l.sortOrder}
			}
		}
	}

	// ── render ────────────────────────────────────────────────────────────────
	out.invoice = renderInvoices(plans, in, eInv)
	out.invoiceItem = renderInvoiceItems(plans, in, eItem)
	out.receipt = renderReceipts(receipts, in, eRec)
	out.receiptPayment = renderReceiptPayments(receipts, in, ePay)
	out.settle = renderSettle(keySet(out.invoiceHN), in)
	rep.entity(EntInvoiceSettle).Statements = 1
	rep.entity(EntInvoiceSettle).SourceRows = len(plans)

	rep.notef("เงิน: นำเข้า %d บิล (invoice) · %d บรรทัด (invoice_item) · %d ใบรับเงิน (receipt) จาก bills %d / payment_summary %d แถว",
		len(plans), eItem.Statements, len(receipts), len(mc.bills), len(mc.payments))
	rep.notef("invoice.no / receipt.receipt_no ยกเลขเดิมจากระบบเก่ามาทั้งหมด และไม่แตะ doc_counter — เลขชุดใหม่ที่ระบบออกต่อจากนี้จึงเริ่มนับใหม่ ไม่ต่อจากเลขที่ยกมา")
	rep.notef("VAT = %s ทุกใบ เพราะต้นทางไม่มีข้อมูล VAT (bills.price_vat เป็น 0 ทุกแถว และ price_ex_vat = price_total) — ไม่ได้คิดอัตราขึ้นเอง",
		MoneyVATRate)
	rep.notef("invoice.paid_amount / bill_discount / status ไม่ได้คัดลอกจากบิล แต่คำนวณจาก receipt ที่นำเข้าจริงด้วยสูตรเดียวกับ AddInvoicePayment ของ V4 — ยอดในใบกับยอดในใบเสร็จจึงเป็นตัวเลขเดียวกันโดยโครงสร้าง")
	if MoneyUseBranchDocNumbers {
		rep.notef("ใช้เลขที่เอกสารแบบรายสาขา (invoice_branch_no / receipt_branch_no) — เลขระดับคลินิก (invoice_no / receipt_no) ไม่ถูกนำเข้า เพราะ V4 มีคอลัมน์เลขที่เดียวต่อเอกสาร")
	} else {
		rep.notef("ใช้เลขที่เอกสารระดับคลินิก (invoice_no / receipt_no) — เลขรายสาขา (invoice_branch_no / receipt_branch_no) ไม่ถูกนำเข้า เพราะ V4 มีคอลัมน์เลขที่เดียวต่อเอกสาร")
	}
	rep.notef("course_bought (ยอดคอร์สคงเหลือของลูกค้า) ไม่ถูกเขียนในรอบนี้ตามมติ — บรรทัดคอร์สลงเป็น invoice_item(kind='course') เพื่อให้ยอดเงินครบ แต่สิทธิ์คงเหลือยังไม่ถูกสร้าง")

	return out, nil
}

// planBills validates and maps every bill, dropping the ones policy excludes.
func planBills(mc *mongoCatalog, rep *Report, eInv, eItem *EntityResult) ([]billPlan, error) {
	var plans []billPlan
	seenNo := map[string]string{} // invoice no → bill _id, to catch in-file duplicates
	unknownTypes := map[string]int{}
	noCodeLines, useCourseLines, coursePurchaseLines := 0, 0, 0
	statusSkips := map[string]int{}

	for n, d := range mc.bills {
		mongoID := docObjectID(d, "_id")

		importIt, reason := billImportable(d.Text("status"))
		if !importIt {
			statusSkips[reason]++
			eInv.skip(reason)
			continue
		}

		no := billDocNo(d)
		if no == "" {
			eInv.skip("ไม่มีเลขที่ใบเสร็จ/ใบแจ้งหนี้ในต้นทาง")
			rep.warnf("bills doc %d (%s): ไม่มีทั้ง invoice_no และ invoice_branch_no — ข้ามบิลนี้ เพราะ invoice.no ว่างไม่ได้", n+1, mongoID)
			continue
		}
		if prev, dup := seenNo[no]; dup {
			// Two bills claiming one document number. There is no safe merge: one of
			// them is a real charge that would vanish. Refuse the run — the same rule
			// the duplicate-HN case follows.
			return nil, fmt.Errorf("bills: เลขที่เอกสาร %q ซ้ำในไฟล์ (bill %s และ %s) — invoice.no ต้องไม่ซ้ำต่อคลินิก จึงปฏิเสธทั้งสคริปต์",
				no, prev, mongoID)
		}
		seenNo[no] = mongoID

		hn := strings.TrimSpace(d.Text("hn"))
		if hn == "" {
			eInv.skip("ไม่มี HN — ไม่รู้ว่าเป็นบิลของคนไข้คนไหน")
			rep.warnf("bills %s (เลขที่ %s): ไม่มี HN — ข้ามบิลนี้ เพราะเก็บเงินโดยไม่รู้เจ้าของบิลไม่ได้", mongoID, no)
			continue
		}

		branch := firstNonEmpty(d.Text("branch_code"), BranchCode)
		name, _ := clip(d.Text("patient_name"), 255)

		bp := billPlan{
			mongoID:     mongoID,
			no:          no,
			hn:          hn,
			patientName: name,
			branchCode:  branch,
			createdAt:   mongoTSLit(d, "created_at", mongoTSLit(d, "visitdate", "now()")),
			visitDate:   mongoDateLit(d, "visitdate", "NULL"),
			priceTotal:  moneyLit(d.Text("price_total")),
			balance:     moneyLit(d.Text("price_total_balance")),
			fPriceTotal: moneyFloat(d.Text("price_total")),
			fBalance:    moneyFloat(d.Text("price_total_balance")),
		}

		lines, lineTotal, lerr := planBillLines(d, mongoID, no, rep, eItem, unknownTypes,
			&noCodeLines, &useCourseLines, &coursePurchaseLines)
		if lerr != nil {
			return nil, lerr
		}
		if len(lines) == 0 {
			eInv.skip("ไม่มีรายการในบิล (list ว่าง)")
			rep.warnf("bills %s (เลขที่ %s): ไม่มีรายการในบิลเลย — ข้าม เพราะใบที่ไม่มีรายการคิดยอดไม่ได้", mongoID, no)
			continue
		}
		bp.lines = lines
		bp.fLineTotal = lineTotal
		plans = append(plans, bp)
	}

	if len(unknownTypes) > 0 {
		var parts []string
		for t, c := range unknownTypes {
			parts = append(parts, fmt.Sprintf("%s×%d", t, c))
		}
		sort.Strings(parts)
		rep.warnf("invoice_item: มีชนิดรายการที่ mapping นี้ไม่รู้จัก (%s) — บรรทัดนั้นไม่ถูกนำเข้า เพราะ invoice_item.kind เป็นตัวตัดสินว่าจะตัดสต็อกหรือไม่ เดาผิดคือสต็อกเพี้ยน",
			strings.Join(parts, ", "))
	}
	if noCodeLines > 0 {
		rep.notef("invoice_item: %d บรรทัดไม่มีรหัสสินค้าในต้นทาง (บรรทัดขายคอร์ส) — ลงเป็น code ว่างและ catalog_item_id = NULL ตามที่ต้นทางมี",
			noCodeLines)
	}
	if useCourseLines > 0 || coursePurchaseLines > 0 {
		rep.notef("invoice_item คอร์ส: %d บรรทัดเป็นการ 'ขายคอร์ส' (use_course=false · V4 นับเป็นยอดขายคอร์ส) · %d บรรทัดเป็นการ 'ตัดสิทธิ์คอร์ส' (use_course=true · ราคา 0 ไม่เพิ่มยอดบิล) — แยกด้วยฟิลด์ course_bill ของต้นทาง ไม่ได้เดาจากรหัสสินค้า",
			coursePurchaseLines, useCourseLines)
	}
	for reason, cnt := range statusSkips {
		rep.notef("bills: ข้าม %d ใบ เพราะ %s", cnt, reason)
	}
	return plans, nil
}

// planBillLines maps one bill's list[] onto invoice_item rows and returns their net
// total, which is what the bill's own price_total is checked against.
func planBillLines(d mongodump.Doc, mongoID, no string, rep *Report, eItem *EntityResult,
	unknownTypes map[string]int, noCodeLines, useCourseLines, coursePurchaseLines *int) ([]linePlan, float64, error) {

	raw, ok := d.Get("list")
	if !ok || raw == nil {
		return nil, 0, nil
	}
	arr, isArr := raw.([]any)
	if !isArr {
		return nil, 0, fmt.Errorf("bills %s (เลขที่ %s): field list ไม่ใช่ array", mongoID, no)
	}

	var lines []linePlan
	var total float64
	for i, item := range arr {
		line, isDoc := item.(mongodump.Doc)
		if !isDoc {
			continue
		}
		eItem.SourceRows++

		legacyType := line.Text("types")
		kind, known := invoiceItemKind(legacyType)
		if !known {
			unknownTypes[firstNonEmpty(legacyType, "(ว่าง)")]++
			eItem.skip("ชนิดรายการไม่รู้จัก: " + firstNonEmpty(legacyType, "(ว่าง)"))
			continue
		}

		code := strings.TrimSpace(line.Text("code"))
		name := firstNonEmpty(line.Text("name"), line.Text("other_name"), code)
		if name == "" {
			// invoice_item.name is NOT NULL and a nameless charge is unreadable on a
			// receipt, so it is reported rather than filled with a placeholder.
			eItem.skip("ไม่มีชื่อรายการ")
			rep.warnf("bills %s (เลขที่ %s) บรรทัดที่ %d: ไม่มีทั้ง name และ code — ไม่นำเข้าบรรทัดนี้", mongoID, no, i+1)
			continue
		}
		if code == "" {
			*noCodeLines++
		}

		// Selling a course and redeeming one are both types='course' in the legacy data,
		// and `course_bill` is what tells them apart. Measured over this export:
		//
		//	course_bill set,   no code   146 lines  ฿1,910,729  → the course was SOLD
		//	no course_bill,    code set  447 lines  ฿0          → a session was REDEEMED
		//
		// The direction matters: V4 reads course SALES as kind='course' AND
		// use_course=false (CourseBoughtLines, DashboardCourseLineCount). Marking a sale
		// as a redemption reports ฿1.9M of course revenue as zero. It does not move any
		// bill total — a redeemed session is priced at ฿0 — but it decides whether the
		// clinic can see what it sold.
		useCourse := kind == "course" && docObjectID(line, "course_bill") == ""
		if useCourse {
			*useCourseLines++
		} else if kind == "course" {
			*coursePurchaseLines++
		}

		nameLit, _ := clip(name, 500)
		codeLit, _ := clip(code, 64)
		price := line.Text("price")
		qty := firstNonEmpty(line.Text("amount"), "1")

		lines = append(lines, linePlan{
			kind:         kind,
			code:         codeLit,
			name:         nameLit,
			price:        moneyLit(price),
			qty:          moneyLit(qty),
			legacyIndex:  intFromMoney(firstNonEmpty(line.Text("indexof"), strconv.Itoa(i+1))),
			filePos:      i,
			useCourse:    useCourse,
			courseBillID: docObjectID(line, "course_bill"),
		})

		// V4's net-total formula: GREATEST(price*qty - discount, 0), and the legacy
		// line carries no per-line discount (discounts live on the receipt).
		lineNet := moneyFloat(price) * moneyFloat(qty)
		if lineNet > 0 {
			total += lineNet
		}
	}

	// sort_order is RENUMBERED 0..n-1 in the source's own display order, and that is
	// what makes the line insert idempotent: (clinic_code, invoice_id, sort_order) is
	// unique by construction, so re-running the script writes nothing twice.
	//
	// The legacy `indexof` cannot be used directly. It repeats within a bill, and a bill
	// legitimately carries the SAME item twice (two of the same treatment in one visit).
	// Keying on (sort_order, name) instead dropped the second copy silently — bill
	// 680007804 came out at ฿10,000 against ฿20,000 collected, which guard 3 caught.
	sort.SliceStable(lines, func(a, b int) bool {
		if lines[a].legacyIndex != lines[b].legacyIndex {
			return lines[a].legacyIndex < lines[b].legacyIndex
		}
		return lines[a].filePos < lines[b].filePos
	})
	for i := range lines {
		lines[i].sortOrder = i
	}
	return lines, total, nil
}

// planReceipts maps payment_summary onto receipt + receipt_payment, attached to the
// bills that survived. A receipt whose bill was skipped is skipped too: money filed
// against a document that does not exist is money nobody can find again.
func planReceipts(mc *mongoCatalog, byBillID map[string]*billPlan, rep *Report,
	eRec, ePay *EntityResult) ([]receiptPlan, error) {

	// Group by bill first so installment numbers can be checked for collisions:
	// receipt has a UNIQUE (clinic_code, invoice_id, installment_no).
	type pending struct {
		doc mongodump.Doc
		bp  *billPlan
	}
	byBill := map[string][]pending{}
	var billOrder []string
	statusSkips := map[string]int{}
	orphans := 0

	for _, d := range mc.payments {
		importIt, reason := paymentImportable(d.Text("status"))
		if !importIt {
			statusSkips[reason]++
			eRec.skip(reason)
			continue
		}
		billID := docObjectID(d, "bill_id")
		bp, ok := byBillID[billID]
		if !ok {
			orphans++
			eRec.skip("bill_id ไม่ตรงกับบิลที่นำเข้า (บิลถูกข้าม หรือไม่มีในไฟล์)")
			continue
		}
		if _, seen := byBill[billID]; !seen {
			billOrder = append(billOrder, billID)
		}
		byBill[billID] = append(byBill[billID], pending{doc: d, bp: bp})
	}

	seenNo := map[string]string{}
	renumbered := 0
	percentDiscount := 0
	noMoneyReceipts := 0
	var channelMismatch []string
	var out []receiptPlan

	sort.Strings(billOrder) // deterministic output
	for _, billID := range billOrder {
		group := byBill[billID]
		// Sort by the legacy instalment number, then by time, so the order in the file
		// cannot change the numbering.
		sort.SliceStable(group, func(i, j int) bool {
			oi, oj := intFromMoney(group[i].doc.Text("order")), intFromMoney(group[j].doc.Text("order"))
			if oi != oj {
				return oi < oj
			}
			return mongoTime(group[i].doc, "created_at").Before(mongoTime(group[j].doc, "created_at"))
		})

		// Keep the legacy instalment number when the bill's numbers are unique; fall
		// back to a sequence when they collide, because the UNIQUE index would reject
		// the second row and take the whole import down with it.
		uniq := map[int]bool{}
		collide := false
		for _, p := range group {
			o := intFromMoney(p.doc.Text("order"))
			if o <= 0 || uniq[o] {
				collide = true
				break
			}
			uniq[o] = true
		}

		for i, p := range group {
			d := p.doc
			no := receiptDocNo(d)
			if no == "" {
				eRec.skip("ไม่มีเลขที่ใบเสร็จในต้นทาง")
				rep.warnf("payment_summary %s: ไม่มีทั้ง receipt_no และ receipt_branch_no — ข้าม เพราะ receipt.receipt_no ว่างไม่ได้",
					docObjectID(d, "_id"))
				continue
			}
			if prev, dup := seenNo[no]; dup {
				return nil, fmt.Errorf("payment_summary: เลขที่ใบเสร็จ %q ซ้ำในไฟล์ (%s และ %s) — receipt_no ต้องไม่ซ้ำต่อคลินิก จึงปฏิเสธทั้งสคริปต์",
					no, prev, docObjectID(d, "_id"))
			}
			seenNo[no] = docObjectID(d, "_id")

			legacyOrder := intFromMoney(d.Text("order"))
			installment := legacyOrder
			if collide {
				installment = i + 1
				if installment != legacyOrder {
					renumbered++
				}
			}

			rp := receiptPlan{
				no:            no,
				invoiceNo:     p.bp.no,
				installmentNo: installment,
				legacyOrder:   legacyOrder,
				hn:            p.bp.hn,
				patientName:   p.bp.patientName,
				branchCode:    firstNonEmpty(d.Text("paid_branch.branch_code"), p.bp.branchCode, BranchCode),
				doctorCode:    strings.TrimSpace(d.Text("doctor")),
				remark:        strings.TrimSpace(d.Text("payment_remark")),
				payDate:       mongoDateLit(d, "created_at", "NULL"),
				createdAt:     mongoTSLit(d, "created_at", "now()"),
				pointApply:    moneyLit(firstNonEmpty(d.Text("use_point"), "0")),
				fClosed:       moneyFloat(d.Text("payment_type.price_total")),
			}
			if insurer := strings.TrimSpace(d.Text("insurance.insurance_name")); insurer != "" && insurer != "0" {
				rp.insurer, _ = clip(insurer, 255)
			}
			if appr := strings.TrimSpace(d.Text("gov_direct.appr_code")); appr != "" {
				rp.approveCode, _ = clip(appr, 64)
			}

			// One receipt_payment per channel that carries money. A zero channel is not
			// written: a receipt line for ฿0 is noise on a document people read.
			idx := 0
			for _, ch := range moneyChannels {
				amt := d.Text("payment_type." + ch.field)
				if !positiveMoney(amt) {
					continue
				}
				amountLit := moneyLit(amt)
				rp.channels = append(rp.channels, channelPlan{index: idx, method: ch.method, amount: amountLit})
				rp.amountTexts = append(rp.amountTexts, amountLit)
				rp.fPaid += moneyFloat(amt)
				idx++
			}
			rp.paidExpr = sumExpr(rp.amountTexts)
			rp.discountExpr = "(" + moneyLit(d.Text("payment_type.price_total")) + " - " + rp.paidExpr + ")"

			// A receipt cannot take in MORE money than the bill value it settled — that
			// would be a negative discount, which is an overpayment nobody recorded.
			// One receipt is dropped and named, rather than the whole import refused.
			if rp.fPaid-rp.fClosed > MoneyTolerance {
				eRec.skip("รับเงินมากกว่ายอดที่ใบเสร็จปิด (ส่วนลดจะติดลบ)")
				if len(channelMismatch) < 10 {
					channelMismatch = append(channelMismatch,
						fmt.Sprintf("%s (รับ %.2f > ปิดยอด %.2f)", no, rp.fPaid, rp.fClosed))
				}
				delete(seenNo, no)
				continue
			}
			if receiptDiscountPercentActive(d) {
				percentDiscount++
			}
			if rp.fPaid == 0 && rp.fClosed > 0 {
				noMoneyReceipts++
			}

			ePay.SourceRows += len(rp.channels)
			out = append(out, rp)
		}
	}

	for reason, cnt := range statusSkips {
		rep.notef("payment_summary: ข้าม %d แถว เพราะ %s", cnt, reason)
	}
	if orphans > 0 {
		rep.warnf("payment_summary: %d แถวอ้าง bill_id ที่ไม่ได้นำเข้า (บิลถูกข้ามตามสถานะ หรือไม่มีในไฟล์) — เงินก้อนนี้ไม่ถูกนำเข้า ต้องตามเก็บด้วยมือถ้าเป็นรายการจริง",
			orphans)
	}
	if renumbered > 0 {
		rep.warnf("receipt: %d ใบถูกเปลี่ยนเลขงวด (installment_no) เพราะเลขงวดในต้นทางซ้ำหรือว่าง — V4 บังคับว่าเลขงวดต่อใบแจ้งหนี้ต้องไม่ซ้ำ · เลขที่ใบเสร็จ (receipt_no) ยังเป็นเลขเดิม",
			renumbered)
	}
	if len(channelMismatch) > 0 {
		rep.warnf("receipt: ข้ามใบเสร็จที่รับเงินมากกว่ายอดที่ปิด (ส่วนลดจะติดลบ) — ตัวอย่าง: %s · บิลของใบเสร็จเหล่านี้จะถูกข้ามตามไปด้วย ต้องตรวจด้วยมือ",
			strings.Join(channelMismatch, "; "))
	}
	if noMoneyReceipts > 0 {
		rep.notef("receipt: %d ใบปิดยอดโดยไม่มีเงินเข้าเลย (voucher / ของแถม / ชดเชยบริการ) — ลงเป็น paid_amount = 0 และ discount เท่ากับยอดที่ปิด · เหตุผลเดิมจากช่อง payment_remark ถูกยกไปเก็บใน receipt.note ด้วย",
			noMoneyReceipts)
	}
	if percentDiscount > 0 {
		rep.notef("receipt: %d ใบระบุส่วนลดเป็นเปอร์เซ็นต์ในระบบเก่า — ไม่ต้องแปลงอัตรานั้นเอง เพราะส่วนลดถูกคิดจากส่วนต่าง (ยอดที่ปิด − เงินที่รับ) ซึ่งได้จำนวนเงินจริงอยู่แล้ว",
			percentDiscount)
	}
	return out, nil
}

// sumExpr renders a list of numeric literals as one SQL addition, or 0 for none. The
// addition is left for Postgres to do in `numeric`: adding the amounts in Go would mean
// float64, and a receipt that is ฿0.10 out is a receipt that fails guard 1.
func sumExpr(amounts []string) string {
	if len(amounts) == 0 {
		return "0"
	}
	return "(" + strings.Join(amounts, " + ") + ")"
}

// balanceBills fills in each bill's discount and drops the ones that cannot balance.
//
// THE DISCOUNT IS DERIVED, NOT READ. The legacy system has a discount field, but it does
// not use it consistently: 19 bills in this export were closed for less than their value
// with the reason typed into a free-text remark instead ("Soft Opening ลด 15%",
// "vocher 1,000 baht form k.Aim", "ราคาพาร์ทเนอร์", "K.เปิ้ลแถม IV Soft Glow ไปให้").
// Reading the field would have imported those as unpaid debt and sent the clinic chasing
// patients who had already been given the discount.
//
// So the bill-level discount is whatever was neither collected nor is still owed:
//
//	discount = price_total − Σ money received − price_total_balance
//
// which reconciles every bill by construction. That leaves exactly two ways to fail:
//
//	Σ(price × qty) ≠ price_total   the lines do not describe the bill they are on
//	discount < 0                   more money came in than the bill was worth
//
// Both are refused per bill and named, because both mean the source disagrees with
// itself and no amount of mapping can settle it.
func balanceBills(plans []billPlan, receipts []receiptPlan, rep *Report,
	eInv *EntityResult) ([]billPlan, []receiptPlan) {

	paidByBill := map[string]float64{}
	amountsByBill := map[string][]string{}
	for _, r := range receipts {
		paidByBill[r.invoiceNo] += r.fPaid
		amountsByBill[r.invoiceNo] = append(amountsByBill[r.invoiceNo], r.amountTexts...)
	}

	bad := map[string]bool{}
	var lineMismatch, overpaid []string
	discounted, discountTotal := 0, 0.0

	for i := range plans {
		p := &plans[i]
		if diff := p.fLineTotal - p.fPriceTotal; diff > MoneyTolerance || diff < -MoneyTolerance {
			bad[p.no] = true
			if len(lineMismatch) < 10 {
				lineMismatch = append(lineMismatch,
					fmt.Sprintf("%s (รายการรวม %.2f ≠ ยอดบิล %.2f)", p.no, p.fLineTotal, p.fPriceTotal))
			}
			continue
		}

		residual := p.fPriceTotal - paidByBill[p.no] - p.fBalance
		if residual < -MoneyTolerance {
			bad[p.no] = true
			if len(overpaid) < 10 {
				overpaid = append(overpaid, fmt.Sprintf("%s (ยอดบิล %.2f · รับ %.2f · ค้าง %.2f → ส่วนลดติดลบ %.2f)",
					p.no, p.fPriceTotal, paidByBill[p.no], p.fBalance, residual))
			}
			continue
		}
		if residual > MoneyTolerance {
			discounted++
			discountTotal += residual
		}
		// Rendered as arithmetic over the SOURCE digits so Postgres computes it in
		// numeric and a reviewer can see the three numbers it came from.
		p.discountExpr = "(" + p.priceTotal + " - " + sumExpr(amountsByBill[p.no]) + " - " + p.balance + ")"
	}

	if discounted > 0 {
		rep.notef("invoice.bill_discount: %d ใบมีส่วนลด รวม %.2f บาท — คิดจาก ยอดบิล − เงินที่รับจริง − ยอดค้าง ไม่ได้อ่านจากช่องส่วนลดของระบบเก่า เพราะระบบเก่าบันทึกส่วนลดบางส่วนไว้ในหมายเหตุเท่านั้น",
			discounted, discountTotal)
	}
	if len(bad) == 0 {
		return plans, receipts
	}

	keptPlans := make([]billPlan, 0, len(plans))
	for _, p := range plans {
		if bad[p.no] {
			eInv.skip("ยอดในบิลขัดกันเอง")
			continue
		}
		keptPlans = append(keptPlans, p)
	}
	keptReceipts := make([]receiptPlan, 0, len(receipts))
	for _, r := range receipts {
		if bad[r.invoiceNo] {
			continue
		}
		keptReceipts = append(keptReceipts, r)
	}

	if len(lineMismatch) > 0 {
		rep.warnf("invoice: ข้าม %d ใบ เพราะผลรวมรายการไม่เท่ากับยอดบิลในต้นทาง — ตัวอย่าง: %s",
			len(lineMismatch), strings.Join(lineMismatch, "; "))
	}
	if len(overpaid) > 0 {
		rep.warnf("invoice: ข้าม %d ใบ เพราะเงินที่รับมากกว่ายอดบิล (ส่วนลดติดลบ — น่าจะเป็นเงินมัดจำหรือบิลที่ยอดถูกแก้ทีหลัง) — ตัวอย่าง: %s",
			len(overpaid), strings.Join(overpaid, "; "))
	}
	return keptPlans, keptReceipts
}

// ── renderers ────────────────────────────────────────────────────────────────

// renderInvoices writes the invoice header. status/paid_amount/bill_discount are left
// at their defaults here and filled by renderSettle once the items and receipts exist —
// there is nothing to compute them from yet.
func renderInvoices(plans []billPlan, in Input, e *EntityResult) string {
	var s sqlBuilder
	for _, p := range plans {
		s.comment("%s · HN %s · %s", p.no, p.hn, oneLine(p.patientName))
		s.comment("   ส่วนลดท้ายบิล = ยอดบิล %s − เงินที่รับจริง − ยอดค้าง %s", p.priceTotal, p.balance)
		s.addf(`INSERT INTO invoice
    (clinic_code, no, hn, patient_name, branch_code, status, bill_discount, paid_amount,
     doctor_fee_stamped, course_stamped, created_at, updated_at)
SELECT %s, %s, %s, %s, %s, 'edit', %s, 0,
       false, false, %s, now()
WHERE NOT EXISTS (
    SELECT 1 FROM invoice WHERE clinic_code = %s AND no = %s
);

`, lit(in.ClinicCode), lit(p.no), lit(p.hn), nullLit(p.patientName), lit(p.branchCode),
			p.discountExpr, p.createdAt, lit(in.ClinicCode), lit(p.no))
	}
	e.Statements = s.stmts
	return s.String()
}

// invoiceIDByNo resolves invoice.id in SQL. The id is generated by the database, so it
// cannot be known while the file is being written; (clinic_code, no) is UNIQUE, which
// makes this lookup exact.
func invoiceIDByNo(clinic, no string) string {
	return fmt.Sprintf("(SELECT id FROM invoice WHERE clinic_code = %s AND no = %s)", lit(clinic), lit(no))
}

// receiptIDByNo resolves receipt.id the same way, on UNIQUE (clinic_code, receipt_no).
func receiptIDByNo(clinic, no string) string {
	return fmt.Sprintf("(SELECT id FROM receipt WHERE clinic_code = %s AND receipt_no = %s)", lit(clinic), lit(no))
}

func renderInvoiceItems(plans []billPlan, in Input, e *EntityResult) string {
	var s sqlBuilder
	for _, p := range plans {
		s.comment("รายการของใบ %s", p.no)
		for _, l := range p.lines {
			// catalog_item_id is resolved by code so the line joins the canonical
			// catalog. An unmatched code leaves it NULL rather than failing: the charge
			// happened, and the money must not depend on the catalog being complete.
			catalogRef := "NULL"
			if l.code != "" {
				catalogRef = catalogItemIDByCode(in.ClinicCode, l.code)
			}
			s.addf(`INSERT INTO invoice_item
    (clinic_code, invoice_id, kind, code, name, price, qty, discount, use_course,
     sort_order, catalog_item_id, compensations, created_at)
SELECT %s, %s, %s, %s, %s, %s, %s, 0, %s,
       %d, %s, '[]'::jsonb, %s
WHERE %s IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM invoice_item ii
       WHERE ii.clinic_code = %s AND ii.invoice_id = %s AND ii.sort_order = %d
  );

`, lit(in.ClinicCode), invoiceIDByNo(in.ClinicCode, p.no), lit(l.kind), nullLit(l.code),
				lit(l.name), l.price, l.qty, boolLit(l.useCourse),
				l.sortOrder, catalogRef, p.createdAt,
				invoiceIDByNo(in.ClinicCode, p.no),
				lit(in.ClinicCode), invoiceIDByNo(in.ClinicCode, p.no), l.sortOrder)
		}
	}
	e.Statements = s.stmts
	return s.String()
}

func renderReceipts(receipts []receiptPlan, in Input, e *EntityResult) string {
	var s sqlBuilder
	for _, r := range receipts {
		s.comment("ใบเสร็จ %s · ใบแจ้งหนี้ %s · งวด %d", r.no, r.invoiceNo, r.installmentNo)
		s.addf(`INSERT INTO receipt
    (clinic_code, invoice_id, receipt_no, installment_no, hn, patient_name,
     paid_amount, discount, deposit_apply, gift_card_apply, point_apply, point_spent,
     point_earned, vat_amount, vat_rate, note, detail, doctor_code, pay_date,
     branch_code, created_at)
SELECT %s, %s, %s, %d, %s, %s,
       %s, %s, 0, 0, %s, 0,
       0, 0, %s, %s, %s, %s, %s,
       %s, %s
WHERE %s IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM receipt WHERE clinic_code = %s AND receipt_no = %s
  );

`, lit(in.ClinicCode), invoiceIDByNo(in.ClinicCode, r.invoiceNo), lit(r.no), r.installmentNo,
			nullLit(r.hn), nullLit(r.patientName),
			r.paidExpr, r.discountExpr, r.pointApply,
			MoneyVATRate, nullLit(r.remark), nullLit(r.remark), nullLit(r.doctorCode), r.payDate,
			lit(r.branchCode), r.createdAt,
			invoiceIDByNo(in.ClinicCode, r.invoiceNo),
			lit(in.ClinicCode), lit(r.no))
	}
	e.Statements = s.stmts
	return s.String()
}

func renderReceiptPayments(receipts []receiptPlan, in Input, e *EntityResult) string {
	var s sqlBuilder
	for _, r := range receipts {
		if len(r.channels) == 0 {
			continue
		}
		s.comment("ช่องทางจ่ายของใบเสร็จ %s", r.no)
		for _, ch := range r.channels {
			insurer := "NULL"
			if ch.method == "insurance" && r.insurer != "" {
				insurer = lit(r.insurer)
			}
			approve := "NULL"
			if ch.method == "credit" && r.approveCode != "" {
				approve = lit(r.approveCode)
			}
			s.addf(`INSERT INTO receipt_payment
    (clinic_code, receipt_id, method, amount, payment_index, reference, approve_code,
     insurer, created_at)
SELECT %s, %s, %s, %s, %d, NULL, %s, %s, %s
WHERE %s IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM receipt_payment rp
       WHERE rp.clinic_code = %s AND rp.receipt_id = %s AND rp.payment_index = %d
  );

`, lit(in.ClinicCode), receiptIDByNo(in.ClinicCode, r.no), lit(ch.method), ch.amount, ch.index,
				approve, insurer, r.createdAt,
				receiptIDByNo(in.ClinicCode, r.no),
				lit(in.ClinicCode), receiptIDByNo(in.ClinicCode, r.no), ch.index)
		}
	}
	e.Statements = s.stmts
	return s.String()
}

// renderSettle fills paid_amount, bill_discount and status from the rows that were just
// written, using V4's OWN formula (AddInvoicePayment in internal/db/query/invoice.sql).
//
// Copying the legacy paid_amount instead would let the invoice and its receipts hold two
// different answers to "how much came in". Deriving it makes them the same number, which
// is why three of the five money guards below can only ever pass.
func renderSettle(invoiceNos map[string]bool, in Input) string {
	paid := `(SELECT COALESCE(SUM(r.paid_amount), 0) FROM receipt r
                 WHERE r.clinic_code = inv.clinic_code AND r.invoice_id = inv.id)`
	net := `(SELECT COALESCE(SUM(GREATEST(ii.price * ii.qty - ii.discount, 0)), 0) FROM invoice_item ii
                 WHERE ii.clinic_code = inv.clinic_code AND ii.invoice_id = inv.id)`

	return fmt.Sprintf(`-- ยอดที่จ่ายแล้วและสถานะ คำนวณจาก receipt/invoice_item ที่เพิ่งเขียน ด้วยสูตรเดียวกับ
-- AddInvoicePayment ของ V4 (internal/db/query/invoice.sql) ไม่ได้คัดลอกตัวเลขจากบิลเก่า
-- — ใบแจ้งหนี้กับใบเสร็จจึงพูดตัวเลขเดียวกันโดยโครงสร้าง
--
-- bill_discount ไม่ถูกแตะที่นี่: มันถูกเขียนตอน INSERT จากส่วนต่างของต้นทาง
-- (ยอดบิล − เงินที่รับ − ยอดค้าง) ซึ่งเป็นข้อมูลที่มีแต่ในไฟล์ ไม่มีในฐานข้อมูล
--
-- ใบที่ไม่มีใบเสร็จเลย (บิลสถานะ send) ได้ paid_amount = 0 → เป็นหนี้ค้างเต็มจำนวน
UPDATE invoice inv
   SET paid_amount = %s,
       status = CASE WHEN %s >= %s - inv.bill_discount - %s THEN 'full_paid' ELSE 'partially_paid' END,
       updated_at = now()
 WHERE inv.clinic_code = %s
   AND inv.no = ANY (ARRAY[%s]);

`, paid, paid, net, strconv.FormatFloat(MoneyTolerance, 'f', -1, 64),
		lit(in.ClinicCode), sqlTextArray(invoiceNos))
}

// ── source-value helpers ─────────────────────────────────────────────────────

// billDocNo returns the document number this import writes into invoice.no.
func billDocNo(d mongodump.Doc) string {
	if MoneyUseBranchDocNumbers {
		return firstNonEmpty(d.Text("invoice_branch_no"), d.Text("invoice_no"))
	}
	return firstNonEmpty(d.Text("invoice_no"), d.Text("invoice_branch_no"))
}

// receiptDocNo returns the document number this import writes into receipt.receipt_no.
func receiptDocNo(d mongodump.Doc) string {
	if MoneyUseBranchDocNumbers {
		return firstNonEmpty(d.Text("receipt_branch_no"), d.Text("receipt_no"))
	}
	return firstNonEmpty(d.Text("receipt_no"), d.Text("receipt_branch_no"))
}

// NOTE: the legacy discount fields (discount.assign.qty / discount.percent.qty) are
// deliberately NOT read as the discount amount. Measured against this export they
// reconcile only 959 of 1,267 bills, because the clinic recorded many discounts as a
// remark and simply took less money. The discount is derived instead — see balanceBills.

// receiptDiscountPercentActive reports a receipt whose discount was recorded as a
// percentage. Only used for the report: the amount itself comes from the difference
// between what the receipt closed and what it collected, so the rate is not needed.
func receiptDiscountPercentActive(d mongodump.Doc) bool {
	return docBool(d, "discount.percent.status") && positiveMoney(d.Text("discount.percent.qty"))
}

// moneyFloat parses a source amount for CHECKING ONLY. Nothing this returns is ever
// written: stored amounts go through moneyLit, which hands the digits to Postgres
// verbatim, because float64 cannot hold ฿0.10 exactly.
func moneyFloat(s string) float64 {
	t := strings.TrimSpace(s)
	if t == "" || !validNumeric(t) {
		return 0
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0
	}
	return f
}

// mongoTime reads an ISODate field as a time.
func mongoTime(d mongodump.Doc, path string) time.Time {
	v, ok := d.Get(path)
	if !ok || v == nil {
		return time.Time{}
	}
	t, isTime := v.(time.Time)
	if !isTime {
		return time.Time{}
	}
	return t
}

// mongoTSLit renders an ISODate as a UTC timestamptz literal.
//
// It exists because Doc.Text renders a date as RFC3339 ("2025-12-07T07:24:40Z") while
// tsLit parses the MySQL shape ("2025-12-07 07:24:40") — feeding one to the other
// silently yields the fallback, which for a sale would mean every imported bill was
// dated the moment of the import.
func mongoTSLit(d mongodump.Doc, path, fallback string) string {
	t := mongoTime(d, path)
	if t.IsZero() {
		return fallback
	}
	return lit(t.UTC().Format("2006-01-02 15:04:05-07"))
}

// mongoDateLit renders an ISODate as a plain date literal, for `date` columns.
func mongoDateLit(d mongodump.Doc, path, fallback string) string {
	t := mongoTime(d, path)
	if t.IsZero() {
		return fallback
	}
	return lit(t.UTC().Format("2006-01-02"))
}
