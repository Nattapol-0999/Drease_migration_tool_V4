package legacyimport

import (
	"encoding/json"
	"strings"
	"testing"

	"system_cliv4/internal/mongodump"
)

func TestPageExaminationDateIsIndependentOfRevision(t *testing.T) {
	body := strings.ReplaceAll(realSample, `"vd_first": "2026-07-29"`, `"vd_first": "28/07/2026"`)
	man, _ := buildOne(t, body)
	f := man.Files[0]
	if man.Version != 2 || f.VisitDate != "2026-07-28" || f.TakenAt != "2026-07-29T08:34:38.796Z" {
		t.Fatalf("date/version mapping: %+v", f)
	}
	if f.SourceType != "form" || f.SourceRef != "mris.page:6a69baf5846d48306c9757fb" {
		t.Fatalf("provenance: %+v", f)
	}
	var original map[string]any
	if err := json.Unmarshal(f.SourceMetadata, &original); err != nil {
		t.Fatal(err)
	}
	if original["doctype"] != "OPD01004" || original["vd_first"] != "28/07/2026" {
		t.Fatal(original)
	}
}

func TestPageUsesScanForUnknownExaminationDate(t *testing.T) {
	for _, date := range []string{"", "2026-00-00", "2026-02-30", "not-a-date"} {
		t.Run(date, func(t *testing.T) {
			body := strings.Replace(realSample, `"vd_first": "2026-07-29"`, `"vd_first": "`+date+`"`, 1)
			rep := NewReport("TEST")
			man, err := buildPhotos([]byte(body), Input{ClinicCode: "TEST"}, rep)
			if err != nil || man.Files[0].VisitDate != "2026-07-29" {
				t.Fatalf("scan fallback: %v %v", man, err)
			}
			if len(rep.Notes) == 0 {
				t.Fatal("missing review warning")
			}
		})
	}
}

func TestPageSourceTypesKeepFunctionsSeparate(t *testing.T) {
	for _, tc := range []struct {
		doctype, group, want string
		anno                 []any
	}{
		{"before", "1", "before_after", nil}, {" AFTER ", "1", "before_after", nil},
		{"BA2", "", "before_after", nil}, {"BA6", "", "before_after", nil},
		{"OPD01004", "1", "form", []any{mongodump.Doc{"x": int64(1)}}},
		{"DRAWING", "1", "drawing", nil}, {"custom", "29", "before_after", nil},
		{"custom", "1", "unknown", []any{mongodump.Doc{"x": int64(1)}}},
		{"XRAY9", "1", "unknown", nil},
	} {
		t.Run(tc.doctype+tc.group, func(t *testing.T) {
			got, err := pageSourceType(mongodump.Doc{"doctype": tc.doctype, "docgroup": tc.group, "anno": tc.anno}, nil)
			if err != nil || got != tc.want {
				t.Fatalf("got %q %v, want %q", got, err, tc.want)
			}
		})
	}
	got, err := pageSourceType(mongodump.Doc{"doctype": "CUSTOM"}, map[string]string{"custom": "drawing"})
	if got != "drawing" || err != nil {
		t.Fatalf("override: %q %v", got, err)
	}
	if _, err := pageSourceType(mongodump.Doc{"doctype": "CUSTOM"}, map[string]string{"custom": "guess"}); err == nil {
		t.Fatal("invalid mapping accepted")
	}
}

func TestPagePreservesAnnotationsAndAllPages(t *testing.T) {
	body := strings.Replace(realSample, `"anno": [ ]`, `"anno": [{"view":"old", "points":[1,2]}]`, 1)
	body += strings.ReplaceAll(strings.ReplaceAll(realSample, "6a69baf5846d48306c9757fb", "6a69baf5846d48306c975700"), "6a69bb1ed60ec9308fa19098", "6a69bb1ed60ec9308fa19000")
	man, _ := buildOne(t, body)
	if len(man.Files) != 2 {
		t.Fatalf("same patient/date/time should retain 2 pages: %d", len(man.Files))
	}
	var hasAnno bool
	for _, f := range man.Files {
		if strings.Contains(string(f.SourceMetadata), `"points":[1,2]`) {
			hasAnno = true
		}
	}
	if !hasAnno {
		t.Fatal("original annotation metadata lost")
	}
}

func TestPageSkipsTrashDocgroup(t *testing.T) {
	body := strings.Replace(realSample, `"docgroup": "1"`, `"docgroup": "11"`, 1)
	if _, err := buildPhotos([]byte(body), Input{ClinicCode: "TEST"}, NewReport("TEST")); err == nil {
		t.Fatal("trash docgroup imported")
	}
}

func TestPageRejectsRevisionWithoutTimestamp(t *testing.T) {
	body := strings.ReplaceAll(realSample, `"created_at": ISODate("2026-07-29T08:33:57.787Z"),`, ``)
	body = strings.ReplaceAll(body, `"created_at": ISODate("2026-07-29T08:34:38.796Z"),`, ``)
	if _, err := buildPhotos([]byte(body), Input{ClinicCode: "TEST"}, NewReport("TEST")); err == nil {
		t.Fatal("unorderable revisions imported")
	}
}
