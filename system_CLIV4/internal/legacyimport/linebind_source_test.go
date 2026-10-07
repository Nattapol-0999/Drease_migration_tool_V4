package legacyimport

// Where the LINE pairings come from (owner's decision ก, 6 Oct 2026):
//
//	line_patients present        → line_patients, pair_logs ignored
//	only pair_logs               → pairings rebuilt from the LAST event of each pair
//	neither (or both empty)      → the group is SKIPPED, the run is not refused
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

func TestTheSharedLineIDRuleAppliesToPairLogsToo(t *testing.T) {
	shared := pairLogsSample +
		pairLog("64b0a000000000000000000a", "CREATE", "PATIENT", "016800009", lineA, true, "2023-07-30T03:00:00.000Z")
	_, binding, _, rep := buildLineBindOne(t, shared)
	if !strings.Contains(binding, "'016800009'") || strings.Contains(binding, "'016800001'") {
		t.Error("the later of two HNs on one LINE id was not the one kept")
	}
	if !hasWarning(rep, "ไม่นำเข้า HN 016800001") {
		t.Errorf("the HN left behind is not named · %v", rep.Warnings)
	}
}

// ── one HN on several LINE ids (owner's decisions 1ข 2ก 3ข, 7 Oct 2026) ─────────────────

const (
	lineF = "Uffffffffffffffffffffffffffffff6"
	lineG = "Ugggggggggggggggggggggggggggggg7"
	lineH = "Uhhhhhhhhhhhhhhhhhhhhhhhhhhhhhh8"
	lineI = "Uiiiiiiiiiiiiiiiiiiiiiiiiiiiiii9"
	lineJ = "Ujjjjjjjjjjjjjjjjjjjjjjjjjjjjjj0"
	lineK = "Ukkkkkkkkkkkkkkkkkkkkkkkkkkkkkka"
	lineL = "Ullllllllllllllllllllllllllllllb"
)

// boundTogether reports whether one INSERT of the binding batch pairs hn with lineID. The
// comment above each INSERT names its HN unquoted, so only the statement itself matches.
func boundTogether(binding, hn, lineID string) bool {
	for _, stmt := range strings.Split(binding, "INSERT INTO patient_line_binding")[1:] {
		if strings.Contains(stmt, "'"+hn+"'") && strings.Contains(stmt, "'"+lineID+"'") {
			return true
		}
	}
	return false
}

func skippedTotal(e *EntityResult) int {
	n := 0
	for _, c := range e.Skipped {
		n += c
	}
	return n
}

// The refusal in the 7 Oct screenshot: 016600084 paired a second LINE id four days after
// the first. The later one is kept, the earlier one is named, and the run goes on.
func TestPairLogsKeepsTheLatestLineIDOfAnHN(t *testing.T) {
	second := pairLogsSample +
		pairLog("64b0a000000000000000000b", "CREATE", "PATIENT", "016800001", lineF, true, "2023-08-10T03:00:00.000Z")
	_, binding, pointer, rep := buildLineBindOne(t, second)

	if !boundTogether(binding, "016800001", lineF) {
		t.Error("the later LINE id of 016800001 was not bound")
	}
	if strings.Contains(binding, lineA) || strings.Contains(pointer, lineA) {
		t.Error("the earlier LINE id of 016800001 was written too — V4 holds one LINE per HN")
	}
	for _, want := range []string{"HN 016800001: เก็บ LINE Ufffffff", "ไม่นำเข้า LINE Uaaaaaaa",
		"mris.pair_logs", "7 ต.ค. 2569"} {
		if !hasWarning(rep, want) {
			t.Errorf("the warning does not carry %q · warnings = %v", want, rep.Warnings)
		}
	}

	// Still accounted for: 10 events = 3 bindings written + 7 skips, the new one included.
	e := rep.Entities[EntPatientLineBinding]
	written := strings.Count(binding, "INSERT INTO patient_line_binding")
	if written != 3 || e.SourceRows != 10 || written+skippedTotal(e) != e.SourceRows {
		t.Errorf("written %d + skipped %d ≠ source %d (want 3 + 7 = 10) · %v",
			written, skippedTotal(e), e.SourceRows, e.Skipped)
	}
}

