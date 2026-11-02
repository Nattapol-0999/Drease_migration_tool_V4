package legacyimport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"system_cliv4/internal/mongodump"
)

// Patient page images: mris.page -> V4 photos and per-day encounters.
// The CLI creates a versioned copy manifest and never opens a database. The web
// importer copies bytes, then renders/executes reviewable SQL against V4.
//
// Each page keeps its newest timestamped revision. Examination date comes from
// vd_first, with the Bangkok scan day as an explicit fallback. A stable page identity and full original
// metadata preserve exact doctypes, old VN, revisions and annotation data.
// Before/After and known V2 composites are separate from E1 form/drawing pages.
// Unknown types stay unknown; missing/invalid dates are reported, never invented.
// V4 migrations 0420 and 0798 are required before applying the manifest.

// EntPhoto is the entity name for imported patient page images.
const EntPhoto = "photos"

var photoOrder = []string{EntPhoto}

// legacyPageBase is where the v2 server keeps the files whose paths the collection
// stores. `url` is relative to THIS directory, and the tar an operator exports is
// rooted one level above `nas-doctorease/`, so the path inside the archive is
// nasFolder + "/" + legacyPageSubdir + "/" + url.
const (
	nasFolder        = "nas-doctorease"
	legacyPageSubdir = "mris_images/page"
	// V2 already rendered its own thumbnails, and they sit beside the full images under
	// the SAME relative path — measured on a real export: 81 of 82 page files have a twin
	// at thumb/<year>/<dir>/<file>, 250x250 and about 7 KB against 313 KB for the page.
	//
	// So the thumbnail V4 wants does not have to be generated: it is already in the
	// archive and only has to be carried across. Whether a given twin actually exists is
	// something only the caller can see (it holds the tar), which is why this side emits
	// the path and the caller decides — the same split as FileSize.
	legacyThumbSubdir = "mris_images/thumb"
)

// photoCategory is the storage category the bytes land under on the V4 host. It is the
// same one the camera button in the OPD record uses (storage.RelPath(clinic,
// "opd-photo", ext)), so an imported image is served by the existing endpoint with no
// special case anywhere.
const photoCategory = "opd-photo"

// photoThumbCategory is where the carried-over thumbnail lands. It matches the layout the
// spec of 11 Sep 2026 documents for thumb_path (<clinic>/opd-photo-thumb/<name>) so an
// imported thumbnail is served by the same ?thumb=1 path as one the app rendered itself.
const photoThumbCategory = "opd-photo-thumb"

