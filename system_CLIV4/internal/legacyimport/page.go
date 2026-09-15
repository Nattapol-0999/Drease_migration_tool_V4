package legacyimport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"system_cliv4/internal/mongodump"
)

// Patient page images: the legacy `mris.page` collection onto V4 `public.photos`.
//
// WHY THIS ENTITY PRODUCES NO SQL
//
// Every other entity here renders INSERT statements. This one renders a MANIFEST
// instead, because one column cannot be known from the dump: photos.file_size is the
// byte count of the actual image, and the images arrive separately as a tar of the
// legacy public/ folder. The dump names the file; only the caller holding the tar can
// weigh it.
//
// So the split is: this file decides EVERYTHING about the mapping — which revision
// wins, which HN owns the image, what taken_at/doctor_code/area_tag become, where the
// bytes must land — and the caller does exactly two things it cannot get wrong: read
// the size off the file it is copying, and fill those values into a row. No mapping
// decision leaves this package.
//
// WHAT THE SOURCE LOOKS LIKE
//
//	{ _id, hn: "016900066",
//	  page: [ { page_id, page: "001", an, vn, vd_first, vd_last, docgroup,
//	            doctype: "OPD01004", clinic, doctor: "4764", ward, ed, dd,
//	            hyperlink, parent_page_id, bookmark,
//	            rev: [ { rev_id, url: "2026/<dir>/<file>.jpg", created_at, user } ],
//	            anno: [] } ] }
//
// One document per PATIENT, holding an array of pages, each holding an array of
// revisions. A revision is a re-scan/re-save of the same page, so the revisions of one
// page are versions of one thing, not separate images.
//
// DECISIONS TAKEN BY THE CLINIC OWNER (7 Sep 2026), recorded because each one loses or
// keeps real patient data:
//
//   - LATEST REVISION ONLY. Older revisions are neither copied nor recorded. `photos`
//     has no column that could mark one row as "the current version of that page", so
//     importing every revision would show the doctor several near-identical images with
//     nothing to say which one is current. The count of skipped revisions is reported so
//     the loss is visible rather than silent.
//   - consent_given = true on every row.
//   - No schema change, so no migrate_source_ref column to dedup on.
//
// DECISIONS TAKEN HERE (implementation, reversible):
//
//   - visit_id is NO LONGER left NULL. It used to be, on the grounds that SaveMedia (PUT
//     /v3/charts/:hn/:vn/media) does `DELETE FROM photos WHERE clinic_code = ? AND
//     visit_id = ?` — a hard delete — so a bound row would be destroyed the first time a
//     doctor saved media on that encounter.
//
//     That reasoning was right about the danger and wrong about the outcome: the
//     before/after modal reads media PER VISIT, so a page with visit_id NULL can never
//     appear in it at all. Measured on a real migrated clinic: 435 of 437 imported rows
//     had visit_id NULL and the modal was empty. The pages are therefore bound to a visit
//     by the caller (see the web tool's buildInsertSQL), and v4 migration 0420 adds
//     photos.legacy_import so DeleteMediaByVisit skips them instead. Binding without
//     that column deployed WILL destroy the scans — the two changes ship together.
//   - kind comes from the area_tag family: before → before, after → after, everything
//     else → note. The column is CHECK (before|after|note). A scanned OPD form is
//     neither a before nor an after photo, and calling it one would file a lab result in
//     the "ก่อนทำ" slot; but V2's own before/after pages ARE those things and belong in
//     the slots, which is why they are no longer flattened to note.
//   - doctype "trash" is NOT imported (owner's decision, 15 Sep 2026). V2 already threw
//     those pages away; carrying them over refills a bin the clinic emptied. The count is
//     reported so the omission is visible rather than silent.
//   - The on-disk filename is a hash of clinic+rev_id, not a random UUID and not the
//     rev_id itself. Random would break this package's determinism promise (the same
//     dump must produce a byte-identical artifact, so two runs can be diffed); the raw
//     rev_id was declined by the owner. A hash is deterministic, opaque, and collision-
//     free for distinct revisions.

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
	// TakenAt is rev.created_at as RFC3339 UTC. It is half of the duplicate key the
	// caller uses (with file_size), because no column exists to record the source ref.
	TakenAt string `json:"taken_at"`
	// DoctorCode is page.doctor. The legacy numeric code is KEPT by the doctor import
	// (see doctor.go), so this value resolves at the target as-is.
	DoctorCode string `json:"doctor_code,omitempty"`
	// CreatedBy is rev.user. users.id is preserved by the users import
	// (KeepLegacyUserIDs), so the legacy number identifies the same person at the
	// target — but only when that import ran for this clinic. Empty when the value is
	// not a number.
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
	// Note carries the page identity that photos has no columns for: page number,
	// docgroup, and the visit dates. Written for a human reading the record, not for
	// this tool to parse back.
	Note string `json:"note,omitempty"`
}

