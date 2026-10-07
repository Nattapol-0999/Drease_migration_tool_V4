package legacyimport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"system_cliv4/internal/mongodump"
)

// LINE bindings: mris.line_patients → the clinic's own LINE OA group, the canonical
// binding row, and the pointer on patient. When the dump has no line_patients, the
// pairings are rebuilt from mris.pair_logs instead (linebind_pairlogs.go); with neither,
// the group is skipped (build.go).
//
// # WHAT V2 KEEPS
//
// One document per paired patient, and it is the cleanest collection in the export:
// measured on the-gloss, 699 documents with six fields each, every field present in
// every document, 699 distinct line_id and 699 distinct hn — a true one-to-one map.
//
//	hn           the patient, as a V2 HN
//	line_id      the LINE user id (U + 32 hex)
//	active       whether the pairing is still live
//	createdAt    when the customer paired
//	pair_branch  the V2 branch the pairing was taken at
//
// # WHY THREE TABLES AND NOT ONE
//
// A LINE user id is issued BY A CHANNEL. The same person has a different id under
// every OA, and one OA cannot message an id another OA issued — migration 0285 says
// so and spells out the failure it prevents. So a binding is only meaningful when it
// names the OA it was collected under, which is what clinic_lineoa is for and why
// patient_line_binding has a NOT NULL FK to it.
//
//	clinic_lineoa         the OA group. Created EMPTY when the clinic has none, with
//	                      only clinic_code and group_name, so the clinic can fill in
//	                      its real token and LIFF id afterwards in Settings.
//	patient_line_binding  the canonical binding the Customer Portal authorises against.
//	patient.line_id       the same fact denormalised onto the patient row, which is
//	                      what the profile LINE chip and the recall sender read.
//
// Both are written because the live system writes both: of the eight bindings in the
// target, six have a matching patient.line_id carrying the identical group id. Writing
// only one of them would leave the two halves disagreeing.
//
// # WHAT IS NOT WRITTEN
//
// patient_line_link_log. Its rows are the audit trail of a binding CHANGING, and
// migration 0327 treats a row with source='line_register' as evidence that a LINE
// login actually happened. An import is not a login, so inventing that evidence would
// put a false fact in an append-only log.
const (
	// EntLineoaGroup is the OA group row, created only when the clinic has none.
	EntLineoaGroup = "clinic_lineoa"
	// EntPatientLineBinding is the canonical binding the portal reads.
	EntPatientLineBinding = "patient_line_binding"
	// EntPatientLineID is the denormalised pointer on the patient row. Named with the
	// column in it because the entity is NOT the patient table — the patients group
	// writes that, and the two must be legible as different things in the report.
	EntPatientLineID = "patient(line_id)"
)

var linebindOrder = []string{EntLineoaGroup, EntPatientLineBinding, EntPatientLineID}

// lineBind is one V2 pairing, after reading and before rendering.
type lineBind struct {
	hn        string
	lineID    string
	active    bool
	pairedAt  time.Time
	hasPaired bool
	branch    string
	srcID     string // line_patients._id, kept as the binding's verified_ref
}

// legacyOAGroupID is the id of the group this tool creates for a clinic that has none.
//
// Derived from the clinic code with SHA-256 rather than randomly generated, for a
// reason the whole tool depends on: the same dumps must produce a byte-identical .sql
// file so two attempts can be diffed (see the header BuildScript writes). A
// gen_random_uuid() here would make every run differ on a line that matters.
//
// 64 hex characters matches both the column width (varchar(64)) and the shape the app
// itself mints — the groups in the target are 64-hex, and clinic_lineoa.id is app-made
// on purpose ("ฐานข้อมูลไม่สร้างค่านี้ให้" in clinic_lineoa.sql).
func legacyOAGroupID(clinicCode string) string {
	sum := sha256.Sum256([]byte("linebind:" + strings.TrimSpace(clinicCode)))
	return hex.EncodeToString(sum[:])
}

