package legacyimport

import (
	"strings"
	"testing"
	"time"

	"system_cliv4/internal/mongodump"
)

// The rule: a document number claimed by more than one bill is resolved automatically
// ONLY when every claimant is an unpaid draft worth nothing. Anything else refuses the
// whole script, because it is a question about a real charge.
//
// Fixtures use the collapsed forms the scanner produces — mongodump.ObjectID for $oid and
// mongodump.Decimal for $numberDecimal — so Doc.Text reads them the way it reads a real
// dump. A raw map would read back as "" and every assertion here would be vacuous.

// bill builds one bills document.
func bill(id, status, no, total string, lines int) mongodump.Doc {
	list := make([]any, 0, lines)
	for i := 0; i < lines; i++ {
		list = append(list, mongodump.Doc{"code": "ST0" + string(rune('1'+i)), "indexof": int64(i + 1)})
	}
	return mongodump.Doc{
		"_id":         mongodump.ObjectID(id),
		"status":      status,
		"hn":          "016400480",
		"invoice_no":  no,
		"price_total": mongodump.Decimal(total),
		"list":        list,
	}
}

func TestDuplicateZeroValueDraftsAreResolvedKeepingTheCompleteBill(t *testing.T) {
	// The measured pair from ปิ่นลักษณ์: same HN, same number, same millisecond, both
	// unpaid and worth nothing, and the loser's single line is a subset of the winner's.
	bills := []mongodump.Doc{
		bill("60ffc59d3460f704bd14facd", "send", "640054443", "0", 2),
		bill("60ffc59d0c4c7804d14b62b4", "send", "640054443", "0", 1),
	}
	rep, e := &Report{}, &EntityResult{}

	drop, err := resolveDuplicateBillNos(bills, nil, rep, e)
	if err != nil {
		t.Fatalf("a double-submit of two worthless drafts was refused: %v", err)
	}
	if len(drop) != 1 {
		t.Fatalf("dropped %d bills, want 1: %v", len(drop), drop)
	}
	if !drop["60ffc59d0c4c7804d14b62b4"] {
		t.Errorf("the single-line bill was not the one dropped: %v", drop)
	}
	if drop["60ffc59d3460f704bd14facd"] {
		t.Error("the bill with more lines was dropped — the complete document must survive")
	}

	// It has to be reported: a bill that disappears without a line in the report is a
	// bill the clinic cannot ask about.
	joined := strings.Join(rep.Warnings, "\n")
	for _, want := range []string{"640054443", "60ffc59d3460f704bd14facd", "60ffc59d0c4c7804d14b62b4"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the report does not mention %q:\n%s", want, joined)
		}
	}
	if len(e.Skipped) == 0 {
		t.Error("the dropped bill was not counted as a skip on the invoice entity")
	}
}

func TestThreeWayDuplicateDropsEveryLoser(t *testing.T) {
	// 640202709 appeared three times in the real dump.
	bills := []mongodump.Doc{
		bill("6138e44cffd7636bfdea6281", "send", "640202709", "0", 1),
		bill("6138e44c61af936bf6896bb3", "send", "640202709", "0", 3),
		bill("6138e44c61af936bf6896bbd", "send", "640202709", "0", 2),
	}
	drop, err := resolveDuplicateBillNos(bills, nil, &Report{}, &EntityResult{})
	if err != nil {
		t.Fatalf("refused a three-way double-submit: %v", err)
	}
	if len(drop) != 2 {
		t.Fatalf("dropped %d, want 2: %v", len(drop), drop)
	}
	if drop["6138e44c61af936bf6896bb3"] {
		t.Error("the 3-line bill should have been kept")
	}
}

