package legacyimport

import (
	"strings"
	"testing"
)

// realSample is the document shape taken from the clinic's own mris.page collection,
// unchanged except for being wrapped in the insert call a mongosh export produces. It
// carries the two things the mapping has to get right: the page is nested inside an
// array on a per-PATIENT document, and it holds TWO revisions 41 seconds apart, so
// "latest revision only" has something real to choose between.
const realSample = `db.page.insert({
    "_id": ObjectId("6a69baf54a1fd3fab955352f"),
    "hn": "016900066",
    "page": [
        {
            "page_id": ObjectId("6a69baf5846d48306c9757fb"),
            "page": "001",
            "an": "",
            "vn": "",
            "vd_first": "2026-07-29",
            "vd_last": "2026-07-29",
            "docgroup": "1",
            "doctype": "OPD01004",
            "clinic": "",
            "doctor": "4764",
            "ward": "",
            "ed": "",
            "dd": "",
            "hyperlink": "",
            "parent_page_id": null,
            "bookmark": Int32("0"),
            "rev": [
                {
                    "rev_id": ObjectId("6a69baf5846d48306c9757fc"),
                    "url": "2026/a6lRS3lrDDQMibHwzsLaYdWmF/DHw4QgCk91Gg9wIx3W9OzmgDR.jpg",
                    "created_at": ISODate("2026-07-29T08:33:57.787Z"),
                    "user": "634"
                },
                {
                    "rev_id": ObjectId("6a69bb1ed60ec9308fa19098"),
                    "url": "2026/a6lRS3lrDDQMibHwzsLaYdWmF/ZuXbiDCQ7UlzlsFCEUzA3YfDs.jpg",
                    "created_at": ISODate("2026-07-29T08:34:38.796Z"),
                    "user": "634"
                }
            ],
            "anno": [ ]
        }
    ]
});`

func buildOne(t *testing.T, body string) (*PhotoManifest, *Report) {
	t.Helper()
	rep := NewReport("C9D4865")
	man, err := buildPhotos([]byte(body), Input{ClinicCode: "C9D4865"}, rep)
	if err != nil {
		t.Fatalf("buildPhotos: %v", err)
	}
	return man, rep
}

// The whole point of the entity: of two revisions of one page, only the newest is
// imported, and it is the newest by created_at rather than by position in the array.
func TestPageTakesLatestRevisionOnly(t *testing.T) {
	man, rep := buildOne(t, realSample)

	if len(man.Files) != 1 {
		t.Fatalf("files = %d, want 1 (one page → one image)", len(man.Files))
	}
	f := man.Files[0]

	if !strings.HasSuffix(f.SourceURL, "ZuXbiDCQ7UlzlsFCEUzA3YfDs.jpg") {
		t.Errorf("picked %q, want the 08:34:38 revision (ZuXbi…), not the 08:33:57 one", f.SourceURL)
	}
	if f.TakenAt != "2026-07-29T08:34:38.796Z" {
		t.Errorf("taken_at = %q, want the latest revision's created_at", f.TakenAt)
	}
	// The skipped revision must be REPORTED, or the data loss is silent.
	var warned bool
	for _, w := range rep.Warnings {
		if strings.Contains(w, "ข้าม 1 rev") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning about the skipped revision; warnings = %v", rep.Warnings)
	}
}