// parseLineBinds reads the collection into the shape the renderer needs, dropping the
// documents that cannot be written and telling the report why.
func parseLineBinds(docs []mongodump.Doc, e *EntityResult) (binds []lineBind, skippedInactive []string) {
	for _, d := range docs {
		hn := strings.TrimSpace(d.Text("hn"))
		lineID := strings.TrimSpace(d.Text("line_id"))

		if hn == "" {
			// patient_line_binding.patient_id is NOT NULL and is resolved from the HN,
			// so a document with no HN names nobody.
			e.skip("document ไม่มี hn — ไม่รู้ว่า LINE นี้เป็นของใคร")
			continue
		}
		if lineID == "" {
			e.skip("document ไม่มี line_id — ไม่มีอะไรให้ผูก")
			continue
		}

		// A V2 "active": false is the customer having UNLINKED. Carrying it over would
		// re-link somebody who asked not to be, and patient_line_binding has no way to
		// say "revoked at an unknown time" without inventing a revoked_at and a
		// revoked_by that ck_plb_revocation_complete requires in pairs.
		//
		// Owner's decision ก, 6 Oct 2026: skip them.
		if SkipInactiveLineBinding && strings.TrimSpace(d.Text("active")) != "true" {
			e.skip("active = false ใน V2 (ลูกค้ายกเลิกการผูกไปแล้ว) — ไม่นำเข้า")
			skippedInactive = append(skippedInactive,
				fmt.Sprintf("%s (%s)", hn, shortLineID(lineID)))
			continue
		}

		b := lineBind{
			hn:     hn,
			lineID: lineID,
			active: true,
			branch: strings.TrimSpace(d.Text("pair_branch")),
			srcID:  docObjectID(d, "_id"),
		}
		// Read through Get, not Text: the scanner keeps an ISODate as a time.Time and
		// the literal is rendered the same way every other timestamp in this tool is.
		if v, ok := d.Get("createdAt"); ok {
			if t, isTime := v.(time.Time); isTime {
				b.pairedAt = t
				b.hasPaired = true
			}
		}
		binds = append(binds, b)
	}
	return binds, skippedInactive
}

// shortLineID shortens a LINE user id for a report line. The full id is personal data
// and a report is read aloud in meetings; eight characters is enough to tell two
// bindings apart while chasing one of them.
func shortLineID(s string) string {
	if len(s) <= 10 {
		return s
	}
	return s[:8] + "…"
}

// buildLineBind renders the three batches. Returned separately rather than
// concatenated because the report counts them separately and because the group row has
// to land before anything points at it — which is the entity order's job, not a
// comment's.
func buildLineBind(mc *mongoCatalog, in Input, rep *Report) (group, binding, pointer string, err error) {
	eGroup := rep.entity(EntLineoaGroup)
	eBind := rep.entity(EntPatientLineBinding)
	ePtr := rep.entity(EntPatientLineID)

	// line_patients first; pair_logs only when it is absent or empty (owner's decision ก,
	// 6 Oct 2026). build.go has already skipped the group when both are empty, so one of
	// the two holds documents here.
	src := lineSource{name: "mris.line_patients", docs: len(mc.linePatients)}
	var binds []lineBind
	var skippedInactive []string
	if len(mc.linePatients) > 0 {
		binds, skippedInactive = parseLineBinds(mc.linePatients, eBind)
		src.unusedLog = len(mc.pairLogs)
	} else {
		src = lineSource{name: "mris.pair_logs", docs: len(mc.pairLogs), fromLog: true}
		binds, skippedInactive = bindsFromPairLogs(mc.pairLogs, eBind)
	}
	eBind.SourceRows = src.docs
	ePtr.SourceRows = src.docs

	if len(binds) == 0 {
		return "", "", "", fmt.Errorf(
			"%s มี %d เอกสาร แต่ไม่มีแถวที่นำเข้าได้เลย "+
				"(ไม่มี hn/line_id หรือยกเลิกการผูกไปแล้วทั้งหมด)", src.name, src.docs)
	}

	// V4 holds one LINE id per patient and one patient per LINE id, per clinic:
	// patient.line_id is a single column, and ux_patient_clinic_line_id,
	// ux_plb_live_patient and ux_plb_live_line_user say the rest. V2 enforces neither, so
	// both conflicts are settled here, by the owner's decisions:
	//
	//	ข   6 Oct 2026  one LINE id on several HNs → keep the latest pairing
	//	1ข  7 Oct 2026  one HN on several LINE ids → keep the latest too, in line_patients
	//	                and pair_logs alike. This used to refuse the whole run.
	//	2ก  7 Oct 2026  "latest" is the pairing's own time on both sides: createdAt in
	//	                line_patients, the newest event of the pair in pair_logs — the
	//	                time verified_at is given
	//	3ข  7 Oct 2026  both settled in ONE pass, newest first (keepLatestPairings)
	//
	// Every pairing left behind is named in the report, so the clinic can re-pair it in V4
	// if it was the right one after all.
	//
	// Measured on 10.0.133.87: 7 LINE ids, each held by exactly 2 HNs; 4 of the 7 pairs
	// were made on the same day. pair_logs cannot settle any of them — both HNs carry a
	// CREATE and neither a DELETE — which is why a rule is needed at all. The pair_logs-only
	// dump behind the 7 Oct decisions (5,135 events, 4,978 live pairs) has 158 shared LINE
	// ids and 65 patients still on 2 or 3 LINE accounts after them; one pass keeps 4,736.
	binds, lost := keepLatestPairings(binds)
	for range lost.repeats {
		eBind.skip("การผูกเดียวกัน (HN และ LINE เดียวกัน) ซ้ำในต้นทาง — นับเป็นการผูกเดียว")
	}
	for range lost.byHN {
		eBind.skip("HN นี้มี LINE อีกบัญชีที่ผูกล่าสุดกว่า — เก็บการผูกล่าสุดของ HN (มติ 7 ต.ค. 2569)")
	}
	for range lost.byLine {
		eBind.skip("LINE นี้ผูกกับ HN อื่นที่ล่าสุดกว่า — เก็บการผูกล่าสุดของ LINE (มติ 6 ต.ค. 2569)")
	}

	// Deterministic order, same reason as loyalty: the artifact is meant to be diffed.
	sort.Slice(binds, func(i, j int) bool { return binds[i].hn < binds[j].hn })

	clinic := lit(in.ClinicCode)
	groupID := legacyOAGroupID(in.ClinicCode)

	group = renderLineoaGroup(in.ClinicCode, groupID, eGroup)
	binding = renderLineBindings(binds, clinic, eBind)
	pointer = renderLinePointer(binds, clinic, ePtr)

	reportLineBind(binds, skippedInactive, src, in, rep)
	reportSharedLineIDs(lost.byLine, src, rep)
	reportSharedHNs(lost.byHN, src, rep)
	return group, binding, pointer, nil
}