func TestDuplicateWithDifferentMoneyStillRefusesTheWholeScript(t *testing.T) {
	// Two bills, one of them worth ฿1,500. Dropping either one loses a real charge, so
	// this is not a decision the tool gets to make.
	bills := []mongodump.Doc{
		bill("aaaaaaaaaaaaaaaaaaaaaaa1", "send", "640054443", "0", 1),
		bill("aaaaaaaaaaaaaaaaaaaaaaa2", "send", "640054443", "1500", 1),
	}
	_, err := resolveDuplicateBillNos(bills, nil, &Report{}, &EntityResult{})
	if err == nil {
		t.Fatal("a duplicate with money on it was resolved silently")
	}
	for _, want := range []string{"640054443", "aaaaaaaaaaaaaaaaaaaaaaa1", "aaaaaaaaaaaaaaaaaaaaaaa2", "1500"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}
}

func TestDuplicateThatWasAlreadyPaidStillRefuses(t *testing.T) {
	// Zero totals on both, but one is full_paid. A paid document is never dropped on a
	// guess, whatever its total says.
	bills := []mongodump.Doc{
		bill("bbbbbbbbbbbbbbbbbbbbbbb1", "full_paid", "640054443", "0", 2),
		bill("bbbbbbbbbbbbbbbbbbbbbbb2", "send", "640054443", "0", 1),
	}
	_, err := resolveDuplicateBillNos(bills, nil, &Report{}, &EntityResult{})
	if err == nil {
		t.Fatal("a paid duplicate was dropped automatically")
	}
	if !strings.Contains(err.Error(), "full_paid") {
		t.Errorf("the refusal does not say which status blocked it:\n%s", err)
	}
}

func TestDeletedBillsDoNotReserveADocumentNumber(t *testing.T) {
	// A bill voided in V2 never reaches invoice.no, so it must not make a number look
	// taken. Before this resolver existed, planBills reserved the number before the
	// status skip and a live bill could be refused because of a deleted twin.
	bills := []mongodump.Doc{
		bill("ccccccccccccccccccccccc1", "delete", "640054443", "9999", 5),
		bill("ccccccccccccccccccccccc2", "full_paid", "640054443", "1500", 2),
	}
	drop, err := resolveDuplicateBillNos(bills, nil, &Report{}, &EntityResult{})
	if err != nil {
		t.Fatalf("a deleted twin blocked a live bill: %v", err)
	}
	if len(drop) != 0 {
		t.Errorf("dropped %v — there was no conflict to resolve", drop)
	}
}

func TestDuplicateTieGoesToTheEarlierBillAndIsStable(t *testing.T) {
	// Equal line counts: the winner must be decided by position in the file, so the same
	// dump always produces the same script.
	bills := []mongodump.Doc{
		bill("ddddddddddddddddddddddd1", "send", "640202665", "0", 1),
		bill("ddddddddddddddddddddddd2", "send", "640202665", "0", 1),
	}
	for i := 0; i < 3; i++ {
		drop, err := resolveDuplicateBillNos(bills, nil, &Report{}, &EntityResult{})
		if err != nil {
			t.Fatalf("run %d refused: %v", i, err)
		}
		if len(drop) != 1 || !drop["ddddddddddddddddddddddd2"] {
			t.Fatalf("run %d dropped %v, want only the second bill", i, drop)
		}
	}
}

func TestNoDuplicatesMeansNoDropsAndNoReport(t *testing.T) {
	bills := []mongodump.Doc{
		bill("eeeeeeeeeeeeeeeeeeeeeee1", "full_paid", "640000001", "1200", 2),
		bill("eeeeeeeeeeeeeeeeeeeeeee2", "send", "640000002", "0", 1),
		bill("eeeeeeeeeeeeeeeeeeeeeee3", "partially_paid", "640000003", "500", 3),
	}
	rep := &Report{}
	drop, err := resolveDuplicateBillNos(bills, nil, rep, &EntityResult{})
	if err != nil {
		t.Fatalf("refused a clean dump: %v", err)
	}
	if len(drop) != 0 {
		t.Errorf("dropped %v from a dump with no duplicates", drop)
	}
	for _, w := range rep.Warnings {
		if strings.Contains(w, "เลขที่เอกสารซ้ำ") {
			t.Errorf("reported duplicates that do not exist: %s", w)
		}
	}
}