// PhotoFile is one image to copy, plus every column value the row needs except the
// byte count. The caller reads that off the file and fills FileSize in.
type PhotoFile struct {
	// VisitDate is vd_first or, when unavailable, the Bangkok scan day.
	VisitDate  string `json:"visit_date"`
	SourceType string `json:"source_type"`
	SourceRef  string `json:"source_ref"`
	// Preserve the full legacy page, including exact doctype, VN and annotation data.
	SourceMetadata json.RawMessage `json:"source_metadata"`
	// SourcePath is the path INSIDE the exported tar, e.g.
	// "nas-doctorease/mris_images/page/2026/<dir>/<file>.jpg".
	SourcePath string `json:"source_path"`
	// SourceURL is the raw value from the dump, kept so a mismatch report can name what
	// the collection asked for rather than the path this tool derived.
	SourceURL string `json:"source_url"`
	// TargetPath is the path RELATIVE to the V4 uploads dir:
	// "<clinic>/opd-photo/<hash><ext>". It is also photos.file_path verbatim.
	TargetPath string `json:"target_path"`

	// ThumbSourcePath is where V2's own thumbnail for this image WOULD be inside the tar.
	// ThumbTargetPath is where it would land, and becomes photos.thumb_path.
	//
	// Both are emitted unconditionally: this side cannot open the archive, so it states
	// where to look and the caller reports whether the file was there. A missing twin is
	// not an error — thumb_path is nullable and the app falls back to the full image.
	//
	// The extension is taken from the SOURCE, not forced to .jpg. ServePhoto picks
	// Content-Type from the path, so naming a PNG .jpg would send the wrong header for a
	// file that is otherwise fine.
	ThumbSourcePath string `json:"thumb_source_path"`
	ThumbTargetPath string `json:"thumb_target_path"`

	// ── photos columns ───────────────────────────────────────────────────────
	HN         string `json:"hn"`
	ClinicCode string `json:"clinic_code"`
	// TakenAt is rev.created_at as RFC3339 UTC, independent of VisitDate.
	TakenAt string `json:"taken_at"`
	// DoctorCode is page.doctor. The legacy numeric code is KEPT by the doctor import
	// (see doctor.go), so this value resolves at the target as-is.
	DoctorCode string `json:"doctor_code,omitempty"`
	// CreatedBy stays empty: rev.user is a legacy id, not a V4 account identity.
	// Its original value is retained in SourceMetadata instead.
	CreatedBy string `json:"created_by,omitempty"`
	// AreaTag is page.doctype folded to its FAMILY: every "OPD…" code becomes "OPD",
	// every "BA…" becomes "BA", and "before"/"after" pass through untouched. One real
	// clinic emitted 15 distinct doctype codes (OPD08020, OPD01007, OPD07001, BA2, BA6,
	// …); the digits are a V2 form number that means nothing at the target, and 15 tags
	// no v4 screen has ever heard of is a filter nobody can use. Clipped to the column
	// width. Folding decided by the clinic owner, 15 Sep 2026.
	AreaTag string `json:"area_tag"`
	// Kind is photos.kind — CHECK (before|after|note) — and it decides which slot the
	// per-visit before/after modal drops the image into. Derived from AreaTag, never read
	// from the source: see areaKind.
	Kind string `json:"kind"`
	// Note is a human-readable summary; SourceMetadata preserves the structured identity: page number,
	// docgroup, and the visit dates. Written for a human reading the record, not for
	// this tool to parse back.
	Note string `json:"note,omitempty"`
}

// PhotoManifest is the whole copy plan for one clinic.
type PhotoManifest struct {
	Version    int    `json:"version"`
	ClinicCode string `json:"clinic_code"`
	// UploadsSubdir is the directory every TargetPath sits under, so a caller can
	// create it once.
	UploadsSubdir string `json:"uploads_subdir"`
	// UploadsThumbSubdir is the same for ThumbTargetPath. A separate directory rather than
	// a suffix inside the first one, so a thumbnail can never be mistaken for the image.
	UploadsThumbSubdir string      `json:"uploads_thumb_subdir"`
	Files              []PhotoFile `json:"files"`
}

// legacyTrashDoctype is the doctype V2 stamps on a page the clinic deleted. Those pages are
// skipped, not imported: V2 already threw them away.
const legacyTrashDoctype = "trash"

// areaFamily folds a V2 doctype into the tag that is stored.
//
// "OPD08020", "OPD01007", "OPD07001" … are all the same thing to anyone reading a record —
// a scanned OPD form — and the digits are a V2 form number with no meaning at the target.
// "BA2"/"BA6" are the same story. "before"/"after" are already meaningful and pass straight
// through, as does anything else the export happens to contain (kept verbatim rather than
// bucketed into "other", so an unexpected value is visible instead of disguised).
func areaFamily(doctype string) string {
	t := strings.TrimSpace(doctype)
	switch {
	case strings.HasPrefix(t, "OPD"):
		return "OPD"
	case strings.HasPrefix(t, "BA"):
		return "BA"
	default:
		return t
	}
}

// areaKind maps the stored tag to photos.kind, whose CHECK allows only before|after|note.
//
// Only V2's own "before"/"after" pages become before/after. Everything else is a note: a
// scanned form, a lab result or a consent page is not a before-photo, and putting one in
// that slot would make the modal state something the source never said.
func areaKind(areaTag string) string {
	switch strings.ToLower(areaTag) {
	case "before", "after":
		return strings.ToLower(areaTag)
	default:
		return "note"
	}
}

