package legacyimport

import (
	"strings"
	"testing"

	"system_cliv4/internal/mongodump"
)

// receipt.detail is varchar(255) and receipt.note is text. The same payment_remark used
// to be written into BOTH, unclipped.
//
// WHAT THAT COST, MEASURED
//
// kvk-clinic (C52AEF6), 1 October 2026: one receipt out of 19,311 carried a remark of
// 26,286 characters — a whole month's appointment roster pasted into V2's remark box.
// psql stopped after 109 seconds with
//
//	ERROR: value too long for type character varying(255)
//	STATEMENT: INSERT INTO receipt
//
// and because the generated script is 27 transactions rather than one (max_locks_per_
// transaction — see script.go), everything before the money phase had already committed:
// 2,057 patients, 19,313 invoices and 29,267 invoice lines landed, while receipts,
// receipt_payment, course entitlements, loyalty and the whole SKU phase did not. The
// tenant had to be deleted and re-imported.
//
// These tests pin the two halves of the fix: the plan clips to each column's real width,
// and the renderer takes `note` and `detail` from DIFFERENT fields.

// oneReceiptFixture builds the smallest payment_summary doc planReceipts will accept,
// with the given remark, doctor and insurer. Returns the catalogue and the bill index.
func oneReceiptFixture(remark, doctor, insurer string) (*mongoCatalog, map[string]*billPlan) {
	const billOID = "650000000000000000000001"

	// Nested objects have to be mongodump.Doc, not map[string]any: Doc.Get type-asserts
	// every path segment to Doc, so a plain map reads back as "" and each nested field
	// would silently arrive empty — the fixture trap money_guard_order_test.go warns about.
	d := mongodump.Doc{
		"_id":            mongodump.ObjectID("650000000000000000000002"),
		"bill_id":        mongodump.ObjectID(billOID),
		"status":         "active",
		"order":          "1",
		"receipt_no":     "R690302469",
		"payment_remark": remark,
		"doctor":         doctor,
		"insurance":      mongodump.Doc{"insurance_name": insurer},
		"payment_type":   mongodump.Doc{"price_total": "1000", "payment": "1000"},
		"gov_direct":     mongodump.Doc{"appr_code": ""},
		"use_point":      "0",
		"paid_branch":    mongodump.Doc{"branch_code": ""},
	}

	mc := &mongoCatalog{payments: []mongodump.Doc{d}}
	// branchCode is filled in, so resolveOrKeep returns it without touching the resolver
	// — which is why a nil *branchResolver is safe here.
	bp := &billPlan{no: "B690302499", hn: "016900242", patientName: "อัยยา ถิรพูนทรัพย์", branchCode: "BR00001"}
	return mc, map[string]*billPlan{billOID: bp}
}

// planOneReceipt runs the planner on the fixture and insists on exactly one plan.
//
// The vacuity guard matters: a fixture that silently fails paymentImportable or the
// bill_id lookup yields an EMPTY plan list, and every assertion about clipping would then
// pass while testing nothing.
func planOneReceipt(t *testing.T, remark, doctor, insurer string) (receiptPlan, *Report) {
	t.Helper()
	mc, byBill := oneReceiptFixture(remark, doctor, insurer)
	rep := &Report{}
	eRec, ePay := &EntityResult{}, &EntityResult{}

	plans, err := planReceipts(mc, byBill, nil, rep, eRec, ePay)
	if err != nil {
		t.Fatalf("planReceipts: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("fixture produced %d receipt plans, want 1 — the planner rejected it, so "+
			"nothing below is actually being tested (skipped: %v)", len(plans), eRec.Skipped)
	}
	return plans[0], rep
}