func TestBillsWithNoDocumentNumberAreNotTreatedAsDuplicatesOfEachOther(t *testing.T) {
	// planBills skips these with its own message. Two blank numbers are not a collision,
	// and counting them as one would refuse a dump this rule has nothing to say about.
	bills := []mongodump.Doc{
		bill("fffffffffffffffffffffff1", "send", "", "0", 1),
		bill("fffffffffffffffffffffff2", "send", "", "0", 1),
	}
	drop, err := resolveDuplicateBillNos(bills, nil, &Report{}, &EntityResult{})
	if err != nil {
		t.Fatalf("two bills with no number were treated as a duplicate: %v", err)
	}
	if len(drop) != 0 {
		t.Errorf("dropped %v", drop)
	}
}

func TestZeroValueDraftRecognisesOnlyUnpaidZeroBills(t *testing.T) {
	cases := []struct {
		status string
		total  string
		want   bool
		why    string
	}{
		{"send", "0", true, "the shape this rule exists for"},
		{"send", "0.00", true, "same number, written differently"},
		{"send", "0.005", true, "inside MoneyTolerance, so still zero"},
		{"send", "1500", false, "has money on it"},
		{"send", "-1500", false, "has money on it, negative"},
		{"SEND", "0", true, "status compared case-insensitively"},
		{"full_paid", "0", false, "paid documents are never dropped on a guess"},
		{"partially_paid", "0", false, "part-paid is still paid"},
		{"watting", "0", false, "not the unpaid-draft status this rule names"},
		{"", "0", false, "no status at all"},
	}
	for _, c := range cases {
		got := isZeroValueDraftBill(bill("1", c.status, "640000001", c.total, 1))
		if got != c.want {
			t.Errorf("status=%q total=%q -> %v, want %v (%s)", c.status, c.total, got, c.want, c.why)
		}
	}
}

// ── door two: a bill that holds nothing and took no money ────────────────────────────
//
// Measured on the 4 Oct export: 650146639 was claimed by two bills, both full_paid with a
// zero total, no lines, the same usecourse, created seven milliseconds apart — and each
// with its own receipt, 650112321 and 650112440, zero in every channel. V2 marks a course
// deduction full_paid because the money was taken when the course was sold, so "paid" here
// says nothing about a charge existing. This dump links all 4,741 payments through
// payment_summary.bill_id; the bill's own pay_id never matches a payment _id, which is why
// both directions are indexed.
//
// Owner's decisions: 1ก (resolve only when no money reached any of them) and
// "เลขบิลซ้ำ ให้เอาตัวล่าสุด" (the latest bill survives).

// billFull builds a bills document with the fields door two reads.
func billFull(id, status, no, total, balance, payID string, lines int) mongodump.Doc {
	d := bill(id, status, no, total, lines)
	if balance != "" {
		d["price_total_balance"] = mongodump.Decimal(balance)
	}
	if payID != "" {
		d["pay_id"] = mongodump.ObjectID(payID)
	}
	return d
}

// billAt is billFull plus a created_at, which is what decides door two.
func billAt(id, status, no string, lines int, created time.Time) mongodump.Doc {
	d := billFull(id, status, no, "0", "0", "", lines)
	d["created_at"] = created
	return d
}

// paymentRow builds one payment_summary document with every money field at zero.
// billID may be empty, which is how a payment reachable only through the bill's pay_id
// would look.
func paymentRow(id, billID, receiptNo string) mongodump.Doc {
	d := mongodump.Doc{
		"_id":        mongodump.ObjectID(id),
		"status":     "active",
		"receipt_no": receiptNo,
		"payment_type": mongodump.Doc{
			"payment":            mongodump.Decimal("0"),
			"payment_transfer":   mongodump.Decimal("0"),
			"payment_credit":     mongodump.Decimal("0"),
			"payment_sso":        mongodump.Decimal("0"),
			"payment_insurance":  mongodump.Decimal("0"),
			"voucher":            mongodump.Decimal("0"),
			"payment_other":      mongodump.Decimal("0"),
			"price_total":        mongodump.Decimal("0"),
			"price_total_amount": mongodump.Decimal("0"),
		},
	}
	if billID != "" {
		d["bill_id"] = mongodump.ObjectID(billID)
	}
	return d
}