// lineSource says which collection the pairings came from, for the report.
type lineSource struct {
	name      string
	docs      int
	fromLog   bool
	unusedLog int // pair_logs documents present but not read, because line_patients was
}

// sharedLineID is one pairing dropped because another HN holds the same LINE id more
// recently.
type sharedLineID struct {
	lineID string
	kept   lineBind
	gone   lineBind
}

// sharedHN is one pairing dropped because the same HN holds another LINE id more recently.
type sharedHN struct {
	hn   string
	kept lineBind
	gone lineBind
}

// lostPairings is what keepLatestPairings left out, by reason.
type lostPairings struct {
	byLine  []sharedLineID // the LINE id went to a more recent pairing of another HN
	byHN    []sharedHN     // the HN kept a more recent pairing with another LINE id
	repeats int            // the same HN + LINE id written again: one pairing, not two
}

// pairedLater reports whether a is the more recent pairing of the two.
//
// A pairing with a time beats one without; between two with times the later wins; and
// when the times are equal or both missing, the document _id decides (an ObjectId's
// leading bytes are its creation second, so its hex text sorts by insertion order). The
// order is total, so two runs of one dump keep the same pairing.
func pairedLater(a, b lineBind) bool {
	if a.hasPaired != b.hasPaired {
		return a.hasPaired
	}
	if a.hasPaired && !a.pairedAt.Equal(b.pairedAt) {
		return a.pairedAt.After(b.pairedAt)
	}
	if a.srcID != b.srcID {
		return a.srcID > b.srcID
	}
	return a.hn > b.hn
}

// newerPairing is pairedLater made total. Two pairings that tie on time, document id and
// HN are one HN on two LINE ids, and the LINE id settles it — so the same dump always
// walks in the same order and keeps the same pairings.
func newerPairing(a, b lineBind) bool {
	if pairedLater(a, b) {
		return true
	}
	if pairedLater(b, a) {
		return false
	}
	return a.lineID > b.lineID
}

