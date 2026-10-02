package legacyimport

import (
	"strings"
	"testing"

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

	drop, err := resolveDuplicateBillNos(bills, rep, e)
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
	drop, err := resolveDuplicateBillNos(bills, &Report{}, &EntityResult{})
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
	_, err := resolveDuplicateBillNos(bills, &Report{}, &EntityResult{})
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
	_, err := resolveDuplicateBillNos(bills, &Report{}, &EntityResult{})
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
	drop, err := resolveDuplicateBillNos(bills, &Report{}, &EntityResult{})
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
		drop, err := resolveDuplicateBillNos(bills, &Report{}, &EntityResult{})
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
	drop, err := resolveDuplicateBillNos(bills, rep, &EntityResult{})
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
	drop, err := resolveDuplicateBillNos(bills, &Report{}, &EntityResult{})
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
