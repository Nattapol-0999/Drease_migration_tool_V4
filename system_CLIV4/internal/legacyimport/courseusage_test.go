package legacyimport

import (
	"strings"
	"testing"

	"system_cliv4/internal/mongodump"
)

// Tests for the course deduction ledger (bills.list[] use_course → course_bought_usage)
// and for the drug lines of a purchased course (course_list_medicien →
// course_bought_items kind='medication').
//
// Both were deliberately NOT imported before 2 Oct 2026. The ledger because the old note
// in coursebought.go believed the dates did not exist anywhere; the drug lines because
// nobody had asked. The first belief was wrong — the deductions sit on the bills — and
// these tests pin what replaced it, including the parts that are a POLICY and not a fact:
//
//   - FIFO by purchase date picks which entitlement a deduction came off, because the
//     source does not say. The owner chose it over importing only the unambiguous ones.
//   - A treatment line can never account for more than the `used` the source reported.
//   - material_cost 0.00 / state 'complete' is a stated figure, not a computed cost.

// usagePlans builds two entitlements for one patient that BOTH contain LH001, bought on
// different days, plus a course whose only content is a drug. That is the shape every
// interesting case needs: a FIFO choice and a medication line.
func usagePlans() []entitlementPlan {
	return []entitlementPlan{
		{
			sourceRef:  "ref-old",
			hn:         "016900001",
			name:       "คอร์สเก่า",
			branchCode: "BR00001",
			createdAt:  "'2026-01-10 03:00:00+00'",
			invoiceNo:  "B001",
			lines: []entitlementLine{{
				kind: courseLineTreatment, code: "LH001", name: "Laser",
				perCourse: "2", qtyTotal: "2", used: "1",
			}},
		},
		{
			sourceRef:  "ref-new",
			hn:         "016900001",
			name:       "คอร์สใหม่",
			branchCode: "BR00001",
			createdAt:  "'2026-05-20 03:00:00+00'",
			invoiceNo:  "B002",
			lines: []entitlementLine{{
				kind: courseLineTreatment, code: "LH001", name: "Laser",
				perCourse: "3", qtyTotal: "3", used: "1",
			}},
		},
		{
			sourceRef:  "ref-drug",
			hn:         "016900001",
			name:       "คอร์สยา",
			branchCode: "BR00001",
			createdAt:  "'2026-03-01 03:00:00+00'",
			invoiceNo:  "B003",
			lines: []entitlementLine{{
				kind: courseLineMedication, code: "IT001", name: "Vitamin C",
				perCourse: "2", qtyTotal: "2", used: "0",
			}},
		},
	}
}

func usageRef(code, at, bill string, qty string) courseUsageRef {
	return courseUsageRef{
		hn: "016900001", invoiceNo: bill, branchCode: "BR00002",
		createdAt: "'" + at + "+00'", code: code, name: code, qty: qty,
	}
}

func assignOne(t *testing.T, plans []entitlementPlan, refs []courseUsageRef) (
	[]usageRow, *Report, *EntityResult) {
	t.Helper()
	rep := NewReport("C9C9611")
	e := rep.entity(EntCourseBoughtUsage)
	rows := assignCourseUsage(plans, refs, rep, e)
	return rows, rep, e
}

// The deduction goes to the course bought FIRST, which is the owner's FIFO decision.
func TestCourseUsageTakesTheOldestCourseFirst(t *testing.T) {
	plans := usagePlans()
	rows, _, _ := assignOne(t, plans, []courseUsageRef{
		usageRef("LH001", "2026-06-01 04:00:00", "B500", "1"),
	})

	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if got := plans[rows[0].slot.planIdx].sourceRef; got != "ref-old" {
		t.Errorf("deduction landed on %q, want ref-old (bought 2026-01-10)", got)
	}
}

