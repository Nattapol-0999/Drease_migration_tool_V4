package legacyimport

import (
	"fmt"
	"sort"
	"strings"

	"system_cliv4/internal/mongodump"
)

// Course entitlements: what a customer PAID FOR and has not used yet.
//
//	course_bills               → course_bought
//	course_bills.course_list[]          → course_bought_items (kind='treatment')
//	course_bills.course_list_medicien[] → course_bought_items (kind='medication')
//	bills.list[] (use_course=true)      → course_bought_usage
//
// This is the one import that DELETES before it writes, by an explicit decision of the
// clinic owner. The target already held 169 rows carrying the same migrate_source_ref
// values (the legacy ObjectIds), so an earlier run had imported this collection and
// stopped halfway: price was 0.00 on every row, code was blank, invoice_id /
// invoice_item_id / catalog_item_id were NULL on all 169, and course_bought_items was
// EMPTY — so no course knew which treatments it contained and nothing could be deducted.
// Patching those rows in place was the alternative; the owner chose to replace them.
//
// The delete is safe to state precisely: every foreign key pointing at course_bought is
// ON DELETE CASCADE (course_bought_items, course_bought_usage, course_share,
// course_share_entitlement) or ON DELETE SET NULL (doctorbill), and all of those tables
// hold ZERO rows for this clinic. That was checked against the live schema, not assumed.
//
// WHAT IS NOT WRITTEN, and why
//
// course_usage stays empty.
//
// course_bought_usage IS written, decided by the clinic owner on 2 Oct 2026. The
// earlier rule here said it could not be: course_bills keeps a remaining count and no
// dates, so a usage history would have to be invented. That was true of course_bills
// and wrong about the export as a whole — the deductions are on the BILLS, as lines of
// kind 'course' with no `course_bill` pointer (planBillLines turns exactly those into
// invoice_item.use_course=true). Each one carries a real timestamp, a real branch and a
// real item code. What it does NOT carry is which entitlement it came off, because the
// pointer field is empty by definition for a deduction.
//
// So the WHEN is imported and the WHICH is decided here, FIFO by purchase date, by the
// owner's explicit choice over the alternative of writing only the unambiguous ones.
// At the clinic this was decided for, 18,545 of 39,790 matched deductions had exactly
// one candidate; the rest had 2 to 86. assignCourseUsage never exceeds the balance the
// source reported (that is the capacity it spends), and every deduction it cannot place
// is counted in the report rather than dropped silently.
//
// periods_used / course_bought_items.used are NOT recomputed from the ledger for
// treatments: the source's remaining count is the balance the clinic has been working
// from, and the import is not the place to move it. For a course's DRUGS the source
// keeps no remaining count at all, so there the ledger is the only thing that can say
// how many were taken, and `used` is filled from it.

// Entitlement entity names, in write order.
const (
	EntCourseBoughtPurge = "course_bought(ลบของเดิม)"
	EntCourseBought      = "course_bought"
	EntCourseBoughtItems = "course_bought_items"
	EntCourseBoughtUsage = "course_bought_usage"
)

var courseBoughtOrder = []string{
	EntCourseBoughtPurge,
	EntCourseBought,
	EntCourseBoughtItems,
	EntCourseBoughtUsage,
}

// courseStatusForV4 maps the legacy entitlement status onto the three values V4 actually
// understands — active / used / expire (internal/db/query/course_report.sql,
// portal.sql, course_bought.sql). There is no fourth.
//
//	active    116 → active
//	expire     17 → expire
//	inactive   32 → expire   ← decided by the clinic owner
//
// The `inactive` case needed a decision because V4 has no such value, and writing it
// verbatim would leave 32 paid-for courses visible in the customer portal but impossible
// to deduct — every V4 query that matters filters on status='active'. Mapping to `used`
// was rejected on the data: 30 of the 32 still have sessions remaining, so calling them
// used would tell a customer they had spent something they had not. `expire` is V4's label
// for "closed but NOT forfeited" — portal.sql states outright that an expired course is a
// conversation with the customer, not a deletion — which is the nearest true thing to what
// `inactive` meant.
func courseStatusForV4(legacy string) (status string, known bool) {
	switch strings.ToLower(strings.TrimSpace(legacy)) {
	case "active":
		return "active", true
	case "expire", "expired":
		return "expire", true
	case "inactive":
		return "expire", true
	default:
		return "", false
	}
}

