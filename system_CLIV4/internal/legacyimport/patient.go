package legacyimport

import (
	"fmt"
	"sort"
	"strings"

	"system_cliv4/internal/legacydump"
)

type patientKind int

const (
	pText patientKind = iota // varchar/text, clipped at maxLen
	pInt                     // integer
	pDate                    // legacy "DD/MM/YYYY" -> date
	pTS                      // legacy "YYYY-MM-DD HH:MM:SS" (UTC) -> timestamptz
	pNum                     // legacy double -> varchar, 0 -> NULL
	// pUserRef is a legacy users.id. It is NOT copied: V4 issues its own ids, so the value
	// is translated through import_user_map. Copying it would point the column at whichever
	// clinic reached that number first — on the shared test database, 36 of 112 accounts.
	pUserRef
	// pPrefix and pGender turn a legacy 2-digit lookup code into the WORD it stands for.
	// See legacyPrefixName / legacyGenderName for why, and for what the codes mean.
	pPrefix
	pGender
)

// legacyPrefixName resolves patient.Prefixname from the code V2 stores there.
//
// # WHY THIS TRANSLATION EXISTS
//
// V2 keeps a 2-digit code in patient.Prefixname and the words in its own `_prefix` table.
// V4 has no prefix table at all — patient."Prefixname" is varchar(32) holding the word
// itself, which is what the app writes ("นาย", "นางสาว"). Copying the code straight across
// is what produced 1,939 patients whose title reads "03" on screen.
//
// THE MAP IS `_prefix` FROM THE CLINIC'S OWN DUMP, read row by row (9 rows), with the three
// Thai abbreviations expanded — the clinic owner's decision, 15 Sep 2026:
//
//	01 นาย    -> นาย
//	02 นาง    -> นาง
//	03 น.ส.   -> นางสาว      (expanded)
//	04 ด.ช.   -> เด็กชาย      (expanded)
//	05 ด.ญ.   -> เด็กหญิง     (expanded)
//	06 อื่นๆ   -> ""          (dropped: "อื่นๆ" is not a title anyone is addressed by)
//	07 Mr.    -> Mr.
//	08 Miss.  -> Miss.
//	09 Mrs.   -> Mrs.
//
// The expansion is deliberate and not a formatting whim: it matches what V4's own screens
// write, so an imported patient and one keyed in today read the same. 06 becomes blank
// rather than being carried over, because a record that says the patient's title is
// "other" tells the reader nothing that an empty field does not.
//
// Hardcoded rather than read from the dump at run time: the caller receives ONE mongo file
// and one MySQL file, and `_prefix` is a 9-row reference list that has been identical in
// every V2 export seen. A code outside this map is left blank AND reported — never guessed.
var legacyPrefixName = map[string]string{
	"01": "นาย",
	"02": "นาง",
	"03": "นางสาว",
	"04": "เด็กชาย",
	"05": "เด็กหญิง",
	"06": "",
	"07": "Mr.",
	"08": "Miss.",
	"09": "Mrs.",
}

// legacyGenderName resolves patient.gender the same way, from V2's `_gender` (3 rows).
//
//	01 ชาย   -> ชาย
//	02 หญิง  -> หญิง
//	03 อื่นๆ  -> ไม่ระบุ      (owner's wording, 15 Sep 2026)
//
// "ไม่ระบุ" rather than the source's "อื่นๆ" because that is the phrase the target already
// uses for an unstated sex, and the two mean the same thing in this column.
var legacyGenderName = map[string]string{
	"01": "ชาย",
	"02": "หญิง",
	"03": "ไม่ระบุ",
}

// patientCol is one target column fed from one source column. "Created_at" feeds two
// targets, which is why this is a slice and not a map.
type patientCol struct {
	target string // already quoted where the V4 column is case-sensitive
	source string
	kind   patientKind
	maxLen int
}

