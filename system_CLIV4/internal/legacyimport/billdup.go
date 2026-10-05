package legacyimport

import (
	"fmt"
	"math"
	"sort"
	"strconv"
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
//     document number the clinic cannot look up in V2. Rejected HERE, for this shape —
//     where both bills are the same patient's double-click — and later adopted for a
//     different shape, two different patients. See door three below.

// A SECOND SHAPE, MEASURED LATER (owner's decisions ก2 then 1ก, 4 Oct 2026)
//
// The `send` rule above refuses a duplicate whose bills are marked paid, whatever their
// total. That turned out to be too tight for the way V2 records a COURSE DEDUCTION.
//
// Measured on the 4 Oct export (6,157 bills, 6,155 distinct numbers, 2 numbers claimed
// twice):
//
//	650146639 x2   HN 016400278  status full_paid  total 0  balance 0  0 lines
//	               both carry usecourse 6210d58730e636069367cf3e
//	               created 11:34:46.479 and .486 — seven milliseconds apart
//	               neither has a payment_summary row on either side of the link
//	640073635 x2   HN 016400118  status delete     total 0  0 and 1 lines
//	               already skipped, because `delete` bills are not imported
//
// A course deduction is `full_paid` because there is nothing left to collect — the money
// was taken when the course was sold. So "paid" here does not mean a charge exists, and
// refusing on the status alone blocks a 6,157-bill clinic over a double-click.
//
// The new door is narrow and measurable rather than status-based. A group may be resolved
// when EVERY bill in it:
//
//   - has no lines at all (bills.list is empty), AND
//   - has a zero total AND a zero outstanding balance, AND
//   - carries no money on any receipt attached to it: every payment_summary row linked to
//     the bill, in either direction, has zero in every channel of moneyChannels AND a
//     zero payment_type.price_total
//
// Together those are evidence that no money and no content exist, which the status field
// cannot give. In the measured export only SIX bills out of 6,157 have no lines at all,
// and four of them are these duplicates — while 3,246 bills are `full_paid` with a zero
// total and real lines. Had this keyed on status instead, those 3,246 would have become
// droppable too, and a future collision between two genuine course deductions would
// quietly lose one of them.
//
// THE RECEIPT RULE CHANGED ONCE, ON MEASUREMENT (owner's decision 1ก)
//
// The first version of this door refused whenever ANY payment_summary row was attached.
// Measured against the export, that refused the very pair it was written for: this dump
// links every one of its 4,741 payments through payment_summary.bill_id (the bill's own
// pay_id never matches a payment _id), and each of the two duplicates has its own receipt
// — 650112321 and 650112440, both zero in every channel. So "a receipt exists" turned out
// to be the normal state of a course deduction, not a sign of money.
//
// What matters is whether the receipt carries money, and that is read with the same
// positiveMoney and payment_type fields planReceipts uses, so the two can never disagree
// about what counts as a payment.
//
// WHAT ACTUALLY REACHES V4, WHICH IS LESS THAN "ONE OF THE TWO"
//
// Measured on the generated script: NEITHER bill is imported. Dropping the duplicate is
// what lets the file be produced, but the survivor then meets the older rule in planBills
// — a bill whose planned lines come to nothing is skipped, because a bill with no lines
// cannot be totalled — and the receipts of both bills become payments pointing at a bill
// that is not there, which planReceipts skips in turn.
//
// So the real outcome for a group like this is: the document number and every receipt
// number under it are absent from V4, and the rest of the clinic imports. The report says
// that rather than "kept the latest", because an operator reading "kept" would go looking
// for a bill that is not there.
//
// WHY THE DROP IS STILL NECESSARY
//
// It would seem simpler to leave both bills alone and let planBills skip them. It does not
// work: planBills reserves the document number BEFORE it plans the lines, and a second
// guard then refuses a number that is still claimed twice ("ยังซ้ำหลังจากตัวแก้เลขซ้ำ
// ทำงานแล้ว"). Measured — that alternative was built and run against this dump, and it
// refused the whole script. The duplicate has to be resolved here, by name.
//
// WHICH BILL SURVIVES (owner: "เลขบิลซ้ำ ให้เอาตัวล่าสุด")
//
// The latest created_at wins, with position in the file breaking a tie — in the 4 Oct
// export every bill carries created_at and no duplicate pair shares one to the
// millisecond, while the ปิ่นลักษณ์ export did have a pair with identical timestamps.
// Line counts cannot decide this door because every member has zero lines by definition.
//
// Door one is deliberately left alone: it admits bills that DO have lines, and the owner's
// 2 Oct rule there — keep the most complete document — was decided against a measured pair
// whose loser was a subset of the winner. Switching that one to "latest" would throw away
// real invoice lines.

// A THIRD SHAPE: ONE NUMBER, TWO DIFFERENT PATIENTS (owner's decisions ก then แคบ, 4 Oct 2026)
//
// Doors one and two both end in a bill being dropped, so both are narrow enough that
// dropping cannot lose anything: the members are worthless drafts, or empty and unpaid.
// Neither can help with a duplicate where both bills are REAL, and the Kasetsart
// University infirmary export (9f815aaa, 4 Oct 2026) is nothing but that shape.
//
// Measured on it — 43,074 bills, 42,926 importable and numbered, 47 numbers claimed twice,
// every group exactly two bills:
//
//	groups whose two bills share an HN   0
//	groups naming two DIFFERENT patients 47
//	status of the older copy             full_paid 42 · send 5
//	status of the newer copy             send 47
//	value on the older copies            ฿14,305   · 118 lines · ฿2,155 collected
//	value on the newer copies            ฿14,962.25 · 142 lines · ฿0 collected
//	bills with no lines at all           0, on either side
//	months                               older Jan–Jun 2026, newer Jun–Aug 2026
//
// Five months apart, different patients, real lines on both sides. That is not a save
// button pressed twice: V2's counter wrapped and started handing out numbers it had
// already issued. Receipt numbers are untouched by it — all 33,754 are distinct.
//
// So there is nothing here to drop. Both bills are somebody's visit, and the older one is
// the copy that was paid and has a receipt under it.
//
// THE RULE: THE OLDER COPY KEEPS THE NUMBER, THE NEWER ONE GETS A SUFFIX
//
// 690008624 stays with the bill that was issued first — the one the clinic can still look
// up in V2, and the one the receipt belongs to — and the later bill is written as
// 690008624-2. Nothing is dropped, no money moves, and 47 of 42,926 documents (0.11%)
// carry a number with a suffix on it.
//
// WHY "DIFFERENT PATIENTS" IS PART OF THE RULE, NOT AN AFTERTHOUGHT (owner: แคบ)
//
// Suffixing unconditionally would also admit the shape doors one and two exist for: the
// SAME patient's bill recorded twice, with lines on both copies. Importing both of those
// is charging one visit twice, which is worse than refusing — a refusal costs a round trip,
// a double charge costs the clinic's trust and is invisible once it is in.
//
// Two bills naming two different HNs cannot be one visit recorded twice. That is the whole
// test, it is measurable, and it admits all 47 of the measured groups while leaving the
// same-patient case exactly as refused as it was.
//
// Every member must also carry a created_at, because the rule turns on which copy is
// older. Without one there is no older copy, only a position in a file, and that is not a
// good enough reason to decide which patient keeps the real document number.
//
// PRECEDENCE, WHICH IS DELIBERATE
//
// Doors one and two are tried first and keep their behaviour even when the group names
// different patients. Both of them drop a document that holds no money and no content;
// this one invents a number. Where both could apply, dropping nothing of value beats
// inventing something. None of the 47 measured groups can reach door one or two anyway —
// every bill in them has lines and a non-zero total.
//
// WHY THIS CANNOT DISTURB V4's OWN NUMBERING
//
// V4 issues its own document numbers from doc_counter (AllocDocSeq → newReceiptNo,
// taxinvoice.FormatDocNo), never from MAX(invoice.no), so a legacy number with a suffix on
// it is an opaque string to V4 and cannot shift the next number it hands out. invoice.no is
// varchar(32) with no CHECK on its characters (migration 0076), and the suffix is checked
// against that width here rather than being discovered by psql.
//
// Re-running the same script is still a no-op: the generated INSERT dedups on
// (clinic_code, no), so the second run finds 690008624-2 already there, under the same HN.
// GUARD 4a is untouched and still refuses if a number this script wants is held at the
// target by a DIFFERENT patient.

// zeroValueDraftStatus is the only status that makes a bill droppable on its own: V2's
// "issued but not paid". A paid bill is only ever dropped through the empty-and-unpaid
// door described above.
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

// paymentLinks indexes payment_summary both ways a bill can be attached to one, so the
// checks below are map lookups rather than a scan per bill.
//
// Both directions are indexed because the export uses both: a bill points at its payment
// through pay_id, and a payment points back through bill_id. Trusting one of them would
// let a bill with money on it through whenever the dump recorded the link the other way.
// The documents are kept, not just their presence, because the question is how much money
// a receipt carries and not whether it exists.
type paymentLinks struct {
	byID   map[string][]mongodump.Doc // payment_summary._id
	byBill map[string][]mongodump.Doc // payment_summary.bill_id
}

func indexPaymentLinks(payments []mongodump.Doc) paymentLinks {
	l := paymentLinks{
		byID:   map[string][]mongodump.Doc{},
		byBill: map[string][]mongodump.Doc{},
	}
	for _, p := range payments {
		if id := docObjectID(p, "_id"); id != "" {
			l.byID[id] = append(l.byID[id], p)
		}
		if b := docObjectID(p, "bill_id"); b != "" {
			l.byBill[b] = append(l.byBill[b], p)
		}
	}
	return l
}

// attached returns every payment_summary row linked to this bill, from both directions,
// without listing the same document twice.
func (l paymentLinks) attached(d mongodump.Doc) []mongodump.Doc {
	var out []mongodump.Doc
	seen := map[string]bool{}
	add := func(docs []mongodump.Doc) {
		for _, p := range docs {
			id := docObjectID(p, "_id")
			if id != "" && seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, p)
		}
	}
	if id := docObjectID(d, "pay_id"); id != "" {
		add(l.byID[id])
	}
	if id := docObjectID(d, "_id"); id != "" {
		add(l.byBill[id])
	}
	return out
}