// entitlementPlan is one legacy course_bills row, resolved.
type entitlementPlan struct {
	sourceRef   string // course_bills._id → course_bought.migrate_source_ref
	hn          string
	patientName string
	branchCode  string

	// Three different names for the same course, kept apart because they resolve
	// differently at the target (see resolveCourseCatalogID in coursecatalog.go):
	courseCode  string // the code THIS tool's catalog pass minted (CSxxx)
	catalogName string // course_items.name_th — the course's own catalog name
	name        string // course_bills.name — what was typed on the bill at sale time

	price        string
	periodsTotal int
	periodsUsed  int
	isBuffet     bool
	startDate    string // SQL literal
	dueDate      string // SQL literal
	status       string
	createdAt    string // SQL literal

	// invoiceNo / saleSortOrder locate the sale. An empty invoiceNo means the entitlement
	// could not be tied to an imported bill, which is reported rather than hidden.
	invoiceNo     string
	saleSortOrder int
	hasSaleLine   bool

	lines []entitlementLine
}

// Line kinds course_bought_items accepts. A drug line is 'medication'; everything the
// course bundles as a service is 'treatment'. V4 maps the same two from
// catalog_item_child at sale time (billing/service.go), so these are its words.
const (
	courseLineTreatment  = "treatment"
	courseLineMedication = "medication"
)

// entitlementLine is one item bundled inside a customer's course — a treatment or a
// drug.
type entitlementLine struct {
	kind      string
	code      string
	name      string
	perCourse string
	qtyTotal  string
	used      string
}

// buildCourseBought renders the entitlement block.
func buildCourseBought(mc *mongoCatalog, money moneyBatches, in Input, rep *Report) (
	purgeSQL, boughtSQL, itemsSQL, usageSQL string, err error) {

	if len(mc.courseBills) == 0 {
		return "", "", "", "", nil
	}

	ePurge := rep.entity(EntCourseBoughtPurge)
	eBought := rep.entity(EntCourseBought)
	eItems := rep.entity(EntCourseBoughtItems)
	eUsage := rep.entity(EntCourseBoughtUsage)
	eBought.SourceRows = len(mc.courseBills)

	br := newBranchResolver(in)
	defer br.report(rep, "course_bought")

	plans, err := planEntitlements(mc, money, br, rep, eBought, eItems)
	if err != nil {
		return "", "", "", "", err
	}
	if len(plans) == 0 {
		rep.warnf("course_bought: ไม่มีสิทธิ์คอร์สที่นำเข้าได้เลยจาก %d แถว", len(mc.courseBills))
		return "", "", "", "", nil
	}

	// The purge is limited to the branches these plans actually write, so a second site's
	// import cannot delete the first site's entitlements. Taken from the resolved plans
	// rather than Input.BranchMap: what matters is where rows LAND, not what was offered.
	seen := map[string]bool{}
	var branches []string
	for _, p := range plans {
		if b := strings.TrimSpace(p.branchCode); b != "" && !seen[b] {
			seen[b] = true
			branches = append(branches, b)
		}
	}
	purgeSQL = renderCourseBoughtPurge(in.ClinicCode, branches)
	ePurge.Statements = 2
	boughtSQL = renderCourseBought(plans, in, eBought)
	// The deductions are resolved BEFORE the item lines are rendered: a course's drug
	// lines take their `used` from this assignment, so the ledger and the balance come
	// out of one decision instead of two that can disagree.
	usage := assignCourseUsage(plans, money.courseUsage, rep, eUsage)
	itemsSQL = renderCourseBoughtItems(plans, in, eItems)
	usageSQL = renderCourseBoughtUsage(plans, usage, in, eUsage)

	return purgeSQL, boughtSQL, itemsSQL, usageSQL, nil
}