// The choice is by created_at, not by position in the array.
//
// The timestamps are SWAPPED so the newest revision is now the FIRST element. Together
// with TestPageTakesLatestRevisionOnly (where the newest is last and wins), this pins
// the behaviour to the timestamp: neither "first" nor "last" explains both results.
//
// A dump's array order is not a guarantee of anything, and a page re-scanned twice is
// exactly where picking the wrong one puts an out-of-date image in a medical record.
func TestPageLatestRevisionIsByTimestampNotPosition(t *testing.T) {
	const older = `ISODate("2026-07-29T08:33:57.787Z")`
	const newer = `ISODate("2026-07-29T08:34:38.796Z")`
	swapped := strings.Replace(realSample, older, "@@TMP@@", 1)
	swapped = strings.Replace(swapped, newer, older, 1)
	swapped = strings.Replace(swapped, "@@TMP@@", newer, 1)
	if strings.Count(swapped, newer) != 1 || strings.Count(swapped, older) != 1 {
		t.Fatalf("test fixture did not swap cleanly")
	}

	man, _ := buildOne(t, swapped)
	if len(man.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(man.Files))
	}
	// DHw4… is the FIRST element and now carries the newer timestamp, so it must win.
	if !strings.HasSuffix(man.Files[0].SourceURL, "DHw4QgCk91Gg9wIx3W9OzmgDR.jpg") {
		t.Errorf("picked %q, want the first element now that it holds the newest created_at", man.Files[0].SourceURL)
	}
	if man.Files[0].TakenAt != "2026-07-29T08:34:38.796Z" {
		t.Errorf("taken_at = %q, want the newest timestamp", man.Files[0].TakenAt)
	}
}

// Every photos column the caller will write comes from here, so each one is pinned.
func TestPageMapsEveryColumn(t *testing.T) {
	man, _ := buildOne(t, realSample)
	f := man.Files[0]

	if f.HN != "016900066" {
		t.Errorf("hn = %q", f.HN)
	}
	if f.ClinicCode != "C9D4865" {
		t.Errorf("clinic_code = %q", f.ClinicCode)
	}
	// The fixture's doctype is OPD01004 and the stored tag is the FAMILY, not the code.
	// Changed by the clinic owner on 15 Sep 2026: one clinic produced 15 distinct doctype
	// codes, the digits are a V2 form number that means nothing at the target, and 15 tags
	// no v4 screen knows about is a filter nobody can use.
	if f.AreaTag != "OPD" {
		t.Errorf("area_tag = %q, want the doctype family \"OPD\"", f.AreaTag)
	}
	// A scanned OPD form is not a before-photo. kind decides which slot the before/after
	// modal fills, and putting a form there would make the chart claim something the
	// source never said.
	if f.Kind != "note" {
		t.Errorf("kind = %q, want note for a scanned OPD form", f.Kind)
	}
	if f.DoctorCode != "4764" {
		t.Errorf("doctor_code = %q, want page.doctor kept as-is (doctor.go preserves legacy codes)", f.DoctorCode)
	}
	// created_by is deliberately NOT carried, even though rev.user holds 634 in this fixture.
	//
	// It used to be, and it was correct while the users import preserved the legacy users.id.
	// V4 issues its own ids now, so 634 identifies whichever account reached that number, in
	// whichever clinic — and the photo import is a separate run from the SQL one, so it cannot
	// read import_user_map to translate. A blank "who uploaded this" is a gap; a number
	// pointing at a stranger is a false record.
	if f.CreatedBy != "" {
		t.Errorf("created_by = %q, want empty: a legacy users.id no longer identifies anyone at the target",
			f.CreatedBy)
	}
	// The note carries what photos has no columns for.
	for _, want := range []string{"หน้า 001", "docgroup 1", "วันตรวจ 2026-07-29"} {
		if !strings.Contains(f.Note, want) {
			t.Errorf("note %q is missing %q", f.Note, want)
		}
	}
	// vn is empty in this document, so it must not appear as an empty label.
	if strings.Contains(f.Note, "VN") {
		t.Errorf("note %q mentions VN even though the source vn is empty", f.Note)
	}
}