// PhotoManifest is the whole copy plan for one clinic.
type PhotoManifest struct {
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
	switch areaTag {
	case "before", "after":
		return areaTag
	default:
		return "note"
	}
}

// buildPhotos reads the `page` collection and returns the copy plan. It renders no SQL:
// see the package comment above for why.
func buildPhotos(body []byte, in Input, rep *Report) (*PhotoManifest, error) {
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
		ClinicCode:         in.ClinicCode,
		UploadsSubdir:      path.Join(in.ClinicCode, photoCategory),
		UploadsThumbSubdir: path.Join(in.ClinicCode, photoThumbCategory),
	}

	// dedupe within the manifest itself. Two revisions cannot share a rev_id, but a
	// malformed export could repeat a document, and copying the same bytes twice would
	// produce two rows for one image.
	seenTarget := map[string]bool{}
	// The caller's duplicate key at the target is (hn, taken_at, file_size). If two
	// DIFFERENT images in this very manifest share hn+taken_at, that key can only
	// separate them by size — worth saying out loud rather than discovering later.
	sameInstant := map[string][]string{}

	pages, revs, skippedRevs := 0, 0, 0
	// Pages V2 had already deleted. Counted rather than dropped in silence: "437 files in
	// the archive, 404 rows imported" has to have a stated reason.
	trashed := 0
	// How the imported pages landed across photos.kind. The before/after modal only fills
	// its two slots from before/after, so this number is what says how much of the record
	// will be reachable there.
	kinds := map[string]int{}

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

			target := path.Join(man.UploadsSubdir, photoFileName(in.ClinicCode, revID)+ext)
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
			kinds[kind]++

			note, cut := clip(photoNote(pd), 0)
			tr.note("note", cut)

			takenAt := latestAt.UTC().Format(time.RFC3339Nano)
			key := hn + "|" + takenAt
			sameInstant[key] = append(sameInstant[key], url)

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
				SourcePath: path.Join(nasFolder, legacyPageSubdir, url),
				SourceURL:  url,
				TargetPath: target,
				// Same `url`, the other tree, and the SAME hashed name so the pair is
				// obvious on disk: <hash>.jpg in opd-photo and <hash>.jpg in
				// opd-photo-thumb. Determinism is preserved because the name is still
				// derived from clinic + rev_id.
				ThumbSourcePath: path.Join(nasFolder, legacyThumbSubdir, url),
				ThumbTargetPath: path.Join(man.UploadsThumbSubdir,
					photoFileName(in.ClinicCode, revID)+ext),
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
	for key, urls := range sameInstant {
		if len(urls) > 1 {
			parts := strings.SplitN(key, "|", 2)
			rep.warnf("photos: hn %s มี %d รูปที่ created_at ตรงกันเป๊ะ (%s) — ตัวกันนำเข้าซ้ำแยกได้ด้วยขนาดไฟล์เท่านั้น ถ้าขนาดเท่ากันด้วยจะถูกมองเป็นรูปเดียว",
				parts[0], len(urls), parts[1])
		}
	}
	if len(man.Files) == 0 {
		return nil, fmt.Errorf("ไฟล์ -page ไม่มีรูปที่นำเข้าได้เลย (ไม่มี hn ที่มี rev พร้อม url)")
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