// planEntitlements resolves and filters the entitlements.
func planEntitlements(mc *mongoCatalog, money moneyBatches, br *branchResolver, rep *Report,
	eBought, eItems *EntityResult) ([]entitlementPlan, error) {

	var plans []entitlementPlan
	seenRef := map[string]bool{}
	unknownStatus := map[string]int{}
	statusRemapped := 0
	noSaleLine, noInvoice := 0, 0
	unresolvedHeal := 0
	medLines, medNoItem, medZeroQty := 0, 0, 0
	// medicien_id → the drug's items code / name, the same index catalog.go builds for
	// list_medicien. Built once per run.
	itemCodeByID := map[string]string{}
	itemNameByID := map[string]string{}
	for _, it := range mc.items {
		id := docObjectID(it, "_id")
		if id == "" {
			continue
		}
		if c := it.Text("code"); c != "" {
			itemCodeByID[id] = c
		}
		if n := firstNonEmpty(it.Text("name_th"), it.Text("name_en")); n != "" {
			itemNameByID[id] = n
		}
	}
	buffetRows := 0

	for _, d := range mc.courseBills {
		ref := docObjectID(d, "_id")
		if ref == "" {
			eBought.skip("ไม่มี _id (ใช้เป็นคีย์กันซ้ำ)")
			continue
		}
		if seenRef[ref] {
			// migrate_source_ref is UNIQUE per clinic; two rows claiming one id would
			// mean one customer's balance overwrites another's.
			return nil, fmt.Errorf("course_bills: _id %q ซ้ำในไฟล์ — ใช้เป็น migrate_source_ref ซึ่งต้องไม่ซ้ำ", ref)
		}
		seenRef[ref] = true

		hn := strings.TrimSpace(firstNonEmpty(d.Text("data_hn.hn"), d.Text("hn")))
		if hn == "" {
			eBought.skip("ไม่มี HN — ไม่รู้ว่าเป็นสิทธิ์ของใคร")
			rep.warnf("course_bills %s: ไม่มี HN — ข้าม เพราะสิทธิ์ที่ไม่รู้เจ้าของ ตัดใช้ไม่ได้และหาเจ้าของไม่เจอ", ref)
			continue
		}

		courseItemID := docObjectID(d, "course_item_id")
		code := mc.courseCode[courseItemID]
		if code == "" {
			eBought.skip("course_item_id ไม่ตรงกับคอร์สในแคตตาล็อก")
			continue
		}

		status, known := courseStatusForV4(d.Text("status"))
		if !known {
			unknownStatus[firstNonEmpty(d.Text("status"), "(ว่าง)")]++
			eBought.skip("สถานะที่เครื่องมือนี้ไม่รู้จัก: " + firstNonEmpty(d.Text("status"), "(ว่าง)"))
			continue
		}
		if strings.EqualFold(strings.TrimSpace(d.Text("status")), "inactive") {
			statusRemapped++
		}

		// Where was it sold? Two independent links exist in the export and both are used:
		// the bill LINE that sold it (bills.list[].course_bill → this _id) gives the exact
		// invoice_item, and the entitlement's own bill_id gives at least the invoice.
		//
		// An entitlement whose bill was NOT imported gets neither. The clinic owner decided
		// those are SKIPPED when the bill was deleted in V2 (the sale was cancelled) and
		// KEPT when there was never a bill at all (a giveaway that never crossed a counter).
		sale, hasSale := money.courseSale[ref]
		billID := docObjectID(d, "bill_id")
		invoiceNo := ""
		switch {
		case hasSale:
			invoiceNo = sale.invoiceNo
		case billID == "":
			noInvoice++
		default:
			if no, ok := money.invoiceNoByBillID[billID]; ok {
				invoiceNo = no
				noSaleLine++
			} else {
				eBought.skip("บิลต้นทางถูกลบ จึงไม่มีใบแจ้งหนี้ให้ผูก")
				continue
			}
		}

		total := intFromMoney(d.Text("number_of_time_total_default"))
		remaining := intFromMoney(d.Text("number_of_time_total"))
		used := total - remaining
		if used < 0 {
			// More left than was bought. Not a number to carry forward silently.
			rep.warnf("course_bills %s (HN %s): คงเหลือ %d มากกว่าที่ซื้อ %d — ตั้งใช้ไปแล้ว = 0 และรายงานไว้",
				ref, hn, remaining, total)
			used = 0
		}
		buffet := docBool(d, "infinity_use")
		if buffet {
			buffetRows++
		}

		saleName, _ := clip(firstNonEmpty(d.Text("name"), code), 500)
		patientName, _ := clip(d.Text("data_hn.name"), 255)

		p := entitlementPlan{
			sourceRef:     ref,
			hn:            hn,
			patientName:   patientName,
			branchCode:    br.resolve(d.Text("create_by_branch")),
			courseCode:    code,
			catalogName:   mc.courseName[courseItemID],
			name:          saleName,
			price:         moneyLit(d.Text("price_of_course")),
			periodsTotal:  total,
			periodsUsed:   used,
			isBuffet:      buffet,
			startDate:     mongoDateLit(d, "start_date", "NULL"),
			dueDate:       mongoDateLit(d, "due_date", "NULL"),
			status:        status,
			createdAt:     mongoTSLit(d, "created_at", "now()"),
			invoiceNo:     invoiceNo,
			saleSortOrder: sale.sortOrder,
			hasSaleLine:   hasSale,
		}

		// The bundled treatments. Every one of the 290 lines in this export resolves to a
		// service, so an unresolved line is a real anomaly rather than routine.
		for _, line := range docArray(d, "course_list") {
			eItems.SourceRows++
			healID := docObjectID(line, "heal_id")
			childCode := mc.healCode[healID]
			if childCode == "" {
				unresolvedHeal++
				eItems.skip("heal_id ไม่ตรงกับบริการใด")
				continue
			}
			lineName, _ := clip(firstNonEmpty(mc.healName[healID], childCode), 500)
			perCourse := intFromMoney(firstNonEmpty(line.Text("number_of_time_default"), "1"))
			left := intFromMoney(line.Text("number_of_time"))
			lineUsed := perCourse - left
			if lineUsed < 0 {
				lineUsed = 0
			}
			p.lines = append(p.lines, entitlementLine{
				kind:      courseLineTreatment,
				code:      childCode,
				name:      lineName,
				perCourse: fmt.Sprintf("%d", perCourse),
				qtyTotal:  fmt.Sprintf("%d", perCourse),
				used:      fmt.Sprintf("%d", lineUsed),
			})
		}
		// The drugs the course includes. The source carries them after all:
		// medicien_id is the drug and qty_use how many units the course is worth.
		// course_bought_items takes them as kind='medication' — unlike catalog_course_line
		// it has no parent_service column, so the heal_id on the entry is not needed here.
		// Counted and skipped before 2 Oct 2026; imported since, by the owner's decision.
		for _, line := range docArray(d, "course_list_medicien") {
			eItems.SourceRows++
			medLines++
			drugID := docObjectID(line, "medicien_id")
			drugCode := itemCodeByID[drugID]
			if drugCode == "" {
				medNoItem++
				eItems.skip("ยาในคอร์ส: medicien_id ไม่ตรง items ใด")
				continue
			}
			qty := intFromMoney(firstNonEmpty(line.Text("qty_use"), "1"))
			if qty <= 0 {
				medZeroQty++
				eItems.skip("ยาในคอร์ส: qty_use เป็น 0 หรือน้อยกว่า")
				continue
			}
			drugName, _ := clip(firstNonEmpty(itemNameByID[drugID], drugCode), 500)
			p.lines = append(p.lines, entitlementLine{
				kind:      courseLineMedication,
				code:      drugCode,
				name:      drugName,
				perCourse: fmt.Sprintf("%d", qty),
				qtyTotal:  fmt.Sprintf("%d", qty),
				// `used` is filled by assignCourseUsage. The source keeps no remaining count
				// for a course's drugs, so the usage ledger is the only thing that can say how
				// many were taken — 0 until a deduction is placed on this line.
				used: "0",
			})
		}

		plans = append(plans, p)
	}

	// Deterministic output: the same export must produce a byte-identical script.
	sort.SliceStable(plans, func(i, j int) bool { return plans[i].sourceRef < plans[j].sourceRef })

	rep.notef("course_bought: นำเข้า %d สิทธิ์ · %d บรรทัดในคอร์ส (course_bought_items) — ปลายทางเดิมมี 0 บรรทัด จึงตัดสิทธิ์ไม่ได้เลยก่อนหน้านี้",
		len(plans), countLines(plans))
	if statusRemapped > 0 {
		rep.warnf("course_bought: %d สิทธิ์ที่ต้นทางเป็น status=inactive ถูกเขียนเป็น 'expire' — V4 รู้จักแค่ active/used/expire ถ้าเขียน inactive ตรง ๆ ลูกค้าจะเห็นคอร์สแต่ตัดใช้ไม่ได้ · 'expire' ใน V4 คือ 'ปิดไว้ ยังไม่ยึดสิทธิ์' ถ้าคลินิกจะเปิดคืนให้แก้เป็น active ทีละใบ",
			statusRemapped)
	}
	if noSaleLine > 0 {
		rep.notef("course_bought: %d สิทธิ์ผูกใบแจ้งหนี้ได้ แต่หาบรรทัดที่ขายไม่ได้ → invoice_item_id เป็น NULL (ต้นทางไม่มีบรรทัดชี้กลับ)", noSaleLine)
	}
	if noInvoice > 0 {
		rep.warnf("course_bought: %d สิทธิ์ไม่มี bill_id ในต้นทางเลย (ของแถมที่ไม่เคยผ่านบิล) → invoice_id เป็น NULL · ยอดคงเหลือใช้ได้ปกติ แต่สอบกลับไปหาการขายไม่ได้", noInvoice)
	}
	if len(unknownStatus) > 0 {
		var parts []string
		for s, c := range unknownStatus {
			parts = append(parts, fmt.Sprintf("%s×%d", s, c))
		}
		sort.Strings(parts)
		rep.warnf("course_bought: ข้ามสิทธิ์ที่มีสถานะไม่รู้จัก (%s) — ไม่เดาว่าลูกค้ายังใช้ได้หรือไม่", strings.Join(parts, ", "))
	}
	if unresolvedHeal > 0 {
		rep.warnf("course_bought_items: %d บรรทัดอ้างบริการที่ไม่มีใน listheals — คอร์สนั้นจะมีรายการไม่ครบ ตัดสิทธิ์รายการนั้นไม่ได้", unresolvedHeal)
	}
	if buffetRows > 0 {
		rep.notef("course_bought: %d สิทธิ์เป็นคอร์สแบบไม่จำกัดครั้ง (is_buffet) — V4 ไม่นับ periods เมื่อ is_buffet เป็น true", buffetRows)
	}
	if medLines > 0 {
		rep.notef("course_bought_items: นำเข้าบรรทัดยาในคอร์ส (course_list_medicien) %d บรรทัดเป็น kind='medication' — ต้นทางมี medicien_id กับ qty_use จริง ไม่ได้แต่งขึ้น · ข้าม [medicien_id ไม่ตรง items: %d, qty_use<=0: %d]",
			medLines, medNoItem, medZeroQty)
	}
	rep.notef("course_usage ไม่ถูกเขียน — V4 ไม่ได้ใช้ตารางนี้ · ประวัติการใช้สิทธิ์อยู่ที่ course_bought_usage")
	rep.notef("course_bought.course_id และ receipt_id ปล่อย NULL — ตัวชี้ที่ V4 ใช้จริงคือ catalog_item_id ส่วนงวดที่จ่าย ต้นทางไม่ได้ระบุ")

	return plans, nil
}