// The source path has to be the path INSIDE the tar the operator exports, and the
// target has to be the relative path v4's storage layer expects.
func TestPagePaths(t *testing.T) {
	man, _ := buildOne(t, realSample)
	f := man.Files[0]

	wantSrc := "nas-doctorease/mris_images/page/2026/a6lRS3lrDDQMibHwzsLaYdWmF/ZuXbiDCQ7UlzlsFCEUzA3YfDs.jpg"
	if f.SourcePath != wantSrc {
		t.Errorf("source_path = %q\n         want %q", f.SourcePath, wantSrc)
	}
	if man.UploadsSubdir != "C9D4865/opd-photo" {
		t.Errorf("uploads_subdir = %q", man.UploadsSubdir)
	}
	if !strings.HasPrefix(f.TargetPath, "C9D4865/opd-photo/") {
		t.Errorf("target_path = %q, want it under the clinic's opd-photo dir", f.TargetPath)
	}
	if !strings.HasSuffix(f.TargetPath, ".jpg") {
		t.Errorf("target_path = %q, want the source extension kept", f.TargetPath)
	}
	// The name must be neither the raw rev_id nor the legacy filename.
	if strings.Contains(f.TargetPath, "6a69bb1ed60ec9308fa19098") {
		t.Errorf("target_path %q leaks the rev_id as the filename", f.TargetPath)
	}
	if strings.Contains(f.TargetPath, "ZuXbi") {
		t.Errorf("target_path %q reuses the legacy filename", f.TargetPath)
	}
}

// The artifact is meant to be diffed between two attempts, so the same dump must
// produce the same manifest — including the generated filename.
func TestPageManifestIsDeterministic(t *testing.T) {
	a, _ := buildOne(t, realSample)
	b, _ := buildOne(t, realSample)
	if a.Files[0].TargetPath != b.Files[0].TargetPath {
		t.Errorf("target_path differs between runs: %q vs %q", a.Files[0].TargetPath, b.Files[0].TargetPath)
	}
}

// A different clinic must not produce the same on-disk name for the same revision:
// the path is clinic-scoped and so is the hash.
func TestPageFileNameIsClinicScoped(t *testing.T) {
	rep := NewReport("OTHER1")
	other, err := buildPhotos([]byte(realSample), Input{ClinicCode: "OTHER1"}, rep)
	if err != nil {
		t.Fatalf("buildPhotos: %v", err)
	}
	mine, _ := buildOne(t, realSample)
	nameOf := func(p string) string { return p[strings.LastIndex(p, "/")+1:] }
	if nameOf(other.Files[0].TargetPath) == nameOf(mine.Files[0].TargetPath) {
		t.Errorf("two clinics produced the same filename %q", nameOf(mine.Files[0].TargetPath))
	}
}

// A document with no hn cannot be written (photos.hn is NOT NULL) and must be reported
// rather than dropped quietly.
func TestPageRefusesDocumentWithoutHN(t *testing.T) {
	body := strings.Replace(realSample, `"hn": "016900066",`, `"hn": "",`, 1)
	rep := NewReport("C9D4865")
	_, err := buildPhotos([]byte(body), Input{ClinicCode: "C9D4865"}, rep)
	if err == nil {
		t.Fatal("expected an error when no importable image remains")
	}
	if n := rep.Entities[EntPhoto].Skipped["document ไม่มี hn (photos.hn เป็น NOT NULL)"]; n != 1 {
		t.Errorf("skip counter for the missing hn = %d, want 1", n)
	}
}

// A page whose revisions carry no url has no image to copy.
func TestPageSkipsPageWithNoUsableRevision(t *testing.T) {
	body := strings.ReplaceAll(realSample, `.jpg"`, `"`)
	body = strings.ReplaceAll(body, `"url": "2026/a6lRS3lrDDQMibHwzsLaYdWmF/DHw4QgCk91Gg9wIx3W9OzmgDR`, `"url": "`)
	body = strings.ReplaceAll(body, `"url": "2026/a6lRS3lrDDQMibHwzsLaYdWmF/ZuXbiDCQ7UlzlsFCEUzA3YfDs`, `"url": "`)
	rep := NewReport("C9D4865")
	if _, err := buildPhotos([]byte(body), Input{ClinicCode: "C9D4865"}, rep); err == nil {
		t.Fatal("expected an error: nothing is importable")
	}
	if n := rep.Entities[EntPhoto].Skipped["หน้ามี rev แต่ทุกอันไม่มี url"]; n != 1 {
		t.Errorf("skip counter = %d, want 1; skipped = %v", n, rep.Entities[EntPhoto].Skipped)
	}
}

