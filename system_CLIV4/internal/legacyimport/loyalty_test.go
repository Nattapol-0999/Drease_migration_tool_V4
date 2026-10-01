package legacyimport

// Tests for mris.points → loyalty_account.
//
// Every case here is a decision about money, so each one is pinned rather than described:
// the FIRST document of an HN is the balance (not the sum, not the largest), an HN holding
// nothing gets no account, refuse rather than round, and never write a policy.
//
// The first two were decided the other way on 15 Sep 2026 and corrected on 28 Sep 2026 —
// see the header of loyalty.go for V2's own code showing why. These tests exist so the
// correction cannot be undone by accident.

import (
	"strings"
	"testing"
)

// pointsSample mirrors the two shapes that forced decisions, taken from real exports:
//
//	016500001 — one document holding 0. V2 shows nothing saved; no account is opened.
//	016400544 — the ordinary case, one document with a balance.
//	016400156 — THREE documents. The first holds 5, the others 17000 and 1. Anjali
//	            (C6C936C) really carries this shape: two HN whose first document is 0
//	            beside a stale one holding 17,000. First=5, MAX=17000, SUM=17006, so the
//	            three candidate rules give three different answers and the test can only
//	            pass under one of them.
const pointsSample = `db.points.insert({
    "_id": ObjectId("632804179ea97d1971cf651f"),
    "total_point": NumberDecimal("0"),
    "hn": "016500001",
    "created_at": ISODate("2022-09-19T05:54:31.661Z"),
    "updated_at": ISODate("2022-09-19T05:54:31.662Z")
});
db.points.insert({
    "_id": ObjectId("632bec38da83ba1966c3777b"),
    "total_point": NumberDecimal("3"),
    "hn": "016400544",
    "created_at": ISODate("2022-09-22T05:01:44.506Z"),
    "updated_at": ISODate("2022-09-22T05:01:44.506Z")
});
db.points.insert({
    "_id": ObjectId("633d6e4dda83ba1966c3e451"),
    "total_point": NumberDecimal("5"),
    "hn": "016400156",
    "created_at": ISODate("2022-10-05T11:45:17.631Z"),
    "updated_at": ISODate("2022-10-05T11:45:17.631Z")
});
db.points.insert({
    "_id": ObjectId("633d6e4dda83ba1966c3e453"),
    "total_point": NumberDecimal("17000"),
    "hn": "016400156",
    "created_at": ISODate("2022-10-05T11:45:17.632Z"),
    "updated_at": ISODate("2022-10-05T11:45:17.633Z")
});
db.points.insert({
    "_id": ObjectId("633d6e4dda83ba1966c3e455"),
    "total_point": NumberDecimal("1"),
    "hn": "016400156",
    "created_at": ISODate("2022-10-05T11:45:17.634Z"),
    "updated_at": ISODate("2022-10-05T11:45:17.635Z")
});`

func buildLoyaltyOne(t *testing.T, body string) (string, *Report) {
	t.Helper()
	mc, err := parseMongoCatalog([]byte(body))
	if err != nil {
		t.Fatalf("parseMongoCatalog: %v", err)
	}
	rep := NewReport("C9C9611")
	sql, err := buildLoyalty(mc, Input{ClinicCode: "C9C9611"}, rep)
	if err != nil {
		t.Fatalf("buildLoyalty: %v", err)
	}
	return sql, rep
}