// visitDateLayout is the one shape V2 writes vd_first in, and the one the target's DATE
// literal is rendered from.
const visitDateLayout = "2006-01-02"

// earliestVisitYear is the floor below which a vd_first is not believed. No clinic record
// predates it, and PostgreSQL refuses year 0000 outright, so letting such a value through
// would cost the whole import rather than one date.
const earliestVisitYear = 1900

// clinicZone is where a scan's calendar day is read. A fixed +07:00 rather than
// time.LoadLocation("Asia/Bangkok"): Thailand keeps no daylight saving, and a fixed zone
// keeps the manifest identical whether or not the machine running the CLI ships tzdata.
var clinicZone = time.FixedZone("Asia/Bangkok", 7*60*60)

// visitDateSource says where a VisitDate came from, so the report can count each kind.
type visitDateSource int

const (
	visitDateFromV2 visitDateSource = iota
	visitDateFromScan
	visitDateUnknown
)

// photoVisitDate decides the day an image is filed under.
//
// page.vd_first first, because it is the visit V2 itself shows the page under, and a form
// scanned a week after the visit belongs to the visit rather than to the day it was fed to
// the scanner (measured on Anjali's V2: 3,522 of 239,812 pages scanned after their วันตรวจ,
// none before). It is accepted only as an exact YYYY-MM-DD naming a real day: anything else
// would either be refused by the DATE column or file a page on a day that never existed.
//
// The scan day is the fallback the owner chose. It is read in clinic time because that is
// the day the clinic lived: 08:00 UTC is the same afternoon, 20:00 UTC is already the next
// morning in Bangkok.
func photoVisitDate(pd mongodump.Doc, scannedAt time.Time) (string, visitDateSource) {
	if v := strings.TrimSpace(pd.Text("vd_first")); v != "" {
		if day, err := examinationDate(v); err == nil {
			return day, visitDateFromV2
		}
	}
	if day := clinicDayOf(scannedAt); day != "" {
		return day, visitDateFromScan
	}
	return "", visitDateUnknown
}

// clinicDayOf is the clinic-time calendar day of an instant, or "" for an instant that is
// not one (a revision with no timestamp leaves the zero time behind).
func clinicDayOf(at time.Time) string {
	if at.IsZero() || at.In(clinicZone).Year() < earliestVisitYear || at.In(clinicZone).Year() > 2200 {
		return ""
	}
	return at.In(clinicZone).Format(visitDateLayout)
}

// examinationDate validates the source day; photoVisitDate applies the scan-day fallback.
func examinationDate(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	for _, layout := range []string{"2006-01-02", "02/01/2006"} {
		if d, err := time.Parse(layout, raw); err == nil && d.Year() >= 1900 && d.Year() <= 2200 {
			return d.Format("2006-01-02"), nil
		}
	}
	return "", fmt.Errorf("วันที่ตรวจ vd_first ไม่ถูกต้อง: %q", raw)
}

func pageSourceType(pd mongodump.Doc, overrides map[string]string) (string, error) {
	doctype := strings.ToLower(strings.TrimSpace(pd.Text("doctype")))
	if doctype == "before" || doctype == "after" {
		return "before_after", nil
	}
	for code, sourceType := range overrides {
		if strings.EqualFold(strings.TrimSpace(code), doctype) {
			switch sourceType {
			case "before_after", "form", "drawing":
				return sourceType, nil
			default:
				return "", fmt.Errorf("page type mapping %q: ประเภท %q ไม่ถูกต้อง", code, sourceType)
			}
		}
	}
	// BeforeAfterController produces these composites; neither side may be invented.
	if strings.TrimSpace(pd.Text("docgroup")) == "29" {
		return "before_after", nil
	}
	switch doctype {
	case "ba2", "ba6", "ba10", "bav":
		return "before_after", nil
	case "drawing", "draw", "freehand":
		return "drawing", nil
	}
	if strings.HasPrefix(strings.ToUpper(doctype), "OPD") {
		return "form", nil
	}
	// Annotation data alone does not identify the originating function: a scanned
	// form or lab result may have ink too. Require a known doctype or explicit mapping.
	return "unknown", nil
}