// keepLatestPairings walks the pairings from the most recent to the oldest and keeps one
// when neither its HN nor its LINE id is already held by a pairing kept before it
// (owner's decision 3ข, 7 Oct 2026). What is left holds every HN once and every LINE id
// once, which patient.line_id, ux_patient_clinic_line_id, ux_plb_live_patient and
// ux_plb_live_line_user require together.
//
// One pass, not a LINE pass followed by an HN pass. Two passes strand a pairing: when the
// latest holder of a LINE id goes on to keep a newer LINE id of its own, the LINE pass has
// already turned the older holder away, and the id ends up bound to nobody. Measured on
// the 7 Oct dump that is 3 patients — one of them 016701866, whose LINE id 016701865
// paired one minute later and gave up 42 minutes after that for a LINE id of its own.
// Walked newest first, the id is still free when the older pairing is reached, so 016701866
// keeps it.
//
// A pairing that loses on both sides — its HN and its LINE id each held by someone else —
// is reported once, under its HN.
func keepLatestPairings(binds []lineBind) (kept []lineBind, lost lostPairings) {
	order := make([]lineBind, len(binds))
	copy(order, binds)
	sort.SliceStable(order, func(i, j int) bool { return newerPairing(order[i], order[j]) })

	byHN := map[string]lineBind{}
	byLine := map[string]lineBind{}
	for _, b := range order {
		if h, held := byHN[b.hn]; held {
			if h.lineID == b.lineID {
				lost.repeats++
			} else {
				lost.byHN = append(lost.byHN, sharedHN{hn: b.hn, kept: h, gone: b})
			}
			continue
		}
		if l, held := byLine[b.lineID]; held {
			lost.byLine = append(lost.byLine, sharedLineID{lineID: b.lineID, kept: l, gone: b})
			continue
		}
		byHN[b.hn] = b
		byLine[b.lineID] = b
		kept = append(kept, b)
	}
	return kept, lost
}

// pairedWhen is a pairing's time as a report line shows it.
func pairedWhen(b lineBind) string {
	if !b.hasPaired {
		return "ไม่ระบุเวลา"
	}
	return b.pairedAt.UTC().Format("2006-01-02 15:04")
}

// reportSharedHNs names every pairing left out because its HN kept a more recent LINE id.
// A WARNING for the same reason as the shared LINE ids: it decides which LINE account can
// open the patient's record in V4, and the clinic has to be able to see every one of them.
func reportSharedHNs(shared []sharedHN, src lineSource, rep *Report) {
	if len(shared) == 0 {
		return
	}
	sort.Slice(shared, func(i, j int) bool {
		if shared[i].hn != shared[j].hn {
			return shared[i].hn < shared[j].hn
		}
		return newerPairing(shared[i].gone, shared[j].gone)
	})
	patients := map[string]bool{}
	lines := make([]string, 0, len(shared))
	for _, s := range shared {
		patients[s.hn] = true
		lines = append(lines, fmt.Sprintf("HN %s: เก็บ LINE %s (ผูก %s) · ไม่นำเข้า LINE %s (ผูก %s)",
			s.hn, shortLineID(s.kept.lineID), pairedWhen(s.kept),
			shortLineID(s.gone.lineID), pairedWhen(s.gone)))
	}
	rep.warnf("LINE: %d การผูกใน %s เป็น LINE บัญชีที่สองขึ้นไปของคนไข้คนเดียวกัน (%d HN) — "+
		"เก็บการผูกล่าสุดของแต่ละ HN ไว้ตามมติเจ้าของคลินิก (7 ต.ค. 2569) "+
		"เพราะ V4 ให้คนไข้หนึ่งคนผูก LINE ได้บัญชีเดียวต่อคลินิก · "+
		"ถ้าบัญชีที่ไม่ได้นำเข้าเป็นบัญชีที่คนไข้ใช้อยู่จริง ให้คนไข้ผูก LINE ใหม่ใน V4 · %s",
		len(shared), src.name, len(patients), strings.Join(lines, " · "))
}

// reportSharedLineIDs names every pairing left out because its LINE id went to another
// HN. A WARNING: it changes which patient a LINE account belongs to in V4, and the
// clinic has to be able to see every one of those.
func reportSharedLineIDs(shared []sharedLineID, src lineSource, rep *Report) {
	if len(shared) == 0 {
		return
	}
	sort.Slice(shared, func(i, j int) bool {
		if shared[i].kept.hn != shared[j].kept.hn {
			return shared[i].kept.hn < shared[j].kept.hn
		}
		return shared[i].gone.hn < shared[j].gone.hn
	})
	lines := make([]string, 0, len(shared))
	for _, s := range shared {
		lines = append(lines, fmt.Sprintf("LINE %s: เก็บ HN %s (ผูก %s) · ไม่นำเข้า HN %s (ผูก %s)",
			shortLineID(s.lineID), s.kept.hn, pairedWhen(s.kept), s.gone.hn, pairedWhen(s.gone)))
	}
	rep.warnf("LINE: %d การผูกใน %s ใช้ LINE บัญชีเดียวกับ HN อื่น — เก็บการผูกล่าสุดไว้ตามมติเจ้าของคลินิก "+
		"(6 ต.ค. 2569) เพราะ V4 ให้ LINE หนึ่งบัญชีผูกได้คนเดียวต่อคลินิก · "+
		"HN ที่ไม่ได้นำเข้าต้องให้คนไข้ผูก LINE ใหม่ใน V4 ถ้าเป็นเจ้าของบัญชีจริง · %s",
		len(shared), src.name, strings.Join(lines, " · "))
}

