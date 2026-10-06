package legacyimport

// Tests for mris.line_patients → clinic_lineoa + patient_line_binding + patient.line_id.
//
// Each case pins a decision rather than describing behaviour, because every one of them
// is about a person's identity and consent:
//
//	an unlinked pairing is NOT carried over          (consent, owner's decision ก)
//	the OA group is created only when there is none   (and refuses to guess between two)
//	a duplicate hn refuses the run                    (one patient, two LINE accounts)
//	a line_id held by two HNs keeps the LATEST one    (owner's decision ข, 6 Oct 2026)
//	the HN decides the patient, not pair_branch       (owner's decision, 6 Oct 2026)
//	an existing binding at the target always wins     (a real login beats an import)
//	patient_line_link_log is never written            (an import is not a login)

import (
	"strings"
	"testing"
)

// linePatientsSample mirrors the real shapes in the-gloss's export:
//
//	026700031  the ordinary case
//	026700012  a second ordinary one, to prove ordering is by HN and not by document
//	026700006  active:false — the customer unlinked, and must not be re-linked
//	016700099  an HN whose branch prefix (01) disagrees with pair_branch (02). Real:
//	           one of the 699 documents is exactly this. The HN decides.
const linePatientsSample = `db.line_patients.insert({
    "_id": ObjectId("6716350ada59e5b842d3db0b"),
    "hn": "026700031",
    "active": true,
    "createdAt": ISODate("2024-10-21T11:03:38.371Z"),
    "line_id": "U212cb71e81ff10f4304c5f9f8db31450",
    "pair_branch": "02"
});
db.line_patients.insert({
    "_id": ObjectId("67164387da59e5b842d3ea7d"),
    "hn": "026700012",
    "active": true,
    "createdAt": ISODate("2024-10-21T12:05:27.316Z"),
    "line_id": "Ua9e73da7e086b1aaae57abc10d9483ed",
    "pair_branch": "02"
});
db.line_patients.insert({
    "_id": ObjectId("671645d1da59e5b842d3ed18"),
    "hn": "026700006",
    "active": false,
    "createdAt": ISODate("2024-10-21T12:15:13.122Z"),
    "line_id": "U611ffcc2e5eea489a8e82866f2a04c93",
    "pair_branch": "02"
});
db.line_patients.insert({
    "_id": ObjectId("67165f08da59e5b842d405e2"),
    "hn": "016700099",
    "active": true,
    "createdAt": ISODate("2024-10-25T09:22:31.045Z"),
    "line_id": "U7d506a34c99c041cd2507142ec1fe3c6",
    "pair_branch": "02"
});`

func buildLineBindOne(t *testing.T, body string) (group, binding, pointer string, rep *Report) {
	t.Helper()
	mc, err := parseMongoCatalog([]byte(body))
	if err != nil {
		t.Fatalf("parseMongoCatalog: %v", err)
	}
	rep = NewReport("C2EC1E3")
	in := Input{
		ClinicCode: "C2EC1E3",
		BranchMap:  map[string]string{"01": "BR00001", "02": "BR00002"},
	}
	group, binding, pointer, err = buildLineBind(mc, in, rep)
	if err != nil {
		t.Fatalf("buildLineBind: %v", err)
	}
	return group, binding, pointer, rep
}

func TestUnlinkedPairingIsNotCarriedOver(t *testing.T) {
	_, binding, pointer, rep := buildLineBindOne(t, linePatientsSample)

	// The unlinked customer's own id must appear nowhere that writes.
	unlinked := "U611ffcc2e5eea489a8e82866f2a04c93"
	if strings.Contains(binding, unlinked) {
		t.Error("an active:false pairing was written as a binding — the customer unlinked")
	}
	if strings.Contains(pointer, unlinked) {
		t.Error("an active:false pairing was stamped onto patient.line_id")
	}
	if strings.Contains(binding, "026700006") {
		t.Error("the unlinked customer's HN still appears in the binding batch")
	}

	// And it is reported, not silently dropped: a count nobody can see is the same as
	// losing the row.
	e := rep.Entities[EntPatientLineBinding]
	if e == nil {
		t.Fatal("no report entity for the bindings")
	}
	if e.SourceRows != 4 {
		t.Errorf("SourceRows = %d, want 4 (all documents, including the skipped one)", e.SourceRows)
	}
	var skipped int
	for reason, n := range e.Skipped {
		if strings.Contains(reason, "active = false") {
			skipped += n
		}
	}
	if skipped != 1 {
		t.Errorf("skipped-as-unlinked = %d, want 1 · Skipped = %v", skipped, e.Skipped)
	}
	if !hasWarning(rep, "active = false") {
		t.Errorf("no warning names the unlinked pairing · warnings = %v", rep.Warnings)
	}
}