// The decision with three real alternatives. V2 reads PointSummary.findOne({hn}), so the
// first document is the balance the patient has actually been shown; the others are
// leftovers V2 can neither display nor spend.
func TestLoyaltyUsesTheFirstDocumentOfOneHN(t *testing.T) {
	sql, rep := buildLoyaltyOne(t, pointsSample)

	if !strings.Contains(sql, ", 5, p.id") {
		t.Errorf("hn 016400156 did not take the FIRST document's 5 points\n%s", sql)
	}
	// The two rules this replaces, pinned as absent so neither can come back quietly.
	if strings.Contains(sql, ", 17000, p.id") {
		t.Errorf("hn 016400156 took the LARGEST document (17000) instead of the first\n%s", sql)
	}
	if strings.Contains(sql, ", 17006, p.id") {
		t.Errorf("hn 016400156 summed its documents (17006) instead of taking the first\n%s", sql)
	}
	if !strings.Contains(sql, ", 3, p.id") {
		t.Errorf("hn 016400544 should carry 3 points\n%s", sql)
	}
	// Which document was used has to be visible in the artifact, not only in the total.
	if !strings.Contains(sql, "ใช้ใบแรกตามที่ V2 อ่าน") {
		t.Errorf("no comment recording that the first document was used\n%s", sql)
	}
	if !strings.Contains(sql, "ใบที่เหลือ 17000, 1") {
		t.Errorf("the artifact does not say what was left behind\n%s", sql)
	}
	var noted bool
	for _, n := range rep.Notes {
		if strings.Contains(n, "มีหลายเอกสารในต้นทาง") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("the report does not say an HN carried several documents; notes = %v", rep.Notes)
	}
	// A leftover that disagrees with the balance is a number somebody might expect to see,
	// so it is named rather than counted.
	var warned bool
	for _, w := range rep.Warnings {
		if strings.Contains(w, "016400156") && strings.Contains(w, "17000") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning naming the HN whose other documents disagree; warnings = %v", rep.Warnings)
	}
}

// An HN holding nothing gets no row. V4 opens a loyalty_account on the first movement that
// awards something, never on a look, so a row at zero is a row the running app would not
// have written. (Owner's rule, 28 Sep 2026 — it replaces the 15 Sep rule of importing every
// HN including the zeros.)
func TestLoyaltySkipsZeroBalances(t *testing.T) {
	sql, rep := buildLoyaltyOne(t, pointsSample)
	if strings.Contains(sql, "'016500001'") {
		t.Errorf("the zero-balance patient got an account\n%s", sql)
	}
	if strings.Contains(sql, ", 0, p.id") {
		t.Errorf("a row was written with balance 0\n%s", sql)
	}
	if n := rep.Entities[EntLoyaltyAccount].Skipped["ยอดแต้มเป็น 0 — ไม่เปิดบัญชี (มติเจ้าของคลินิก 28 ก.ย. 2569)"]; n != 1 {
		t.Errorf("skip counter = %d, want 1; skipped = %v", n, rep.Entities[EntLoyaltyAccount].Skipped)
	}
	var noted bool
	for _, n := range rep.Notes {
		if strings.Contains(n, "ข้าม 1 hn ที่ยอดเป็น 0") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("the report does not count the skipped zero balances; notes = %v", rep.Notes)
	}
	// And the artifact says it too, so the number of rows is explainable from the SQL alone.
	if !strings.Contains(sql, "hn ที่ยอดเป็น 0 ไม่ถูกเปิดบัญชี 1 ราย") {
		t.Errorf("the script does not record the skipped zero balances\n%s", sql)
	}
}

// An export where every balance is zero opens no account and MUST NOT fail.
//
// This pins a regression, not a preference. The first version of the zero-balance rule
// refused such an export, and because an error from buildLoyalty aborts BuildAll, a real
// clinic with 2,269 hn all at zero could not generate ANY sql — its bills, page, items and
// ten other collections were blocked by a points collection that simply had nothing in it
// (seen 30 Sep 2026). A clinic that runs no points scheme looks exactly like this.
func TestLoyaltyOpensNoAccountWhenEveryBalanceIsZero(t *testing.T) {
	body := `db.points.insert({
    "_id": ObjectId("632804179ea97d1971cf651f"),
    "total_point": NumberDecimal("0"),
    "hn": "016500001",
    "created_at": ISODate("2022-09-19T05:54:31.661Z")
});
db.points.insert({
    "_id": ObjectId("632804179ea97d1971cf6520"),
    "total_point": NumberDecimal("0"),
    "hn": "016500002",
    "created_at": ISODate("2022-09-19T05:54:31.662Z")
});`
	mc, err := parseMongoCatalog([]byte(body))
	if err != nil {
		t.Fatalf("parseMongoCatalog: %v", err)
	}
	rep := NewReport("C9C9611")
	sql, err := buildLoyalty(mc, Input{ClinicCode: "C9C9611"}, rep)
	if err != nil {
		t.Fatalf("an all-zero export must not fail — it blocks every other group: %v", err)
	}
	if strings.Contains(executableSQL(sql), "INSERT INTO loyalty_account") {
		t.Errorf("an account was opened for a patient holding nothing\n%s", sql)
	}
	if !strings.Contains(sql, "ยอดเป็น 0 ทุกราย จึงไม่เปิดบัญชีแต้มให้ใครเลย") {
		t.Errorf("the artifact does not say why it wrote nothing\n%s", sql)
	}
	if !strings.Contains(sql, "ไม่ใช่ข้อผิดพลาด") {
		t.Errorf("the artifact does not say this is a normal outcome\n%s", sql)
	}
	var noted bool
	for _, n := range rep.Notes {
		if strings.Contains(n, "ยอดเป็น 0 ทุกราย") && strings.Contains(n, "กลุ่มอื่นนำเข้าต่อได้") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("the report does not say the other groups can still import; notes = %v", rep.Notes)
	}
	// And no policy hint: there is no balance to make visible, so the statement would be noise.
	if strings.Contains(sql, "<BAHT_PER_POINT>") {
		t.Errorf("the policy hint was printed even though no balance was imported\n%s", sql)
	}
}