// A file that holds some other collection must say so instead of reporting "no images".
func TestPageRejectsWrongCollection(t *testing.T) {
	rep := NewReport("C9D4865")
	_, err := buildPhotos([]byte(`db.patient.insert({"hn":"1"});`), Input{ClinicCode: "C9D4865"}, rep)
	if err == nil {
		t.Fatal("expected an error for a dump with no page collection")
	}
	if !strings.Contains(err.Error(), "patient") {
		t.Errorf("error %q should name the collections it did find", err)
	}
}

// ── area_tag family folding · kind · trash (owner's decisions, 15 Sep 2026) ──────────

// Every doctype the real clinic actually produced, folded to the tag that gets stored and
// the kind that decides the modal slot. The counts in the comments are from that clinic's
// 437 pages, so this table is not hypothetical.
func TestPageFoldsDoctypeIntoFamilyAndDerivesKind(t *testing.T) {
	cases := []struct {
		doctype  string
		wantTag  string
		wantKind string
		why      string
	}{
		{"OPD08020", "OPD", "note", "117 pages — a scanned OPD form, not a before-photo"},
		{"OPD01007", "OPD", "note", "42 pages"},
		{"OPD07001", "OPD", "note", "18 pages"},
		{"OPD01000", "OPD", "note", "12 pages"},
		{"OPD08021", "OPD", "note", "7 pages"},
		{"OPD01004", "OPD", "note", "6 pages"},
		{"OPD01005", "OPD", "note", "5 pages"},
		{"OPD01006", "OPD", "note", "4 pages"},
		{"OPD01008", "OPD", "note", "1 page"},
		{"OPD07003", "OPD", "note", "1 page"},
		{"BA2", "BA", "note", "5 pages — a V2 series number, not a before/after label"},
		{"BA6", "BA", "note", "5 pages"},
		{"before", "before", "before", "124 pages — V2 said before, so it fills the ก่อน slot"},
		{"after", "after", "after", "57 pages"},
	}

	for _, c := range cases {
		body := strings.Replace(realSample, `"doctype": "OPD01004",`,
			`"doctype": "`+c.doctype+`",`, 1)
		if !strings.Contains(body, `"doctype": "`+c.doctype+`"`) {
			t.Fatalf("fixture did not take doctype %q", c.doctype)
		}
		man, _ := buildOne(t, body)
		if len(man.Files) != 1 {
			t.Fatalf("doctype %q: files = %d, want 1", c.doctype, len(man.Files))
		}
		if got := man.Files[0].AreaTag; got != c.wantTag {
			t.Errorf("doctype %q → area_tag %q, want %q (%s)", c.doctype, got, c.wantTag, c.why)
		}
		if got := man.Files[0].Kind; got != c.wantKind {
			t.Errorf("doctype %q → kind %q, want %q (%s)", c.doctype, got, c.wantKind, c.why)
		}
	}
}

// An unfamiliar doctype is kept verbatim rather than bucketed into "other": a value nobody
// planned for should be visible in the record, not disguised as a known one.
func TestPageKeepsUnknownDoctypeVerbatim(t *testing.T) {
	body := strings.Replace(realSample, `"doctype": "OPD01004",`, `"doctype": "XRAY9",`, 1)
	man, _ := buildOne(t, body)
	if got := man.Files[0].AreaTag; got != "XRAY9" {
		t.Errorf("area_tag = %q, want the unknown doctype kept as-is", got)
	}
	if got := man.Files[0].Kind; got != "note" {
		t.Errorf("kind = %q, want note: only V2's own before/after fill the modal slots", got)
	}
}