// receiptCarriesMoney reports whether one payment_summary row records any money.
//
// Read with the same fields planReceipts writes from — every channel in moneyChannels,
// plus payment_type.price_total, which is the bill value the receipt closed. A receipt
// that closed ฿1,500 with nothing in any channel is still about money: that is a discount
// or a write-off, and dropping it silently would erase it.
func receiptCarriesMoney(p mongodump.Doc) bool {
	for _, ch := range moneyChannels {
		if positiveMoney(p.Text("payment_type." + ch.field)) {
			return true
		}
	}
	return math.Abs(moneyFloat(p.Text("payment_type.price_total"))) > MoneyTolerance
}

// receiptNosFor lists the receipt numbers attached to a bill, for the report: dropping the
// bill drops them, and a document number that disappears has to be named.
func receiptNosFor(d mongodump.Doc, links paymentLinks) []string {
	var out []string
	for _, p := range links.attached(d) {
		if no := receiptDocNo(p); no != "" {
			out = append(out, no)
		}
	}
	sort.Strings(out)
	return out
}

// isEmptyZeroValueBill reports whether a bill holds nothing and no money reached it — the
// second shape of duplicate that can be resolved without asking. Status is deliberately
// not part of it: a course deduction is `full_paid` and still has no charge of its own.
func isEmptyZeroValueBill(d mongodump.Doc, links paymentLinks) bool {
	if billLineCount(d) != 0 {
		return false
	}
	if math.Abs(moneyFloat(d.Text("price_total"))) > MoneyTolerance {
		return false
	}
	// A zero total with an outstanding balance is a contradiction in the source. Refuse
	// rather than pick which of the two numbers to believe.
	if math.Abs(moneyFloat(d.Text("price_total_balance"))) > MoneyTolerance {
		return false
	}
	for _, p := range links.attached(d) {
		if receiptCarriesMoney(p) {
			return false
		}
	}
	return true
}