// patient_id is read at the target, and an HN with no patient row writes nothing. Both come
// from the same construction: INSERT … SELECT FROM patient.
func TestLoyaltyResolvesPatientIDAndSkipsUnknownHN(t *testing.T) {
	sql, _ := buildLoyaltyOne(t, pointsSample)
	if !strings.Contains(sql, "FROM patient p") {
		t.Errorf("the insert does not select from patient, so patient_id would be NULL\n%s", sql)
	}
	if !strings.Contains(sql, `p."HN" =`) {
		t.Errorf("the patient lookup is not keyed on HN\n%s", sql)
	}
	// Executable part only: the artifact documents the loyalty_policy statement as a
	// comment, and that one legitimately uses VALUES.
	if strings.Contains(executableSQL(sql), "VALUES (") {
		t.Errorf("a VALUES insert cannot resolve patient_id — it must be INSERT … SELECT\n%s", sql)
	}
	// Re-running must not overwrite a balance the clinic edited on screen.
	if !strings.Contains(sql, "ON CONFLICT (clinic_code, hn) DO NOTHING") {
		t.Errorf("missing the idempotency guard\n%s", sql)
	}
}

// A fractional balance is refused, not rounded: loyalty_account.balance is `integer`, and
// turning 2.5 points into 2 decides what a patient owns.
func TestLoyaltyRefusesFractionalPoints(t *testing.T) {
	body := strings.Replace(pointsSample, `NumberDecimal("3")`, `NumberDecimal("2.5")`, 1)
	mc, err := parseMongoCatalog([]byte(body))
	if err != nil {
		t.Fatalf("parseMongoCatalog: %v", err)
	}
	_, err = buildLoyalty(mc, Input{ClinicCode: "C9C9611"}, NewReport("C9C9611"))
	if err == nil {
		t.Fatal("expected a refusal for a fractional point total")
	}
	if !strings.Contains(err.Error(), "016400544") {
		t.Errorf("the error should name the HN it refused: %v", err)
	}
}

// A negative FIRST document is refused with every offending HN named, rather than letting
// CHECK (balance >= 0) stop at whichever row Postgres reached first.
func TestLoyaltyRefusesNegativeBalance(t *testing.T) {
	body := strings.Replace(pointsSample, `NumberDecimal("3")`, `NumberDecimal("-4")`, 1)
	mc, err := parseMongoCatalog([]byte(body))
	if err != nil {
		t.Fatalf("parseMongoCatalog: %v", err)
	}
	_, err = buildLoyalty(mc, Input{ClinicCode: "C9C9611"}, NewReport("C9C9611"))
	if err == nil {
		t.Fatal("expected a refusal for a negative balance")
	}
	if !strings.Contains(err.Error(), "016400544") || !strings.Contains(err.Error(), "-4") {
		t.Errorf("the error should name the HN and the value: %v", err)
	}
}