// renderLineoaGroup writes the empty OA group, and the guard that refuses to guess.
func renderLineoaGroup(clinicCode, groupID string, e *EntityResult) string {
	var s sqlBuilder
	clinic := lit(clinicCode)

	s.comment("")
	s.comment("LINE OA ของคลินิก — สร้างเปล่าไว้ก่อน ถ้าคลินิกนี้ยังไม่มีกลุ่มเลย")
	s.comment("")
	s.comment("ใส่แค่ clinic_code กับ group_name · ที่เหลือมี default ครบและผ่าน CHECK ทั้งสามตัว")
	s.comment("(audience='customer' · is_active=true · pairing_token_serial=1 ·")
	s.comment(" token_enc_version=0 · secret_enc_version=0 · webhook_token สุ่มให้เอง 64 ตัว)")
	s.comment("")
	s.comment("กลุ่มที่ยังไม่มี token และไม่มีสาขาใน clinic_lineoa_branch ใช้งานจริงไม่ได้อยู่ดี")
	s.comment("(CountLineoaGroupBranches: กลุ่มที่ไม่มีสาขา \"ยังไม่พร้อมใช้งานแม้คอนฟิกครบ\")")
	s.comment("จึงปลอดภัยที่จะสร้างค้างไว้แล้วให้คลินิกไปตั้งค่าเพิ่มที่ Settings → LINE OA ทีหลัง")
	s.comment("")
	s.comment("ชื่อกลุ่มอ่านจาก clinic ที่ปลายทาง ไม่ใช่ค่าที่ฝังมาจากไฟล์ — ตัวนำเข้ารู้แค่ clinic_code")
	s.comment("")

	// id is generated here and not by the database: clinic_lineoa.id is app-minted by
	// design, and deriving it from the clinic code keeps two runs of the same dump
	// byte-identical.
	s.addf(`INSERT INTO clinic_lineoa (id, clinic_code, group_name)
SELECT %s, %s,
       COALESCE(NULLIF(btrim(c.tha_name), ''), NULLIF(btrim(c.eng_name), ''), c.clinic_code) || '_old'
  FROM clinic c
 WHERE c.clinic_code = %s
   AND c.code = c.clinic_code
   AND NOT EXISTS (SELECT 1 FROM clinic_lineoa g WHERE g.clinic_code = %s);

`, lit(groupID), clinic, clinic, clinic)

	// The refusal has to live in the SQL, not in Go: how many groups the clinic has is a
	// fact about the TARGET, and this side has never seen it.
	//
	// Two or more groups means the import cannot name the OA that issued these ids, and
	// patient_line_binding.clinic_lineoa_id is NOT NULL — there is no "unknown" to write.
	// Migration 0285 settled the same question for patient.line_oa_group_id and chose to
	// leave it NULL rather than guess ("ไม่เขียนคำเดาลงไปให้มันดูเหมือนข้อเท็จจริง"); a
	// binding has no such option, so the honest move is to stop and let a human say which
	// group these pairings belong to.
	s.addf(`DO $linebind_group$
DECLARE n bigint;
BEGIN
    SELECT count(*) INTO n FROM clinic_lineoa WHERE clinic_code = %s;
    IF n = 0 THEN
        RAISE EXCEPTION 'ไม่มีกลุ่ม LINE OA ของคลินิก %% และสร้างให้ไม่ได้ '
            'เพราะไม่พบแถวของคลินิกนี้ในตาราง clinic — สร้างคลินิกให้เรียบร้อยก่อน', %s;
    END IF;
    IF n > 1 THEN
        RAISE EXCEPTION 'คลินิก %% มีกลุ่ม LINE OA %% กลุ่ม — ตัวนำเข้าเลือกให้เองไม่ได้ '
            'ว่า LINE id ชุดนี้ออกมาจากกลุ่มไหน (LINE id เป็นค่าต่อ channel) '
            'ให้นำเข้าทีละกลุ่มด้วยมือ หรือรวมกลุ่มที่ปลายทางก่อน', %s, n;
    END IF;
END $linebind_group$;

`, clinic, clinic, clinic)

	e.SourceRows = 1
	e.Statements = s.stmts
	return s.String()
}