// billLineCount is the number of entries in bills.list[], read straight from the document.
//
// Counted here rather than from a planned billPlan on purpose: this decision runs BEFORE
// any bill is planned, so the lines of a bill that is about to be dropped are never
// mapped and never counted into the invoice_item statistics.
func billLineCount(d mongodump.Doc) int { return len(docArray(d, "list")) }

// describeBillForDup renders one member of a duplicate group for an error message.
//
// Whether a payment is attached is included because it is usually the reason the group
// was refused, and it is not something the operator can see from the other four fields.
func describeBillForDup(d mongodump.Doc, links paymentLinks) string {
	paid := ""
	for _, p := range links.attached(d) {
		if receiptCarriesMoney(p) {
			paid = fmt.Sprintf(" · มีใบรับเงินที่มีเงินจริง (%s)", receiptDocNo(p))
			break
		}
	}
	return fmt.Sprintf("%s (hn %s · สถานะ %s · ยอด %s · %d รายการ%s)",
		docObjectID(d, "_id"),
		strings.TrimSpace(d.Text("hn")),
		strings.TrimSpace(d.Text("status")),
		strings.TrimSpace(d.Text("price_total")),
		billLineCount(d),
		paid)
}

// ── door three: one number, two different patients ──────────────────────────────────

// dupSuffixSep separates a legacy document number from the copy counter door three adds.
//
// A hyphen, because V2's own numbers in this scheme are bare digits (YY0NNNNNN) and
// C-prefixed course bills (CYY0NNNNNN) — see applyDocNoBranchMarker — so nothing V2 issues
// can already look like a suffixed number, and an operator reading 690008624-2 can see
// both which V2 document it came from and that this importer renamed it.
const dupSuffixSep = "-"