// Each line accounts for at most the `used` the source reported. A third deduction has
// nowhere to go once both LH001 lines have spent their one used session each, and it is
// REPORTED rather than forced onto a full line — forcing it would make a customer's used
// count exceed what they bought.
func TestCourseUsageNeverExceedsTheSourceBalance(t *testing.T) {
	plans := usagePlans()
	rows, rep, e := assignOne(t, plans, []courseUsageRef{
		usageRef("LH001", "2026-06-01 04:00:00", "B500", "1"),
		usageRef("LH001", "2026-06-02 04:00:00", "B501", "1"),
		usageRef("LH001", "2026-06-03 04:00:00", "B502", "1"),
	})

	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (one per `used` the source reported)", len(rows))
	}
	refs := []string{plans[rows[0].slot.planIdx].sourceRef, plans[rows[1].slot.planIdx].sourceRef}
	if refs[0] != "ref-old" || refs[1] != "ref-new" {
		t.Errorf("deductions landed on %v, want [ref-old ref-new]", refs)
	}
	if total := e.SourceRows; total != 3 {
		t.Errorf("SourceRows = %d, want 3", total)
	}
	if len(e.Skipped) == 0 {
		t.Error("the deduction that did not fit was dropped without a skip reason")
	}
	if !hasWarning(rep, "เกินยอดที่ต้นทางบอก") {
		t.Errorf("no warning about the deduction that did not fit; warnings = %v", rep.Warnings)
	}
}

// `used` on a treatment line is NOT recomputed: the source's remaining count is the
// balance the clinic works from, and the ledger only explains it.
func TestCourseUsageLeavesTreatmentUsedAlone(t *testing.T) {
	plans := usagePlans()
	assignOne(t, plans, []courseUsageRef{
		usageRef("LH001", "2026-06-01 04:00:00", "B500", "1"),
	})

	if got := plans[0].lines[0].used; got != "1" {
		t.Errorf("treatment used = %q, want the source's 1 — the import must not move a balance", got)
	}
}

// A course's DRUGS are the one place the ledger decides the balance, because the source
// keeps no remaining count for them at all.
func TestCourseUsageFillsMedicationUsedFromTheLedger(t *testing.T) {
	plans := usagePlans()
	rows, _, _ := assignOne(t, plans, []courseUsageRef{
		usageRef("IT001", "2026-06-01 04:00:00", "B500", "1"),
		usageRef("IT001", "2026-06-02 04:00:00", "B501", "1"),
	})

	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if got := plans[2].lines[0].used; got != "2" {
		t.Errorf("medication used = %q, want 2 — it has no source value to preserve", got)
	}
}

// A deduction for something the patient owns no course line for is not given a course.
func TestCourseUsageDoesNotInventACourse(t *testing.T) {
	plans := usagePlans()
	rows, rep, _ := assignOne(t, plans, []courseUsageRef{
		usageRef("ST000463", "2026-06-01 04:00:00", "B500", "1"),
	})

	if len(rows) != 0 {
		t.Fatalf("rows = %d, want 0 — there is no course line with that code", len(rows))
	}
	if !hasWarning(rep, "ไม่ตรงกับรายการในคอร์สใด") {
		t.Errorf("the unmatched deduction was not reported; warnings = %v", rep.Warnings)
	}
}

// invoice_id is the PURCHASE invoice, which is what the column means in V4
// (InsertCourseBoughtUsage snapshots it from course_bought). Writing the bill the session
// was taken on would put a different fact in the column.
func TestCourseUsageSQLPointsAtThePurchaseInvoice(t *testing.T) {
	plans := usagePlans()
	rows, rep, e := assignOne(t, plans, []courseUsageRef{
		usageRef("LH001", "2026-06-01 04:00:00", "B500", "1"),
	})
	sql := renderCourseBoughtUsage(plans, rows, Input{ClinicCode: "C9C9611"}, e)
	_ = rep

	if !strings.Contains(sql, "'B001'") {
		t.Errorf("the purchase invoice B001 is not resolved\n%s", sql)
	}
	if strings.Contains(sql, "'B500'") {
		t.Errorf("the usage bill B500 leaked into invoice_id\n%s", sql)
	}
	if !strings.Contains(sql, "receipt_id") || !strings.Contains(sql, "NULL,") {
		t.Errorf("receipt_id is not left NULL\n%s", sql)
	}
}

