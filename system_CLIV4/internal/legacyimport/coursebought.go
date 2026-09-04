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
//	course_bills.course_list[] → course_bought_items (kind='treatment')
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
// course_usage / course_bought_usage stay empty. V4 records WHEN each session was used;
// the legacy collection records only how many are LEFT. Filling a usage history would
// mean inventing dates, so the balance is carried on the header (periods_used) and the
// history is honestly absent.

// Entitlement entity names, in write order.
const (
	EntCourseBoughtPurge = "course_bought(ลบของเดิม)"
	EntCourseBought      = "course_bought"
	EntCourseBoughtItems = "course_bought_items"
)

var courseBoughtOrder = []string{
	EntCourseBoughtPurge,
	EntCourseBought,
	EntCourseBoughtItems,
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

// entitlementLine is one bundled treatment inside a customer's course.
type entitlementLine struct {
	code      string
	name      string
	perCourse string
	qtyTotal  string
	used      string
}

// buildCourseBought renders the entitlement block.
func buildCourseBought(mc *mongoCatalog, money moneyBatches, in Input, rep *Report) (
	purgeSQL, boughtSQL, itemsSQL string, err error) {

	if len(mc.courseBills) == 0 {
		return "", "", "", nil
	}

	ePurge := rep.entity(EntCourseBoughtPurge)
	eBought := rep.entity(EntCourseBought)
	eItems := rep.entity(EntCourseBoughtItems)
	eBought.SourceRows = len(mc.courseBills)

	plans, err := planEntitlements(mc, money, rep, eBought, eItems)
	if err != nil {
		return "", "", "", err
	}
	if len(plans) == 0 {
		rep.warnf("course_bought: ไม่มีสิทธิ์คอร์สที่นำเข้าได้เลยจาก %d แถว", len(mc.courseBills))
		return "", "", "", nil
	}

	purgeSQL = renderCourseBoughtPurge(in.ClinicCode)
	ePurge.Statements = 2
	boughtSQL = renderCourseBought(plans, in, eBought)
	itemsSQL = renderCourseBoughtItems(plans, in, eItems)

	return purgeSQL, boughtSQL, itemsSQL, nil
}

// planEntitlements resolves and filters the entitlements.
func planEntitlements(mc *mongoCatalog, money moneyBatches, rep *Report,
	eBought, eItems *EntityResult) ([]entitlementPlan, error) {

	var plans []entitlementPlan
	seenRef := map[string]bool{}
	unknownStatus := map[string]int{}
	statusRemapped := 0
	noSaleLine, noInvoice := 0, 0
	unresolvedHeal := 0
	medLines := 0
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
			branchCode:    firstNonEmpty(d.Text("create_by_branch"), BranchCode),
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
				code:      childCode,
				name:      lineName,
				perCourse: fmt.Sprintf("%d", perCourse),
				qtyTotal:  fmt.Sprintf("%d", perCourse),
				used:      fmt.Sprintf("%d", lineUsed),
			})
		}
		medLines += len(docArray(d, "course_list_medicien"))

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
		rep.notef("course_bought_items: %d บรรทัดยาในคอร์ส (course_list_medicien) ยังไม่นำเข้ารอบนี้ — course_bought_items รองรับ kind='medication' ได้ (ไม่ต้องมี parent_service เหมือน catalog_course_line) ถ้าต้องการให้เพิ่ม บอกได้",
			medLines)
	}
	rep.notef("course_usage / course_bought_usage ไม่ถูกเขียน — ต้นทางบอกแค่จำนวนครั้งที่เหลือ ไม่ได้บอกว่าใช้ครั้งไหนวันไหน จึงเก็บยอดไว้ที่ periods_used และไม่แต่งประวัติการใช้ขึ้นมา")
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
// removes. It is scoped to one clinic AND to rows that carry a migrate_source_ref — a row
// created by a HUMAN through the app has no such reference and is deliberately left alone,
// because replacing an import is not a licence to delete somebody's work.
func renderCourseBoughtPurge(clinic string) string {
	return fmt.Sprintf(`-- ⚠ ลบสิทธิ์คอร์สที่นำเข้าไว้รอบก่อน แล้วสร้างใหม่ทั้งชุด (มติเจ้าของคลินิก)
--
-- ลบเฉพาะแถวที่มี migrate_source_ref = แถวที่มาจากการนำเข้า · แถวที่คนสร้างเองในระบบ
-- (migrate_source_ref เป็น NULL) ไม่ถูกแตะ เพราะการนำเข้าใหม่ไม่ใช่ใบอนุญาตลบงานคน
--
-- ปลอดภัยเพราะ: FK ทุกตัวที่ชี้มาที่ course_bought เป็น ON DELETE CASCADE
-- (course_bought_items · course_bought_usage · course_share · course_share_entitlement)
-- หรือ ON DELETE SET NULL (doctorbill) และตารางเหล่านั้นว่างทั้งหมดสำหรับคลินิกนี้
-- (ตรวจกับสคีมาจริงแล้ว ไม่ได้สันนิษฐาน)
DO $purge$
DECLARE
    doomed   int;
    handmade int;
BEGIN
    SELECT count(*) FILTER (WHERE migrate_source_ref IS NOT NULL),
           count(*) FILTER (WHERE migrate_source_ref IS NULL)
      INTO doomed, handmade
      FROM course_bought
     WHERE clinic_code = %s;

    RAISE NOTICE 'course_bought: จะลบ %% แถวที่มาจากการนำเข้า · เก็บไว้ %% แถวที่คนสร้างเอง',
                 doomed, handmade;
END $purge$;

DELETE FROM course_bought
 WHERE clinic_code = %s
   AND migrate_source_ref IS NOT NULL;

`, lit(clinic), lit(clinic))
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
SELECT %s, %s, 'treatment', 0, %s, %s, NULL,
       %s, %s, %s, %s, %s
WHERE %s IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM course_bought_items cbi
       WHERE cbi.clinic_code = %s
         AND cbi.course_bought_id = %s
         AND cbi.code = %s
  );

`, lit(in.ClinicCode), courseBoughtIDByRef(in.ClinicCode, p.sourceRef),
				lit(l.code), lit(l.name),
				l.perCourse, l.qtyTotal, l.used, p.createdAt,
				catalogItemIDByCode(in.ClinicCode, l.code),
				courseBoughtIDByRef(in.ClinicCode, p.sourceRef),
				lit(in.ClinicCode), courseBoughtIDByRef(in.ClinicCode, p.sourceRef), lit(l.code))
		}
	}
	e.Statements = s.stmts
	return s.String()
}