func TestLineBindingWritesAllThreeTablesForALivePairing(t *testing.T) {
	group, binding, pointer, _ := buildLineBindOne(t, linePatientsSample)

	// The group is created, and only when the clinic has none.
	if !strings.Contains(group, "INSERT INTO clinic_lineoa (id, clinic_code, group_name)") {
		t.Error("the OA group is not created")
	}
	if !strings.Contains(group, "NOT EXISTS (SELECT 1 FROM clinic_lineoa g WHERE g.clinic_code") {
		t.Error("the group INSERT is not gated on the clinic having no group yet")
	}
	// Only the three columns the owner asked for. Pinned on the INSERT's column list and
	// not on the batch text, because the comment above the statement NAMES the defaulted
	// columns in order to explain them — searching the whole batch failed on its own
	// documentation.
	//
	// Any fourth column here would be a value this tool invented for a config it has
	// never seen: a token, a LIFF id or a webhook secret that no LINE channel issued.
	const head = "INSERT INTO clinic_lineoa ("
	i := strings.Index(group, head)
	if i < 0 {
		t.Fatal("no INSERT INTO clinic_lineoa")
	}
	cols := group[i+len(head):]
	cols = cols[:strings.Index(cols, ")")]
	if cols != "id, clinic_code, group_name" {
		t.Errorf("the group INSERT writes (%s) — it must create an EMPTY group with only "+
			"id, clinic_code, group_name and let every other column default", cols)
	}
	// group_name is read from the target, because this side only knows the clinic code.
	if !strings.Contains(group, "'_old'") {
		t.Error("the group name does not carry the _old suffix")
	}
	if !strings.Contains(group, "c.tha_name") {
		t.Error("the group name is not read from the clinic row at the target")
	}

	// Three live pairings, three bindings, three pointers.
	if n := strings.Count(binding, "INSERT INTO patient_line_binding"); n != 3 {
		t.Errorf("binding INSERTs = %d, want 3", n)
	}
	if n := strings.Count(pointer, "UPDATE patient p"); n != 3 {
		t.Errorf("pointer UPDATEs = %d, want 3", n)
	}

	// The binding carries V2's own pairing time and source document, not now().
	if !strings.Contains(binding, "2024-10-21 11:03:38+00") {
		t.Error("verified_at is not V2's createdAt")
	}
	if !strings.Contains(binding, "'6716350ada59e5b842d3db0b'") {
		t.Error("verified_ref does not carry line_patients._id")
	}
	if !strings.Contains(binding, LineBindVerifiedMethod) {
		t.Errorf("verified_method is not %q", LineBindVerifiedMethod)
	}

	// The pointer writes the same group id as the binding, which is what the live rows do.
	if !strings.Contains(pointer, "line_oa_group_id = (SELECT g.id FROM clinic_lineoa g") {
		t.Error("patient.line_oa_group_id is not set from clinic_lineoa.id")
	}
	if !strings.Contains(pointer, "line_linked_at   = '2024-10-21 11:03:38+00'") {
		t.Error("line_linked_at is not V2's createdAt")
	}
}

