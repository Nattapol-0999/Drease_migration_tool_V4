package legacyimport

import (
	"fmt"
	"sort"
)

// Course DEDUCTIONS: when each session was actually taken.
//
//	bills.list[] where kind='course' and course_bill is empty → course_bought_usage
//
// Where the data comes from, and what it does not contain
//
// A legacy bill line of kind 'course' means one of two opposite things, told apart by a
// single field: with a `course_bill` pointer it SOLD an entitlement, without one it
// CONSUMED a session. planBillLines in money.go is where that is decided, and
// invoice_item.use_course is the flag it writes. This file reads the second kind.
//
// Such a line carries a real timestamp, a real branch and a real item code. It does not
// carry which entitlement it came off — the pointer that would say so is empty, by
// definition, on exactly these lines. Nothing in the export recovers it.
//
// The clinic owner chose FIFO by purchase date over importing only the deductions that
// had a single possible source. The alternative was measured first, on the clinic this
// was decided for: of 39,790 deductions that matched a course the patient owned, 18,545
// had one candidate and the rest had between 2 and 86.
//
// What FIFO is NOT allowed to do
//
// It never spends more than the source already said was spent. A treatment line's
// capacity is its `used` count from course_bills — so the ledger can at most explain the
// balance the clinic has been working from, never move it. A deduction that finds no
// line with capacity left is counted in the report, not forced onto a full one.
//
// A course's DRUGS are the one place the ledger decides the balance, because the source
// keeps no remaining count for them at all. There capacity is the full quantity and
// `used` is written from what this assignment placed.

// usageSlot addresses one line inside one plan.
type usageSlot struct {
	planIdx int
	lineIdx int
}

// usageRow is one resolved deduction, ready to render.
type usageRow struct {
	slot      usageSlot
	sessions  int
	usedAt    string // SQL literal
	branch    string
	invoiceNo string // the bill the deduction was taken on — for the generated comment
}

// assignCourseUsage places every deduction on a course line and returns the ledger.
//
// It mutates the `used` of medication lines (and only those) for the reason given above.
// Both the candidate order and the order of the deductions themselves are sorted, so the
// same export produces a byte-identical script.
func assignCourseUsage(plans []entitlementPlan, usage []courseUsageRef, rep *Report,
	e *EntityResult) []usageRow {
	e.SourceRows = len(usage)
	if len(plans) == 0 || len(usage) == 0 {
		return nil
	}
	// Candidates per (HN, item code), oldest purchase first — that is the FIFO.
	byKey := map[string][]usageSlot{}
	for pi := range plans {
		for li := range plans[pi].lines {
			k := plans[pi].hn + "\x00" + plans[pi].lines[li].code
			byKey[k] = append(byKey[k], usageSlot{pi, li})
		}
	}
	for k := range byKey {
		cands := byKey[k]
		sort.SliceStable(cands, func(i, j int) bool {
			a, b := plans[cands[i].planIdx], plans[cands[j].planIdx]
			if a.createdAt != b.createdAt {
				return a.createdAt < b.createdAt
			}
			return a.sourceRef < b.sourceRef
		})
	}
	// How much each line may still account for. See the file comment: a treatment is
	// capped at what the source says was used, a drug at the full quantity.
	room := map[usageSlot]int{}
	for pi := range plans {
		for li := range plans[pi].lines {
			l := plans[pi].lines[li]
			if l.kind == courseLineMedication {
				room[usageSlot{pi, li}] = intFromMoney(l.qtyTotal)
				continue
			}
			room[usageSlot{pi, li}] = intFromMoney(l.used)
		}
	}
	refs := append([]courseUsageRef(nil), usage...)
	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].createdAt != refs[j].createdAt {
			return refs[i].createdAt < refs[j].createdAt
		}
		if refs[i].invoiceNo != refs[j].invoiceNo {
			return refs[i].invoiceNo < refs[j].invoiceNo
		}
		return refs[i].sortOrder < refs[j].sortOrder
	})
	var out []usageRow
	placedMed := map[usageSlot]int{}
	noCourse, noRoom, noCode := 0, 0, 0
	for _, r := range refs {
		if r.code == "" {
			noCode++
			e.skip("บรรทัดตัดสิทธิ์ไม่มีรหัสสินค้า — หาคอร์สที่ถูกหักไม่ได้")
			continue
		}
		cands := byKey[r.hn+"\x00"+r.code]
		if len(cands) == 0 {
			noCourse++
			e.skip("บรรทัดตัดสิทธิ์ไม่ตรงกับรายการในคอร์สใดของคนไข้คนนั้น")
			continue
		}
		qty := intFromMoney(r.qty)
		if qty <= 0 {
			qty = 1
		}
		placed := false
		for _, s := range cands {
			if room[s] < qty {
				continue
			}
			room[s] -= qty
			if plans[s.planIdx].lines[s.lineIdx].kind == courseLineMedication {
				placedMed[s] += qty
			}
			branch := r.branchCode
			if branch == "" {
				// branch_code is NOT NULL with a non-blank CHECK, and the course the
				// session came off is the only other place that knows a branch.
				branch = plans[s.planIdx].branchCode
			}
			out = append(out, usageRow{
				slot:      s,
				sessions:  qty,
				usedAt:    r.createdAt,
				branch:    branch,
				invoiceNo: r.invoiceNo,
			})
			placed = true
			break
		}
		if !placed {
			noRoom++
			e.skip("ทุกใบที่เข้าเกณฑ์ถูกหักครบตามยอดที่ต้นทางบอกแล้ว — ไม่หักเกิน")
		}
	}
	for s, n := range placedMed {
		plans[s.planIdx].lines[s.lineIdx].used = fmt.Sprintf("%d", n)
	}
	rep.notef("course_bought_usage: ลงประวัติการใช้สิทธิ์ %d รายการจากบรรทัดตัดสิทธิ์ %d บรรทัดในบิล — วันเวลา/สาขา/รหัสของจริงจากบิล ส่วน 'หักจากใบไหน' ใช้ FIFO ตามวันที่ซื้อ (มติเจ้าของคลินิก) เพราะต้นทางไม่ได้บันทึกไว้",
		len(out), len(usage))
	if noCourse > 0 {
		rep.warnf("course_bought_usage: %d บรรทัดตัดสิทธิ์ไม่ตรงกับรายการในคอร์สใดของคนไข้คนนั้น — ไม่สร้างคอร์สขึ้นมารองรับ จึงไม่มีประวัติการใช้ของบรรทัดนั้น", noCourse)
	}
	if noRoom > 0 {
		rep.warnf("course_bought_usage: %d บรรทัดตัดสิทธิ์เกินยอดที่ต้นทางบอกว่าใช้ไปแล้ว — ไม่เขียน เพราะจะทำให้ยอดใช้มากกว่ายอดที่ซื้อ ยอดคงเหลือเดิมไม่ถูกแตะ", noRoom)
	}
	if noCode > 0 {
		rep.warnf("course_bought_usage: %d บรรทัดตัดสิทธิ์ไม่มีรหัสสินค้าในต้นทาง — หาคอร์สที่ถูกหักไม่ได้", noCode)
	}
	return out
}

