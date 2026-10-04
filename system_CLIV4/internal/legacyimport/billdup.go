package legacyimport

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"system_cliv4/internal/mongodump"
)

// Two bills claiming one document number, and the one case where that can be resolved
// without losing money.
//
// WHY THIS EXISTS — measured, not guessed
//
// ปิ่นลักษณ์คลินิกการแพทย์แผนไทย, pulled 2 Oct 2026: 9,505 bills, 9,345 importable, and
// THREE document numbers claimed by more than one bill —
//
//	640202709 x3   HN 016400260  status send  total 0  created 16:26:52.182 / .226 / .272
//	640054443 x2   HN 016400480  status send  total 0  created 08:36:45.138 (identical)
//	640202665 x2   HN 016400260  status send  total 0  created 16:26:52.065 / .080
//
// Same patient, same day, milliseconds apart, every one an unpaid draft worth nothing.
// That is a save button pressed twice in V2, not two charges. In the 640054443 pair the
// two documents are not even equals: one carries several lines and the other carries a
// single line that is a subset of them.
//
// planBills used to refuse the whole script on the first of these, which is the right
// default — `invoice.no` is unique per clinic, and silently merging two real charges
// would make one of them vanish. But refusing also means a 9,345-bill clinic cannot be
// imported at all because of four worthless duplicates.
//
// SO THE RULE IS NARROW ON PURPOSE (owner's decision, 2 Oct 2026)
//
// A duplicate group is resolved automatically ONLY when every bill in it is an unpaid
// draft worth nothing: status `send` and price_total within MoneyTolerance of zero. The
// bill with the most lines is kept — in the measured pair that is the complete one — and
// ties go to whichever came first in the file, so two runs of the same dump produce the
// same script.
//
// Anything else still refuses the run. Two duplicates with DIFFERENT money, or any
// duplicate that was paid, is a question about a real charge and this tool does not get
// to answer it.
//
// WHAT WAS CONSIDERED AND REJECTED
//
//   - Switching to invoice_branch_no (MoneyUseBranchDocNumbers = true). Measured on the
//     same dump: 4,886 bills have no branch number at all and 361 branch numbers are
//     duplicated across 2,615 bills, one of them 466 times. Far worse than what it fixes.
//   - Suffixing the duplicates (640054443-2). Keeps every row at the price of inventing a
//     document number the clinic cannot look up in V2.

// zeroValueDraftStatus is the only status a bill can carry and still be dropped as a
// duplicate: V2's "issued but not paid". A paid bill is never silently dropped.
const zeroValueDraftStatus = "send"

// dupBillMember is one bill inside a group that shares a document number.
type dupBillMember struct {
	doc   mongodump.Doc
	id    string
	pos   int // position in the file: the deterministic tie-break
	lines int
}

// isZeroValueDraftBill reports whether a bill is an unpaid draft worth nothing, which is
// the only shape of duplicate this resolves without asking.
func isZeroValueDraftBill(d mongodump.Doc) bool {
	if strings.ToLower(strings.TrimSpace(d.Text("status"))) != zeroValueDraftStatus {
		return false
	}
	return math.Abs(moneyFloat(d.Text("price_total"))) <= MoneyTolerance
}

// billLineCount is the number of entries in bills.list[], read straight from the document.
//
// Counted here rather than from a planned billPlan on purpose: this decision runs BEFORE
// any bill is planned, so the lines of a bill that is about to be dropped are never
// mapped and never counted into the invoice_item statistics.
func billLineCount(d mongodump.Doc) int { return len(docArray(d, "list")) }

// describeBillForDup renders one member of a duplicate group for an error message.
func describeBillForDup(d mongodump.Doc) string {
	return fmt.Sprintf("%s (hn %s · สถานะ %s · ยอด %s · %d รายการ)",
		docObjectID(d, "_id"),
		strings.TrimSpace(d.Text("hn")),
		strings.TrimSpace(d.Text("status")),
		strings.TrimSpace(d.Text("price_total")),
		billLineCount(d))
}

// droppedIDs lists the members of a duplicate group except the one being kept.
func droppedIDs(g []dupBillMember, keep int) []string {
	out := make([]string, 0, len(g)-1)
	for i, m := range g {
		if i != keep {
			out = append(out, fmt.Sprintf("%s (%d รายการ)", m.id, m.lines))
		}
	}
	return out
}

