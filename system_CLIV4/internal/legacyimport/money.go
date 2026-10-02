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
	// courseUsage is every bill line that DEDUCTED a course session
	// (use_course=true). The legacy line carries no `course_bill` pointer — that is
	// literally how use_course is decided in planBillLines — so the line says WHAT was
	// consumed and WHEN, but not from which entitlement. coursebought.go resolves that
	// by FIFO and reports what it could not place.
	courseUsage []courseUsageRef
	// invoiceNoByBillID is bills _id → invoice no, for entitlements that know only their
	// bill.
	invoiceNoByBillID map[string]string

	// overpaidInSource are the invoice numbers whose SOURCE received more money than the
	// bill was worth. They are imported that way on purpose — the cash reached the clinic
	// and the import exists to make V4's totals equal V2's — so the post-import
	// over-payment guard has to skip them by name instead of failing the whole run over an
	// anomaly that was already in the source. Every one is named in the report.
	overpaidInSource map[string]bool
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

// courseUsageRef is one bill line that consumed a course session. It is the only
// honest record of WHEN a session was used: course_bills keeps a remaining count and
// no dates at all, while this line sits on a real bill with a real timestamp.
type courseUsageRef struct {
	hn         string
	invoiceNo  string
	branchCode string
	createdAt  string // SQL literal — when the session was consumed
	code       string
	name       string
	qty        string
	sortOrder  int
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
	// remark is the WHOLE payment_remark, bound for receipt.note, which is `text`.
	// detail is the same value clipped to receipt.detail's varchar(255).
	//
	// TWO FIELDS, NOT ONE. The same string used to be written into both columns, and a
	// single receipt in the kvk-clinic export carried a remark of 26,286 characters —
	// somebody had pasted a whole month's appointment roster into the remark box in V2.
	// psql stopped on "value too long for type character varying(255)" after 109 seconds,
	// and because the script is 27 transactions and not one, the groups before the money
	// phase had already committed: the tenant was left half-loaded. One un-clipped value
	// out of 19,311 receipts did that.
	//
	// Clipped HERE rather than at render time because this is where `rep` is, so the
	// truncation is reported in the same place as every other entity's.
	remark      string
	detail      string
	payDate     string // SQL literal (date)
	createdAt   string // SQL literal
	pointApply  string
	insurer     string
	approveCode string
	channels    []channelPlan

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
	// One resolver for the whole money block: bills and receipts must agree on which V4
	// branch a legacy code means, and the counts are reported once for both.
	br := newBranchResolver(in)
	defer br.report(rep, "invoice/receipt")
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
	plans, err := planBills(mc, br, rep, eInv, eItem)
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
	receipts, err := planReceipts(mc, byBillID, br, rep, eRec, ePay)
	if err != nil {
		return out, err
	}

	// A bill's discount is whatever the receipts did not collect and the patient does
	// not still owe, so it can only be worked out once the receipts are known.
	var overpaidInSource map[string]bool
	plans, receipts, overpaidInSource = balanceBills(plans, receipts, rep, eInv)
	out.overpaidInSource = overpaidInSource
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
			if l.useCourse {
				out.courseUsage = append(out.courseUsage, courseUsageRef{
					hn:         p.hn,
					invoiceNo:  p.no,
					branchCode: p.branchCode,
					createdAt:  p.createdAt,
					code:       l.code,
					name:       l.name,
					qty:        l.qty,
					sortOrder:  l.sortOrder,
				})
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
func planBills(mc *mongoCatalog, br *branchResolver, rep *Report, eInv, eItem *EntityResult) ([]billPlan, error) {
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

		// Translated through the branch map: the bill names the LEGACY branch and V4 owns
		// its own codes, so writing the source value straight through would file the
		// invoice at a branch that does not exist.
		branch := br.resolve(d.Text("branch_code"))
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
func planReceipts(mc *mongoCatalog, byBillID map[string]*billPlan, br *branchResolver,
	rep *Report, eRec, ePay *EntityResult) ([]receiptPlan, error) {

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
	// Width clipping is reported per column, like every other entity does it, so a
	// value that had to be cut shows up in the report instead of only in the database.
	tr := truncations{}
	var cut bool
	renumbered := 0
	percentDiscount := 0
	noMoneyReceipts := 0
	var channelMismatch []string
	overpaidReceipts := 0
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
				// The branch that TOOK the money, which can differ from the one that
				// issued the bill. p.bp.branchCode is already a resolved V4 code, so it
				// is used as the fallback rather than resolved twice.
				branchCode: resolveOrKeep(br, d.Text("paid_branch.branch_code"), p.bp.branchCode),
				payDate:    mongoDateLit(d, "created_at", "NULL"),
				createdAt:  mongoTSLit(d, "created_at", "now()"),
				pointApply: moneyLit(firstNonEmpty(d.Text("use_point"), "0")),
				fClosed:    moneyFloat(d.Text("payment_type.price_total")),
			}
			// receipt.doctor_code is varchar(64).
			rp.doctorCode, cut = clip(strings.TrimSpace(d.Text("doctor")), 64)
			tr.note("doctor_code", cut)
			// receipt.note is text, so the remark is kept whole; receipt.detail is
			// varchar(255) and gets the clipped form. V4 reads detail as a one-line
			// label (cockpit's product_name, the customer profile's title), so the
			// long form has no place there even when it would fit.
			rp.remark = strings.TrimSpace(d.Text("payment_remark"))
			rp.detail, cut = clip(rp.remark, 255)
			tr.note("detail", cut)
			if insurer := strings.TrimSpace(d.Text("insurance.insurance_name")); insurer != "" && insurer != "0" {
				// receipt_payment.insurer is varchar(64), not 255. The 255 here was a
				// latent copy of the width used for name columns.
				rp.insurer, cut = clip(insurer, 64)
				tr.note("insurer", cut)
			}
			if appr := strings.TrimSpace(d.Text("gov_direct.appr_code")); appr != "" {
				rp.approveCode, cut = clip(appr, 64)
				tr.note("approve_code", cut)
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
			// GREATEST keeps discount non-negative. A receipt that took in MORE money than
			// the amount it closed would otherwise render a negative discount, and the
			// column is a discount, not a credit note.
			rp.discountExpr = "GREATEST(" + moneyLit(d.Text("payment_type.price_total")) +
				" - " + rp.paidExpr + ", 0)"

			// A receipt that took in MORE money than the value it settled is kept, not
			// dropped. Decided by the clinic owner for the-gloss on 2026-09-09 and again
			// for wanaka-sanctuary on 2026-09-22, on the same ground both times: the cash
			// reached the clinic, and "ยอด V2 = ยอด V4" is the whole point of the import.
			// Dropping the receipt loses the money AND takes its bill down with it.
			//
			// Nothing downstream needs teaching. paid_amount is Σ channels so guard 1 still
			// balances; renderSettle derives invoice.paid_amount from the receipts so guard
			// 2 does too; and balanceBills sees cash above the bill value, flags the bill in
			// sourceOverpaid and clamps bill_discount — which is exactly the named exemption
			// guard 3 already honours. The anomaly stays reported, the money stays in.
			if rp.fPaid-rp.fClosed > MoneyTolerance {
				if len(channelMismatch) < 10 {
					channelMismatch = append(channelMismatch,
						fmt.Sprintf("%s (รับ %.2f > ปิดยอด %.2f)", no, rp.fPaid, rp.fClosed))
				}
				overpaidReceipts++
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
		rep.warnf("receipt: %d ใบรับเงินมากกว่ายอดที่ปิด — นำเข้าตามต้นทางเพื่อให้ยอดเงิน V4 เท่ากับ V2 · ส่วนลดของใบเหล่านี้ถูกตรึงที่ 0 (ไม่ให้ติดลบ) และบิลของมันจะถูกยกเว้นจากด่านห้ามเก็บเกินยอด · ต้องตามไปแก้ที่ต้นทางหรือออกใบลดหนี้ใน V4 — ตัวอย่าง: %s",
			overpaidReceipts, strings.Join(channelMismatch, "; "))
	}
	if noMoneyReceipts > 0 {
		rep.notef("receipt: %d ใบปิดยอดโดยไม่มีเงินเข้าเลย (voucher / ของแถม / ชดเชยบริการ) — ลงเป็น paid_amount = 0 และ discount เท่ากับยอดที่ปิด · เหตุผลเดิมจากช่อง payment_remark ถูกยกไปเก็บใน receipt.note ด้วย",
			noMoneyReceipts)
	}
	if percentDiscount > 0 {
		rep.notef("receipt: %d ใบระบุส่วนลดเป็นเปอร์เซ็นต์ในระบบเก่า — ไม่ต้องแปลงอัตรานั้นเอง เพราะส่วนลดถูกคิดจากส่วนต่าง (ยอดที่ปิด − เงินที่รับ) ซึ่งได้จำนวนเงินจริงอยู่แล้ว",
			percentDiscount)
	}
	if sum := tr.summary(); sum != "" {
		rep.warnf("receipt: ตัดข้อความให้พอดีความกว้างคอลัมน์ %s — ข้อความเต็มของช่องหมายเหตุยังอยู่ครบใน receipt.note (เป็น text ไม่จำกัดความยาว) ที่ถูกตัดคือ receipt.detail ซึ่งเป็นป้ายบรรทัดเดียวบนหน้าจอ",
			sum)
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
// refusedBillListLimit is how many refused bills are named in the report. It is high on
// purpose: these are the bills the clinic will not find in V4, and the operator needs the
// whole list to act on them. The cap exists only so a pathological export cannot print a
// megabyte of warnings.
const refusedBillListLimit = 500

func balanceBills(plans []billPlan, receipts []receiptPlan, rep *Report,
	eInv *EntityResult) ([]billPlan, []receiptPlan, map[string]bool) {

	paidByBill := map[string]float64{}
	amountsByBill := map[string][]string{}
	for _, r := range receipts {
		paidByBill[r.invoiceNo] += r.fPaid
		amountsByBill[r.invoiceNo] = append(amountsByBill[r.invoiceNo], r.amountTexts...)
	}

	bad := map[string]bool{}
	var lineMismatch, overpaid []string
	discounted, discountTotal := 0, 0.0
	retotalled, staleBalance := 0, 0
	var retotalDelta, staleAmount float64
	sourceOverpaid := map[string]bool{}

	for i := range plans {
		p := &plans[i]

		// ── repair 1: the bill total comes from the LINES when the header disagrees ──
		//
		// It used to refuse the bill. That was the safe answer while the two numbers were
		// equally credible, and the export proved they are not: the disagreeing headers in
		// this tenant are 0.00 against lines worth ฿3,999 / ฿13,000 / ฿25,500, with real
		// receipts attached. A zero header with money received is a header that was never
		// recalculated, not a bill worth nothing — and refusing it removed the sale, the
		// cash and (because entitlements hang off the bill) the patient's course as well.
		//
		// The lines are what was sold, so they decide the value. The header is reported,
		// never silently dropped, so the difference stays auditable.
		if diff := p.fLineTotal - p.fPriceTotal; diff > MoneyTolerance || diff < -MoneyTolerance {
			retotalled++
			retotalDelta += diff
			if len(lineMismatch) < refusedBillListLimit {
				lineMismatch = append(lineMismatch,
					fmt.Sprintf("%s (ยอดบิลเดิม %.2f → ใช้ผลรวมรายการ %.2f)", p.no, p.fPriceTotal, p.fLineTotal))
			}
			p.priceTotal = lineSumExpr(p.lines)
			p.fPriceTotal = p.fLineTotal
		}

		paid := paidByBill[p.no]

		// ── repair 2: ignore price_total_balance when it makes the discount negative ──
		//
		// discount = total − received − outstanding reconciles every well-formed bill, and
		// those keep using it untouched. Where it goes negative the outstanding figure is
		// stale: measured here, 42 bills whose own receipts already close them still carry
		// an outstanding amount (bill ฿2,995 · received ฿2,995 · outstanding ฿1,000), so
		// the field double-counts money that was paid. Dropping it from the arithmetic
		// leaves the two numbers the clinic can verify — the bill total and the cash — and
		// makes the remainder an ordinary unpaid balance.
		residual := p.fPriceTotal - paid - p.fBalance
		useBalance := true
		if residual < -MoneyTolerance {
			staleBalance++
			staleAmount += -residual
			useBalance = false
			residual = p.fPriceTotal - paid
			if len(overpaid) < refusedBillListLimit {
				overpaid = append(overpaid, fmt.Sprintf("%s (ยอดบิล %.2f · รับ %.2f · ค้างในต้นทาง %.2f → ไม่ใช้ยอดค้าง)",
					p.no, p.fPriceTotal, paid, p.fBalance))
			}
		}

		// Cash above the bill's value is carried through as-is: the money reached the
		// clinic and "ยอด V2 = ยอด V4" is the point of the import. The bill is flagged so
		// the post-import over-payment guard can name it as a KNOWN source anomaly instead
		// of failing a run over a number the source already contained.
		if residual < -MoneyTolerance {
			sourceOverpaid[p.no] = true
			residual = 0
		}
		if residual > MoneyTolerance {
			discounted++
			discountTotal += residual
		}

		// Rendered as arithmetic over the SOURCE digits so Postgres computes it in
		// numeric and a reviewer can see the numbers it came from. GREATEST keeps the
		// column non-negative when the cash exceeded the bill.
		expr := "GREATEST(" + p.priceTotal + " - " + sumExpr(amountsByBill[p.no])
		if useBalance {
			expr += " - " + p.balance
		}
		p.discountExpr = expr + ", 0)"
	}

	if discounted > 0 {
		rep.notef("invoice.bill_discount: %d ใบมีส่วนลด รวม %.2f บาท — คิดจาก ยอดบิล − เงินที่รับจริง − ยอดค้าง ไม่ได้อ่านจากช่องส่วนลดของระบบเก่า เพราะระบบเก่าบันทึกส่วนลดบางส่วนไว้ในหมายเหตุเท่านั้น",
			discounted, discountTotal)
	}
	if retotalled > 0 {
		rep.warnf("invoice: %d ใบ ยอดหัวบิลในต้นทางไม่เท่ากับผลรวมรายการ — ใช้ผลรวมรายการเป็นยอดบิล (ต่างกันรวม %.2f บาท) · รายการ: %s",
			retotalled, retotalDelta, strings.Join(lineMismatch, "; "))
	}
	if staleBalance > 0 {
		rep.warnf("invoice: %d ใบ ช่องยอดค้างในต้นทางค้างค่าเก่า (นับเงินที่จ่ายแล้วซ้ำ รวม %.2f บาท) — คิดส่วนลดจาก ยอดบิล − เงินที่รับจริง เท่านั้น · รายการ: %s",
			staleBalance, staleAmount, strings.Join(overpaid, "; "))
	}
	if len(sourceOverpaid) > 0 {
		nos := make([]string, 0, len(sourceOverpaid))
		for no := range sourceOverpaid {
			nos = append(nos, no)
		}
		sort.Strings(nos)
		rep.warnf("invoice: %d ใบ ต้นทางรับเงินมากกว่ายอดบิล — นำเข้าตามต้นทาง (paid_amount มากกว่ายอดสุทธิ) เพื่อให้ยอดเงิน V4 เท่ากับ V2 · ด่านห้ามเก็บเกินยอดจะยกเว้นใบเหล่านี้และไม่ปฏิเสธการนำเข้า · ต้องตามไปแก้ที่ต้นทางหรือออกใบลดหนี้ใน V4: %s",
			len(sourceOverpaid), strings.Join(nos, ", "))
	}

	// Nothing is dropped any more: every importable bill is repaired rather than refused,
	// so `bad` stays empty unless a future rule needs it. Kept as the single place a
	// future refusal would hook into.
	if len(bad) == 0 {
		return plans, receipts, sourceOverpaid
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
	return keptPlans, keptReceipts, sourceOverpaid
}

// lineSumExpr renders Σ(price × qty) as SQL arithmetic over the line literals, so the
// repaired bill total is computed by Postgres in `numeric` from the same digits the
// invoice_item rows carry. Summing in Go would put money through float64 and could leave
// the header a cent away from its own lines.
func lineSumExpr(lines []linePlan) string {
	if len(lines) == 0 {
		return "0"
	}
	parts := make([]string, 0, len(lines))
	for _, l := range lines {
		parts = append(parts, "("+l.price+" * "+l.qty+")")
	}
	return "(" + strings.Join(parts, " + ") + ")"
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
			MoneyVATRate, nullLit(r.remark), nullLit(r.detail), nullLit(r.doctorCode), r.payDate,
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
		return applyDocNoBranchMarker(firstNonEmpty(d.Text("invoice_branch_no"), d.Text("invoice_no")))
	}
	return applyDocNoBranchMarker(firstNonEmpty(d.Text("invoice_no"), d.Text("invoice_branch_no")))
}

// receiptDocNo returns the document number this import writes into receipt.receipt_no.
func receiptDocNo(d mongodump.Doc) string {
	if MoneyUseBranchDocNumbers {
		return applyDocNoBranchMarker(firstNonEmpty(d.Text("receipt_branch_no"), d.Text("receipt_no")))
	}
	return applyDocNoBranchMarker(firstNonEmpty(d.Text("receipt_no"), d.Text("receipt_branch_no")))
}

/**
 * applyDocNoBranchMarker stamps a branch digit into a legacy V2 document number.
 *
 * WHY THIS EXISTS
 * Every V2 instance of a multi-site tenant issues the SAME running numbers, because each
 * site runs its own copy of V2. V4 keeps one number space per clinic
 * (uniq_invoice_clinic_no / uniq_receipt_clinic_no), so importing a second site's
 * documents as-is either collides with the first site's numbers or, worse, re-inserts
 * documents that are already there under a different identity — the dedup in the
 * generated script is `WHERE NOT EXISTS (… AND no = '<number>')`, i.e. the number IS the
 * identity.
 *
 * THE RULE (the-gloss, owner K.Art, 2026-09-09)
 * The third character of a V2 number is always '0' — the shape is YY0NNNNNN, and
 * C-prefixed numbers (bills that spend a course) are CYY0NNNNNN. That zero becomes the
 * branch marker, so ศรีราชา's 670017358 is written as 672017358 while ฉะเชิงเทรา keeps its
 * original numbers. Lossless: the digit replaced carries no information. Verified
 * collision-free against the live BR00002 rows before the first remap ran.
 *
 * Numbers that do NOT match the V2 shape are returned untouched — a V4-native number
 * (INV-260917-F26F, RCP-6909-BR00002-0001) is not part of this numbering scheme and
 * rewriting it would invent a document number that never existed.
 *
 * DocNoBranchMarker is "" for every clinic that does not need this, so the default
 * behaviour — carry the source number verbatim — is unchanged.
 */
func applyDocNoBranchMarker(no string) string {
	marker := strings.TrimSpace(DocNoBranchMarker)
	if marker == "" || len(marker) != 1 || marker[0] < '1' || marker[0] > '9' {
		return no
	}
	s := strings.TrimSpace(no)
	// YY0NNNNNN  → the marker replaces index 2
	if len(s) == 9 && isAllDigits(s) && s[2] == '0' {
		return s[:2] + marker + s[3:]
	}
	// CYY0NNNNNN → the marker replaces index 3
	if len(s) == 10 && (s[0] == 'C' || s[0] == 'c') && isAllDigits(s[1:]) && s[3] == '0' {
		return s[:3] + marker + s[4:]
	}
	return no
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
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