// buildPhotos reads the `page` collection and returns the copy plan. It renders no SQL:
// see the package comment above for why.
func buildPhotos(body []byte, in Input, rep *Report) (*PhotoManifest, error) {
	normalizedTypes := map[string]string{}
	for code, sourceType := range in.PageTypeMap {
		key := strings.ToLower(strings.TrimSpace(code))
		if key == "" || (sourceType != "before_after" && sourceType != "form" && sourceType != "drawing") {
			return nil, fmt.Errorf("page type mapping %q: ประเภท %q ไม่ถูกต้อง", code, sourceType)
		}
		if previous, ok := normalizedTypes[key]; ok && previous != sourceType {
			return nil, fmt.Errorf("page type mapping %q มีประเภทขัดแย้งกัน", code)
		}
		if (key == "before" || key == "after") && sourceType != "before_after" {
			return nil, fmt.Errorf("ไม่เปลี่ยนประเภท %q ออกจาก Before–After", code)
		}
		normalizedTypes[key] = sourceType
	}
	in.PageTypeMap = normalizedTypes
	cols, err := mongodump.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("page dump: %w", err)
	}
	var docs []mongodump.Doc
	for _, c := range cols {
		if c.Name == "page" {
			docs = append(docs, c.Docs...)
		}
	}
	if len(docs) == 0 {
		var seen []string
		for _, c := range cols {
			seen = append(seen, fmt.Sprintf("%s(%d)", c.Name, len(c.Docs)))
		}
		if len(seen) == 0 {
			return nil, fmt.Errorf("ไฟล์ -page ไม่มี insert ของ collection ใดเลย")
		}
		return nil, fmt.Errorf("ไฟล์ -page ไม่มี collection ชื่อ \"page\" — เจอ: %s", strings.Join(seen, ", "))
	}

	e := rep.entity(EntPhoto)
	e.SourceRows = len(docs)
	tr := truncations{}
	// Counted so the loss is stated once with a number, rather than either hidden or repeated
	// as a warning per photo.
	droppedCreatedBy := 0

	man := &PhotoManifest{
		Version:            2,
		ClinicCode:         in.ClinicCode,
		UploadsSubdir:      path.Join(in.ClinicCode, photoCategory),
		UploadsThumbSubdir: path.Join(in.ClinicCode, photoThumbCategory),
	}

	// What the file NAME is salted with. Defaults to the clinic code, which is what it
	// has always been and what a first import wants. It is overridable only because a
	// clinic can be renamed between two imports, and the name is the sole link from a V2
	// revision to a photos row already at the target — see Input.PhotoNameSalt. Note
	// this changes the NAME only: the bytes still land under in.ClinicCode above, and
	// photos.clinic_code is still in.ClinicCode.
	nameSalt := strings.TrimSpace(in.PhotoNameSalt)
	if nameSalt == "" {
		nameSalt = in.ClinicCode
	}
	if nameSalt != in.ClinicCode {
		rep.notef("photos: ตั้งชื่อไฟล์โดยใช้ salt %q ไม่ใช่ %q — ใช้เมื่อคลินิกเปลี่ยนโค้ดหลังนำเข้ารอบก่อน "+
			"เพื่อให้ชื่อไฟล์ตรงกับแถวที่ V4 มีอยู่แล้ว (ไฟล์ยังลงใต้ %q และ photos.clinic_code ยังเป็น %q)",
			nameSalt, in.ClinicCode, man.UploadsSubdir, in.ClinicCode)
	}

	// dedupe within the manifest itself. Two revisions cannot share a rev_id, but a
	// malformed export could repeat a document, and copying the same bytes twice would
	// produce two rows for one image.
	seenTarget := map[string]bool{}

	pages, revs, skippedRevs := 0, 0, 0
	// Pages V2 had already deleted. Counted rather than dropped in silence: "437 files in
	// the archive, 404 rows imported" has to have a stated reason.
	trashed := 0
	// How the imported pages landed across photos.kind. The before/after modal only fills
	// its two slots from before/after, so this number is what says how much of the record
	// will be reachable there.
	kinds := map[string]int{}
	// Where each VisitDate came from, how many distinct patient-days that makes (the most
	// visits the web tool can open), and the two shapes of vd_first worth a sentence: a page
	// whose visit is dated AFTER its own scan (a form filled in ahead, or a bad date — a
	// Buddhist-era year looks exactly like this) and a page spanning several visit days
	// (filed under the first).
	dateFrom := map[visitDateSource]int{}
	patientDays := map[string]bool{}
	datedAfterScan, spansDays := 0, 0
	afterScanSample := ""

	for n, d := range docs {
		hn := strings.TrimSpace(d.Text("hn"))
		if hn == "" {
			e.skip("document ไม่มี hn (photos.hn เป็น NOT NULL)")
			rep.warnf("page document %d: ไม่มี hn — ข้ามทั้ง document", n+1)
			continue
		}

		pageList, ok := arrayOf(d, "page")
		if !ok || len(pageList) == 0 {
			e.skip("hn ไม่มีหน้าเลย (page[] ว่าง)")
			continue
		}

		for _, pv := range pageList {
			pd, isDoc := pv.(mongodump.Doc)
			if !isDoc {
				e.skip("page[] มีสมาชิกที่ไม่ใช่ document")
				continue
			}
			pages++
			if strings.EqualFold(strings.TrimSpace(pd.Text("doctype")), legacyTrashDoctype) || strings.TrimSpace(pd.Text("docgroup")) == "11" {
				e.skip("V2 ทิ้งหน้านี้แล้ว (doctype=trash)")
				trashed++
				continue
			}
			sourceType, typeErr := pageSourceType(pd, in.PageTypeMap)
			if typeErr != nil {
				return nil, typeErr
			}

			revList, hasRev := arrayOf(pd, "rev")
			if !hasRev || len(revList) == 0 {
				e.skip("หน้าไม่มี rev เลย (ไม่มีไฟล์รูป)")
				continue
			}
			revs += len(revList)

			latest, latestAt, count := latestRevision(revList)
			if latest == nil {
				e.skip("หน้ามี rev แต่ทุกอันไม่มี url")
				continue
			}
			if latestAt.IsZero() {
				e.skip("revision ไม่มีเวลาที่ใช้ตรวจล่าสุดได้")
				rep.warnf("photos: hn %s หน้า %s ไม่มีเวลา revision ที่ใช้ได้ — ต้องตรวจสอบ", hn, pd.Text("page"))
				continue
			}
			visitDate, from := photoVisitDate(pd, latestAt)
			if visitDate == "" {
				e.skip("ไม่มีวันตรวจหรือวันสแกนที่ใช้ได้ — ต้องตรวจสอบ")
				continue
			}
			dateFrom[from]++
			patientDays[hn+"|"+visitDate] = true
			skippedRevs += count - 1

			url := strings.TrimSpace(latest.Text("url"))
			revID := docObjectID(*latest, "rev_id")
			if revID == "" {
				// Without a rev_id there is nothing stable to name the file after, and a
				// random name would break determinism. The url is unique in practice, so
				// it is the fallback identity.
				revID = url
			}

			ext := strings.ToLower(path.Ext(url))
			if ext == "" {
				ext = ".jpg"
			}

			target := path.Join(man.UploadsSubdir, photoFileName(nameSalt, revID)+ext)
			if seenTarget[target] {
				e.skip("rev ซ้ำในไฟล์ต้นทาง (rev_id เดียวกันปรากฏสองครั้ง)")
				continue
			}
			seenTarget[target] = true

			// Pages V2 deleted are dropped here, BEFORE the target path is reserved, so a
			// trashed page cannot claim a filename or count as imported.
			doctype := strings.TrimSpace(pd.Text("doctype"))
			if doctype == legacyTrashDoctype {
				e.skip("V2 ทิ้งหน้านี้แล้ว (doctype=trash)")
				trashed++
				delete(seenTarget, target)
				continue
			}

			areaTag, cut := clip(areaFamily(doctype), 16)
			tr.note("area_tag", cut)
			kind := areaKind(areaTag)
			if kind == "before" || kind == "after" {
				areaTag = kind
			}
			kinds[kind]++
			if sourceType == "unknown" {
				rep.warnf("photos: hn %s doctype %q ไม่ทราบฟังก์ชัน — เก็บในคลังภาพพร้อมประเภท unknown; ใช้ -page-type-map ระบุประเภท", hn, doctype)
			}
			if anno, ok := arrayOf(pd, "anno"); ok && len(anno) > 0 {
				rep.warnf("photos: hn %s หน้า %s เก็บ anno เดิมใน source_metadata; ยังไม่แปลงเป็นเส้นวาด V4", hn, pd.Text("page"))
			}
			sourceRef := docObjectID(pd, "page_id")
			if sourceRef == "" {
				if strings.TrimSpace(pd.Text("page")) == "" {
					e.skip("ไม่มี page_id หรือเลขหน้าให้ระบุตัวตน")
					continue
				}
				sourceRef = "page-key:" + photoFileName(in.ClinicCode, hn+"\x00"+visitDate+"\x00"+doctype+"\x00"+pd.Text("page"))
				rep.warnf("photos: hn %s หน้า %s ไม่มี page_id — ใช้วันที่+doctype+เลขหน้าเป็นตัวระบุ", hn, pd.Text("page"))
			}
			metadataDoc := mongodump.Doc{}
			for k, value := range pd {
				metadataDoc[k] = value
			}
			var legacyPaths []string
			for _, value := range revList {
				if rev, ok := value.(mongodump.Doc); ok {
					u := strings.TrimSpace(rev.Text("url"))
					if u == "" {
						continue
					}
					id := docObjectID(rev, "rev_id")
					if id == "" {
						id = u
					}
					ext := strings.ToLower(path.Ext(u))
					if ext == "" {
						ext = ".jpg"
					}
					legacyPaths = append(legacyPaths, path.Join(man.UploadsSubdir, photoFileName(nameSalt, id)+ext))
				}
			}
			metadataDoc["_legacy_file_paths"] = legacyPaths
			metadataDoc["_visit_date_source"] = "vd_first"
			if from == visitDateFromScan {
				metadataDoc["_visit_date_source"] = "scan"
			}
			metadata, err := json.Marshal(metadataDoc)
			if err != nil {
				return nil, fmt.Errorf("page metadata: %w", err)
			}

			note, cut := clip(photoNote(pd), 0)
			tr.note("note", cut)

			takenAt := latestAt.UTC().Format(time.RFC3339Nano)

			if from == visitDateFromV2 {
				// Both strings are YYYY-MM-DD, so string order is calendar order.
				if scanned := clinicDayOf(latestAt); scanned != "" && visitDate > scanned {
					datedAfterScan++
					if afterScanSample == "" {
						afterScanSample = fmt.Sprintf("hn %s วันตรวจ %s แต่สแกน %s", hn, visitDate, scanned)
					}
				}
				if last := strings.TrimSpace(pd.Text("vd_last")); last != "" && last != visitDate {
					spansDays++
				}
			}

			doctorCode, cut := clip(strings.TrimSpace(pd.Text("doctor")), 32)
			tr.note("doctor_code", cut)

			// created_by is no longer carried. rev.user is a legacy users.id, and since V4
			// issues its own ids that number identifies nobody at the target — it would name
			// whichever account happened to reach it, in whichever clinic. The photo import
			// is a separate run from the SQL one, so it cannot read import_user_map either
			// (a TEMP table living inside the import transaction).
			//
			// Left empty rather than filled with a number that no longer means anything: a
			// blank "who uploaded this" is a gap, a wrong one is a false record.
			createdBy := ""
			if u := strings.TrimSpace(latest.Text("user")); u != "" {
				droppedCreatedBy++
			}

			man.Files = append(man.Files, PhotoFile{
				VisitDate:      visitDate,
				SourceType:     sourceType,
				SourceRef:      "mris.page:" + sourceRef,
				SourceMetadata: metadata,
				SourcePath:     path.Join(nasFolder, legacyPageSubdir, url),
				SourceURL:      url,
				TargetPath:     target,
				// Same `url`, the other tree, and the SAME hashed name so the pair is
				// obvious on disk: <hash>.jpg in opd-photo and <hash>.jpg in
				// opd-photo-thumb. Determinism is preserved because the name is still
				// derived from clinic + rev_id.
				ThumbSourcePath: path.Join(nasFolder, legacyThumbSubdir, url),
				ThumbTargetPath: path.Join(man.UploadsThumbSubdir,
					photoFileName(nameSalt, revID)+ext),
				HN:         hn,
				ClinicCode: in.ClinicCode,
				TakenAt:    takenAt,
				DoctorCode: doctorCode,
				CreatedBy:  createdBy,
				AreaTag:    areaTag,
				Kind:       kind,
				Note:       note,
			})
		}
	}

	// Deterministic order: the manifest is an artifact an operator may diff between two
	// attempts, and map iteration above does not preserve document order.
	sort.Slice(man.Files, func(i, j int) bool {
		if man.Files[i].HN != man.Files[j].HN {
			return man.Files[i].HN < man.Files[j].HN
		}
		return man.Files[i].TargetPath < man.Files[j].TargetPath
	})

	e.Statements = len(man.Files)
	if sum := tr.summary(); sum != "" {
		rep.warnf("photos: truncated %s", sum)
	}
	if droppedCreatedBy > 0 {
		rep.notef("photos: %d รูปมี rev.user ในต้นทางแต่ไม่บันทึก created_by — เลขนั้นคือ users.id ของระบบเดิม "+
			"ซึ่ง V4 ไม่ได้ใช้อีกแล้ว (V4 ออก id เอง) จึงไม่ชี้ตัวใครที่ปลายทาง · ปล่อยว่างดีกว่าใส่เลขที่ชี้คนผิด",
			droppedCreatedBy)
	}

	rep.notef("photos: %d hn · %d หน้า · %d rev ในไฟล์ → นำเข้า %d รูป (rev ล่าสุดของแต่ละหน้า)",
		len(docs), pages, revs, len(man.Files))
	if skippedRevs > 0 {
		rep.warnf("photos: ข้าม %d rev ที่ไม่ใช่อันล่าสุด — ประวัติการแก้ของหน้าเหล่านั้นไม่ถูกนำเข้า (มติเจ้าของคลินิก)", skippedRevs)
	}
	if trashed > 0 {
		rep.notef("photos: ข้าม %d หน้าที่ V2 ทิ้งแล้ว (doctype=trash) — ไม่นำเข้าตามมติเจ้าของคลินิก "+
			"ฉะนั้นจำนวนไฟล์ในถุงจะมากกว่าจำนวนแถวเท่าจำนวนนี้", trashed)
	}
	rep.notef("photos: kind ที่ได้ — before %d · after %d · note %d · "+
		"โมดัลก่อน-หลังเติมช่องได้จาก before/after เท่านั้น ส่วน note อ่านได้จากแถบรูปของแฟ้ม",
		kinds["before"], kinds["after"], kinds["note"])
	rep.notef("photos: วันของ visit — ตามวันตรวจของ V2 (vd_first) %d รูป · ใช้วันที่สแกนตามเวลาไทยแทน %d รูป → %d visit (คนไข้ × วัน)",
		dateFrom[visitDateFromV2], dateFrom[visitDateFromScan], len(patientDays))
	if datedAfterScan > 0 {
		rep.warnf("photos: %d รูปมีวันตรวจหลังวันที่สแกน เช่น %s", datedAfterScan, afterScanSample)
	}
	if spansDays > 0 {
		rep.notef("photos: %d หน้ามีวันตรวจหลายวัน (vd_first ≠ vd_last) — ยื่นไว้ที่วันแรก", spansDays)
	}
	if len(man.Files) == 0 {
		return nil, fmt.Errorf("ไฟล์ -page ไม่มีหน้าที่ผ่านการตรวจวันที่ตรวจ revision และตัวระบุ — ตรวจรายงานก่อนนำเข้า")
	}
	return man, nil
}

