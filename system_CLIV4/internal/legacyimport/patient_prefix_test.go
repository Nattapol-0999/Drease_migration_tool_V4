package legacyimport

// Tests for patient.Prefixname and patient.gender.
//
// V2 stores a 2-digit lookup code in both columns and keeps the words in its own `_prefix`
// (9 rows) and `_gender` (3 rows) tables. V4 has no such tables — the column holds the word.
// Copying the code across is what put "03" in 1,939 patients' title and "02" in 4,026
// patients' sex on the shared test database, so each row of the map is pinned here.
//
// Every expectation below is a decision the clinic owner made on 15 Sep 2026.

import (
	"strings"
	"testing"

	"system_cliv4/internal/legacydump"
)

// patientRow builds the smallest legacy row buildPatient will accept, then overrides fields.
// legacydump.Row is map[string]*string because it has to tell SQL NULL from ” — hence the
// address-of on every value.
func patientRow(hn string, kv map[string]string) legacydump.Row {
	r := legacydump.Row{"HN": &hn}
	for k, v := range kv {
		val := v
		r[k] = &val
	}
	return r
}

func buildPatientOne(t *testing.T, rows []legacydump.Row) (string, *Report) {
	t.Helper()
	rep := NewReport("C9C9611")
	sql, err := buildPatient([]string{"HN", "Prefixname", "gender"}, rows,
		Input{
			ClinicCode:    "C9C9611",
			BranchMap:     map[string]string{"01": "BR00001"},
			DefaultBranch: "BR00001",
		}, rep)
	if err != nil {
		t.Fatalf("buildPatient: %v", err)
	}
	return sql, rep
}

// The whole map, code by code, exactly as `_prefix` holds it with the owner's three
// expansions applied. A table test rather than nine tests: the point is that no row of the
// lookup is missing, and a gap is easiest to see as a gap in a list.
func TestPatientPrefixCodeBecomesTheWord(t *testing.T) {
	cases := []struct {
		code string
		want string // "" means the column must be NULL
		why  string
	}{
		{"01", "นาย", "_prefix 01"},
		{"02", "นาง", "_prefix 02"},
		{"03", "นางสาว", "_prefix says น.ส. — expanded by the owner's decision, and it is what V4's own screens write"},
		{"04", "เด็กชาย", "_prefix says ด.ช. — expanded"},
		{"05", "เด็กหญิง", "_prefix says ด.ญ. — expanded"},
		{"06", "", "_prefix says อื่นๆ — dropped: a title of \"other\" says nothing an empty field does not"},
		{"07", "Mr.", "_prefix 07, English side kept as-is"},
		{"08", "Miss.", "_prefix 08 — V2 spells it Miss., kept verbatim"},
		{"09", "Mrs.", "_prefix 09 — present in _prefix even though it is missing from the screenshot that started this"},
	}

	for _, c := range cases {
		sql, _ := buildPatientOne(t, []legacydump.Row{
			patientRow("HN-"+c.code, map[string]string{"Prefixname": c.code}),
		})
		if c.want == "" {
			// The code must not survive into the row, and no word may be invented for it.
			if strings.Contains(sql, "'"+c.code+"'") {
				t.Errorf("code %s: the raw code reached the SQL (%s)\n%s", c.code, c.why, sql)
			}
			continue
		}
		if !strings.Contains(sql, "'"+c.want+"'") {
			t.Errorf("code %s -> want %q in the SQL (%s)\n%s", c.code, c.want, c.why, sql)
		}
		if strings.Contains(sql, "'"+c.code+"'") {
			t.Errorf("code %s: the raw code is still in the SQL beside the word", c.code)
		}
	}
}

func TestPatientGenderCodeBecomesTheWord(t *testing.T) {
	cases := []struct {
		code string
		want string
		why  string
	}{
		{"01", "ชาย", "_gender 01"},
		{"02", "หญิง", "_gender 02"},
		{"03", "ไม่ระบุ", "_gender says อื่นๆ — the owner chose the target's own wording for an unstated sex"},
	}

	for _, c := range cases {
		sql, _ := buildPatientOne(t, []legacydump.Row{
			patientRow("HN-G-"+c.code, map[string]string{"gender": c.code}),
		})
		if !strings.Contains(sql, "'"+c.want+"'") {
			t.Errorf("gender %s -> want %q (%s)\n%s", c.code, c.want, c.why, sql)
		}
		if strings.Contains(sql, "'"+c.code+"'") {
			t.Errorf("gender %s: the raw code is still in the SQL", c.code)
		}
	}
}

// Free text in these columns is REAL DATA and survives untouched. The reference database has
// 'สรรเพชญ', 'พศิกา' and 'กานัตถ' sitting in Prefixname; rewriting them to blank to satisfy a
// lookup would delete what someone actually typed.
func TestPatientPrefixKeepsFreeTextVerbatim(t *testing.T) {
	for _, v := range []string{"สรรเพชญ", "พศิกา", "ศาสตราจารย์", "คุณ"} {
		sql, rep := buildPatientOne(t, []legacydump.Row{
			patientRow("HN-FT", map[string]string{"Prefixname": v}),
		})
		if !strings.Contains(sql, "'"+v+"'") {
			t.Errorf("free text %q was not kept verbatim\n%s", v, sql)
		}
		for _, w := range rep.Warnings {
			if strings.Contains(w, "ไม่ได้นิยามไว้") {
				t.Errorf("free text %q was reported as an unresolved code; warnings = %v", v, rep.Warnings)
			}
		}
	}
}