// The stated cost, and the fact that it is stated. If this ever becomes a computed
// figure, this test is where the change has to be argued.
func TestCourseUsageSQLWritesTheStatedMaterialCost(t *testing.T) {
	plans := usagePlans()
	rows, _, e := assignOne(t, plans, []courseUsageRef{
		usageRef("LH001", "2026-06-01 04:00:00", "B500", "1"),
	})
	sql := renderCourseBoughtUsage(plans, rows, Input{ClinicCode: "C9C9611"}, e)

	if !strings.Contains(sql, "0.00, 'complete'") {
		t.Errorf("material_cost / material_cost_state are not the decided pair\n%s", sql)
	}
	if strings.Contains(sql, "visit_ref") {
		t.Errorf("visit_ref is written; it holds a VN (varchar(8)), not a bill number\n%s", sql)
	}
}

// Re-running the script must not double the history.
func TestCourseUsageSQLHasARerunGuard(t *testing.T) {
	plans := usagePlans()
	rows, _, e := assignOne(t, plans, []courseUsageRef{
		usageRef("LH001", "2026-06-01 04:00:00", "B500", "1"),
	})
	sql := renderCourseBoughtUsage(plans, rows, Input{ClinicCode: "C9C9611"}, e)

	for _, want := range []string{"NOT EXISTS", "u.course_bought_item_id", "u.used_at", "u.sessions_used"} {
		if !strings.Contains(sql, want) {
			t.Errorf("the re-run guard does not key on %s\n%s", want, sql)
		}
	}
}

// The same export must produce a byte-identical script, including when the deductions
// arrive in a different order.
func TestCourseUsageOutputIsDeterministic(t *testing.T) {
	refsA := []courseUsageRef{
		usageRef("LH001", "2026-06-01 04:00:00", "B500", "1"),
		usageRef("IT001", "2026-06-02 04:00:00", "B501", "1"),
	}
	refsB := []courseUsageRef{refsA[1], refsA[0]}

	render := func(refs []courseUsageRef) string {
		plans := usagePlans()
		rows, _, e := assignOne(t, plans, refs)
		return renderCourseBoughtUsage(plans, rows, Input{ClinicCode: "C9C9611"}, e)
	}
	if a, b := render(refsA), render(refsB); a != b {
		t.Errorf("output depends on input order\n--- A ---\n%s\n--- B ---\n%s", a, b)
	}
}

// ── the drug lines of a purchased course ────────────────────────────────────

// courseBoughtMedSample is one entitlement holding one treatment and one drug.
const courseBoughtMedSample = `db.getCollection("listheals").insert([
  { _id: ObjectId("6a00000000000000000000a1"), code: "LH004", name_th: "Basic Bright" }
]);
db.getCollection("items").insert([
  { _id: ObjectId("6c00000000000000000000c1"), code: "IT001", name_th: "Vitamin C 500" }
]);
db.getCollection("course_items").insert([
  { _id: ObjectId("6b00000000000000000000b1"),
    name_th: "Bright Set",
    price: NumberDecimal("9000"),
    list_item: [
      { heal_id: ObjectId("6a00000000000000000000a1"), number_of_time: "1" }
    ]
  }
]);
db.getCollection("course_bills").insert([
  { _id: ObjectId("6d00000000000000000000d1"),
    hn: "016900001",
    course_item_id: ObjectId("6b00000000000000000000b1"),
    name: "Bright Set",
    status: "active",
    price_of_course: NumberDecimal("9000"),
    number_of_time_default: "1",
    number_of_time: "0",
    created_at: ISODate("2026-03-01T03:00:00Z"),
    course_list: [
      { heal_id: ObjectId("6a00000000000000000000a1"), number_of_time_default: "1", number_of_time: "0" }
    ],
    course_list_medicien: [
      { heal_id: ObjectId("6a00000000000000000000a1"), medicien_id: ObjectId("6c00000000000000000000c1"), qty_use: "2" }
    ]
  }
]);`