func TestReceiptNoteKeepsTheWholeRemarkAndDetailFitsVarchar255(t *testing.T) {
	// The shape of the value that broke the real import: many short tab-separated lines.
	roster := "ตารางคิวนัด เดือน เมษายน 2569\n" +
		strings.Repeat("\tคิวที่\tเวลานัด\tชื่อ-นามสกุล\tเบอร์โทร\tคอร์ส/รายการที่ทำ\tHN.\tหมายเหตุ\n", 400)
	if len([]rune(roster)) <= 255 {
		t.Fatalf("the fixture remark is only %d characters — it has to exceed 255 to test "+
			"anything", len([]rune(roster)))
	}
	// The planner trims the remark before using it, so the trimmed form is what both
	// columns are expected to hold.
	kept := strings.TrimSpace(roster)

	p, rep := planOneReceipt(t, roster, "D001", "")

	// note is text: nothing may be lost.
	if p.remark != kept {
		t.Errorf("the remark bound for receipt.note was altered: %d characters in, %d out",
			len([]rune(kept)), len([]rune(p.remark)))
	}
	// detail is varchar(255): exactly the first 255 CHARACTERS, not bytes. Clipping bytes
	// would split a Thai character and corrupt the text.
	want := string([]rune(kept)[:255])
	if p.detail != want {
		t.Errorf("receipt.detail is not the first 255 characters of the remark:\ngot  %q\nwant %q",
			p.detail, want)
	}
	if n := len([]rune(p.detail)); n != 255 {
		t.Errorf("receipt.detail is %d characters, varchar(255) holds 255", n)
	}

	// A value that had to be cut must be reported. A silent truncation is how a clinic
	// finds out from a screen instead of from the import.
	joined := strings.Join(rep.Warnings, "\n")
	if !strings.Contains(joined, "detail") {
		t.Errorf("the clipped detail was not reported:\n%s", joined)
	}
}

func TestReceiptShortRemarkIsNotClippedAndNothingIsReported(t *testing.T) {
	const short = "vocher 1,000 baht form k.Aim"

	p, rep := planOneReceipt(t, short, "D001", "")

	if p.remark != short || p.detail != short {
		t.Errorf("a %d-character remark was altered: note=%q detail=%q",
			len([]rune(short)), p.remark, p.detail)
	}
	for _, w := range rep.Warnings {
		if strings.Contains(w, "ตัดข้อความ") {
			t.Errorf("a truncation was reported for a remark that fits: %s", w)
		}
	}
}

func TestReceiptDoctorCodeAndInsurerStayInsideVarchar64(t *testing.T) {
	// Both columns are varchar(64). insurer used to be clipped at 255 — a width copied
	// from the name columns — and doctor_code was not clipped at all.
	longDoctor := strings.Repeat("D", 200)
	longInsurer := strings.Repeat("บริษัทประกันชีวิต ", 30)

	p, _ := planOneReceipt(t, "ok", longDoctor, longInsurer)

	if n := len([]rune(p.doctorCode)); n != 64 {
		t.Errorf("receipt.doctor_code is %d characters, varchar(64) holds 64", n)
	}
	if n := len([]rune(p.insurer)); n != 64 {
		t.Errorf("receipt_payment.insurer is %d characters, varchar(64) holds 64", n)
	}
}

func TestRenderReceiptsTakesNoteAndDetailFromDifferentFields(t *testing.T) {
	// The bug was `nullLit(r.remark), nullLit(r.remark)` — the same field twice. Sentinels
	// make that visible no matter what the surrounding SQL looks like.
	plans := []receiptPlan{{
		no:            "R1",
		invoiceNo:     "B1",
		installmentNo: 1,
		hn:            "016900242",
		patientName:   "อัยยา",
		branchCode:    "BR00001",
		doctorCode:    "D001",
		remark:        "NOTE-SENTINEL-full-remark",
		detail:        "DETAIL-SENTINEL-clipped",
		payDate:       "NULL",
		createdAt:     "now()",
		pointApply:    "0",
		paidExpr:      "0",
		discountExpr:  "0",
	}}

	sql := renderReceipts(plans, Input{ClinicCode: "MBU1"}, &EntityResult{})

	if got := strings.Count(sql, "'NOTE-SENTINEL-full-remark'"); got != 1 {
		t.Errorf("the note value appears %d times, want 1:\n%s", got, head(sql, 900))
	}
	if got := strings.Count(sql, "'DETAIL-SENTINEL-clipped'"); got != 1 {
		t.Errorf("the detail value appears %d times, want 1 — renderReceipts is not "+
			"reading receiptPlan.detail:\n%s", got, head(sql, 900))
	}
	// Column order in the INSERT is (… note, detail, doctor_code …), so the note value has
	// to come first. A swap would put a 26,286-character blob back into varchar(255).
	ni := strings.Index(sql, "'NOTE-SENTINEL-full-remark'")
	di := strings.Index(sql, "'DETAIL-SENTINEL-clipped'")
	if ni > di {
		t.Errorf("the detail value is emitted before the note value — the two columns are "+
			"swapped:\n%s", head(sql, 900))
	}
}