// maxDupSuffix bounds the search for a free number. Two copies is what the measured export
// has; the ceiling exists so a pathological group cannot spin, and reaching it refuses.
const maxDupSuffix = 99

// invoiceNoLimit is what invoice.no can hold, read from the one table of column widths
// every generated value is checked against, so a suffix can never be the thing that
// overflows the column.
//
// Zero when the entry is missing, which makes suffixing refuse rather than guess — a width
// invented here could be wider than the database and would move the failure back to psql,
// which is the whole class of bug widths.go exists to end.
func invoiceNoLimit() int { return columnWidths["invoice"]["no"] }

// billsAreDifferentPatients reports whether every bill in the group names a different,
// non-blank HN — the evidence that the group cannot be one visit recorded twice.
//
// A blank HN disqualifies the group: planBills skips such a bill anyway, and "unknown" is
// not a patient that can be told apart from another "unknown".
func billsAreDifferentPatients(g []dupBillMember) bool {
	seen := map[string]bool{}
	for _, m := range g {
		hn := strings.TrimSpace(m.doc.Text("hn"))
		if hn == "" || seen[hn] {
			return false
		}
		seen[hn] = true
	}
	return true
}

// everyBillHasACreationTime reports whether the group can be ordered by age at all.
func everyBillHasACreationTime(g []dupBillMember) bool {
	for _, m := range g {
		if mongoTime(m.doc, "created_at").IsZero() {
			return false
		}
	}
	return true
}

// oldestFirst orders a duplicate group by age, with position in the file breaking a tie,
// so the same dump always renames the same bills.
func oldestFirst(g []dupBillMember) []dupBillMember {
	out := make([]dupBillMember, len(g))
	copy(out, g)
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := mongoTime(out[i].doc, "created_at"), mongoTime(out[j].doc, "created_at")
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return out[i].pos < out[j].pos
	})
	return out
}

// nextFreeSuffixedNo finds the first "<base>-N" that no other bill in this run claims and
// that still fits invoice.no.
//
// taken holds every number already claimed — the numbers read out of the file AND the
// suffixes handed out so far — because a suffix that collides with either would simply
// move the duplicate rather than resolve it.
func nextFreeSuffixedNo(base string, taken map[string]bool) (string, bool) {
	limit := invoiceNoLimit()
	if limit <= 0 {
		return "", false
	}
	for n := 2; n <= maxDupSuffix; n++ {
		cand := base + dupSuffixSep + strconv.Itoa(n)
		if len([]rune(cand)) > limit {
			return "", false
		}
		if !taken[cand] {
			return cand, true
		}
	}
	return "", false
}

