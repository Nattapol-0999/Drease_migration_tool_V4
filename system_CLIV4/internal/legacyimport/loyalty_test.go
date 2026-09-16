package legacyimport

// Tests for mris.points → loyalty_account.
//
// Every case here is a decision the clinic owner made about money on 15 Sep 2026, so each
// one is pinned rather than described: SUM (not last, not max) when a patient has several
// documents, keep the zero balances, refuse rather than round, and never write a policy.

import (
	"strings"
	"testing"
)

// pointsSample is the shape taken from the clinic's own mris.points collection, including
// the case that forced a decision: HN 016400156 carries TWO documents of 1 point each, so
// SUM (2) and LAST/MAX (1) disagree.
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
    "total_point": NumberDecimal("1"),
    "hn": "016400156",
    "created_at": ISODate("2022-10-05T11:45:17.631Z"),
    "updated_at": ISODate("2022-10-05T11:45:17.631Z")
});
db.points.insert({
    "_id": ObjectId("633d6e4dda83ba1966c3e453"),
    "total_point": NumberDecimal("1"),
    "hn": "016400156",
    "created_at": ISODate("2022-10-05T11:45:17.632Z"),
    "updated_at": ISODate("2022-10-05T11:45:17.633Z")
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

// The decision that had a real alternative: two documents of 1 point become 2, not 1.
func TestLoyaltySumsEveryDocumentOfOneHN(t *testing.T) {
	sql, rep := buildLoyaltyOne(t, pointsSample)

	// 4 documents → 3 patients.
	for _, want := range []string{"'016500001'", "'016400544'", "'016400156'"} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL is missing hn %s", want)
		}
	}
	// 1 + 1 = 2. LAST or MAX would both have written 1, which is the whole point.
	if !strings.Contains(sql, ", 2, p.id") {
		t.Errorf("hn 016400156 did not sum to 2 (SUM was the owner's choice; LAST/MAX give 1)\n%s", sql)
	}
	if !strings.Contains(sql, ", 3, p.id") {
		t.Errorf("hn 016400544 should carry 3 points\n%s", sql)
	}
	// The merge has to be visible in the artifact, not just in the total.
	if !strings.Contains(sql, "รวม 2 เอกสารเป็น 2 แต้ม") {
		t.Errorf("no comment recording that two documents were merged\n%s", sql)
	}
	var noted bool
	for _, n := range rep.Notes {
		if strings.Contains(n, "มีหลายเอกสารในต้นทาง") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("the report does not say a balance was merged; notes = %v", rep.Notes)
	}
}

// Zero balances are kept: enrolled-with-nothing-saved is not the same as never enrolled.
// 1,193 of the reference clinic's 1,216 patients are in this state, so dropping them would
// discard most of the entity.
func TestLoyaltyKeepsZeroBalances(t *testing.T) {
	sql, rep := buildLoyaltyOne(t, pointsSample)
	if !strings.Contains(sql, "'016500001'") {
		t.Error("the zero-balance patient was dropped")
	}
	if !strings.Contains(sql, ", 0, p.id") {
		t.Errorf("no row carries balance 0\n%s", sql)
	}
	var noted bool
	for _, n := range rep.Notes {
		if strings.Contains(n, "ยอด 0 อยู่ 1 hn") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("the report does not count the zero balances; notes = %v", rep.Notes)
	}
}

// patient_id is read at the target, and an HN with no patient row writes nothing. Both come
// from the same construction: INSERT … SELECT FROM patient.
func TestLoyaltyResolvesPatientIDAndSkipsUnknownHN(t *testing.T) {
	sql, _ := buildLoyaltyOne(t, pointsSample)
	if !strings.Contains(sql, "SELECT %s") && !strings.Contains(sql, "FROM patient p") {
		t.Errorf("the insert does not select from patient, so patient_id would be NULL\n%s", sql)
	}
	if !strings.Contains(sql, `p."HN" =`) {
		t.Errorf("the patient lookup is not keyed on HN\n%s", sql)
	}
	if strings.Contains(sql, "VALUES (") {
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

// A negative total is refused with every offending HN named, rather than letting
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
	// And nothing that would actually write those tables.
	if strings.Contains(sql, "INSERT INTO loyalty_txn") {
		t.Error("loyalty_txn must not be written: the owner chose not to import point_logs")
	}
	if strings.Contains(sql, "INSERT INTO loyalty_policy") {
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