func TestExistingBindingAtTheTargetWins(t *testing.T) {
	_, binding, pointer, _ := buildLineBindOne(t, linePatientsSample)

	// Both partial unique indexes are mirrored, which is the shape migration 0327 uses:
	// a live binding for this LINE id, or for this patient, stops the insert.
	if n := strings.Count(binding, "b.revoked_at IS NULL"); n != 6 {
		t.Errorf("revoked_at guards = %d, want 6 (two per binding × 3)", n)
	}
	if !strings.Contains(binding, "b.line_user_id =") || !strings.Contains(binding, "b.patient_id = p.id") {
		t.Error("the binding is not guarded on BOTH the LINE id and the patient")
	}

	// The pointer never overwrites a LINE id the target already holds: a login the
	// customer did themselves is newer and truer than anything carried from V2.
	if n := strings.Count(pointer, "p.line_id IS NULL"); n != 3 {
		t.Errorf("line_id IS NULL guards = %d, want 3", n)
	}
	// And it cannot collide with ux_patient_clinic_line_id by giving one clinic's id to
	// two patients.
	if n := strings.Count(pointer, "NOT EXISTS (SELECT 1 FROM patient o"); n != 3 {
		t.Errorf("cross-patient line_id guards = %d, want 3", n)
	}
}

func TestLineBindRefusesToGuessBetweenTwoOAGroups(t *testing.T) {
	group, _, _, _ := buildLineBindOne(t, linePatientsSample)

	// The count is a fact about the TARGET, so the refusal has to live in the SQL.
	if !strings.Contains(group, "DO $linebind_group$") {
		t.Fatal("no runtime guard on the number of OA groups")
	}
	if !strings.Contains(group, "IF n > 1 THEN") {
		t.Error("the guard does not refuse a clinic with more than one group")
	}
	if !strings.Contains(group, "IF n = 0 THEN") {
		t.Error("the guard does not refuse a clinic that has no group and could not be given one")
	}
	if !strings.Contains(group, "RAISE EXCEPTION") {
		t.Error("the guard warns instead of refusing — a binding under the wrong OA is unusable")
	}
}

func TestHNDecidesThePatientNotPairBranch(t *testing.T) {
	_, binding, _, rep := buildLineBindOne(t, linePatientsSample)

	// 016700099 was paired at branch 02 but its HN belongs to 01. The row is still
	// written, keyed on the HN, and the branch is not used to place it.
	if !strings.Contains(binding, "'016700099'") {
		t.Error("the cross-branch pairing was dropped — the HN decides, so it must be imported")
	}
	if strings.Contains(binding, "branch_code") {
		t.Error("the binding writes a branch — patient_line_binding has no branch, and the " +
			"patient row already carries the one V4 allocated")
	}
	// Reported, because two sources disagreeing is worth knowing even when the answer is
	// settled.
	if !hasNote(rep, "pair_branch") {
		t.Errorf("the branch disagreement is not reported · notes = %v", rep.Notes)
	}
}

func TestLineBindNeverWritesTheLinkLog(t *testing.T) {
	group, binding, pointer, _ := buildLineBindOne(t, linePatientsSample)

	// patient_line_link_log rows are evidence that a LINE login happened — migration 0327
	// reads source='line_register' as exactly that. An import is not a login.
	all := group + binding + pointer
	for _, line := range strings.Split(all, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue // the comment explaining the omission is allowed to name it
		}
		if strings.Contains(line, "patient_line_link_log") {
			t.Errorf("the script writes patient_line_link_log: %s", strings.TrimSpace(line))
		}
	}
}

func TestLineBindRefusesADuplicateHN(t *testing.T) {
	dup := linePatientsSample + `
db.line_patients.insert({
    "_id": ObjectId("6716350ada59e5b842d3dbff"),
    "hn": "026700031",
    "active": true,
    "createdAt": ISODate("2025-01-02T03:04:05.000Z"),
    "line_id": "Ucccccccccccccccccccccccccccccccc",
    "pair_branch": "02"
});`
	mc, err := parseMongoCatalog([]byte(dup))
	if err != nil {
		t.Fatalf("parseMongoCatalog: %v", err)
	}
	_, _, _, err = buildLineBind(mc, Input{ClinicCode: "C2EC1E3"}, NewReport("C2EC1E3"))
	if err == nil {
		t.Fatal("two documents for one HN were accepted — V4 allows one LINE per HN per clinic")
	}
	if !strings.Contains(err.Error(), "026700031") {
		t.Errorf("the refusal does not name the duplicated HN: %v", err)
	}
}