// withMoney puts an amount into one channel of a payment built by paymentRow.
func withMoney(p mongodump.Doc, field, amount string) mongodump.Doc {
	pt, _ := p["payment_type"].(mongodump.Doc)
	pt[field] = mongodump.Decimal(amount)
	return p
}

func ts(ms int) time.Time {
	return time.Date(2022, 2, 19, 11, 34, 46, ms*int(time.Millisecond), time.UTC)
}

func TestEmptyZeroValueDuplicateIsResolvedAndKeepsTheLatest(t *testing.T) {
	// The real pair, receipts included. Nothing on either document and no money on
	// either receipt, so dropping one cannot lose a line or a baht.
	bills := []mongodump.Doc{
		billAt("6210d5d630e636069367cf52", "full_paid", "650146639", 0, ts(479)),
		billAt("6210d5d6649e54060af3b3a9", "full_paid", "650146639", 0, ts(486)),
	}
	payments := []mongodump.Doc{
		paymentRow("6210d5d630e636069367cf53", "6210d5d630e636069367cf52", "650112321"),
		paymentRow("6210d5d6649e54060af3b3aa", "6210d5d6649e54060af3b3a9", "650112440"),
	}
	rep, e := &Report{}, &EntityResult{}

	drop, err := resolveDuplicateBillNos(bills, payments, rep, e)
	if err != nil {
		t.Fatalf("an empty, money-free course-deduction duplicate was refused: %v", err)
	}
	// .486 is later than .479, so the first bill in the file is the one that goes.
	if len(drop) != 1 || !drop["6210d5d630e636069367cf52"] {
		t.Fatalf("dropped %v, want only the earlier bill", drop)
	}

	joined := strings.Join(rep.Warnings, "\n")
	for _, want := range []string{
		"650146639",
		"เก็บใบล่าสุด",
		"ไม่เข้า V4",
		"650112321",
		"650112440",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the report does not mention %q:\n%s", want, joined)
		}
	}
	if len(e.Skipped) == 0 {
		t.Error("the dropped bill was not counted as a skip")
	}
}

func TestEmptyDuplicateWhereOneReceiptCarriesMoneyRefuses(t *testing.T) {
	// Same shape, except one receipt took ฿500 cash. Somebody's money is attached to one
	// of these documents, so the tool does not choose.
	bills := []mongodump.Doc{
		billAt("aaaaaaaaaaaaaaaaaaaaaab1", "full_paid", "650146639", 0, ts(479)),
		billAt("aaaaaaaaaaaaaaaaaaaaaab2", "full_paid", "650146639", 0, ts(486)),
	}
	payments := []mongodump.Doc{
		paymentRow("pay1pay1pay1pay1pay1pay1", "aaaaaaaaaaaaaaaaaaaaaab1", "650112321"),
		withMoney(paymentRow("pay2pay2pay2pay2pay2pay2", "aaaaaaaaaaaaaaaaaaaaaab2", "650112440"),
			"payment", "500"),
	}
	_, err := resolveDuplicateBillNos(bills, payments, &Report{}, &EntityResult{})
	if err == nil {
		t.Fatal("a duplicate whose receipt took money was resolved automatically")
	}
	if !strings.Contains(err.Error(), "650112440") {
		t.Errorf("the refusal does not name the receipt with money on it:\n%s", err)
	}
}