// patientMapping is the column-compatible subset of the legacy patient table. V4's
// patient table was derived from this legacy schema, so most names match one-to-one.
//
// "Age" is deliberately absent (SkipLegacyAgeColumn): the legacy values are strings cut
// mid-character ('36 ปี 11 เด') and derived anyway. Also absent, with no V4 column:
// general_id, Implantation_ID, SSO, lead_tracker, lead_tracker_other, drug_regular,
// Illness_path, dental_path, insurance_code, user_last_update,
// future_appointment_count, Prefixname_en.
var patientMapping = []patientCol{
	{`"HN"`, "HN", pText, 20},
	// Prefixname and gender hold a lookup CODE in V2 and the word itself in V4 —
	// translated, not copied. See legacyPrefixName.
	{`"Prefixname"`, "Prefixname", pPrefix, 32},
	{`"Firstname"`, "Firstname", pText, 128},
	{`"Lastname"`, "Lastname", pText, 128},
	{`patient_middlename`, "patient_middlename", pText, 128},
	{`firstname_en`, "firstname_en", pText, 128},
	{`lastname_en`, "lastname_en", pText, 128},
	{`"Nickname"`, "Nickname", pText, 128},
	{`"Tel"`, "Tel", pText, 32},
	{`"ID_Card"`, "ID_Card", pText, 32},
	{`identification_number`, "identification_number", pText, 32},
	{`passport_no`, "passport_no", pText, 32},
	{`"Birthday"`, "Birthday", pDate, 0},
	{`"Company"`, "Company", pText, 255},
	{`allergy`, "allergy", pText, 0},
	{`food_allergy`, "food_allergy", pText, 0},
	{`congenital_disease`, "congenital_disease", pText, 0},
	{`gender`, "gender", pGender, 16},
	{`blood`, "blood", pText, 8},
	{`email`, "email", pText, 255},
	{`contact_name`, "contact_name", pText, 255},
	{`contact_phone`, "contact_phone", pText, 32},
	{`contact_name_2`, "contact_name_2", pText, 255},
	{`contact_phone_2`, "contact_phone_2", pText, 32},
	{`patient_insure`, "patient_insure", pText, 64},
	{`career`, "career", pText, 128},
	{`religion`, "religion", pText, 64},
	{`patient_race`, "patient_race", pText, 64},
	{`patient_nationality`, "patient_nationality", pText, 64},
	{`patient_vip_level`, "patient_vip_level", pText, 16},
	{`patient_status`, "patient_status", pText, 32},
	{`patient_star`, "patient_star", pText, 16},
	{`patient_source`, "patient_source", pText, 64},
	{`patient_source_other`, "patient_source_other", pText, 255},
	{`patient_source_promotion`, "patient_source_promotion", pText, 255},
	{`patient_source_know_us`, "patient_source_know_us", pText, 255},
	{`"Braces_ID"`, "Braces_ID", pText, 64},
	{`"Braces_ID_int"`, "Braces_ID_int", pInt, 0},
	{`old_hn`, "old_hn", pText, 64},
	{`old_hn_int`, "old_hn_int", pInt, 0},
	// branch_code is not in this table-driven list because it needs translating rather
	// than copying: the source value is the LEGACY branch code and V4 owns its own.
	// buildPatient resolves it through Input.BranchMap and writes it as a fixed column.
	// (migration 0320 made patient.branch_code NOT NULL with a non-blank CHECK.)
	{`alert_right_treatment`, "alert_right_treatment", pText, 0},
	{`hospital`, "hospital", pText, 255},
	{`detail_other`, "detail_other", pText, 0},
	{`weight`, "weight", pNum, 16},
	{`height`, "height", pNum, 16},
	{`history_pregnant`, "history_pregnant", pText, 0},
	{`history_surgery`, "history_surgery", pText, 0},
	{`history_surgery_date`, "history_surgery_date", pText, 32},
	{`message_group`, "message_group", pText, 64},
	{`note`, "note", pText, 0},
	{`patient_medicine_always`, "patient_medicine_always", pText, 0},
	{`user_insert`, "user_insert", pUserRef, 0},
	{`rank_id`, "rank_id", pText, 64},
	{`profile_picture`, "profile_picture", pText, 500},
	{`active`, "active", pText, 8},
	{`"Created_at"`, "Created_at", pTS, 0},
	// acquired_at = first-touch timestamp; migration 0172 documents seeding it from
	// "Created_at" (cheap now, expensive to backfill later).
	{`acquired_at`, "Created_at", pTS, 0},
}