// courseBoughtItemIDByCode resolves the course line a deduction came off. kind is part of
// the key because a course can bundle a service and a drug under codes this tool cannot
// promise are distinct.
func courseBoughtItemIDByCode(clinic, ref, code, kind string) string {
	return fmt.Sprintf(
		"(SELECT cbi.id FROM course_bought_items cbi"+
			" WHERE cbi.clinic_code = %s AND cbi.course_bought_id = %s"+
			" AND cbi.code = %s AND cbi.kind = %s)",
		lit(clinic), courseBoughtIDByRef(clinic, ref), lit(code), lit(kind))
}

// renderCourseBoughtUsage writes the ledger.
//
// invoice_id / receipt_id are the PURCHASE documents, not the bill the session was taken
// on. That is what the column means in V4 — InsertCourseBoughtUsage snapshots them from
// course_bought — and every usage row written by the live app follows it. receipt_id is
// left NULL because the source does not say which instalment paid for the course.
//
// material_cost 0.00 with material_cost_state='complete' is the clinic owner's decision
// of 2 Oct 2026, matching what the live app writes. It is a stated figure, not a cost
// this import computed: the legacy export carries no lot cost for a past deduction.
//
// visit_ref / visit_date stay NULL. visit_ref is varchar(8) and holds a VN (migration
// 0449); a bill number is neither, and guessing a visit would invent a clinical record.
func renderCourseBoughtUsage(plans []entitlementPlan, rows []usageRow, in Input,
	e *EntityResult) string {
	var s sqlBuilder
	for _, r := range rows {
		p := plans[r.slot.planIdx]
		l := p.lines[r.slot.lineIdx]
		invoiceRef := "NULL"
		if p.invoiceNo != "" {
			invoiceRef = invoiceIDByNo(in.ClinicCode, p.invoiceNo)
		}
		itemRef := courseBoughtItemIDByCode(in.ClinicCode, p.sourceRef, l.code, l.kind)
		s.comment("HN %s · ใช้ %s ×%d เมื่อ %s (บิล %s · คอร์ส %s)",
			p.hn, oneLine(l.name), r.sessions, r.usedAt, r.invoiceNo, oneLine(p.name))
		s.addf(`INSERT INTO course_bought_usage
    (clinic_code, course_bought_id, course_bought_item_id, invoice_id, receipt_id,
     kind, code, name, sessions_used, used_by, branch_code, used_at, created_at,
     catalog_item_id, material_cost, material_cost_state)
SELECT %s, %s, %s, %s, NULL,
       %s, %s, %s, %d, NULL, %s, %s, %s,
       %s, 0.00, 'complete'
WHERE %s IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM course_bought_usage u
       WHERE u.clinic_code = %s
         AND u.course_bought_item_id = %s
         AND u.used_at = %s
         AND u.sessions_used = %d
  );
`, lit(in.ClinicCode), courseBoughtIDByRef(in.ClinicCode, p.sourceRef), itemRef,
			invoiceRef,
			lit(l.kind), lit(l.code), lit(l.name), r.sessions, lit(r.branch),
			r.usedAt, r.usedAt,
			catalogItemIDByCode(in.ClinicCode, l.code),
			itemRef,
			lit(in.ClinicCode), itemRef, r.usedAt, r.sessions)
	}
	e.Statements = s.stmts
	return s.String()
}