// Decision 2ก: "latest" is the newest event of the pair, the time verified_at gets — not
// the first CREATE. Real: 016516285 paired U2635b90… on 2023-11-30 and again on 2024-08-03,
// and U0e0918f… once on 2024-03-25 in between. The re-paired account is kept.
func TestARepairedPairingCountsFromItsNewestEvent(t *testing.T) {
	body := pairLog("65686b5a0000000000000001", "CREATE", "PATIENT", "016800010", lineG, true, "2023-11-30T11:34:30.000Z") +
		pairLog("66016190000000000000000a", "CREATE", "PATIENT", "016800010", lineH, true, "2024-03-25T11:35:57.000Z") +
		pairLog("66ad9e48000000000000000b", "CREATE", "PATIENT", "016800010", lineG, true, "2024-08-03T03:04:40.000Z")
	_, binding, _, rep := buildLineBindOne(t, body)

	if !boundTogether(binding, "016800010", lineG) || strings.Contains(binding, lineH) {
		t.Errorf("the pairing re-made on 2024-08-03 did not win over the one of 2024-03-25:\n%s", binding)
	}
	if !strings.Contains(binding, "'2024-08-03 03:04:40+00'") {
		t.Error("the kept pairing is not stamped with its newest event")
	}
	if !hasWarning(rep, "ไม่นำเข้า LINE Uhhhhhhh… (ผูก 2024-03-25 11:35)") {
		t.Errorf("the pairing left out is not named with its time · %v", rep.Warnings)
	}
}

// Decision 3ข: one pass, newest first. Real: 016701865 paired 016701866's LINE id one
// minute after it, then its own 42 minutes later. 016701865 keeps its own, and the id it
// gave up goes back to 016701866 — two passes would have left it bound to nobody.
func TestALineIDGivenUpByItsLatestHolderGoesBackToTheOlderOne(t *testing.T) {
	body := pairLog("6720d5000000000000000001", "CREATE", "PATIENT", "016800020", lineI, true, "2024-10-29T12:29:04.000Z") +
		pairLog("6720d54c0000000000000002", "CREATE", "PATIENT", "016800021", lineI, true, "2024-10-29T12:30:04.000Z") +
		pairLog("6720df320000000000000003", "CREATE", "PATIENT", "016800021", lineJ, true, "2024-10-29T13:12:18.000Z")
	_, binding, pointer, rep := buildLineBindOne(t, body)

	if !boundTogether(binding, "016800021", lineJ) {
		t.Error("016800021 lost its own, newest LINE id")
	}
	if !boundTogether(binding, "016800020", lineI) {
		t.Errorf("the LINE id 016800021 gave up did not go back to 016800020:\n%s", binding)
	}
	if boundTogether(binding, "016800021", lineI) {
		t.Error("016800021 kept two LINE ids")
	}
	if got := strings.Count(pointer, "UPDATE patient p"); got != 2 {
		t.Errorf("stamped %d patients, want 2", got)
	}
	if !hasWarning(rep, "HN 016800021: เก็บ LINE Ujjjjjjj") {
		t.Errorf("the pairing 016800021 gave up is not named · %v", rep.Warnings)
	}
	if hasWarning(rep, "ไม่นำเข้า HN 016800020") {
		t.Error("016800020 is reported as left out although it kept the LINE id")
	}
}