// buildPatient maps the legacy `patient` table onto V4 `public.patient`.
//
// Idempotent through migrate_source_ref = the legacy HN, upserted on the partial unique
// index uq_patient_migrate_src (clinic_code, migrate_source_ref) WHERE
// migrate_source_ref IS NOT NULL (migration 0174) — the predicate is repeated in
// ON CONFLICT so Postgres can infer it.
//
// RLS: patient has row-level security (migration 0040) whose policy passes when the
// app.clinic_code GUC is unset OR equals the row's clinic_code. The importer runs as
// the table owner without setting that GUC, so the policy passes.
//
// srcCols is the column list the parser actually read. It is only used to explain the
// one failure that looks like data but is not: see the refusal at the end.
func buildPatient(srcCols []string, rows []legacydump.Row, in Input, rep *Report) (string, error) {
	e := rep.entity(EntPatient)
	e.SourceRows = len(rows)
	tr := truncations{}
	br := newBranchResolver(in)
	defer br.report(rep, "patient")

	cols := make([]string, 0, len(patientMapping)+4)
	cols = append(cols, "clinic_code", "migrate_source_ref", "branch_code")
	for _, c := range patientMapping {
		cols = append(cols, c.target)
	}
	cols = append(cols, "updated_at")

	var s sqlBuilder
	seen := map[string]bool{}
	badDates, badInts := 0, 0
	noHN := 0
	// Lookup codes that no map row explains, per target column, with a count each. Reported
	// by CODE rather than as a total: "3 unresolved" tells the operator nothing, while
	// "Prefixname=10 (3 rows)" is something they can check against their own _prefix table.
	unknownCodes := map[string]map[string]int{}
	// What the translation actually produced. Printed because these two columns read as a
	// code on screen when this mapping is wrong, and the numbers are how anyone sees at a
	// glance that it worked.
	prefixOut := map[string]int{}
	genderOut := map[string]int{}

	for n, r := range rows {
		hn := r.Get("HN")
		if hn == "" {
			// Skipped, not fatal. HN is this record's whole identity in V4 — it is the
			// dedup key here, what appointments join on, and what the clinic calls the
			// patient by — so a row without one cannot be written as anything. But one
			// unusable row is no reason to reject the other thousands, which is what
			// refusing here used to do. Counted, and reported below.
			noHN++
			e.skip("HN ว่าง (ข้ามแถวนั้น — ไม่มี HN คือไม่มีตัวระบุคนไข้)")
			continue
		}
		// (clinic_code, HN) must be unique WITHIN a file: a duplicate HN in the same
		// dump means two records claim one identity, which the importer must not merge
		// silently. A duplicate against a PRIOR run of the same clinic is different —
		// ON CONFLICT DO NOTHING below skips it so a re-run is idempotent.
		if seen[hn] {
			return "", fmt.Errorf("patient row %d: duplicate HN %q in the dump — (clinic_code, HN) must be unique", n+1, hn)
		}
		seen[hn] = true

		vals := make([]string, 0, len(cols))
		vals = append(vals, lit(in.ClinicCode), lit(hn), lit(br.resolve(r.Get("branch_code"))))
		for _, c := range patientMapping {
			v, ok := patientValue(c, r, tr)
			if !ok {
				switch c.kind {
				case pDate, pTS:
					badDates++
				case pPrefix, pGender:
					if unknownCodes[c.target] == nil {
						unknownCodes[c.target] = map[string]int{}
					}
					unknownCodes[c.target][r.Get(c.source)]++
				default:
					badInts++
				}
			}
			switch c.kind {
			case pPrefix:
				prefixOut[v]++
			case pGender:
				genderOut[v]++
			}
			vals = append(vals, v)
		}
		vals = append(vals, "now()")

		// Guarded on (clinic_code, HN) — the patient's real identity — rather than on
		// migrate_source_ref. A target can already hold this patient from an earlier
		// import that left migrate_source_ref NULL; keying on the ref would miss that
		// row and then violate the unique index on HN. Guarding on HN is idempotent
		// whatever the earlier import did.
		s.addf(`INSERT INTO patient (%s)
SELECT %s
WHERE NOT EXISTS (
    SELECT 1 FROM patient p WHERE p.clinic_code = %s AND p."HN" = %s
);

`, joinCols(cols), joinCols(vals), lit(in.ClinicCode), lit(hn))
	}

	// REFUSAL: rows exist but not one of them had an HN.
	//
	// That is not data with gaps, it is the wrong column. Row.Get is an exact map lookup, so
	// a file whose column is `hn` or `HN_code` reads as empty on EVERY row — and with the
	// skip above, the run would then "succeed" having written no patients at all. A silent
	// zero is the worst outcome available: the operator moves on believing the clinic is
	// migrated. So this stops, and names what the parser actually saw, because the fix is to
	// correct the column name and nothing in a generic message would point there.
	if noHN > 0 && noHN == len(rows) {
		return "", fmt.Errorf("patient: ไม่มีแถวไหนมี HN เลย (%d แถว) — ไฟล์นี้น่าจะใช้ชื่อคอลัมน์อื่น "+
			"ตัวนำเข้าต้องการคอลัมน์ชื่อ `HN` ตรงตัวพิมพ์%s · คอลัมน์ที่อ่านได้จากไฟล์: %s",
			len(rows), hnCandidates(srcCols), columnList(srcCols, 40))
	}
	if noHN > 0 {
		rep.warnf("patient: ข้าม %d แถวเพราะ HN ว่าง (จาก %d แถว) — แถวที่เหลือนำเข้าครบ",
			noHN, len(rows))
	}

	e.Statements = s.stmts
	if sum := tr.summary(); sum != "" {
		rep.warnf("patient: truncated %s", sum)
	}
	if badDates > 0 {
		rep.warnf("patient: %d unparseable date/time value(s) imported as NULL", badDates)
	}
	if badInts > 0 {
		rep.warnf("patient: %d unparseable number(s) imported as NULL", badInts)
	}

	// The translated result, stated so it is checkable without opening the database.
	rep.notef("patient: คำนำหน้าแปลจากรหัส V2 (_prefix) เป็นคำ — %s", literalTally(prefixOut))
	rep.notef("patient: เพศแปลจากรหัส V2 (_gender) เป็นคำ — %s", literalTally(genderOut))
	for target, codes := range unknownCodes {
		rep.warnf("patient: %s มีรหัสที่ _prefix/_gender ของ V2 ไม่ได้นิยามไว้ — %s · "+
			"แถวเหล่านั้นปล่อยว่างไว้ ไม่เดาคำแทน (เดาแล้วจะมีคำที่ไม่มีใครเขียนอยู่ในเวชระเบียน)",
			target, codeTally(codes))
	}
	return s.String(), nil
}

