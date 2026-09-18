package legacyimport

// The patient group is cut into transactions BY ROW COUNT, which no other group is.
//
// WHY THIS ONE GROUP IS DIFFERENT
//
// Every other group needed only the group-level split in script.go, because what those
// transactions run out of is RELATION locks — one per table, index and toast — and 500 rows
// hold exactly the same relation locks as 15,000.
//
// patient does not behave that way. trg_patient_privacy_hn_lock fires BEFORE INSERT FOR EACH
// ROW and privacy_lock_patient_hn (migration 0404) takes a pg_advisory_xact_lock per HN it
// claims: the patient's own HN, plus old_hn when the patient carries one. Advisory locks sit
// in the same shared lock table as relation locks and are released only at COMMIT, so they
// pile up for the length of the transaction.
//
// Measured on a tenant already in drease_temporary — 6,439 patients, 1,445 of them with an
// old_hn — the import left 7,884 rows in privacy_patient_hn_ownership. 1.22 locks per patient.
//
// The export that failed carries 14,899 patients: about 18,200 locks in one transaction.
// max_locks_per_transaction was raised 64 → 4,096 on that server, and 18,200 still does not
// fit. Hence batching. 2,000 rows ≈ 2,440 locks, about 1.7x inside the budget.
//
// WHAT THESE TESTS PROTECT
//
// Two things that fail silently if they regress:
//
//  1. The boundary count. If the modulo drifts (counting rows read instead of rows written,
//     or emitting after instead of before) the last batch grows past the budget and the run
//     dies at whatever row happens to cross it — a number that moves with the data, so it
//     reads as a mystery rather than as this.
//  2. The two SET LOCAL statements after every new BEGIN. app.clinic_code is what the
//     FORCE-RLS policies match on. A batch that opens without it matches no rows: every
//     INSERT in that batch writes ZERO and psql still prints INSERT 0 1 for each.

import (
	"strings"
	"testing"

	"system_cliv4/internal/legacydump"
)

// realWorldPatientCount is the export that produced the out-of-shared-memory failure. Kept as
// the literal number because the arithmetic below is the whole point of the test.
const realWorldPatientCount = 14899

func manyPatients(n int) []legacydump.Row {
	rows := make([]legacydump.Row, 0, n)
	for i := 0; i < n; i++ {
		// HN has to be distinct per row: buildPatient guards on (clinic_code, HN), so
		// repeats would still emit but would tell us nothing about batch sizes.
		rows = append(rows, patientRow(hnFor(i), map[string]string{"Prefixname": "นาย", "gender": "1"}))
	}
	return rows
}

func hnFor(i int) string {
	digits := "0123456789"
	out := []byte("00000000")
	for p := len(out) - 1; p >= 0 && i > 0; p-- {
		out[p] = digits[i%10]
		i /= 10
	}
	return string(out)
}

func TestPatientGroupIsCutIntoTransactionsOfTwoThousandRows(t *testing.T) {
	sqlText, _ := buildPatientOne(t, manyPatients(realWorldPatientCount))

	inserts := strings.Count(sqlText, "INSERT INTO patient (")
	if inserts != realWorldPatientCount {
		t.Fatalf("emitted %d patient INSERTs, want %d — the fixture is wrong, not the batching",
			inserts, realWorldPatientCount)
	}

	// 14,899 rows with a boundary before every 2,000th written row after the first:
	// 2000, 4000, 6000, 8000, 10000, 12000, 14000 = 7 boundaries = 8 transactions.
	wantBoundaries := (realWorldPatientCount - 1) / patientsPerTransaction
	gotCommits := strings.Count(sqlText, "\nCOMMIT;")
	if gotCommits != wantBoundaries {
		t.Errorf("patient group has %d internal COMMITs, want %d (%d rows / %d per txn)",
			gotCommits, wantBoundaries, realWorldPatientCount, patientsPerTransaction)
	}

	gotBegins := strings.Count(sqlText, "\nBEGIN;")
	if gotBegins != gotCommits {
		t.Errorf("%d BEGIN against %d COMMIT — an unbalanced script leaves psql in a state "+
			"where the next group's COMMIT closes the wrong transaction", gotBegins, gotCommits)
	}

	// The silent one. Every BEGIN this group opens must re-arm both settings.
	clinicSettings := strings.Count(sqlText, "app.clinic_code")
	if clinicSettings != gotBegins {
		t.Errorf("%d BEGIN but app.clinic_code set %d times — a batch without it writes zero "+
			"rows under FORCE RLS and still reports success", gotBegins, clinicSettings)
	}
}

func TestPatientBatchNeverEndsOnAnEmptyTransaction(t *testing.T) {
	// Exactly on the boundary is the case that tempts an off-by-one: emitting the boundary
	// AFTER the 2,000th row would leave a BEGIN with nothing behind it, and the next group's
	// COMMIT would then close an empty transaction while the group's own COMMIT dangles.
	sqlText, _ := buildPatientOne(t, manyPatients(patientsPerTransaction))

	if got := strings.Count(sqlText, "\nCOMMIT;"); got != 0 {
		t.Errorf("%d rows produced %d internal COMMITs, want 0 — a batch that exactly fills "+
			"the budget needs no boundary", patientsPerTransaction, got)
	}
	if strings.HasSuffix(strings.TrimSpace(sqlText), "BEGIN;") {
		t.Error("patient group ends on a bare BEGIN")
	}
}

func TestPatientRowsWithoutHNDoNotCountTowardsABatch(t *testing.T) {
	// The reason the counter is `emitted` and not the loop index. Rows with no HN are skipped
	// and take no locks; counting them would cut the batches short of 2,000 — harmless — but
	// the same mistake in reverse (counting written rows as read) overfills them, and this is
	// the test that pins which one the code does.
	rows := manyPatients(patientsPerTransaction)
	blank := ""
	for i := 0; i < 50; i++ {
		rows = append(rows, legacydump.Row{"HN": &blank})
	}

	sqlText, _ := buildPatientOne(t, rows)

	if got := strings.Count(sqlText, "INSERT INTO patient ("); got != patientsPerTransaction {
		t.Fatalf("wrote %d patients, want %d", got, patientsPerTransaction)
	}
	if got := strings.Count(sqlText, "\nCOMMIT;"); got != 0 {
		t.Errorf("%d internal COMMITs, want 0 — the 50 HN-less rows were counted as written",
			got)
	}
}