// A negative value in a LATER document is reported but does not stop the import: V2 never
// shows it, so it cannot be this patient's balance. Refusing on it would block a clinic
// over a number nobody can reach.
func TestLoyaltyReportsButKeepsGoingOnANegativeLaterDocument(t *testing.T) {
	body := strings.Replace(pointsSample, `NumberDecimal("17000")`, `NumberDecimal("-9")`, 1)
	sql, rep := buildLoyaltyOne(t, body)
	if !strings.Contains(sql, ", 5, p.id") {
		t.Errorf("the first document's balance was not written\n%s", sql)
	}
	var warned bool
	for _, w := range rep.Warnings {
		if strings.Contains(w, "ยอดติดลบ") && strings.Contains(w, "016400156") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning about the negative leftover; warnings = %v", rep.Warnings)
	}
}

// A document with no hn cannot be written and must be counted, not dropped in silence.
func TestLoyaltySkipsDocumentWithoutHN(t *testing.T) {
	body := strings.Replace(pointsSample, `"hn": "016500001",`, `"hn": "",`, 1)
	_, rep := buildLoyaltyOne(t, body)
	if n := rep.Entities[EntLoyaltyAccount].Skipped["document ไม่มี hn (loyalty_account.hn เป็น NOT NULL)"]; n != 1 {
		t.Errorf("skip counter = %d, want 1; skipped = %v", n, rep.Entities[EntLoyaltyAccount].Skipped)
	}
}

// point_logs is deliberately not imported, and loyalty_policy deliberately not written.
// Both omissions have to be stated in the artifact AND the report — migration 0238 says a
// balance with no ledger is worse than no balance, so this exception cannot be silent.
func TestLoyaltyRecordsWhatItDeliberatelyLeavesOut(t *testing.T) {
	body := pointsSample + `
db.point_logs.insert({
    "_id": ObjectId("6328050e9ea97d1971cf6567"),
    "type": "add_point",
    "point": NumberDecimal("1"),
    "expire_at": null,
    "hn": "016400544",
    "payment_id": ObjectId("6328050e9ea97d1971cf6560"),
    "created_at": ISODate("2022-09-19T05:58:38.58Z")
});`
	sql, rep := buildLoyaltyOne(t, body)

	if !strings.Contains(sql, "point_logs ของ V2 (1 เอกสาร) ไม่ถูกนำเข้า") {
		t.Errorf("the script does not record that the ledger was left out\n%s", sql)
	}
	if !strings.Contains(sql, "loyalty_policy ก็ไม่ถูกเขียน") {
		t.Errorf("the script does not record that no policy was written\n%s", sql)
	}
	// And nothing that would actually write those tables. Checked against the EXECUTABLE
	// part only: the artifact prints the policy statement as a comment on purpose (see
	// TestLoyaltyPrintsThePolicyStatementItRefusesToRun), so a plain substring search over
	// the whole script would confuse "documented" with "executed".
	exec := executableSQL(sql)
	if strings.Contains(exec, "INSERT INTO loyalty_txn") {
		t.Error("loyalty_txn must not be written: the owner chose not to import point_logs")
	}
	if strings.Contains(exec, "INSERT INTO loyalty_policy") {
		t.Error("loyalty_policy must not be written: the owner chose not to set a rate")
	}

	var warnedLedger, notedPolicy bool
	for _, w := range rep.Warnings {
		if strings.Contains(w, "point_logs") {
			warnedLedger = true
		}
	}
	for _, n := range rep.Notes {
		if strings.Contains(n, "loyalty_policy") {
			notedPolicy = true
		}
	}
	if !warnedLedger {
		t.Errorf("no warning about the missing ledger; warnings = %v", rep.Warnings)
	}
	if !notedPolicy {
		t.Errorf("no note about the missing policy; notes = %v", rep.Notes)
	}
}