func planEntitlementsOne(t *testing.T, body string) ([]entitlementPlan, *Report) {
	t.Helper()
	mc, err := parseMongoCatalog([]byte(body))
	if err != nil {
		t.Fatalf("parseMongoCatalog: %v", err)
	}
	rep := NewReport("C9C9611")
	in := Input{ClinicCode: "C9C9611"}
	// The catalog pass is what MINTS the course codes (CS001…), and the entitlement pass
	// resolves its course through that map — it is the only link the export has, because
	// course_bills carries no code of its own.
	if _, _, cerr := buildCourses(mc, in, rep); cerr != nil {
		t.Fatalf("buildCourses: %v", cerr)
	}
	plans, err := planEntitlements(mc, moneyBatches{}, newBranchResolver(in), rep,
		rep.entity(EntCourseBought), rep.entity(EntCourseBoughtItems))
	if err != nil {
		t.Fatalf("planEntitlements: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1 — the fixture was rejected, so nothing below is tested", len(plans))
	}
	return plans, rep
}

// The drug is imported as its own line, kind 'medication', from real source fields.
func TestCourseBoughtMedicationLineIsImported(t *testing.T) {
	plans, rep := planEntitlementsOne(t, courseBoughtMedSample)

	var med *entitlementLine
	for i := range plans[0].lines {
		if plans[0].lines[i].kind == courseLineMedication {
			med = &plans[0].lines[i]
		}
	}
	if med == nil {
		t.Fatalf("no medication line; lines = %+v", plans[0].lines)
	}
	if med.code != "IT001" {
		t.Errorf("code = %q, want IT001 (resolved from medicien_id)", med.code)
	}
	if med.name != "Vitamin C 500" {
		t.Errorf("name = %q, want the item's name_th", med.name)
	}
	if med.qtyTotal != "2" || med.perCourse != "2" {
		t.Errorf("qty = %s/%s, want 2/2 from qty_use", med.perCourse, med.qtyTotal)
	}
	if med.used != "0" {
		t.Errorf("used = %q, want 0 — the source has no remaining count for a drug", med.used)
	}
	if !hasNote(rep, "course_list_medicien") {
		t.Errorf("the drug lines are not reported; notes = %v", rep.Notes)
	}
}

// The rendered row must carry the line's own kind, and the re-run guard has to include it
// — a course can bundle a service and a drug this tool cannot promise have distinct codes.
func TestCourseBoughtItemsSQLCarriesTheLineKind(t *testing.T) {
	plans, _ := planEntitlementsOne(t, courseBoughtMedSample)
	e := &EntityResult{Skipped: map[string]int{}}
	sql := renderCourseBoughtItems(plans, Input{ClinicCode: "C9C9611"}, e)

	if got := strings.Count(sql, "INSERT INTO course_bought_items"); got != 2 {
		t.Fatalf("inserts = %d, want 2 (one treatment + one drug)\n%s", got, sql)
	}
	if !strings.Contains(sql, "'medication'") {
		t.Errorf("no medication row\n%s", sql)
	}
	if !strings.Contains(sql, "'treatment'") {
		t.Errorf("the treatment row lost its kind\n%s", sql)
	}
	if !strings.Contains(sql, "cbi.kind =") {
		t.Errorf("the re-run guard does not key on kind\n%s", sql)
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

func hasWarning(rep *Report, substr string) bool {
	for _, w := range rep.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func hasNote(rep *Report, substr string) bool {
	for _, n := range rep.Notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

var _ = mongodump.Doc{}
