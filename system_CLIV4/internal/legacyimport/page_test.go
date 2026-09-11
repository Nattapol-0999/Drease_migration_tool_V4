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
	if f.AreaTag != "OPD01004" {
		t.Errorf("area_tag = %q, want the legacy doctype", f.AreaTag)
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