// latestRevision picks the revision with the newest created_at among those that carry a
// url, and reports how many carried one at all.
//
// Ties keep the FIRST in file order rather than picking arbitrarily: a stable choice is
// what makes two runs of the same dump agree.
func latestRevision(revs []any) (latest *mongodump.Doc, at time.Time, withURL int) {
	for i := range revs {
		rd, isDoc := revs[i].(mongodump.Doc)
		if !isDoc {
			continue
		}
		if strings.TrimSpace(rd.Text("url")) == "" {
			continue
		}
		withURL++

		ts, ok := docTime(rd, "created_at")
		if !ok {
			// A revision with no usable timestamp cannot win a comparison, but it must
			// still be able to be the only candidate.
			if latest == nil {
				d := rd
				latest, at = &d, time.Time{}
			}
			continue
		}
		if latest == nil || ts.After(at) {
			d := rd
			latest, at = &d, ts
		}
	}
	return latest, at, withURL
}

// photoNote records the page identity that `photos` has no columns for. Kept short and
// readable: a doctor sees this text in the record.
func photoNote(pd mongodump.Doc) string {
	var parts []string
	if p := strings.TrimSpace(pd.Text("page")); p != "" {
		parts = append(parts, "หน้า "+p)
	}
	if g := strings.TrimSpace(pd.Text("docgroup")); g != "" {
		parts = append(parts, "docgroup "+g)
	}
	if v := strings.TrimSpace(pd.Text("vd_first")); v != "" {
		last := strings.TrimSpace(pd.Text("vd_last"))
		if last != "" && last != v {
			parts = append(parts, "วันตรวจ "+v+"–"+last)
		} else {
			parts = append(parts, "วันตรวจ "+v)
		}
	}
	if vn := strings.TrimSpace(pd.Text("vn")); vn != "" {
		parts = append(parts, "VN "+vn)
	}
	if len(parts) == 0 {
		return ""
	}
	return "นำเข้าจากระบบเดิม · " + strings.Join(parts, " · ")
}

// photoFileName derives the on-disk name from clinic + revision id. See the package
// comment for why it is a hash rather than a random UUID or the raw id.
func photoFileName(clinic, revID string) string {
	sum := sha256.Sum256([]byte(clinic + "\x00" + revID))
	return hex.EncodeToString(sum[:16])
}

// arrayOf reads a field that must be an array.
func arrayOf(d mongodump.Doc, field string) ([]any, bool) {
	v, ok := d.Get(field)
	if !ok || v == nil {
		return nil, false
	}
	arr, isArr := v.([]any)
	return arr, isArr
}

// docTime reads a field that must be a date (ISODate / {"$date": …}).
func docTime(d mongodump.Doc, field string) (time.Time, bool) {
	v, ok := d.Get(field)
	if !ok || v == nil {
		return time.Time{}, false
	}
	t, isTime := v.(time.Time)
	return t, isTime
}