// literalTally prints a "value=count" summary of SQL literals, NULL last. Sorted so two runs
// over one file read the same.
func literalTally(m map[string]int) string {
	if len(m) == 0 {
		return "(ไม่มีแถว)"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if (keys[i] == "NULL") != (keys[j] == "NULL") {
			return keys[j] == "NULL"
		}
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		label := strings.Trim(k, "'")
		if k == "NULL" {
			label = "(ว่าง)"
		}
		parts = append(parts, fmt.Sprintf("%s %d", label, m[k]))
	}
	return strings.Join(parts, " · ")
}

// codeTally prints the unresolved codes with their counts, sorted by code.
func codeTally(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%q %d แถว", k, m[k]))
	}
	return strings.Join(parts, " · ")
}

// hnCandidates names the columns that differ from "HN" only by case or by carrying it as a
// word, which is what a mistyped export actually looks like. Offered first because reading
// the answer out of a 60-column list is work the message can do instead.
func hnCandidates(cols []string) string {
	var near []string
	for _, c := range cols {
		if c == "HN" {
			continue
		}
		if strings.EqualFold(c, "HN") || strings.Contains(strings.ToLower(c), "hn") {
			near = append(near, "`"+c+"`")
		}
	}
	if len(near) == 0 {
		return ""
	}
	return " · ที่ใกล้เคียงในไฟล์นี้: " + strings.Join(near, ", ")
}