func TestEmptyDuplicateWhereAReceiptClosedAValueRefuses(t *testing.T) {
	// Nothing in any channel, but the receipt closed a ฿1,500 bill — a discount or a
	// write-off. That is still a statement about money.
	bills := []mongodump.Doc{
		billAt("bbbbbbbbbbbbbbbbbbbbbbc1", "full_paid", "650146639", 0, ts(479)),
		billAt("bbbbbbbbbbbbbbbbbbbbbbc2", "full_paid", "650146639", 0, ts(486)),
	}
	payments := []mongodump.Doc{
		withMoney(paymentRow("pay3pay3pay3pay3pay3pay3", "bbbbbbbbbbbbbbbbbbbbbbc1", "650112321"),
			"price_total", "1500"),
	}
	_, err := resolveDuplicateBillNos(bills, payments, &Report{}, &EntityResult{})
	if err == nil {
		t.Fatal("a receipt that closed a real value was treated as money-free")
	}
}

func TestEmptyDuplicateFindsAPaymentLinkedThroughPayId(t *testing.T) {
	// The other direction: the bill names the payment. Indexing only bill_id would miss
	// money attached this way.
	bills := []mongodump.Doc{
		billFull("ccccccccccccccccccccccd1", "full_paid", "650146639", "0", "0", "payXpayXpayXpayXpayXpayX", 0),
		billFull("ccccccccccccccccccccccd2", "full_paid", "650146639", "0", "0", "", 0),
	}
	payments := []mongodump.Doc{
		withMoney(paymentRow("payXpayXpayXpayXpayXpayX", "", "650112321"), "payment_transfer", "900"),
	}
	_, err := resolveDuplicateBillNos(bills, payments, &Report{}, &EntityResult{})
	if err == nil {
		t.Fatal("money linked through pay_id was not seen")
	}
}

func TestPaidDuplicateWithLinesIsStillRefused(t *testing.T) {
	// The 3,246-bill shape in the measured export: full_paid, zero total, real lines.
	// Door two must not open for it, or a future collision between two genuine course
	// deductions would silently lose one.
	bills := []mongodump.Doc{
		billAt("ddddddddddddddddddddddd1", "full_paid", "650146639", 3, ts(479)),
		billAt("ddddddddddddddddddddddd2", "full_paid", "650146639", 2, ts(486)),
	}
	_, err := resolveDuplicateBillNos(bills, nil, &Report{}, &EntityResult{})
	if err == nil {
		t.Fatal("two paid bills with real lines were resolved automatically")
	}
}

func TestEmptyDuplicateWithAnOutstandingBalanceIsRefused(t *testing.T) {
	bills := []mongodump.Doc{
		billFull("eeeeeeeeeeeeeeeeeeeeeef1", "full_paid", "650146639", "0", "1500", "", 0),
		billFull("eeeeeeeeeeeeeeeeeeeeeef2", "full_paid", "650146639", "0", "0", "", 0),
	}
	_, err := resolveDuplicateBillNos(bills, nil, &Report{}, &EntityResult{})
	if err == nil {
		t.Fatal("a bill with an outstanding balance was treated as worth nothing")
	}
}

func TestMixedEmptyAndNonEmptyDuplicateIsRefused(t *testing.T) {
	bills := []mongodump.Doc{
		billAt("fffffffffffffffffffffff1", "full_paid", "650146639", 0, ts(479)),
		billAt("fffffffffffffffffffffff2", "full_paid", "650146639", 1, ts(486)),
	}
	_, err := resolveDuplicateBillNos(bills, nil, &Report{}, &EntityResult{})
	if err == nil {
		t.Fatal("a group where only some members are empty was resolved")
	}
}

func TestEmptyDuplicateBreaksATimestampTieByPositionAndIsStable(t *testing.T) {
	// The ปิ่นลักษณ์ export had a pair created in the same millisecond. With equal times
	// the later position wins, and the answer must not move between runs.
	bills := []mongodump.Doc{
		billAt("1111111111111111111111a1", "full_paid", "650146639", 0, ts(479)),
		billAt("1111111111111111111111a2", "full_paid", "650146639", 0, ts(479)),
		billAt("1111111111111111111111a3", "full_paid", "650146639", 0, ts(479)),
	}
	for i := 0; i < 3; i++ {
		drop, err := resolveDuplicateBillNos(bills, nil, &Report{}, &EntityResult{})
		if err != nil {
			t.Fatalf("run %d refused: %v", i, err)
		}
		if len(drop) != 2 || drop["1111111111111111111111a3"] {
			t.Fatalf("run %d dropped %v, want the first two", i, drop)
		}
	}
}

