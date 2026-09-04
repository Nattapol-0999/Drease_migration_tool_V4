package legacyimport

import (
	"fmt"

	"system_cliv4/internal/legacydump"
)

type patientKind int

const (
	pText patientKind = iota // varchar/text, clipped at maxLen
	pInt                     // integer
	pDate                    // legacy "DD/MM/YYYY" -> date
	pTS                      // legacy "YYYY-MM-DD HH:MM:SS" (UTC) -> timestamptz
	pNum                     // legacy double -> varchar, 0 -> NULL
)

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
	{`"Prefixname"`, "Prefixname", pText, 32},
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
	{`gender`, "gender", pText, 16},
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
	// branch_code is NOT set from the source: migration 0320 made patient.branch_code
	// NOT NULL with a non-blank CHECK, and the legacy data carries one branch per
	// clinic (BranchCode). It is written as a fixed column in buildPatient instead.
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
	{`user_insert`, "user_insert", pInt, 0},
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
func buildPatient(rows []legacydump.Row, in Input, rep *Report) (string, error) {
	e := rep.entity(EntPatient)
	e.SourceRows = len(rows)
	tr := truncations{}

	cols := make([]string, 0, len(patientMapping)+4)
	cols = append(cols, "clinic_code", "migrate_source_ref", "branch_code")
	for _, c := range patientMapping {
		cols = append(cols, c.target)
	}
	cols = append(cols, "updated_at")

	var s sqlBuilder
	seen := map[string]bool{}
	badDates, badInts := 0, 0

	for n, r := range rows {
		hn := r.Get("HN")
		if hn == "" {
			return "", fmt.Errorf("patient row %d: empty HN (it is the dedup key)", n+1)
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
		vals = append(vals, lit(in.ClinicCode), lit(hn), lit(BranchCode))
		for _, c := range patientMapping {
			v, ok := patientValue(c, r, tr)
			if !ok {
				switch c.kind {
				case pDate, pTS:
					badDates++
				default:
					badInts++
				}
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
	return s.String(), nil
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
	default:
		v, cut := clip(s, c.maxLen)
		tr.note(c.target, cut)
		return lit(v), true
	}
}