// renumberLaterCopies gives every copy after the oldest a new document number, leaving the
// oldest with the original. It writes into renumber and taken, and returns one report line
// per renamed bill, or false when a free number that fits could not be found.
func renumberLaterCopies(
	no string,
	g []dupBillMember,
	renumber map[string]string,
	taken map[string]bool,
) ([]string, bool) {
	ordered := oldestFirst(g)
	assigned := map[string]string{}
	var lines []string
	for _, m := range ordered[1:] {
		next, ok := nextFreeSuffixedNo(no, taken)
		if !ok {
			return nil, false
		}
		taken[next] = true
		assigned[m.id] = next
		lines = append(lines, fmt.Sprintf("%s → %s (hn %s · สถานะ %s · ยอด %s · %d รายการ)",
			no, next,
			strings.TrimSpace(m.doc.Text("hn")),
			strings.TrimSpace(m.doc.Text("status")),
			strings.TrimSpace(m.doc.Text("price_total")),
			m.lines))
	}
	// Committed only once every copy in the group got a number: a half-renamed group
	// would leave the collision in place and refuse later with a confusing message.
	for id, next := range assigned {
		renumber[id] = next
	}
	return lines, true
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

// resolveDuplicateBillNos decides what happens to a document number that more than one
// bill claims. It returns the bill _ids to drop and the bill _ids to renumber (_id → the
// new document number), which planBills applies before anything is written.
//
// Three doors, in order, each narrower than the problem it solves — see the notes at the
// top of this file. Any group that opens none of them refuses the whole script, and the
// refusal names every such number at once rather than the first one.
func resolveDuplicateBillNos(
	bills, payments []mongodump.Doc,
	rep *Report,
	eInv *EntityResult,
) (map[string]bool, map[string]string, error) {
	links := indexPaymentLinks(payments)
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
	renumber := map[string]string{}
	resolved := make([]string, 0, len(nos))

	// Every number any importable bill claims, which is what a suffix must avoid. Door
	// three adds each suffix it hands out, so two groups cannot be given the same new
	// number and a suffix cannot land on a number the file already uses.
	taken := make(map[string]bool, len(groups))
	for no := range groups {
		taken[no] = true
	}

	// Groups that cannot be resolved are COLLECTED, not returned on sight. Refusing at
	// the first one is what turned a single clinic into three round trips of pull,
	// create, generate, import, fail, delete: each run reported one problem and hid the
	// rest. One refusal now names every number that needs a decision.
	var refusals []string
	// Resolved groups are reported separately per door, because "two worthless drafts",
	// "two empty course deductions" and "two different patients" are different facts
	// about the clinic's data and have different consequences.
	var resolvedEmpty []string
	var renamed []string

	for _, no := range nos {
		g := groups[no]

		// Door one: every member is an unpaid draft worth nothing.
		allDrafts := true
		for _, m := range g {
			if !isZeroValueDraftBill(m.doc) {
				allDrafts = false
				break
			}
		}
		// Door two: every member holds nothing and no money reached any of them.
		allEmpty := true
		for _, m := range g {
			if !isEmptyZeroValueBill(m.doc, links) {
				allEmpty = false
				break
			}
		}
		if !allDrafts && !allEmpty {
			// Door three: two real bills, but of DIFFERENT patients, so the group cannot
			// be one visit recorded twice and nothing may be dropped. The oldest copy
			// keeps the number and the later ones are suffixed.
			if billsAreDifferentPatients(g) && everyBillHasACreationTime(g) {
				if lines, ok := renumberLaterCopies(no, g, renumber, taken); ok {
					renamed = append(renamed, lines...)
					continue
				}
			}
			parts := make([]string, 0, len(g))
			for _, x := range g {
				parts = append(parts, describeBillForDup(x.doc, links))
			}
			refusals = append(refusals, fmt.Sprintf("%s ซ้ำ %d ใบ: %s", no, len(g), strings.Join(parts, " | ")))
			continue
		}

		best := 0
		if allDrafts {
			// Door one keeps the most complete document. In the measured pair the loser's
			// single line is a subset of the winner's, so "most lines" is also
			// "loses nothing".
			for i := 1; i < len(g); i++ {
				if g[i].lines > g[best].lines || (g[i].lines == g[best].lines && g[i].pos < g[best].pos) {
					best = i
				}
			}
		} else {
			// Door two keeps the LATEST (owner: เอาตัวล่าสุด). Line counts cannot decide
			// it — every member has none — so created_at does, and position in the file
			// breaks a tie, which the ปิ่นลักษณ์ export proved is a real case.
			for i := 1; i < len(g); i++ {
				ti, tb := mongoTime(g[i].doc, "created_at"), mongoTime(g[best].doc, "created_at")
				if ti.After(tb) || (ti.Equal(tb) && g[i].pos > g[best].pos) {
					best = i
				}
			}
		}
		skipReason := "เลขที่เอกสารซ้ำในต้นทาง (บันทึกซ้ำ · ใบร่างยอด 0) — เก็บใบที่มีรายการมากกว่า"
		if !allDrafts {
			skipReason = "เลขที่เอกสารซ้ำในต้นทาง (บันทึกซ้ำ · ไม่มีรายการ ยอด 0 ไม่มีเงินในใบรับเงิน) — เก็บใบล่าสุดไว้เป็นตัวแทน"
		}
		// Every receipt number under this document number, not just the dropped bills'.
		// The survivor of an all-empty group is skipped later by planBills anyway, so its
		// receipt goes too — reporting only the dropped bill's would read as if the other
		// one survived.
		var lostReceipts []string
		if !allDrafts {
			for _, m := range g {
				lostReceipts = append(lostReceipts, receiptNosFor(m.doc, links)...)
			}
			sort.Strings(lostReceipts)
		}
		for i, m := range g {
			if i == best {
				continue
			}
			drop[m.id] = true
			if eInv != nil {
				eInv.skip(skipReason)
			}
		}
		if allDrafts {
			resolved = append(resolved, fmt.Sprintf("%s: เก็บ %s (%d รายการ) ข้าม %s",
				no, g[best].id, g[best].lines, strings.Join(droppedIDs(g, best), ", ")))
		} else {
			line := fmt.Sprintf("%s: %d ใบ (%s) — ตัวแทนที่เก็บไว้คือใบล่าสุด %s",
				no, len(g), strings.Join(append(droppedIDs(g, best), g[best].id+" (ล่าสุด)"), ", "), g[best].id)
			if len(lostReceipts) > 0 {
				line += fmt.Sprintf(" · เลขที่ใบเสร็จใต้เลขนี้: %s", strings.Join(lostReceipts, ", "))
			}
			resolvedEmpty = append(resolvedEmpty, line)
		}
	}

	if len(refusals) > 0 {
		return nil, nil, fmt.Errorf("bills: เลขที่เอกสารซ้ำ %d เลขที่จัดการอัตโนมัติไม่ได้ — invoice.no ต้องไม่ซ้ำต่อคลินิก "+
			"จึงปฏิเสธทั้งสคริปต์ · จะจัดการอัตโนมัติได้ ต้องเข้าข้อใดข้อหนึ่ง และต้องเข้าทุกใบในกลุ่ม: "+
			"(1) เป็นใบร่างที่ยังไม่เก็บเงินและยอด 0 (สถานะ %s) → เก็บใบที่มีรายการมากสุด หรือ "+
			"(2) ไม่มีรายการเลย ยอด 0 ค้างชำระ 0 และใบรับเงินที่ผูกอยู่ต้องไม่มีเงินในช่องใดเลย (สถานะใดก็ได้) → เก็บใบล่าสุด หรือ "+
			"(3) เป็นของคนไข้ต่างคนกันทุกใบ และทุกใบมีเวลาที่สร้าง → ไม่ทิ้งใบไหน "+
			"แต่เติมท้ายเลขให้ใบที่ใหม่กว่า (เลขเดิม%s2) · "+
			"กลุ่มที่เป็นคนไข้คนเดียวกันยังปฏิเสธเสมอ เพราะอาจเป็นการบันทึกการมาครั้งเดียวซ้ำสองใบ "+
			"ซึ่งถ้าเข้าทั้งคู่จะกลายเป็นคิดเงินซ้ำ · รายการทั้งหมด ไม่ใช่แค่เลขแรก:\n  %s",
			len(refusals), zeroValueDraftStatus, dupSuffixSep,
			strings.Join(refusals, "\n  "))
	}

	if len(resolved) > 0 && rep != nil {
		rep.warnf("bills: เลขที่เอกสารซ้ำ %d เลข — ทุกใบเป็นใบร่างยอด 0 สถานะ %s "+
			"ซึ่งคือการกดบันทึกซ้ำในระบบเก่า จึงเก็บใบที่มีรายการมากที่สุดไว้ใบเดียวต่อเลข เงินไม่หายไป · %s",
			len(resolved), zeroValueDraftStatus, strings.Join(resolved, " · "))
	}
	if len(resolvedEmpty) > 0 && rep != nil {
		rep.warnf("bills: เลขที่เอกสารซ้ำ %d เลข — ทุกใบไม่มีรายการเลย ยอด 0 และใบรับเงินที่ผูกอยู่ไม่มีเงินในช่องใดเลย "+
			"(รูปแบบของการกดบันทึกซ้ำตอนตัดใช้คอร์ส ซึ่ง V2 ให้สถานะ full_paid เพราะเก็บเงินไปตอนขายคอร์สแล้ว) "+
			"จึงตัดให้เหลือใบเดียวต่อเลข โดยเก็บใบล่าสุดตามมติเจ้าของคลินิก เพื่อให้สร้างสคริปต์ได้ · "+
			"**ผลจริงคือเลขที่เอกสารเหล่านี้และใบเสร็จใต้มันจะไม่เข้า V4 เลย** เพราะใบที่เก็บไว้ยังไม่มีรายการ "+
			"จึงถูกข้ามอีกชั้นด้วยกฎ 'ใบที่ไม่มีรายการคิดยอดไม่ได้' และใบเสร็จที่ไม่มีบิลก็ถูกข้ามตามไป · "+
			"ไม่มีเงินหายเพราะทุกใบและทุกใบเสร็จเป็น 0 บาท · %s",
			len(resolvedEmpty), strings.Join(resolvedEmpty, " · "))
	}
	if len(renamed) > 0 && rep != nil {
		rep.warnf("bills: เลขที่เอกสารซ้ำ %d เลข ที่เป็นของ**คนไข้ต่างคนกัน** — ระบบเก่าวนเลขกลับมาใช้ซ้ำ "+
			"ไม่ใช่การกดบันทึกซ้ำ จึงไม่ทิ้งใบไหนเลยตามมติเจ้าของคลินิก: ใบที่เก่ากว่าคงเลขเดิมไว้ "+
			"(เป็นใบที่ค้นใน V2 ได้ และเป็นใบที่ใบเสร็จผูกอยู่) ส่วนใบที่ใหม่กว่าถูกเติมท้ายเลขเป็น %s2 · "+
			"บิลเข้า V4 ครบทุกใบ เงินไม่หาย รายการไม่หาย · "+
			"ผลข้างเคียงที่ต้องรู้: %d ใบนี้ถือเลขที่เอกสารที่ค้นใน V2 ตรงตัวไม่ได้ ต้องตัดท้าย %s2 ออกก่อน · %s",
			len(renamed), dupSuffixSep, len(renamed), dupSuffixSep, strings.Join(renamed, " · "))
	}
	if (len(resolved) > 0 || len(resolvedEmpty) > 0) && rep != nil {
		rep.notef("bills: ถ้าใบที่ซ้ำมียอดไม่เท่ากัน เก็บเงินแล้ว หรือมีใบรับเงินผูกอยู่ จะยังปฏิเสธทั้งสคริปต์เหมือนเดิม "+
			"(ข้ามไปทั้งหมด %d ใบจาก %d เลข)", len(drop), len(resolved)+len(resolvedEmpty))
	}
	return drop, renumber, nil
}