func TestEmptyDuplicateIgnoresFilePositionWhenTimestampsDiffer(t *testing.T) {
	// The latest bill wins even when it comes FIRST in the file, or "latest" would just
	// be "last in the file" wearing a different name.
	bills := []mongodump.Doc{
		billAt("2222222222222222222222b1", "full_paid", "650146639", 0, ts(900)),
		billAt("2222222222222222222222b2", "full_paid", "650146639", 0, ts(100)),
	}
	drop, err := resolveDuplicateBillNos(bills, nil, &Report{}, &EntityResult{})
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if len(drop) != 1 || !drop["2222222222222222222222b2"] {
		t.Fatalf("dropped %v, want the older bill even though it is later in the file", drop)
	}
}

func TestDoorOneStillKeepsTheMostCompleteDraft(t *testing.T) {
	// Door one admits bills WITH lines, so its 2 Oct rule stands: the complete document
	// survives. Switching it to "latest" would throw away real invoice lines — here the
	// latest bill has one line and the earlier has three.
	bills := []mongodump.Doc{
		billAt("3333333333333333333333c1", "send", "640202709", 3, ts(100)),
		billAt("3333333333333333333333c2", "send", "640202709", 1, ts(900)),
	}
	drop, err := resolveDuplicateBillNos(bills, nil, &Report{}, &EntityResult{})
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if len(drop) != 1 || !drop["3333333333333333333333c2"] {
		t.Fatalf("dropped %v, want the single-line bill — door one keeps the complete one", drop)
	}
}

func TestIsEmptyZeroValueBillReadsEveryCondition(t *testing.T) {
	links := indexPaymentLinks([]mongodump.Doc{
		paymentRow("payfreepayfreepayfree001", "billfree1billfree1billfr", "650100001"),
		withMoney(paymentRow("paycashpaycashpaycash001", "billcash1billcash1billca", "650100002"), "payment", "250"),
		withMoney(paymentRow("payclosepayclosepayclo001", "billclose1billclose1bill", "650100003"), "price_total", "99"),
	})
	cases := []struct {
		name string
		doc  mongodump.Doc
		want bool
	}{
		{"empty, zero, no receipt at all",
			billFull("x1x1x1x1x1x1x1x1x1x1x1x1", "full_paid", "1", "0", "0", "", 0), true},
		{"empty, zero, receipt with no money",
			billFull("billfree1billfree1billfr", "full_paid", "1", "0", "0", "", 0), true},
		{"receipt took cash",
			billFull("billcash1billcash1billca", "full_paid", "1", "0", "0", "", 0), false},
		{"receipt closed a value with nothing in any channel",
			billFull("billclose1billclose1bill", "full_paid", "1", "0", "0", "", 0), false},
		{"has a line", billFull("x2x2x2x2x2x2x2x2x2x2x2x2", "full_paid", "1", "0", "0", "", 1), false},
		{"has money on the bill", billFull("x3x3x3x3x3x3x3x3x3x3x3x3", "full_paid", "1", "1500", "0", "", 0), false},
		{"has an outstanding balance",
			billFull("x4x4x4x4x4x4x4x4x4x4x4x4", "full_paid", "1", "0", "1500", "", 0), false},
		{"inside MoneyTolerance is still zero",
			billFull("x5x5x5x5x5x5x5x5x5x5x5x5", "send", "1", "0.005", "0", "", 0), true},
	}
	for _, c := range cases {
		if got := isEmptyZeroValueBill(c.doc, links); got != c.want {
			t.Errorf("%s -> %v, want %v", c.name, got, c.want)
		}
	}
}