func countLines(plans []entitlementPlan) int {
	n := 0
	for _, p := range plans {
		n += len(p.lines)
	}
	return n
}

// docArray reads a field that must be an array of documents.
func docArray(d mongodump.Doc, path string) []mongodump.Doc {
	raw, ok := d.Get(path)
	if !ok || raw == nil {
		return nil
	}
	list, isArr := raw.([]any)
	if !isArr {
		return nil
	}
	out := make([]mongodump.Doc, 0, len(list))
	for _, it := range list {
		if doc, isDoc := it.(mongodump.Doc); isDoc {
			out = append(out, doc)
		}
	}
	return out
}

// renderCourseBoughtPurge removes the half-imported entitlements before the real ones are
// written.
//
// This is the ONLY destructive statement this tool emits, so it says out loud what it
// removes. It is scoped to rows that carry a migrate_source_ref — a row created by a HUMAN
// through the app has no such reference and is deliberately left alone, because replacing
// an import is not a licence to delete somebody's work.
//
// IT IS ALSO SCOPED TO THE BRANCHES THIS RUN WRITES, and that part was a real near-miss.
// The clinic-only scope was correct while a clinic meant one site. For a multi-site tenant
// it is not: importing site 2 would delete site 1's entitlements — measured on the-gloss,
// a run for BR00002 (2,199 entitlements) would have wiped BR00001's 5,513 rows imported
// hours earlier and never re-created them, because this export knows nothing about them.
// Deleting only the branches being rewritten keeps the "replace the previous import"
// intent without reaching into a branch this run cannot restore.
func renderCourseBoughtPurge(clinic string, branches []string) string {
	scope := ""
	scopeNote := "ทั้งคลินิก (การนำเข้านี้ครอบทุกสาขา)"
	if len(branches) > 0 {
		scope = fmt.Sprintf("\n   AND branch_code = ANY (ARRAY[%s])", sqlTextList(branches))
		scopeNote = "เฉพาะสาขา " + strings.Join(branches, ", ") + " (สาขาอื่นไม่ถูกแตะ)"
	}
	return fmt.Sprintf(`-- ⚠ ลบสิทธิ์คอร์สที่นำเข้าไว้รอบก่อน แล้วสร้างใหม่ทั้งชุด (มติเจ้าของคลินิก)
--
-- ขอบเขต: %s
--
-- ลบเฉพาะแถวที่มี migrate_source_ref = แถวที่มาจากการนำเข้า · แถวที่คนสร้างเองในระบบ
-- (migrate_source_ref เป็น NULL) ไม่ถูกแตะ เพราะการนำเข้าใหม่ไม่ใช่ใบอนุญาตลบงานคน
--
-- และจำกัดที่สาขาที่รอบนี้เขียนเท่านั้น เพราะคลินิกหลายสาขาใช้ clinic_code ร่วมกัน —
-- ถ้าลบทั้งคลินิก การนำเข้าสาขาหนึ่งจะลบสิทธิ์ของสาขาอื่นที่ไฟล์นี้ไม่มีข้อมูลจะสร้างคืน
--
-- ปลอดภัยเพราะ: FK ทุกตัวที่ชี้มาที่ course_bought เป็น ON DELETE CASCADE
-- (course_bought_items · course_bought_usage · course_share · course_share_entitlement)
-- หรือ ON DELETE SET NULL (doctorbill)
DO $purge$
DECLARE
    doomed   int;
    handmade int;
    other    int;
BEGIN
    SELECT count(*) FILTER (WHERE migrate_source_ref IS NOT NULL%s),
           count(*) FILTER (WHERE migrate_source_ref IS NULL),
           count(*) FILTER (WHERE migrate_source_ref IS NOT NULL%s)
      INTO doomed, handmade, other
      FROM course_bought
     WHERE clinic_code = %s;

    RAISE NOTICE 'course_bought: จะลบ %% แถวที่นำเข้าไว้ในสาขาที่รอบนี้เขียน · เก็บไว้ %% แถวที่คนสร้างเอง · เก็บไว้ %% แถวที่นำเข้าไว้ของสาขาอื่น',
                 doomed, handmade, other;
END $purge$;

DELETE FROM course_bought
 WHERE clinic_code = %s
   AND migrate_source_ref IS NOT NULL%s;

`, scopeNote, purgeCountFilter(branches, true), purgeCountFilter(branches, false),
		lit(clinic), lit(clinic), scope)
}

