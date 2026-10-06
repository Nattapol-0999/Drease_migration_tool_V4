package legacyimport

// Where the LINE pairings come from (owner's decision ก, 6 Oct 2026):
//
//	line_patients present        → line_patients, pair_logs ignored
//	only pair_logs               → pairings rebuilt from the LAST event of each pair
//
// The pair_logs shapes are the ones measured on 10.0.133.87: action CREATE / DELETE /
// SET_ACTIVE, type PATIENT / DOCTOR, user_code = the HN, no hn and no pair_branch.

import (
	"strings"
	"testing"
)

func pairLog(id, action, typ, userCode, lineID string, active bool, at string) string {
	a := "false"
	if active {
		a = "true"
	}
	return `db.pair_logs.insert({
    "_id": ObjectId("` + id + `"),
    "action": "` + action + `",
    "user_code": "` + userCode + `",
    "type": "` + typ + `",
    "line_id": "` + lineID + `",
    "active": ` + a + `,
    "createdAt": ISODate("` + at + `")
});
`
}

const (
	lineA = "Uaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1"
	lineB = "Ubbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb2"
	lineC = "Ucccccccccccccccccccccccccccccc3"
	lineD = "Udddddddddddddddddddddddddddddd4"
	lineE = "Ueeeeeeeeeeeeeeeeeeeeeeeeeeeeee5"
)

// Every rule the log is read by, one pair each.
var pairLogsSample = strings.Join([]string{
	// 016800001: one CREATE, still live.
	pairLog("64b0a0000000000000000001", "CREATE", "PATIENT", "016800001", lineA, true, "2023-07-14T03:00:00.000Z"),
	// 016800002: created then deleted — the customer unlinked. Not carried over.
	pairLog("64b0a0000000000000000002", "CREATE", "PATIENT", "016800002", lineB, true, "2023-08-01T03:00:00.000Z"),
	pairLog("64b0a0000000000000000003", "DELETE", "PATIENT", "016800002", lineB, true, "2023-09-01T03:00:00.000Z"),
	// 016800003: created inactive, activated later — live, at the activation time.
	pairLog("64b0a0000000000000000004", "CREATE", "PATIENT", "016800003", lineC, false, "2024-01-01T03:00:00.000Z"),
	pairLog("64b0a0000000000000000005", "SET_ACTIVE", "PATIENT", "016800003", lineC, true, "2024-01-05T03:00:00.000Z"),
	// 016800004: created, deleted, created again — live, at the newest CREATE.
	pairLog("64b0a0000000000000000006", "CREATE", "PATIENT", "016800004", lineD, true, "2024-02-01T03:00:00.000Z"),
	pairLog("64b0a0000000000000000007", "DELETE", "PATIENT", "016800004", lineD, true, "2024-03-01T03:00:00.000Z"),
	pairLog("64b0a0000000000000000008", "CREATE", "PATIENT", "016800004", lineD, true, "2024-04-01T03:00:00.000Z"),
	// a doctor's own LINE account — not a patient pairing at all.
	pairLog("64b0a0000000000000000009", "CREATE", "DOCTOR", "1005", lineE, true, "2024-05-01T03:00:00.000Z"),
}, "")

func TestPairLogsIsUsedWhenThereIsNoLinePatients(t *testing.T) {
	_, binding, pointer, rep := buildLineBindOne(t, pairLogsSample)

	for _, live := range []string{"'016800001'", "'016800003'", "'016800004'"} {
		if !strings.Contains(binding, live) {
			t.Errorf("live pairing %s was not rebuilt from pair_logs", live)
		}
		if !strings.Contains(pointer, live) {
			t.Errorf("live pairing %s was not stamped onto patient.line_id", live)
		}
	}
	if strings.Contains(binding, "016800002") || strings.Contains(binding, lineB) {
		t.Error("a pairing whose last event is DELETE was carried over — the customer unlinked")
	}
	if strings.Contains(binding, "'1005'") || strings.Contains(binding, lineE) {
		t.Error("a DOCTOR event became a patient binding")
	}
	if got := strings.Count(binding, "INSERT INTO patient_line_binding"); got != 3 {
		t.Errorf("wrote %d bindings, want 3", got)
	}

	// The time is the event that made the pair live, not the first CREATE.
	if !strings.Contains(binding, "'2024-01-05 03:00:00+00'") {
		t.Error("016800003 is not stamped with its SET_ACTIVE time")
	}
	if !strings.Contains(binding, "'2024-04-01 03:00:00+00'") {
		t.Error("016800004 is not stamped with its newest CREATE")
	}
	if strings.Contains(binding, "'2024-02-01 03:00:00+00'") {
		t.Error("016800004 carries the time of a CREATE that was later deleted")
	}

	// Provenance says which collection to open.
	if !strings.Contains(binding, "'pair_logs:64b0a0000000000000000008'") {
		t.Error("verified_ref does not name the pair_logs event it came from")
	}

	// Every document is accounted for: 9 events in, 3 bindings, 6 skips with reasons.
	e := rep.Entities[EntPatientLineBinding]
	if e == nil {
		t.Fatal("no report entity for the bindings")
	}
	if e.SourceRows != 9 {
		t.Errorf("SourceRows = %d, want 9 (every pair_logs document)", e.SourceRows)
	}
	skipped := 0
	for _, n := range e.Skipped {
		skipped += n
	}
	if skipped != 6 {
		t.Errorf("skipped = %d, want 6 (1 doctor, 4 earlier events, 1 ended pair) · %v", skipped, e.Skipped)
	}

	// Said as a warning: rebuilt from a log, not read off a list.
	if !hasWarning(rep, "สร้างการผูกจาก mris.pair_logs") {
		t.Errorf("no warning says the pairings were rebuilt from pair_logs · %v", rep.Warnings)
	}
	if !hasWarning(rep, "016800002") {
		t.Errorf("the unlinked pair is not named · %v", rep.Warnings)
	}
}

func TestLinePatientsWinsWhenBothArePresent(t *testing.T) {
	_, binding, _, rep := buildLineBindOne(t, linePatientsSample+"\n"+pairLogsSample)

	if !strings.Contains(binding, "'026700031'") {
		t.Error("line_patients was not used although it was present")
	}
	// "'pair_logs:" with the quote: the prefix also appears in a comment of the batch,
	// and only a verified_ref LITERAL would mean the log was read.
	for _, fromLog := range []string{"016800001", "016800003", "016800004", "'pair_logs:"} {
		if strings.Contains(binding, fromLog) {
			t.Errorf("pair_logs was read although line_patients was present (%s)", fromLog)
		}
	}
	// Not silently ignored either.
	found := false
	for _, n := range rep.Notes {
		if strings.Contains(n, "mris.pair_logs 9 เอกสาร") && strings.Contains(n, "ไม่ได้ใช้") {
			found = true
		}
	}
	if !found {
		t.Errorf("the report does not say pair_logs was present and not used · %v", rep.Notes)
	}
}

func TestADumpWithOnlyPairLogsIsAccepted(t *testing.T) {
	// parseMongoCatalog refuses a dump with nothing it supports; pair_logs alone must
	// count as supported, or a box with no line_patients could never be imported.
	if _, err := parseMongoCatalog([]byte(pairLogsSample)); err != nil {
		t.Fatalf("a dump holding only pair_logs was rejected: %v", err)
	}
}