// A page V2 already deleted is not imported. 33 of the real clinic's 437 pages were trash;
// carrying them over refills a bin the clinic had emptied.
func TestPageSkipsTrashedPages(t *testing.T) {
	body := strings.Replace(realSample, `"doctype": "OPD01004",`, `"doctype": "trash",`, 1)
	rep := NewReport("C9D4865")
	_, err := buildPhotos([]byte(body), Input{ClinicCode: "C9D4865"}, rep)
	if err == nil {
		t.Fatal("expected an error: the only page was trashed, so nothing is importable")
	}
	if n := rep.Entities[EntPhoto].Skipped["V2 ทิ้งหน้านี้แล้ว (doctype=trash)"]; n != 1 {
		t.Errorf("trash skip counter = %d, want 1; skipped = %v", n, rep.Entities[EntPhoto].Skipped)
	}
	// The omission has to be stated with a number, or "437 files but 404 rows" has no
	// explanation anyone can find later.
	var noted bool
	for _, s := range rep.Notes {
		if strings.Contains(s, "V2 ทิ้งแล้ว") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("no note reporting the trashed pages; notes = %v", rep.Notes)
	}
}

// A trashed page must not consume the filename it would have had: the target is reserved
// before the doctype is read, so the skip has to release it. Two pages of the same patient,
// one trashed, prove the survivor still gets its own path.
func TestPageTrashDoesNotStealTheSurvivorsFilename(t *testing.T) {
	const twoPages = `db.page.insert({
    "_id": ObjectId("6a69baf54a1fd3fab955352f"),
    "hn": "016900066",
    "page": [
        {
            "page_id": ObjectId("6a69baf5846d48306c9757fb"),
            "page": "001",
            "doctype": "trash",
            "doctor": "4764",
            "rev": [
                {
                    "rev_id": ObjectId("6a69baf5846d48306c9757fc"),
                    "url": "2026/dir/trashed.jpg",
                    "created_at": ISODate("2026-07-29T08:33:57.787Z"),
                    "user": "634"
                }
            ],
            "anno": [ ]
        },
        {
            "page_id": ObjectId("6a69baf5846d48306c9757fd"),
            "page": "002",
            "doctype": "before",
            "doctor": "4764",
            "rev": [
                {
                    "rev_id": ObjectId("6a69baf5846d48306c9757fe"),
                    "url": "2026/dir/kept.jpg",
                    "created_at": ISODate("2026-07-29T08:34:38.796Z"),
                    "user": "634"
                }
            ],
            "anno": [ ]
        }
    ]
});`

	man, rep := buildOne(t, twoPages)
	if len(man.Files) != 1 {
		t.Fatalf("files = %d, want 1 (the trashed page dropped, the other kept)", len(man.Files))
	}
	f := man.Files[0]
	if f.SourceURL != "2026/dir/kept.jpg" {
		t.Errorf("imported %q, want the non-trashed page", f.SourceURL)
	}
	if f.AreaTag != "before" || f.Kind != "before" {
		t.Errorf("area_tag/kind = %q/%q, want before/before", f.AreaTag, f.Kind)
	}
	if n := rep.Entities[EntPhoto].Skipped["V2 ทิ้งหน้านี้แล้ว (doctype=trash)"]; n != 1 {
		t.Errorf("trash skip counter = %d, want 1", n)
	}
	// Not the "duplicate rev" skip: that would mean the trashed page had held the path.
	if n := rep.Entities[EntPhoto].Skipped["rev ซ้ำในไฟล์ต้นทาง (rev_id เดียวกันปรากฏสองครั้ง)"]; n != 0 {
		t.Errorf("duplicate-target skip fired %d times — the trashed page kept its reservation", n)
	}
}

// The kind breakdown is reported, because it is the only thing that says how much of the
// imported record the before/after modal can actually show.
func TestPageReportsKindBreakdown(t *testing.T) {
	_, rep := buildOne(t, realSample)
	var found string
	for _, s := range rep.Notes {
		if strings.Contains(s, "kind ที่ได้") {
			found = s
		}
	}
	if found == "" {
		t.Fatalf("no note with the kind breakdown; notes = %v", rep.Notes)
	}
	if !strings.Contains(found, "note 1") {
		t.Errorf("note %q should count this fixture's single OPD page as note", found)
	}
}