// purgeCountFilter renders the extra predicate that splits "rows this run replaces" from
// "imported rows of another branch, which stay". Empty branches means the run covers
// everything, so there is no other branch to count.
func purgeCountFilter(branches []string, inScope bool) string {
	if len(branches) == 0 {
		if inScope {
			return ""
		}
		return " AND false"
	}
	if inScope {
		return " AND branch_code = ANY (ARRAY[" + sqlTextList(branches) + "])"
	}
	return " AND NOT (branch_code = ANY (ARRAY[" + sqlTextList(branches) + "]))"
}

// sqlTextList renders a sorted, quoted list for an ARRAY[...] literal.
func sqlTextList(vals []string) string {
	sorted := append([]string(nil), vals...)
	sort.Strings(sorted)
	out := make([]string, 0, len(sorted))
	for _, v := range sorted {
		out = append(out, lit(v))
	}
	return strings.Join(out, ", ")
}

// courseBoughtIDByRef resolves course_bought.id from its migration reference, which is
// UNIQUE per clinic (uq_course_bought_migrate_src).
func courseBoughtIDByRef(clinic, ref string) string {
	return fmt.Sprintf(
		"(SELECT id FROM course_bought WHERE clinic_code = %s AND migrate_source_ref = %s)",
		lit(clinic), lit(ref))
}