// columnList prints the source columns, capped: the point is to let the operator recognise
// the right name, and a 60-item wall of text works against that.
func columnList(cols []string, max int) string {
	if len(cols) == 0 {
		return "(อ่านคอลัมน์ไม่ได้เลย)"
	}
	shown := cols
	tail := ""
	if len(cols) > max {
		shown = cols[:max]
		tail = fmt.Sprintf(" … และอีก %d คอลัมน์", len(cols)-max)
	}
	return strings.Join(shown, ", ") + tail
}

// isLegacyLookupCode reports whether a value LOOKS like one of V2's 2-digit reference codes
// ("01".."99"). It is what separates "a code we cannot resolve" — worth reporting — from
// "someone typed a name into this column", which is data to keep verbatim.
func isLegacyLookupCode(s string) bool {
	if len(s) != 2 {
		return false
	}
	return s[0] >= '0' && s[0] <= '9' && s[1] >= '0' && s[1] <= '9'
}

func patientValue(c patientCol, r legacydump.Row, tr truncations) (string, bool) {
	raw, present := r[c.source]
	if !present || raw == nil {
		return "NULL", true
	}
	s := r.Get(c.source)
	if s == "" {
		return "NULL", true
	}
	switch c.kind {
	case pDate:
		return dateLit(s)
	case pTS:
		return tsLit(s, "NULL"), true
	case pInt:
		return intLit(s)
	case pNum:
		return numLit(s)
	case pUserRef:
		// A non-numeric or zero link is no link. NULL rather than a guess: user_insert says
		// who registered the patient, and naming the wrong person is worse than naming none.
		legacyID, ok := intLit(s)
		if !ok || legacyID == "NULL" || legacyID == "0" {
			// ok is passed through so a non-numeric value is still counted as unparseable,
			// while a plain 0 is not — it is a legitimate "nobody recorded".
			return "NULL", ok
		}
		return userIDByLegacy(legacyID), true
	case pPrefix, pGender:
		table := legacyPrefixName
		if c.kind == pGender {
			table = legacyGenderName
		}
		// A value already stored as the WORD passes through. Not every V2 row holds a code:
		// the same column carries free text in some records (the reference database has
		// 'สรรเพชญ', 'พศิกา', 'กานัตถ' sitting in Prefixname), and rewriting those to blank
		// would delete real data to satisfy a lookup.
		if _, isCode := table[s]; !isCode {
			if isLegacyLookupCode(s) {
				// A 2-digit code this map does not know. Blank, and the caller counts it —
				// inventing a title for an unknown code puts a word in a medical record
				// that nobody wrote.
				return "NULL", false
			}
			v, cut := clip(s, c.maxLen)
			tr.note(c.target, cut)
			return lit(v), true
		}
		word := table[s]
		if word == "" {
			// Mapped ON PURPOSE to nothing (prefix 06 = "อื่นๆ"). Distinct from the unknown
			// case above: this is the decision, so it is not counted as a failure.
			return "NULL", true
		}
		v, cut := clip(word, c.maxLen)
		tr.note(c.target, cut)
		return lit(v), true
	default:
		v, cut := clip(s, c.maxLen)
		tr.note(c.target, cut)
		return lit(v), true
	}
}