// resolveDuplicateBillNos decides which bill keeps a document number that more than one
// bill claims, and returns the set of bill _ids to drop.
//
// It refuses — returning an error that stops the whole script — for any duplicate group
// that is not entirely unpaid zero-value drafts. See the note at the top of this file.
func resolveDuplicateBillNos(bills []mongodump.Doc, rep *Report, eInv *EntityResult) (map[string]bool, error) {
	groups := map[string][]dupBillMember{}
	for i, d := range bills {
		// Only bills that would actually be imported can collide: a `delete` bill never
		// reaches invoice.no, so it must not make a number look taken.
		if importIt, _ := billImportable(d.Text("status")); !importIt {
			continue
		}
		no := billDocNo(d)
		if no == "" {
			continue
		}
		groups[no] = append(groups[no], dupBillMember{
			doc: d, id: docObjectID(d, "_id"), pos: i, lines: billLineCount(d),
		})
	}

	nos := make([]string, 0, len(groups))
	for no, g := range groups {
		if len(g) > 1 {
			nos = append(nos, no)
		}
	}
	sort.Strings(nos) // deterministic reporting and a deterministic first refusal

	drop := map[string]bool{}
	resolved := make([]string, 0, len(nos))

	// Groups that cannot be resolved are COLLECTED, not returned on sight. Refusing at
	// the first one is what turned a single clinic into three round trips of pull,
	// create, generate, import, fail, delete: each run reported one problem and hid the
	// rest. One refusal now names every number that needs a decision.
	var refusals []string

	for _, no := range nos {
		g := groups[no]

		// Every member has to be an unpaid draft worth nothing, or this is a question
		// about real money and the run stops.
		unsafe := false
		for _, m := range g {
			if !isZeroValueDraftBill(m.doc) {
				unsafe = true
				break
			}
		}
		if unsafe {
			parts := make([]string, 0, len(g))
			for _, x := range g {
				parts = append(parts, describeBillForDup(x.doc))
			}
			refusals = append(refusals, fmt.Sprintf("%s ซ้ำ %d ใบ: %s", no, len(g), strings.Join(parts, " | ")))
			continue
		}

		// Keep the most complete one. In the measured pair the loser's single line is a
		// subset of the winner's, so "most lines" is also "loses nothing".
		best := 0
		for i := 1; i < len(g); i++ {
			if g[i].lines > g[best].lines || (g[i].lines == g[best].lines && g[i].pos < g[best].pos) {
				best = i
			}
		}
		for i, m := range g {
			if i == best {
				continue
			}
			drop[m.id] = true
			if eInv != nil {
				eInv.skip("เลขที่เอกสารซ้ำในต้นทาง (บันทึกซ้ำ · ใบร่างยอด 0) — เก็บใบที่มีรายการมากกว่า")
			}
		}
		resolved = append(resolved, fmt.Sprintf("%s: เก็บ %s (%d รายการ) ข้าม %s",
			no, g[best].id, g[best].lines, strings.Join(droppedIDs(g, best), ", ")))
	}

	if len(refusals) > 0 {
		return nil, fmt.Errorf("bills: เลขที่เอกสารซ้ำ %d เลขที่ข้ามอัตโนมัติไม่ได้ — invoice.no ต้องไม่ซ้ำต่อคลินิก "+
			"จึงปฏิเสธทั้งสคริปต์ · จะข้ามอัตโนมัติได้ ต้องเป็นใบร่างที่ยังไม่เก็บเงินและยอดเป็น 0 ทุกใบ "+
			"(สถานะ %s และ price_total = 0) · รายการทั้งหมด ไม่ใช่แค่เลขแรก:\n  %s",
			len(refusals), zeroValueDraftStatus, strings.Join(refusals, "\n  "))
	}

	if len(resolved) > 0 && rep != nil {
		rep.warnf("bills: เลขที่เอกสารซ้ำ %d เลข (เกินมา %d ใบ) — ทุกใบเป็นใบร่างยอด 0 สถานะ %s "+
			"ซึ่งคือการกดบันทึกซ้ำในระบบเก่า จึงเก็บใบที่มีรายการมากที่สุดไว้ใบเดียวต่อเลข เงินไม่หายไป "+
			"(ถ้าใบที่ซ้ำมียอดไม่เท่ากันหรือเก็บเงินแล้ว จะยังปฏิเสธทั้งสคริปต์เหมือนเดิม) · %s",
			len(resolved), len(drop), zeroValueDraftStatus, strings.Join(resolved, " · "))
	}
	return drop, nil
}