// lineoaGroupExpr is the scalar subquery every later statement uses to find the group.
//
// LIMIT 1 is safe because the guard above has already refused anything but exactly one
// group for this clinic; the ORDER BY is there so the expression is still deterministic
// if that guard is ever relaxed.
func lineoaGroupExpr(clinic string) string {
	return fmt.Sprintf(
		"(SELECT g.id FROM clinic_lineoa g WHERE g.clinic_code = %s ORDER BY g.created_at, g.id LIMIT 1)",
		clinic)
}

// renderLineBindings writes the canonical binding rows.
func renderLineBindings(binds []lineBind, clinic string, e *EntityResult) string {
	var s sqlBuilder
	tr := truncations{}
	grp := lineoaGroupExpr(clinic)

	s.comment("")
	s.comment("การผูก LINE ตัวจริงที่ Customer Portal ใช้ตรวจสิทธิ์")
	s.comment("")
	s.comment("id ไม่ต้องใส่ — GENERATED ALWAYS AS IDENTITY")
	s.comment("patient_id อ่านจากปลายทางด้วย HN เหมือน loyalty_account: id เป็นของปลายทาง")
	s.comment("และ uq_patient_clinic_hn ทำให้ค้นได้หนึ่งแถวหรือศูนย์แถว")
	s.comment("verified_at = createdAt ของ V2 (เวลาที่ลูกค้าผูกจริง) ไม่ใช่ now()")
	s.comment("            จาก pair_logs คือเวลาของเหตุการณ์ล่าสุดที่ทำให้คู่นี้ยังผูกอยู่")
	s.comment("verified_ref = _id ของเอกสารต้นทาง เก็บไว้สืบย้อนกลับ")
	s.comment("            line_patients ใส่ _id ตรง ๆ · pair_logs ใส่ %s<_id ของเหตุการณ์ล่าสุด>", pairLogRefPrefix)
	s.comment("verified_method = %s", LineBindVerifiedMethod)
	s.comment("")
	s.comment("NOT EXISTS สองชั้นคือรูปเดียวกับ partial unique index สองตัวของตาราง")
	s.comment("(ux_plb_live_line_user · ux_plb_live_patient ทั้งคู่ WHERE revoked_at IS NULL)")
	s.comment("ที่ migration 0327 ใช้ — การผูกที่ยังใช้งานอยู่ของเดิมชนะเสมอ ไม่ถูกเขียนทับ")
	s.comment("")

	for _, b := range binds {
		lineID, cut := clip(b.lineID, 64)
		tr.note("line_user_id", cut)
		hn, cut := clip(b.hn, 32)
		tr.note("hn", cut)
		ref, cut := clip(b.srcID, 64)
		tr.note("verified_ref", cut)

		// at is the SQL literal; shown is the same instant for the comment above it.
		// Separate values on purpose — printing the literal put quotes in the middle of a
		// sentence an operator reads, and "now()" where V2 had recorded no time at all.
		at, shown := "now()", "ไม่ระบุ"
		if b.hasPaired {
			shown = b.pairedAt.UTC().Format("2006-01-02 15:04:05")
			at = lit(b.pairedAt.UTC().Format("2006-01-02 15:04:05-07"))
		}

		s.comment("hn %s · ผูกเมื่อ %s UTC · สาขาที่ผูกใน V2 %s", b.hn, shown, orDash(b.branch))
		s.addf(`INSERT INTO patient_line_binding
    (clinic_code, clinic_lineoa_id, line_user_id, patient_id,
     verified_method, verified_at, verified_ref)
SELECT %s, %s, %s, p.id, %s, %s, %s
  FROM patient p
 WHERE p.clinic_code = %s
   AND p."HN" = %s
   AND NOT EXISTS (SELECT 1 FROM patient_line_binding b
                    WHERE b.clinic_lineoa_id = %s
                      AND b.line_user_id = %s
                      AND b.revoked_at IS NULL)
   AND NOT EXISTS (SELECT 1 FROM patient_line_binding b
                    WHERE b.clinic_lineoa_id = %s
                      AND b.patient_id = p.id
                      AND b.revoked_at IS NULL);

`, clinic, grp, lit(lineID), lit(LineBindVerifiedMethod), at, nullLit(ref),
			clinic, lit(hn), grp, lit(lineID), grp)
	}

	e.Statements = s.stmts
	return s.String()
}