// invoiceItemIDBySort resolves the invoice line that sold an entitlement.
func invoiceItemIDBySort(clinic, invoiceNo string, sortOrder int) string {
	return fmt.Sprintf(
		"(SELECT ii.id FROM invoice_item ii WHERE ii.clinic_code = %s AND ii.invoice_id = %s AND ii.sort_order = %d)",
		lit(clinic), invoiceIDByNo(clinic, invoiceNo), sortOrder)
}

// patientIDByHN resolves patient.id, so the customer portal can find the entitlement by
// account rather than by HN string.
func patientIDByHN(clinic, hn string) string {
	return fmt.Sprintf(`(SELECT id FROM patient WHERE clinic_code = %s AND "HN" = %s)`, lit(clinic), lit(hn))
}

func renderCourseBought(plans []entitlementPlan, in Input, e *EntityResult) string {
	var s sqlBuilder
	for _, p := range plans {
		invoiceRef, itemRef := "NULL", "NULL"
		if p.invoiceNo != "" {
			invoiceRef = invoiceIDByNo(in.ClinicCode, p.invoiceNo)
			if p.hasSaleLine {
				itemRef = invoiceItemIDBySort(in.ClinicCode, p.invoiceNo, p.saleSortOrder)
			}
		}

		s.comment("HN %s · %s · เหลือ %d จาก %d ครั้ง",
			p.hn, oneLine(p.name), p.periodsTotal-p.periodsUsed, p.periodsTotal)
		s.addf(`INSERT INTO course_bought
    (clinic_code, invoice_id, receipt_id, invoice_item_id, hn, patient_name, branch_code,
     course_id, code, name, price, qty, periods_total, periods_used, is_buffet,
     buffet_count, start_date, due_date, status, created_at, migrate_source_ref,
     patient_id, catalog_item_id)
SELECT %s, %s, NULL, %s, %s, %s, %s,
       NULL,
       %s,
       %s, %s, 1, %d, %d, %s,
       0, %s, %s, %s, %s, %s,
       %s,
       %s
WHERE NOT EXISTS (
    SELECT 1 FROM course_bought
     WHERE clinic_code = %s AND migrate_source_ref = %s
);

`, lit(in.ClinicCode), invoiceRef, itemRef, lit(p.hn), nullLit(p.patientName), lit(p.branchCode),
			courseCodeOfResolved(in.ClinicCode, p.courseCode, p.catalogName, p.name),
			lit(p.name), p.price, p.periodsTotal, p.periodsUsed, boolLit(p.isBuffet),
			p.startDate, p.dueDate, lit(p.status), p.createdAt, lit(p.sourceRef),
			patientIDByHN(in.ClinicCode, p.hn),
			resolveCourseCatalogID(in.ClinicCode, p.courseCode, p.catalogName, p.name),
			lit(in.ClinicCode), lit(p.sourceRef))
	}
	e.Statements = s.stmts
	return s.String()
}