// A 2-digit code the map does not define is left blank AND named in the report — never
// guessed. This is the case that separates "we could not resolve it" from "someone typed a
// name here": both are non-map values, only one is worth a warning.
func TestPatientUnknownCodeIsBlankAndReported(t *testing.T) {
	sql, rep := buildPatientOne(t, []legacydump.Row{
		patientRow("HN-U1", map[string]string{"Prefixname": "10", "gender": "07"}),
		patientRow("HN-U2", map[string]string{"Prefixname": "10"}),
	})
	if strings.Contains(sql, "'10'") || strings.Contains(sql, "'07'") {
		t.Errorf("an unknown code reached the SQL\n%s", sql)
	}

	// Matched on the leading `patient: <target> ` rather than on a substring: the warning
	// TEXT names both tables ("_prefix/_gender"), so searching for "gender" anywhere in it
	// also matches the Prefixname warning.
	var prefixWarn, genderWarn string
	for _, w := range rep.Warnings {
		if !strings.Contains(w, "ไม่ได้นิยามไว้") {
			continue
		}
		switch {
		case strings.HasPrefix(w, `patient: "Prefixname" `):
			prefixWarn = w
		case strings.HasPrefix(w, "patient: gender "):
			genderWarn = w
		}
	}
	if prefixWarn == "" {
		t.Fatalf("no warning naming the unresolved Prefixname code; warnings = %v", rep.Warnings)
	}
	// By code and count, not as a bare total: "2 unresolved" is not something an operator
	// can check against their own _prefix table.
	if !strings.Contains(prefixWarn, `"10" 2 แถว`) {
		t.Errorf("the warning should name the code and how many rows carried it: %q", prefixWarn)
	}
	if genderWarn == "" || !strings.Contains(genderWarn, `"07" 1 แถว`) {
		t.Errorf("gender warning missing or not itemised: %q", genderWarn)
	}
}

// Prefix 06 is mapped to nothing ON PURPOSE, so it must NOT be reported as a failure. The
// difference matters: a warning about a decision teaches the operator to ignore warnings.
func TestPatientPrefix06IsNotReportedAsUnresolved(t *testing.T) {
	_, rep := buildPatientOne(t, []legacydump.Row{
		patientRow("HN-06", map[string]string{"Prefixname": "06"}),
	})
	for _, w := range rep.Warnings {
		if strings.Contains(w, "ไม่ได้นิยามไว้") {
			t.Errorf("prefix 06 was reported as unresolved, but blank is the decision; warnings = %v", rep.Warnings)
		}
	}
}

// The result is printed, because these two columns show a code on screen when the mapping is
// wrong and the tally is how anyone sees at a glance that it is right.
func TestPatientReportsTheTranslatedTally(t *testing.T) {
	_, rep := buildPatientOne(t, []legacydump.Row{
		patientRow("HN-T1", map[string]string{"Prefixname": "01", "gender": "01"}),
		patientRow("HN-T2", map[string]string{"Prefixname": "03", "gender": "02"}),
		patientRow("HN-T3", map[string]string{"Prefixname": "03", "gender": "02"}),
		patientRow("HN-T4", map[string]string{"Prefixname": "06"}),
	})

	var prefixNote, genderNote string
	for _, n := range rep.Notes {
		if strings.Contains(n, "คำนำหน้าแปลจากรหัส") {
			prefixNote = n
		}
		if strings.Contains(n, "เพศแปลจากรหัส") {
			genderNote = n
		}
	}
	if prefixNote == "" || genderNote == "" {
		t.Fatalf("missing the translation tally notes; notes = %v", rep.Notes)
	}
	// Most common first, blanks last.
	if !strings.Contains(prefixNote, "นางสาว 2") || !strings.Contains(prefixNote, "นาย 1") {
		t.Errorf("prefix tally is wrong: %q", prefixNote)
	}
	if !strings.Contains(prefixNote, "(ว่าง) 1") {
		t.Errorf("prefix tally should count the blank row (06 and the missing gender): %q", prefixNote)
	}
	if !strings.Contains(genderNote, "หญิง 2") || !strings.Contains(genderNote, "ชาย 1") {
		t.Errorf("gender tally is wrong: %q", genderNote)
	}
}

// An empty source column stays empty. It is not an unresolved code and must not be warned
// about: most V2 rows simply never had a title recorded.
func TestPatientEmptyPrefixStaysEmptyAndSilent(t *testing.T) {
	_, rep := buildPatientOne(t, []legacydump.Row{
		patientRow("HN-E", map[string]string{"Prefixname": "", "gender": ""}),
	})
	for _, w := range rep.Warnings {
		if strings.Contains(w, "ไม่ได้นิยามไว้") {
			t.Errorf("an empty value was reported as an unresolved code; warnings = %v", rep.Warnings)
		}
	}
}