// One LINE id held by two HNs used to refuse the run. Owner's decision ข (6 Oct 2026):
// keep the LATEST pairing and name the other in the report. The second document here
// pairs 026700077 on 2025-01-02, after 026700031 paired the same id on 2024-10-21.
func TestLineBindKeepsTheLatestPairingOfASharedLineID(t *testing.T) {
	dup := linePatientsSample + `
db.line_patients.insert({
    "_id": ObjectId("6716350ada59e5b842d3dbfe"),
    "hn": "026700077",
    "active": true,
    "createdAt": ISODate("2025-01-02T03:04:05.000Z"),
    "line_id": "U212cb71e81ff10f4304c5f9f8db31450",
    "pair_branch": "02"
});`
	_, binding, pointer, rep := buildLineBindOne(t, dup)

	if !strings.Contains(binding, "'026700077'") {
		t.Error("the latest pairing of the shared LINE id was not written")
	}
	if strings.Contains(binding, "'026700031'") {
		t.Error("the older pairing of the shared LINE id was written too — V4 holds one patient per LINE id")
	}
	if strings.Contains(pointer, "'026700031'") {
		t.Error("the older pairing was stamped onto patient.line_id")
	}
	if got := strings.Count(binding, "U212cb71e81ff10f4304c5f9f8db31450"); got != 2 {
		// One INSERT names the id twice: the value and its NOT EXISTS guard.
		t.Errorf("the shared id appears %d times in the binding batch, want 2 (one row)", got)
	}
	// The other pairings are untouched by the rule.
	for _, hn := range []string{"'026700012'", "'016700099'"} {
		if !strings.Contains(binding, hn) {
			t.Errorf("an unrelated pairing %s was lost", hn)
		}
	}
	// Reported loudly, naming both HNs, so the clinic can re-pair the right one.
	if !hasWarning(rep, "เก็บการผูกล่าสุด") {
		t.Fatalf("no warning says the latest pairing was kept · warnings = %v", rep.Warnings)
	}
	for _, want := range []string{"เก็บ HN 026700077", "ไม่นำเข้า HN 026700031", "6 ต.ค. 2569"} {
		if !hasWarning(rep, want) {
			t.Errorf("the warning does not carry %q · warnings = %v", want, rep.Warnings)
		}
	}
}

// Equal times are settled by the document _id, so the choice does not depend on the order
// documents happen to sit in the dump.
func TestASharedLineIDWithEqualTimesIsDecidedByTheDocumentID(t *testing.T) {
	pair := func(id, hn string) string {
		return `db.line_patients.insert({
    "_id": ObjectId("` + id + `"),
    "hn": "` + hn + `",
    "active": true,
    "createdAt": ISODate("2025-02-08T10:00:00.000Z"),
    "line_id": "Udddddddddddddddddddddddddddddddd"
});
`
	}
	older := pair("67a7258000000000000000aa", "016800001")
	newer := pair("67a7258000000000000000bb", "016800002")
	for _, body := range []string{older + newer, newer + older} {
		_, binding, _, _ := buildLineBindOne(t, body)
		if !strings.Contains(binding, "'016800002'") || strings.Contains(binding, "'016800001'") {
			t.Errorf("with equal times the higher _id must win in any document order:\n%s", body)
		}
	}
}

// The same pairing written twice whole is one pairing, not a duplicate HN: the shared-id
// rule runs first and collapses it.
func TestADocumentRepeatedWholeCollapsesInsteadOfRefusing(t *testing.T) {
	again := linePatientsSample + `
db.line_patients.insert({
    "_id": ObjectId("6716350ada59e5b842d3dbfd"),
    "hn": "026700031",
    "active": true,
    "createdAt": ISODate("2024-10-21T11:03:38.371Z"),
    "line_id": "U212cb71e81ff10f4304c5f9f8db31450",
    "pair_branch": "02"
});`
	_, binding, _, _ := buildLineBindOne(t, again)
	if got := strings.Count(binding, "INSERT INTO patient_line_binding"); got != 3 {
		t.Errorf("wrote %d bindings, want 3 (the repeat collapses into one)", got)
	}
}