func renderCourseBoughtItems(plans []entitlementPlan, in Input, e *EntityResult) string {
	var s sqlBuilder
	for _, p := range plans {
		if len(p.lines) == 0 {
			continue
		}
		s.comment("รายการในคอร์สของ HN %s · %s", p.hn, oneLine(p.name))
		for _, l := range p.lines {
			// heal_id stays 0 (its NOT NULL default): it is a legacy NUMERIC service id and
			// the source carries a Mongo ObjectId, which does not fit. Identity comes from
			// catalog_item_id + code, which is the pair migration 0294 resolves on.
			s.addf(`INSERT INTO course_bought_items
    (clinic_code, course_bought_id, kind, heal_id, code, name, unit,
     per_course, qty_total, used, created_at, catalog_item_id)
SELECT %s, %s, %s, 0, %s, %s, NULL,
       %s, %s, %s, %s, %s
WHERE %s IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM course_bought_items cbi
       WHERE cbi.clinic_code = %s
         AND cbi.course_bought_id = %s
         AND cbi.code = %s
         AND cbi.kind = %s
  );

`, lit(in.ClinicCode), courseBoughtIDByRef(in.ClinicCode, p.sourceRef), lit(l.kind),
				lit(l.code), lit(l.name),
				l.perCourse, l.qtyTotal, l.used, p.createdAt,
				catalogItemIDByCode(in.ClinicCode, l.code),
				courseBoughtIDByRef(in.ClinicCode, p.sourceRef),
				lit(in.ClinicCode), courseBoughtIDByRef(in.ClinicCode, p.sourceRef), lit(l.code),
				lit(l.kind))
		}
	}
	e.Statements = s.stmts
	return s.String()
}