// renderLinePointer writes the denormalised pointer on patient.
func renderLinePointer(binds []lineBind, clinic string, e *EntityResult) string {
	var s sqlBuilder
	grp := lineoaGroupExpr(clinic)

	s.comment("")
	s.comment("สำเนาเดียวกันบนแถว patient — หน้าโปรไฟล์และตัวส่ง recall อ่านจากที่นี่")
	s.comment("")
	s.comment("line_oa_group_id = clinic_lineoa.id ตัวเดียวกับที่ลงใน patient_line_binding")
	s.comment("ของจริงในระบบก็เก็บตรงกันแบบนี้ (6 จาก 8 binding ที่มีอยู่มีค่าตรงกันทั้งสองที่)")
	s.comment("ไม่มี FOREIGN KEY โดยเจตนา — migration 0285 บอกว่าค่านี้เป็นบันทึกทางประวัติศาสตร์")
	s.comment("ว่า id นี้เก็บมาใต้ OA ใด ถ้ากลุ่มถูกลบทีหลัง ความจริงข้อนั้นไม่เปลี่ยน")
	s.comment("")
	s.comment("เงื่อนไข line_id IS NULL: ไม่เขียนทับการผูกที่ปลายทางมีอยู่แล้ว")
	s.comment("การผูกที่ลูกค้าทำเองใหม่กว่าและจริงกว่าของที่ย้ายมาจาก V2 เสมอ")
	s.comment("")

	for _, b := range binds {
		lineID, _ := clip(b.lineID, 64)
		hn, _ := clip(b.hn, 32)

		at := "now()"
		if b.hasPaired {
			at = lit(b.pairedAt.UTC().Format("2006-01-02 15:04:05-07"))
		}

		// The third condition protects ux_patient_clinic_line_id (clinic_code, line_id)
		// WHERE line_id IS NOT NULL. Without it an id already held by ANOTHER patient at
		// this clinic raises 23505 and takes the whole transaction with it.
		s.addf(`UPDATE patient p
   SET line_id          = %s,
       line_oa_group_id = %s,
       line_linked_at   = %s,
       updated_at       = now()
 WHERE p.clinic_code = %s
   AND p."HN" = %s
   AND p.line_id IS NULL
   AND NOT EXISTS (SELECT 1 FROM patient o
                    WHERE o.clinic_code = %s AND o.line_id = %s);

`, lit(lineID), grp, at, clinic, lit(hn), clinic, lit(lineID))
	}

	// How many of each actually landed. A shortfall is not a failure: it means those HNs
	// have no patient row yet, or already carry a LINE id — and the operator has to be
	// able to see which, because the two need different follow-up.
	s.addf(`DO $linebind_done$
DECLARE
    bound   bigint;
    stamped bigint;
    asked   bigint := %d;
BEGIN
    SELECT count(*) INTO bound
      FROM patient_line_binding
     WHERE clinic_code = %s AND verified_method = %s AND revoked_at IS NULL;
    SELECT count(*) INTO stamped
      FROM patient
     WHERE clinic_code = %s AND line_id IS NOT NULL AND btrim(line_id) <> '';

    RAISE NOTICE 'LINE: นำเข้า %% รายการ · binding ที่ปลายทางมี %% แถว · patient.line_id มีค่า %% แถว',
                 asked, bound, stamped;
    IF bound < asked THEN
        RAISE NOTICE 'LINE: binding น้อยกว่าที่ส่งมา %% แถว — HN เหล่านั้นยังไม่มีในคลินิกนี้ '
            'หรือ LINE id นั้นถูกผูกไว้กับคนอื่นอยู่แล้ว (ของเดิมชนะ) '
            'นำเข้ากลุ่ม patients ให้ครบแล้วรันไฟล์นี้ซ้ำได้ — ทุกคำสั่งเขียนซ้ำไม่เพิ่ม',
            asked - bound;
    END IF;
END $linebind_done$;

`, len(binds), clinic, lit(LineBindVerifiedMethod), clinic)

	e.Statements = s.stmts
	return s.String()
}