func TestLineBindRefusesWhenEveryPairingIsUnlinked(t *testing.T) {
	allOff := `db.line_patients.insert({
    "_id": ObjectId("671645d1da59e5b842d3ed18"),
    "hn": "026700006",
    "active": false,
    "createdAt": ISODate("2024-10-21T12:15:13.122Z"),
    "line_id": "U611ffcc2e5eea489a8e82866f2a04c93",
    "pair_branch": "02"
});`
	mc, err := parseMongoCatalog([]byte(allOff))
	if err != nil {
		t.Fatalf("parseMongoCatalog: %v", err)
	}
	_, _, _, err = buildLineBind(mc, Input{ClinicCode: "C2EC1E3"}, NewReport("C2EC1E3"))
	if err == nil {
		t.Fatal("a collection with nothing importable produced a script instead of saying so")
	}
	if !strings.Contains(err.Error(), "line_patients") {
		t.Errorf("the refusal does not name the collection: %v", err)
	}
}

func TestLegacyOAGroupIDIsStableAndTheRightShape(t *testing.T) {
	// Deterministic: the same dumps must produce a byte-identical .sql file so two
	// attempts can be diffed. A random id would differ on a line that matters.
	a := legacyOAGroupID("C2EC1E3")
	b := legacyOAGroupID("C2EC1E3")
	if a != b {
		t.Errorf("the group id is not stable: %s vs %s", a, b)
	}
	if legacyOAGroupID("C6C936C") == a {
		t.Error("two clinics got the same group id — clinic_lineoa.id is a global primary key")
	}
	// varchar(64), and the same 64-hex shape the app itself mints.
	if len(a) != 64 {
		t.Errorf("group id length = %d, want 64 (the column is varchar(64))", len(a))
	}
	for _, r := range a {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("group id is not lowercase hex: %q", a)
		}
	}
}

func TestLineBindIsASelectableGroupThatDependsOnPatients(t *testing.T) {
	ents, err := entitiesForGroups([]string{"linebind"})
	if err != nil {
		t.Fatalf("linebind is not a known group: %v", err)
	}
	for _, want := range []string{EntLineoaGroup, EntPatientLineBinding, EntPatientLineID} {
		if !ents[want] {
			t.Errorf("group linebind does not include %s", want)
		}
	}
	// Indivisible: the three are one fact, and a binding with no group cannot exist.
	if len(ents) != 3 {
		t.Errorf("group linebind resolves to %d entities, want 3: %v", len(ents), ents)
	}

	if deps := groupDependsOn["linebind"]; len(deps) != 1 || deps[0] != "patients" {
		t.Errorf("groupDependsOn[linebind] = %v, want [patients]", deps)
	}

	// The group row has to be ordered before anything that points at it.
	order := EntityOrder()
	pos := map[string]int{}
	for i, e := range order {
		pos[e] = i
	}
	for _, e := range []string{EntLineoaGroup, EntPatientLineBinding, EntPatientLineID} {
		if _, ok := pos[e]; !ok {
			t.Fatalf("%s is missing from the entity order — it would never be written", e)
		}
	}
	if pos[EntLineoaGroup] > pos[EntPatientLineBinding] {
		t.Error("the OA group is ordered after the bindings that reference it")
	}
	if pos[EntPatient] > pos[EntLineoaGroup] {
		t.Error("the LINE batches are ordered before the patients they resolve")
	}

	// The group is resolved once per binding and once per pointer, so the table it is
	// read from must be analysed first.
	if tables := analyzeTargets[EntLineoaGroup]; len(tables) != 1 || tables[0] != "clinic_lineoa" {
		t.Errorf("analyzeTargets[%s] = %v, want [clinic_lineoa]", EntLineoaGroup, tables)
	}
}