// Two LINE ids of one HN at the same instant: the document _id settles it, in either order.
func TestAnHNWithTwoLineIDsAtTheSameTimeIsDecidedByTheDocumentID(t *testing.T) {
	older := pairLog("67a72580000000000000aa01", "CREATE", "PATIENT", "016800030", lineK, true, "2025-02-08T10:00:00.000Z")
	newer := pairLog("67a72580000000000000bb02", "CREATE", "PATIENT", "016800030", lineL, true, "2025-02-08T10:00:00.000Z")
	for _, body := range []string{older + newer, newer + older} {
		_, binding, _, _ := buildLineBindOne(t, body)
		if !boundTogether(binding, "016800030", lineL) || strings.Contains(binding, lineK) {
			t.Errorf("with equal times the higher _id must win in any document order:\n%s", body)
		}
	}
}

// ── the group-level rule: neither collection means skip, not refuse ──────────────────

// itemsOnly is a mongo dump with something the parser supports and nothing about LINE.
const itemsOnly = `db.items.insertMany([{"_id":"i1","clinic_code":"C0TEST1",` +
	`"code":"ST000001","name":"Botox 100U","quantity":5,"price":3500,"active":true}]);` + "\n"

func TestNoLINECollectionSkipsTheGroupInsteadOfRefusing(t *testing.T) {
	// No -only: every group, linebind included, exactly what the web form sends when
	// all of its boxes are ticked. pointsSample is here only because the LOYALTY group
	// still refuses a dump with no `points` — a separate rule, not this one.
	script, rep, err := BuildScript(Input{
		ClinicCode:    "C0TEST1",
		DefaultBranch: "BR00001",
		MongoDump:     []byte(itemsOnly + pointsSample),
	})
	if err != nil {
		t.Fatalf("a dump with no LINE collection refused the whole run: %v", err)
	}
	if strings.Contains(script, "INSERT INTO patient_line_binding") ||
		strings.Contains(script, "INSERT INTO clinic_lineoa") {
		t.Error("LINE statements were written although there was nothing to bind")
	}
	if !hasText(rep.Notes, "ข้ามกลุ่ม linebind") {
		t.Errorf("the skip is not recorded · notes = %v", rep.Notes)
	}
	if hasWarning(rep, "ข้ามกลุ่ม linebind") {
		t.Error("an implicit skip was raised as a warning — nobody asked for LINE by name")
	}
}

func TestAskingForLinebindByNameAndGettingNothingIsAWarning(t *testing.T) {
	// linebind named explicitly next to a group that does have data: the LINE group must
	// not take the catalog down with it.
	script, rep, err := BuildScript(Input{
		ClinicCode:    "C0TEST1",
		DefaultBranch: "BR00001",
		Groups:        []string{"catalog", "linebind"},
		MongoDump:     []byte(itemsOnly),
	})
	if err != nil {
		t.Fatalf("a run naming linebind refused because there was no LINE collection: %v", err)
	}
	if !strings.Contains(script, "INSERT INTO catalog_item") {
		t.Error("the catalog group was lost along with the empty LINE group")
	}
	if !hasWarning(rep, "เลือกกลุ่ม linebind ไว้แต่ไม่มีอะไรให้นำเข้า") {
		t.Errorf("asking for the group by name and getting nothing is not a warning · %v", rep.Warnings)
	}
}

// -only linebind ALONE with nothing to bind is still refused, by the older guard that
// says the selected groups have nothing in this file. That is the right answer for a run
// with no work in it at all: there is no script to hand back, and the operator has to
// hear why rather than receive an empty file.
func TestOnlyLinebindWithNothingToBindIsStillTheEmptyRunRefusal(t *testing.T) {
	_, _, err := BuildScript(Input{
		ClinicCode:    "C0TEST1",
		DefaultBranch: "BR00001",
		Groups:        []string{"linebind"},
		MongoDump:     []byte(itemsOnly),
	})
	if err == nil {
		t.Fatal("an empty run produced a script instead of saying it had nothing to do")
	}
	if !strings.Contains(err.Error(), "linebind") {
		t.Errorf("the refusal does not name the group: %v", err)
	}
}