// executableSQL strips comment lines so an assertion can tell what the script RUNS from
// what it merely documents.
func executableSQL(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// The balances landing correctly was not enough: on 28 Sep 2026 C6C936C imported 544
// accounts and the customer panel still showed 0, because the screen gates on
// policy.active and nothing in the import said so. The statement now travels with the
// artifact — printed, not executed, because the rate prices a liability the clinic owns.
func TestLoyaltyPrintsThePolicyStatementItRefusesToRun(t *testing.T) {
	sql, _ := buildLoyaltyOne(t, pointsSample)

	for _, want := range []string{
		"ต้องทำอีกหนึ่งขั้นหลังรันไฟล์นี้",
		"INSERT INTO loyalty_policy (clinic_code, baht_per_point, min_redeem, active, updated_at)",
		"VALUES ('C9C9611', 0, 0, true, now())",
		"<BAHT_PER_POINT>",
		"DELETE FROM loyalty_policy WHERE clinic_code = 'C9C9611';",
		"Earns() = policy.Active",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("the artifact does not carry %q\n%s", want, sql)
		}
	}

	// Every line of the hint must be a comment. If one ever renders as a statement the
	// import would set a clinic's redemption rate by itself.
	if exec := executableSQL(sql); strings.Contains(exec, "loyalty_policy") {
		t.Errorf("the policy statement is executable, not documented:\n%s", exec)
	}

	// The number that makes the decision reviewable: what one fully-paid bill will award
	// once the switch is on. pointsSample carries no listheals, so it must say so rather
	// than print a silent 0.
	if !strings.Contains(sql, "ไม่ใส่แต้มให้รายการใดเลย") {
		t.Errorf("no statement about the per-bill earning ceiling\n%s", sql)
	}
}

// With services in the dump, the hint states how many points one bill can award — the
// figure that matters because Earns() has no per-bill ceiling of its own.
func TestLoyaltyPolicyHintCountsTheEarningCeiling(t *testing.T) {
	body := pointsSample + `
db.getCollection("listheals").insert([
  { _id: ObjectId("6a00000000000000000000d1"), code: "LH087", name_th: "Anti - Aging",
    cost: NumberDecimal("10000"), point: NumberDecimal("10") },
  { _id: ObjectId("6a00000000000000000000d2"), code: "LH086", name_th: "Anti - Aging",
    cost: NumberDecimal("10000"), point: NumberDecimal("1") },
  { _id: ObjectId("6a00000000000000000000d3"), code: "LH447", name_th: "Member 600,000 ได้ 750,000",
    cost: NumberDecimal("600000"), point: NumberDecimal("750000") }
]);`
	sql, _ := buildLoyaltyOne(t, body)

	// 10 + 1 = 11. The Member package is stored-value credit and must not be counted.
	if !strings.Contains(sql, "รวม 11 แต้มต่อการขายครบทุกรายการในหนึ่งบิล") {
		t.Errorf("the ceiling is not 11 — the credit package was probably counted\n%s", sql)
	}
	if strings.Contains(sql, "750011") || strings.Contains(sql, "รวม 750000") {
		t.Errorf("the stored-value credit was counted into the earning ceiling\n%s", sql)
	}
	if !strings.Contains(sql, "ใส่แต้มไว้ 2 รายการ") {
		t.Errorf("the count of items carrying points is wrong\n%s", sql)
	}
}

// The artifact is meant to be diffed between two runs of the same dump.
func TestLoyaltyIsDeterministic(t *testing.T) {
	a, _ := buildLoyaltyOne(t, pointsSample)
	b, _ := buildLoyaltyOne(t, pointsSample)
	if a != b {
		t.Error("two runs over the same dump produced different SQL")
	}
}

// loyalty is selectable on its own and depends only on patients: a clinic can carry the
// balances over without re-running catalog or money.
func TestLoyaltyGroupIsSelectableAlone(t *testing.T) {
	want, err := entitiesForGroups([]string{"loyalty"})
	if err != nil {
		t.Fatalf("entitiesForGroups: %v", err)
	}
	if !want[EntLoyaltyAccount] {
		t.Errorf("group loyalty does not select %s; got %v", EntLoyaltyAccount, want)
	}
	if len(want) != 1 {
		t.Errorf("group loyalty pulled in more than its own entity: %v", want)
	}
	var inOrder bool
	for _, e := range EntityOrder() {
		if e == EntLoyaltyAccount {
			inOrder = true
		}
	}
	if !inOrder {
		t.Error("EntLoyaltyAccount is missing from the statement order, so its SQL would never be rendered")
	}
	if deps := groupDependsOn["loyalty"]; len(deps) != 1 || deps[0] != "patients" {
		t.Errorf("groupDependsOn[loyalty] = %v, want [patients]", deps)
	}
}