// reportLineBind says what was read, what was left out, and the two things about this
// collection an operator has to decide on rather than discover later.
func reportLineBind(binds []lineBind, skippedInactive []string, src lineSource, in Input, rep *Report) {
	if src.fromLog {
		// A WARNING, not a note: these pairings are reconstructed from an event log, not
		// read off a list of pairings, and whoever checks the result has to know that.
		rep.warnf("LINE: ไม่มี mris.line_patients (หรือมีแต่ว่าง) — สร้างการผูกจาก mris.pair_logs แทน "+
			"ตามมติเจ้าของคลินิก (6 ต.ค. 2569: ใช้ line_patients ก่อน ถ้าไม่มีจึงใช้ pair_logs) · "+
			"%d เหตุการณ์ → นำเข้า %d รายการ · แต่ละคู่ (HN + LINE) ตัดสินจากเหตุการณ์ล่าสุด: "+
			"ยังผูกอยู่ถ้าไม่ใช่ DELETE และ active = true · verified_ref ขึ้นต้นด้วย %q",
			src.docs, len(binds), pairLogRefPrefix)
	} else {
		rep.notef("LINE: %s %d เอกสาร → นำเข้า %d รายการ", src.name, src.docs, len(binds))
		if src.unusedLog > 0 {
			rep.notef("LINE: มี mris.pair_logs %d เอกสารด้วย แต่ไม่ได้ใช้ — "+
				"line_patients เป็นแหล่งหลัก pair_logs ใช้เฉพาะเมื่อไม่มี line_patients (มติ 6 ต.ค. 2569)",
				src.unusedLog)
		}
	}

	if n := len(skippedInactive); n > 0 {
		if src.fromLog {
			rep.warnf("LINE: ข้าม %d คู่ที่เหตุการณ์ล่าสุดใน pair_logs คือยกเลิกการผูก "+
				"(DELETE หรือ active = false — ลูกค้ายกเลิกการผูกแล้ว) — %s",
				n, strings.Join(capList(skippedInactive, 20), ", "))
		} else {
			rep.warnf("LINE: ข้าม %d รายการที่ active = false ใน V2 (ลูกค้ายกเลิกการผูกแล้ว) — %s",
				n, strings.Join(capList(skippedInactive, 20), ", "))
		}
	}

	// The branch written in the document is NOT used to place the row: the HN is, and the
	// patient row already carries the branch V4 allocated for it. Reported because the two
	// disagreeing is worth knowing — a pairing taken at one branch for a patient
	// registered at another is a real thing that happens, not an error.
	if in.BranchMap != nil {
		var crossed []string
		for _, b := range binds {
			if b.branch == "" {
				continue
			}
			if want, ok := in.BranchMap[b.branch]; ok && !strings.HasPrefix(b.hn, b.branch) {
				crossed = append(crossed, fmt.Sprintf("%s (ผูกที่ %s → %s)", b.hn, b.branch, want))
			}
		}
		if len(crossed) > 0 {
			rep.notef("LINE: %d รายการที่ pair_branch ไม่ตรงกับสาขาของ HN — "+
				"ยึด HN เป็นหลัก ไม่ย้ายคนไข้ตาม pair_branch (มติเจ้าของคลินิก 6 ต.ค. 2569) · %s",
				len(crossed), strings.Join(capList(crossed, 10), ", "))
		}
	}

	rep.notef("LINE: ถ้าคลินิกนี้ยังไม่มีกลุ่ม LINE OA สคริปต์จะสร้างให้หนึ่งกลุ่มแบบเปล่า " +
		"(ชื่อ <ชื่อคลินิก>_old) แล้วต้องไปใส่ LIFF id กับ channel token เองที่ " +
		"Settings → LINE OA ก่อนจะส่งข้อความหรือรับผูกใหม่ได้ · " +
		"ตัวการผูกที่ย้ายมาใช้งานได้ทันทีโดยไม่ต้องรอข้อนี้")

	rep.notef("LINE: ไม่เขียน patient_line_link_log — แถวในนั้นคือหลักฐานว่ามีการล็อกอิน LINE " +
		"เกิดขึ้นจริง (migration 0327 อ่าน source='line_register' เป็นหลักฐาน) " +
		"การนำเข้าไม่ใช่การล็อกอิน จึงไม่สร้างหลักฐานปลอมลงใน log ที่เขียนแล้วลบไม่ได้")
}
