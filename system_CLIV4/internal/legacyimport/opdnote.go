package legacyimport

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"system_cliv4/internal/legacydump"
)

// The OPD note lane: V2's visits, OPD notes and vital-sign forms → V4's encounter record.
//
// It is the hand-built Anjali script (C1_apply_opd_C6C936C.sql, 22 Oct 2026) made generic,
// and it keeps every rule the clinic owner settled for that run:
//
//	main  a visit per REAL date from patient_visit_mater — not one box on a synthetic visit
//	1     a V2 VN that V4 already uses that day gets a new number (not skipped, not merged)
//	2     a note (which carries no VN) binds to the LAST visit of its patient-day
//	3     a note whose patient has no visit that day gets a visit opened for it
//	4     vital numbers → vital_sign · the vital form's symptom text → appended to the S box
//	5     every box carries a heading
//	6     the author is carried over from V2 (user_id → doctor.doctor_user_id → doctor_code)
//	7     visits V2 closed (active <> '1') are skipped
//
// and C1's own handling: 0 in a vital column is "never filled" (NULL), a value outside a
// clinical range is dropped, two notes on one patient-day are merged into the one box their
// visit allows, a note blank in all six fields opens a visit but writes no opd_record, and
// the symptom text is appended only to an opd_record this run created.
//
// What a reusable tool needs on top of a one-off script:
//
//   - RE-RUN SAFETY. visit_master has no provenance column, so a visit this lane wrote is
//     recognised by its signature: title = 'นำเข้าจากระบบเดิม' AND created_at = the visit
//     day at 00:00 Bangkok (the photo tool stamps created_at = now(), the app stamps the
//     real time). A patient-day that already holds such a visit was imported before and is
//     left alone entirely — re-running the same dump writes nothing twice.
//   - NOTES NEVER LAND IN SOMEONE ELSE'S VISIT. C1 bound a note to the last visit of the
//     day whoever opened it. Here only visits this lane opened are candidates, so a doctor's
//     own V4 visit, a photo-tool visit and a signed visit are never written into.
//   - VN NUMBERING follows the photo tool (V4_STEERING_PATIENT_OPD §5.2): a deleted visit's
//     VN still counts as used, numbers past 999 are written as they are instead of being cut
//     by lpad, and the app's per-day advisory lock is taken for the dates it still issues
//     numbers on (yesterday onward).
//
// NOT IN THIS LANE, on purpose (same as Anjali): patient_diag — the "การวินิจฉัย" and
// "Note เพิ่มเติม" boxes of V2's OPD page — and patient_opd_physical, which V4 has no
// table for. V4's own patient_diag is no home for the former: the app deletes a visit's
// rows there every time a doctor saves its ICD codes.
const EntOpdNote = "opd_note"

var opdNoteOrder = []string{EntOpdNote}

const (
	// opdImportTitle is what C1 and the photo tool both write; the signature that tells this
	// lane's own visits apart pairs it with a created_at of the visit day at midnight.
	opdImportTitle = "นำเข้าจากระบบเดิม"

	// The box headings of decision 5, word for word as C1 wrote them, so Anjali's imported
	// notes and every later clinic's read the same.
	opdHeadCC  = "อาการสำคัญ"
	opdHeadPI  = "ประวัติปัจจุบัน"
	opdHeadSym = "อาการเบื้องต้น (จากแบบสัญญาณชีพเดิม)"
	opdHeadPE  = "ตรวจร่างกาย"
	opdHeadDX  = "วินิจฉัย / การประเมิน"
	opdHeadMG  = "การจัดการ / แผนการรักษา"
	opdHeadAD  = "คำแนะนำ"

	opdStageBatch = 400
	// opd_record.doctor_code is varchar(32); visit_master.doctor_code is wider, but the
	// two carry the same code so both take the narrower width.
	opdAuthorWidth = 32
	opdHNWidth     = 32
	opdVNWidth     = 8

	// opdOrphanShown caps the orphan HNs named in the report; the count is always whole.
	opdOrphanShown = 20
)

// opdSymptomLabels are the V2 vital form's own field labels. In C1's source 341 of 345
// symptom texts were nothing but this skeleton; stripping the labels is how a text a
// person actually typed is told apart from an untouched form.
var opdSymptomLabels = []string{
	"Chief complaint:", "Chief Complaint:", "Present illness:", "Present Illness:",
	"Physical exam:", "Diagnosis:", "Management:", "Advice:",
}

// opdMeasure is one numeric vital: where it comes from in V2, where it lands in V4, the
// range a real instrument could have produced (owner's answer "ก" during C1), and the
// factor to V4's integer units.
type opdMeasure struct {
	src    []string // V2 columns; the first that holds an in-range value wins
	col    string
	lo, hi float64
	scale  float64
}

var opdMeasures = []opdMeasure{
	{[]string{"weight"}, "weight_grams", 20, 300, 1000},
	{[]string{"height"}, "height_mm", 100, 220, 10},
	{[]string{"heart_rate"}, "heart_rate", 30, 200, 1},
	{[]string{"systolic"}, "systolic", 60, 260, 1},
	{[]string{"diastolic"}, "diastolic", 30, 160, 1},
	{[]string{"temperature"}, "temperature_centi_c", 33, 43, 100},
	{[]string{"spo2", "o2_sat"}, "spo2_pct", 50, 100, 1},
	{[]string{"rr"}, "respiratory_rate", 5, 60, 1},
	{[]string{"pain_score"}, "pain_score", 0, 10, 1},
	{[]string{"DTX"}, "dtx_mg_dl", 10, 1000, 1},
}

// opdVitalText is a vital V4 keeps as free text: the eye exam, excretion and the two fields
// the old form labelled lt/rt and sl/cm. Migration 0610 added these columns for exactly the
// fields V2's form had. numeric marks the V2 columns stored as float (0 = never filled).
type opdVitalText struct {
	src, col string
	width    int
	numeric  bool
}

var opdVitalTexts = []opdVitalText{
	{"visual_acuity_LE", "va_le", 40, false},
	{"visual_acuity_RE", "va_re", 40, false},
	{"tension_LE", "tension_le", 40, false},
	{"tension_RE", "tension_re", 40, false},
	{"autorefraction_LE", "autoref_le", 40, false},
	{"autorefraction_RE", "autoref_re", 40, false},
	{"urinary", "urinary", 500, true},
	{"defection", "defecation", 500, true},
	{"ltrt", "lt_rt", 40, true},
	{"slcm", "sl_cm", 40, true},
}

type opdVisit struct {
	src   int64
	day   string
	vn    string
	hn    string
	dcode string
}

type opdNote struct {
	src                int64
	day, hn            string
	s, o, a, p, advice string
	dcode              string
	count              int
}

type opdVital struct {
	src        int64
	day, vn    string
	ints       map[string]int64
	texts      map[string]string
	symptom    string
	hasMeasure bool
}

// opdCleaner normalises clinical text and counts what it had to remove, so the loss is
// stated once with a number.
type opdCleaner struct{ nul int }

func (c *opdCleaner) text(s string) string {
	if strings.IndexByte(s, 0) >= 0 {
		// Postgres refuses a NUL in text outright, and one would fail the whole file.
		c.nul++
		s = strings.ReplaceAll(s, "\x00", "")
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimSpace(s)
}

// opdField reads the first of several spellings of a column. V2 writes the same field as
// visitdate in one table and visitDate in the next.
func opdField(r legacydump.Row, names ...string) string {
	for _, n := range names {
		if v := r.Get(n); v != "" {
			return v
		}
	}
	return ""
}

// opdDay turns a V2 DATETIME ("2026-10-30 00:00:00") into its calendar day. DATETIME is
// stored without a zone and mysqldump does not convert it, so the first ten characters
// are the day the clinic recorded.
func opdDay(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 10 {
		return "", false
	}
	d := s[:10]
	t, err := time.Parse("2006-01-02", d)
	if err != nil || t.Year() < 1900 {
		return "", false
	}
	return d, true
}

// opdUserKey normalises a V2 user id: patient_opd_note keeps it as varchar, the doctor
// table as int, and C1 matched them through CAST(… AS UNSIGNED).
func opdUserKey(s string) string {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return strconv.FormatInt(n, 10)
	}
	return s
}

func opdAuthors(doctorRows []legacydump.Row) map[string]string {
	out := map[string]string{}
	for _, d := range doctorRows {
		uid, code := opdUserKey(d.Get("doctor_user_id")), d.Get("doctor_code")
		if uid == "" || uid == "0" || code == "" || code == "0" {
			continue
		}
		if _, taken := out[uid]; !taken { // first in the dump, like C1's LIMIT 1
			out[uid] = code
		}
	}
	return out
}

// opdSection renders one box: its heading, then every distinct part in source order.
// Distinct because V2 often saved the same note twice; C1's source had no such copies.
func opdSection(heading string, parts []string) string {
	var kept []string
	seen := map[string]bool{}
	for _, p := range parts {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		kept = append(kept, p)
	}
	if len(kept) == 0 {
		return ""
	}
	return heading + "\n" + strings.Join(kept, "\n\n")
}

func opdJoin(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}

func opdSymptomTyped(s string) bool {
	for _, l := range opdSymptomLabels {
		s = strings.ReplaceAll(s, l, "")
	}
	return strings.TrimSpace(s) != ""
}

// opdNumber parses a V2 float column. 0 and blank are "never filled".
func opdNumber(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f == 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func opdIntLit(m map[string]int64, col string) string {
	if v, ok := m[col]; ok {
		return strconv.FormatInt(v, 10)
	}
	return "NULL"
}

// buildOpdNote renders the OPD note lane as ONE entity. It has to be one: the script commits
// between entities, and the passes share temporary tables that drop at commit.
func buildOpdNote(visitRows, noteRows, vitalRows, doctorRows []legacydump.Row,
	knownHN map[string]bool, in Input, rep *Report) (string, error) {
	e := rep.entity(EntOpdNote)
	e.SourceRows = len(visitRows) + len(noteRows) + len(vitalRows)
	clean := &opdCleaner{}
	tr := truncations{}

	authors := opdAuthors(doctorRows)
	author := func(uid string) string {
		code, cut := clip(authors[opdUserKey(uid)], opdAuthorWidth)
		tr.note("doctor_code", cut)
		return code
	}
	if len(doctorRows) == 0 && (len(visitRows) > 0 || len(noteRows) > 0) {
		rep.notef("opd_note: ไม่มีตาราง doctor ในไฟล์ จึงแปลผู้บันทึกจาก user_id ไม่ได้ — visit และ OPD note จะไม่มีชื่อผู้บันทึก")
	}

	// A year this far out is a Buddhist-era date typed into a Gregorian field. It would sit
	// at the top of the patient's visit list forever, so it is named before anything runs.
	buddhistEra := 0
	countEra := func(day string) {
		if y, _ := strconv.Atoi(day[:4]); y >= 2400 {
			buddhistEra++
		}
	}

	// A medical record whose HN V2's own patient table does not carry belongs to nobody the
	// clinic can name, so it is skipped and listed rather than refusing the whole lane
	// (owner's call, 23 Oct 2026 — the same as orphan bills and the Anjali photos). Only
	// when the patient dump was read: with knownHN nil this run cannot tell, and the SQL
	// guard decides against the live patient table instead. An HN that V2 does carry but
	// V4 does not is still refused there — that is "import patients first", not an orphan.
	orphans := map[string]bool{}
	orphan := func(hn string) bool {
		if knownHN == nil || knownHN[hn] {
			return false
		}
		orphans[hn] = true
		return true
	}

	// ── visits ──────────────────────────────────────────────────────────────────
	var visits []opdVisit
	// hnsAt is filled before the orphan filter: a vital's (date, VN) is checked against
	// every V2 visit, so one shared with an orphan's visit still counts as ambiguous.
	hnsAt := map[[2]string]map[string]bool{}
	for _, r := range visitRows {
		// C1 took active = '1' only. A dump whose table has no active column at all (older
		// V2) is all active — reading the missing column as "closed" would drop every visit.
		if _, hasActive := r["active"]; hasActive && r.Get("active") != "1" {
			e.skip("visit: V2 ปิดไว้ (active ไม่ใช่ 1)")
			continue
		}
		id, err := strconv.ParseInt(r.Get("id"), 10, 64)
		if err != nil {
			e.skip("visit: ไม่มี id")
			continue
		}
		day, ok := opdDay(opdField(r, "visitdate", "visitDate"))
		if !ok {
			e.skip("visit: วันที่ใช้ไม่ได้")
			continue
		}
		hn, vn := r.Get("HN"), r.Get("VN")
		if hn == "" || vn == "" {
			e.skip("visit: ไม่มี HN หรือ VN")
			continue
		}
		if len([]rune(hn)) > opdHNWidth || len([]rune(vn)) > opdVNWidth {
			// Clipping either would quietly move the visit to another patient or number.
			e.skip("visit: HN หรือ VN ยาวเกินช่องของ V4")
			continue
		}
		k := [2]string{day, vn}
		if hnsAt[k] == nil {
			hnsAt[k] = map[string]bool{}
		}
		hnsAt[k][hn] = true
		if orphan(hn) {
			e.skip("visit: HN ไม่มีในทะเบียนคนไข้ของ V2")
			continue
		}
		countEra(day)
		visits = append(visits, opdVisit{src: id, day: day, vn: vn, hn: hn, dcode: author(r.Get("user_id"))})
	}
	sort.Slice(visits, func(i, j int) bool { return visits[i].src < visits[j].src })

	// ── notes, merged per patient-day ───────────────────────────────────────────
	type rawNote struct {
		id                         int64
		cc, pi, pe, dx, mg, ad, dc string
	}
	byDay := map[[2]string][]rawNote{}
	var dayKeys [][2]string
	for _, r := range noteRows {
		id, err := strconv.ParseInt(r.Get("id"), 10, 64)
		if err != nil {
			e.skip("note: ไม่มี id")
			continue
		}
		day, ok := opdDay(opdField(r, "visitDate", "visitdate"))
		if !ok {
			e.skip("note: วันที่ใช้ไม่ได้")
			continue
		}
		hn := r.Get("HN")
		if hn == "" || len([]rune(hn)) > opdHNWidth {
			e.skip("note: ไม่มี HN หรือ HN ยาวเกินช่องของ V4")
			continue
		}
		if orphan(hn) {
			e.skip("note: HN ไม่มีในทะเบียนคนไข้ของ V2")
			continue
		}
		k := [2]string{hn, day}
		if _, seen := byDay[k]; !seen {
			dayKeys = append(dayKeys, k)
		}
		byDay[k] = append(byDay[k], rawNote{
			id: id,
			cc: clean.text(r.Get("chief_complaint")), pi: clean.text(r.Get("presen_illness")),
			pe: clean.text(r.Get("physical_exam")), dx: clean.text(r.Get("diagnosis_impression")),
			mg: clean.text(r.Get("management")), ad: clean.text(r.Get("advice")),
			dc: author(r.Get("user_id")),
		})
	}
	var notes []opdNote
	merged, blank := 0, 0
	for _, k := range dayKeys {
		g := byDay[k]
		sort.Slice(g, func(i, j int) bool { return g[i].id < g[j].id })
		if len(g) > 1 {
			merged++
		}
		pick := func(f func(rawNote) string) []string {
			out := make([]string, 0, len(g))
			for _, x := range g {
				out = append(out, f(x))
			}
			return out
		}
		n := opdNote{
			src: g[0].id, hn: k[0], day: k[1], count: len(g),
			s: opdJoin(opdSection(opdHeadCC, pick(func(x rawNote) string { return x.cc })),
				opdSection(opdHeadPI, pick(func(x rawNote) string { return x.pi }))),
			o:      opdSection(opdHeadPE, pick(func(x rawNote) string { return x.pe })),
			a:      opdSection(opdHeadDX, pick(func(x rawNote) string { return x.dx })),
			p:      opdSection(opdHeadMG, pick(func(x rawNote) string { return x.mg })),
			advice: opdSection(opdHeadAD, pick(func(x rawNote) string { return x.ad })),
		}
		for _, x := range g { // the last author named wins, as in C1
			if x.dc != "" {
				n.dcode = x.dc
			}
		}
		if n.s == "" && n.o == "" && n.a == "" && n.p == "" && n.advice == "" {
			blank++
		}
		countEra(n.day)
		notes = append(notes, n)
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].src < notes[j].src })

	// ── vitals, one per V2 visit ────────────────────────────────────────────────
	//
	// patient_vital_sign names its visit by (date, VN) and carries NO HN. So the visit it
	// belongs to is looked up among the staged visits, and a vital whose (date, VN) names
	// more than one patient is dropped: a blood pressure filed under the wrong person is
	// worse than a blank one.
	type rawVital struct {
		id int64
		r  legacydump.Row
	}
	vitalGroups := map[[2]string][]rawVital{}
	var vitalKeys [][2]string
	for _, r := range vitalRows {
		id, err := strconv.ParseInt(r.Get("id"), 10, 64)
		if err != nil {
			e.skip("vital: ไม่มี id")
			continue
		}
		day, ok := opdDay(opdField(r, "visitdate", "visitDate"))
		vn := r.Get("VN")
		if !ok || vn == "" {
			e.skip("vital: ไม่มีวันที่หรือ VN")
			continue
		}
		k := [2]string{day, vn}
		switch n := len(hnsAt[k]); {
		case n == 0:
			e.skip("vital: ไม่มี visit ของ V2 ที่วันที่+VN ตรงกัน (visit ถูกปิดหรือไม่มีในไฟล์)")
			continue
		case n > 1:
			e.skip("vital: วันที่+VN ตรงกับคนไข้มากกว่า 1 คน — ไม่รู้ว่าเป็นของใคร")
			continue
		}
		owner := ""
		for hn := range hnsAt[k] {
			owner = hn
		}
		if orphans[owner] {
			e.skip("vital: เป็นของ visit ที่ HN ไม่มีในทะเบียนคนไข้ของ V2")
			continue
		}
		if _, seen := vitalGroups[k]; !seen {
			vitalKeys = append(vitalKeys, k)
		}
		vitalGroups[k] = append(vitalGroups[k], rawVital{id: id, r: r})
	}
	dropped := map[string]int{}
	var vitals []opdVital
	for _, k := range vitalKeys {
		g := vitalGroups[k]
		sort.Slice(g, func(i, j int) bool { return g[i].id < g[j].id })
		v := opdVital{src: g[len(g)-1].id, day: k[0], vn: k[1],
			ints: map[string]int64{}, texts: map[string]string{}}
		var symptoms []string
		// Rows are walked oldest first, so a value the newer row also holds overwrites the
		// older one. Measured on a real export: of 155 visits with two or more vital rows,
		// 146 were identical copies and in the other 9 the newest row held every measure.
		for _, x := range g {
			for _, m := range opdMeasures {
				for _, src := range m.src {
					f, filled := opdNumber(x.r.Get(src))
					if !filled {
						continue
					}
					if f < m.lo || f > m.hi {
						dropped[src]++
						continue
					}
					v.ints[m.col] = int64(math.Round(f * m.scale))
					break
				}
			}
			for _, t := range opdVitalTexts {
				val := x.r.Get(t.src)
				if t.numeric {
					f, filled := opdNumber(val)
					if !filled {
						continue
					}
					val = strconv.FormatFloat(f, 'f', -1, 64)
				}
				if val == "" {
					continue
				}
				val, cut := clip(clean.text(val), t.width)
				tr.note(t.col, cut)
				v.texts[t.col] = val
			}
			if sym := clean.text(x.r.Get("symptom_text")); opdSymptomTyped(sym) {
				symptoms = append(symptoms, sym)
			}
		}
		v.symptom = opdSection(opdHeadSym, symptoms)
		v.hasMeasure = len(v.ints) > 0 || len(v.texts) > 0
		if !v.hasMeasure && v.symptom == "" {
			e.skip("vital: ไม่มีค่าที่กรอกจริง (ศูนย์ทั้งหมด / แบบฟอร์มเปล่า)")
			continue
		}
		vitals = append(vitals, v)
	}
	sort.Slice(vitals, func(i, j int) bool { return vitals[i].src < vitals[j].src })

	// ── what the operator should know before running it ─────────────────────────
	//
	// Named before the early return: a file whose every record is an orphan's still has to
	// say why nothing was written.
	if len(orphans) > 0 {
		list := make([]string, 0, len(orphans))
		for hn := range orphans {
			list = append(list, hn)
		}
		sort.Strings(list)
		shown := list
		more := ""
		if len(shown) > opdOrphanShown {
			shown, more = shown[:opdOrphanShown], fmt.Sprintf(" …และอีก %d ราย", len(list)-opdOrphanShown)
		}
		rep.warnf("opd_note: ข้าม visit/OPD note/สัญญาณชีพของ HN %d ราย ที่ไม่มีในทะเบียนคนไข้ของ V2 เอง (เวชระเบียนที่ไม่รู้ว่าเป็นของใคร ไม่ถูกเขียน): %s%s",
			len(list), strings.Join(shown, ", "), more)
	}
	if knownHN == nil && (len(visits) > 0 || len(notes) > 0) {
		rep.notef("opd_note: รอบนี้ไม่ได้อ่านไฟล์คนไข้ จึงเทียบ HN ล่วงหน้าไม่ได้ — ด่าน HN ในสคริปต์จะเทียบกับทะเบียนคนไข้ของ V4 และหยุดทั้งกลุ่มถ้ามี HN ที่ไม่มีคนไข้")
	}
	if len(visits) == 0 && len(notes) == 0 {
		rep.notef("opd_note: ไม่มี visit หรือ OPD note ที่นำเข้าได้ในไฟล์ — ไม่มีอะไรให้เขียน")
		return "", nil
	}
	if buddhistEra > 0 {
		rep.warnf("opd_note: %d visit/note ลงปี ≥ 2400 (น่าจะเป็นปี พ.ศ. ในช่อง ค.ศ.) — จะขึ้นบนสุดของรายการ visit ตลอดไป ตรวจก่อนรัน", buddhistEra)
	}
	if clean.nul > 0 {
		rep.warnf("opd_note: ตัด NUL byte ออกจากข้อความ %d ช่อง (Postgres ไม่รับ NUL ในข้อความ)", clean.nul)
	}
	if sum := tr.summary(); sum != "" {
		rep.warnf("opd_note: ตัดความยาวให้พอดีช่องของ V4 %s", sum)
	}
	if len(dropped) > 0 {
		keys := make([]string, 0, len(dropped))
		for k := range dropped {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %d", k, dropped[k]))
		}
		rep.notef("opd_note: ค่าสัญญาณชีพนอกช่วงที่เครื่องวัดให้ได้ถูกทิ้ง (เหมือนรอบ Anjali): %s", strings.Join(parts, ", "))
	}
	rep.notef("opd_note: visit %d · OPD note %d วัน-คนไข้ (รวมจากหลายใบ %d · ว่างทั้ง 6 ช่อง %d — เปิด visit แต่ไม่เขียน opd_record) · สัญญาณชีพ %d visit",
		len(visits), len(notes), merged, blank, len(vitals))
	rep.notef("opd_note: ไม่นำเข้ากล่อง \"การวินิจฉัย\" และ \"Note เพิ่มเติม\" (patient_diag) และแบบประเมินกายภาพ (patient_opd_physical) — เหมือนรอบ Anjali")

	return renderOpdNote(in.ClinicCode, visits, notes, vitals, e), nil
}

func renderOpdNote(clinic string, visits []opdVisit, notes []opdNote, vitals []opdVital, e *EntityResult) string {
	var s sqlBuilder
	s.comment("OPD note · visit + OPD note + สัญญาณชีพ จาก V2 · กฎเดียวกับรอบ Anjali (C1, 22 ต.ค. 2569)")
	s.comment("ทั้งหมดอยู่ในทรานแซกชันเดียว และหยุดตัวเองแทนการเขียนเวชระเบียนที่ไม่แน่ใจ:")
	s.comment("  HN ที่ไม่ใช่คนไข้ของคลินิกนี้ · คนไข้ไม่มีสาขา · เลข VN ชน · note ผูก visit ไม่ได้ → EXCEPTION")
	s.comment("visit ที่เซ็นแล้ว และ visit ที่ไม่ได้เปิดโดยงานนี้ ไม่ถูกเขียนเลย")
	s.b.WriteString("\n")

	intCols := make([]string, 0, len(opdMeasures))
	for _, m := range opdMeasures {
		intCols = append(intCols, m.col)
	}
	textCols := make([]string, 0, len(opdVitalTexts))
	for _, t := range opdVitalTexts {
		textCols = append(textCols, t.col)
	}

	var vitalDDL strings.Builder
	for _, c := range intCols {
		vitalDDL.WriteString(c + " int, ")
	}
	for _, c := range textCols {
		vitalDDL.WriteString(c + " text, ")
	}
	s.addf(`CREATE TEMP TABLE opd_s_visit (src_id bigint PRIMARY KEY, d date NOT NULL, vn_src text NOT NULL,
    hn text NOT NULL, dcode text NOT NULL DEFAULT '') ON COMMIT DROP;
CREATE TEMP TABLE opd_s_note (src_id bigint PRIMARY KEY, d date NOT NULL, hn text NOT NULL,
    soap_s text, soap_o text, soap_a text, soap_p text, advice text, dcode text NOT NULL DEFAULT '',
    note_count int NOT NULL) ON COMMIT DROP;
CREATE TEMP TABLE opd_s_vital (src_id bigint PRIMARY KEY, d date NOT NULL, vn_src text NOT NULL,
    %shas_measure boolean NOT NULL, symptom text) ON COMMIT DROP;

`, vitalDDL.String())

	stage := func(table, cols string, rows []string) {
		for i := 0; i < len(rows); i += opdStageBatch {
			end := i + opdStageBatch
			if end > len(rows) {
				end = len(rows)
			}
			s.addf("INSERT INTO %s (%s) VALUES\n%s;\n\n", table, cols, strings.Join(rows[i:end], ",\n"))
		}
	}

	vrows := make([]string, 0, len(visits))
	for _, v := range visits {
		vrows = append(vrows, fmt.Sprintf("  (%d, %s, %s, %s, %s)", v.src, lit(v.day), lit(v.vn), lit(v.hn), lit(v.dcode)))
	}
	stage("opd_s_visit", "src_id, d, vn_src, hn, dcode", vrows)

	nrows := make([]string, 0, len(notes))
	for _, n := range notes {
		nrows = append(nrows, fmt.Sprintf("  (%d, %s, %s, %s, %s, %s, %s, %s, %s, %d)", n.src, lit(n.day), lit(n.hn),
			nullLit(n.s), nullLit(n.o), nullLit(n.a), nullLit(n.p), nullLit(n.advice), lit(n.dcode), n.count))
	}
	stage("opd_s_note", "src_id, d, hn, soap_s, soap_o, soap_a, soap_p, advice, dcode, note_count", nrows)

	trows := make([]string, 0, len(vitals))
	for _, v := range vitals {
		vals := []string{strconv.FormatInt(v.src, 10), lit(v.day), lit(v.vn)}
		for _, c := range intCols {
			vals = append(vals, opdIntLit(v.ints, c))
		}
		for _, c := range textCols {
			vals = append(vals, nullLit(v.texts[c]))
		}
		vals = append(vals, strconv.FormatBool(v.hasMeasure), nullLit(v.symptom))
		trows = append(trows, "  ("+strings.Join(vals, ", ")+")")
	}
	stage("opd_s_vital", "src_id, d, vn_src, "+strings.Join(intCols, ", ")+", "+strings.Join(textCols, ", ")+", has_measure, symptom", trows)

	vitalCols := strings.Join(append(append([]string{}, intCols...), textCols...), ", ")
	vitalSel := make([]string, 0, len(intCols)+len(textCols))
	for _, c := range intCols {
		vitalSel = append(vitalSel, "t."+c)
	}
	for _, c := range textCols {
		vitalSel = append(vitalSel, "t."+c)
	}

	body := strings.NewReplacer(
		"{{C}}", lit(clinic),
		"{{MARK}}", lit(opdImportTitle),
		"{{VITAL_COLS}}", vitalCols,
		"{{VITAL_SEL}}", strings.Join(vitalSel, ", "),
	).Replace(opdPassesSQL)
	// Counted by the keyword a top-level statement starts with, so the semicolons inside the
	// DO blocks do not inflate the header's number.
	for _, line := range strings.Split(body, "\n") {
		for _, kw := range []string{"CREATE ", "INSERT ", "DELETE ", "ANALYZE ", "DO ", "WITH "} {
			if strings.HasPrefix(line, kw) {
				s.stmts++
				break
			}
		}
	}
	s.b.WriteString(body)
	e.Statements = s.stmts
	return s.String()
}

// opdPassesSQL is everything after the staging rows. {{C}} is the clinic literal.
//
// "A visit this lane wrote" is the signature title = {{MARK}} AND created_at = the visit
// day at 00:00 Bangkok. Written as opd_mine(...) below in words; spelled out in SQL each
// time because a temp function cannot be created by every role that runs imports.
const opdPassesSQL = `ANALYZE opd_s_visit;
ANALYZE opd_s_note;
ANALYZE opd_s_vital;

CREATE TEMP TABLE opd_stat (k text PRIMARY KEY, n bigint NOT NULL) ON COMMIT DROP;

-- GUARD: the deletion ledger must exist — a deleted visit's VN is still in use (0716).
DO $opd_ver$
BEGIN
  IF to_regclass('public.opd_visit_deletion') IS NULL THEN
    RAISE EXCEPTION 'OPD note: ฐานนี้ยังไม่มีตาราง opd_visit_deletion (migration 0716) — migrate ก่อน';
  END IF;
END $opd_ver$;

-- GUARD: every HN must already be a patient of this clinic. A clinical record for a
-- patient that does not exist is worse than no record (C1).
DO $opd_hn$
DECLARE miss bigint; sample text;
BEGIN
  SELECT count(*), coalesce(string_agg(x.hn, ', ' ORDER BY x.hn), '') INTO miss, sample
    FROM (SELECT DISTINCT u.hn FROM (SELECT hn FROM opd_s_visit UNION SELECT hn FROM opd_s_note) u
           WHERE NOT EXISTS (SELECT 1 FROM patient p WHERE p.clinic_code = {{C}} AND p."HN" = u.hn)) x;
  IF miss > 0 THEN
    RAISE EXCEPTION 'OPD note: HN % รายไม่มีในทะเบียนคนไข้ของคลินิกนี้ — นำเข้าคนไข้ก่อน: %', miss, left(sample, 300);
  END IF;
END $opd_hn$;

-- GUARD: every patient needs a branch — visit_master.branch_code is NOT NULL and never blank.
DO $opd_branch$
DECLARE miss bigint; sample text;
BEGIN
  SELECT count(*), coalesce(string_agg(p."HN", ', ' ORDER BY p."HN"), '') INTO miss, sample
    FROM patient p
   WHERE p.clinic_code = {{C}} AND coalesce(btrim(p.branch_code), '') = ''
     AND p."HN" IN (SELECT hn FROM opd_s_visit UNION SELECT hn FROM opd_s_note);
  IF miss > 0 THEN
    RAISE EXCEPTION 'OPD note: คนไข้ % รายไม่มีสาขา จึงเปิด visit ให้ไม่ได้ — ใส่สาขาก่อน: %', miss, left(sample, 300);
  END IF;
END $opd_branch$;

-- The app's own per-day VN lock, for the days it still issues numbers on.
DO $opd_lock$
BEGIN
  PERFORM pg_advisory_xact_lock(hashtext('opd_vn:' || {{C}} || ':' || to_char(x.d, 'YYYY-MM-DD')))
     FROM (SELECT d FROM opd_s_visit UNION SELECT d FROM opd_s_note) x
    WHERE x.d >= current_date - 1
    ORDER BY x.d;
END $opd_lock$;

-- ══ RE-RUN · a patient-day an earlier run of this lane wrote is left alone ════════
CREATE TEMP TABLE opd_done ON COMMIT DROP AS
SELECT DISTINCT t.visit_date AS d, t."HN"::text AS hn
  FROM visit_master t
 WHERE t.clinic_code = {{C}}
   AND t.title = {{MARK}}
   AND t.created_at = (t.visit_date::timestamp AT TIME ZONE 'Asia/Bangkok')
   AND EXISTS (SELECT 1 FROM (SELECT d, hn FROM opd_s_visit UNION SELECT d, hn FROM opd_s_note) s
                WHERE s.d = t.visit_date AND s.hn = t."HN");
INSERT INTO opd_stat VALUES
  ('ข้าม visit ที่เคยนำเข้าแล้ว', (SELECT count(*) FROM opd_s_visit v
      WHERE EXISTS (SELECT 1 FROM opd_done x WHERE x.d = v.d AND x.hn = v.hn))),
  ('ข้าม OPD note ที่เคยนำเข้าแล้ว', (SELECT count(*) FROM opd_s_note n
      WHERE EXISTS (SELECT 1 FROM opd_done x WHERE x.d = n.d AND x.hn = n.hn)));
DELETE FROM opd_s_visit v USING opd_done x WHERE x.d = v.d AND x.hn = v.hn;
DELETE FROM opd_s_note n USING opd_done x WHERE x.d = n.d AND x.hn = n.hn;

-- ══ PASS A · a VN per V2 visit that is free on its date ═══════════════════════════
-- The V2 VN is kept when V4 has not used it that day (a deleted visit's VN counts as used)
-- and this is the first staged visit to claim it. Every other visit — and every note that
-- needs a visit of its own (PASS B) — continues from the highest number in use that day.
CREATE TEMP TABLE opd_days ON COMMIT DROP AS
SELECT d FROM opd_s_visit UNION SELECT d FROM opd_s_note;

CREATE TEMP TABLE opd_taken ON COMMIT DROP AS
SELECT t.visit_date AS d, t."VN"::text AS vn FROM visit_master t
 WHERE t.clinic_code = {{C}} AND t.visit_date IN (SELECT d FROM opd_days)
UNION
SELECT x.visit_date, x.vn FROM opd_visit_deletion x
 WHERE x.clinic_code = {{C}} AND x.visit_date IN (SELECT d FROM opd_days);
CREATE INDEX ON opd_taken (d, vn);
ANALYZE opd_taken;

CREATE TEMP TABLE opd_flag ON COMMIT DROP AS
SELECT o.src_id, o.d, o.vn_src, o.hn, o.dcode,
       (o.rn = 1 AND NOT EXISTS (SELECT 1 FROM opd_taken k WHERE k.d = o.d AND k.vn = o.vn_src)) AS keep_src
  FROM (SELECT v.*, row_number() OVER (PARTITION BY v.d, v.vn_src ORDER BY v.src_id) AS rn
          FROM opd_s_visit v) o;

CREATE TEMP TABLE opd_base ON COMMIT DROP AS
SELECT x.d, GREATEST(
         COALESCE((SELECT max(k.vn::bigint) FROM opd_taken k
                    WHERE k.d = x.d AND k.vn ~ '^[0-9]{1,15}$'), 0),
         COALESCE((SELECT max(f.vn_src::bigint) FROM opd_flag f
                    WHERE f.d = x.d AND f.keep_src AND f.vn_src ~ '^[0-9]{1,15}$'), 0)) AS base
  FROM opd_days x;

-- kind 0 = a V2 visit whose VN was taken · kind 1 = a note with no visit that day (PASS B)
CREATE TEMP TABLE opd_fresh ON COMMIT DROP AS
SELECT q.kind, q.src_id, q.d, b.base + row_number() OVER (PARTITION BY q.d ORDER BY q.kind, q.src_id) AS n
  FROM (SELECT 0 AS kind, f.src_id, f.d FROM opd_flag f WHERE NOT f.keep_src
        UNION ALL
        SELECT 1, n.src_id, n.d FROM opd_s_note n
         WHERE NOT EXISTS (SELECT 1 FROM opd_s_visit v WHERE v.d = n.d AND v.hn = n.hn)) q
  JOIN opd_base b ON b.d = q.d;

-- Up to 999 a number is written three digits wide, like the app; past it, as it is
-- (lpad would cut 1000 to 100 and collide — measured 18 Sep 2026).
CREATE TEMP TABLE opd_alloc ON COMMIT DROP AS
SELECT f.src_id, f.d, f.hn, f.dcode, f.vn_src, f.keep_src, 0 AS kind,
       CASE WHEN f.keep_src THEN f.vn_src
            WHEN fr.n < 1000 THEN lpad(fr.n::text, 3, '0') ELSE fr.n::text END AS vn
  FROM opd_flag f LEFT JOIN opd_fresh fr ON fr.kind = 0 AND fr.src_id = f.src_id
UNION ALL
SELECT n.src_id, n.d, n.hn, n.dcode, NULL, false, 1,
       CASE WHEN fr.n < 1000 THEN lpad(fr.n::text, 3, '0') ELSE fr.n::text END
  FROM opd_s_note n JOIN opd_fresh fr ON fr.kind = 1 AND fr.src_id = n.src_id;
CREATE INDEX ON opd_alloc (d, vn_src);

-- GUARD: a clash here would mean the allocator is wrong; refuse rather than write.
DO $opd_vn$
DECLARE dup bigint;
BEGIN
  SELECT count(*) INTO dup FROM (SELECT d, vn FROM opd_alloc GROUP BY d, vn HAVING count(*) > 1) x;
  IF dup > 0 THEN
    RAISE EXCEPTION 'OPD note: ตัวออกเลข VN ให้เลขซ้ำกันเอง % คู่ (วันที่+VN)', dup;
  END IF;
  SELECT count(*) INTO dup FROM opd_alloc a WHERE EXISTS (SELECT 1 FROM opd_taken k WHERE k.d = a.d AND k.vn = a.vn);
  IF dup > 0 THEN
    RAISE EXCEPTION 'OPD note: ตัวออกเลข VN เลือกเลขที่ V4 ใช้ไปแล้ว % เลข', dup;
  END IF;
END $opd_vn$;

INSERT INTO opd_stat VALUES
  ('visit ใช้ VN เดิมของ V2', (SELECT count(*) FROM opd_alloc WHERE kind = 0 AND keep_src)),
  ('visit ได้ VN ใหม่เพราะเลขเดิมถูกใช้แล้ว', (SELECT count(*) FROM opd_alloc WHERE kind = 0 AND NOT keep_src)),
  ('visit ที่เปิดให้ OPD note ที่ไม่มี visit วันนั้น', (SELECT count(*) FROM opd_alloc WHERE kind = 1));

INSERT INTO visit_master (clinic_code, "HN", "VN", visit_date, branch_code, doctor_code, title, created_at)
SELECT {{C}}, a.hn, a.vn, a.d, p.branch_code, NULLIF(a.dcode, ''), {{MARK}},
       (a.d::timestamp AT TIME ZONE 'Asia/Bangkok')
  FROM opd_alloc a
  JOIN patient p ON p.clinic_code = {{C}} AND p."HN" = a.hn
 ORDER BY a.d, a.kind, a.src_id;
ANALYZE visit_master;

-- ══ PASS C · bind each note to the LAST visit of its day that this lane opened ════
CREATE TEMP TABLE opd_note_visit ON COMMIT DROP AS
SELECT n.src_id, v.id AS visit_id, v.vn
  FROM opd_s_note n
  JOIN LATERAL (SELECT t.id, t."VN"::text AS vn FROM visit_master t
                 WHERE t.clinic_code = {{C}} AND t.visit_date = n.d AND t."HN" = n.hn
                   AND t.title = {{MARK}}
                   AND t.created_at = (n.d::timestamp AT TIME ZONE 'Asia/Bangkok')
                   AND t.signed_at IS NULL
                 ORDER BY CASE WHEN t."VN" ~ '^[0-9]{1,15}$' THEN t."VN"::bigint ELSE -1 END DESC, t.id DESC
                 LIMIT 1) v ON true;
CREATE INDEX ON opd_note_visit (src_id);

-- GUARD: a note with nowhere to go must stop the run rather than vanish (C1).
DO $opd_nv$
DECLARE lost bigint;
BEGIN
  SELECT count(*) INTO lost FROM opd_s_note n
   WHERE NOT EXISTS (SELECT 1 FROM opd_note_visit nv WHERE nv.src_id = n.src_id);
  IF lost > 0 THEN
    RAISE EXCEPTION 'OPD note: % note ผูก visit ไม่ได้', lost;
  END IF;
END $opd_nv$;

CREATE TEMP TABLE opd_ins (id bigint PRIMARY KEY) ON COMMIT DROP;
WITH ins AS (
  INSERT INTO opd_record (clinic_code, "HN", "VN", visit_date, soap_s, soap_o, soap_a, soap_p,
                          doctor_code, version, created_at, updated_at)
  SELECT {{C}}, n.hn, nv.vn, n.d, n.soap_s, n.soap_o, n.soap_a, n.soap_p, NULLIF(n.dcode, ''), 1,
         (n.d::timestamp AT TIME ZONE 'Asia/Bangkok'), (n.d::timestamp AT TIME ZONE 'Asia/Bangkok')
    FROM opd_s_note n JOIN opd_note_visit nv ON nv.src_id = n.src_id
   WHERE coalesce(n.soap_s, '') || coalesce(n.soap_o, '') || coalesce(n.soap_a, '') || coalesce(n.soap_p, '') <> ''
  ON CONFLICT (clinic_code, visit_date, "VN") DO NOTHING
  RETURNING id)
INSERT INTO opd_ins (id) SELECT id FROM ins;
INSERT INTO opd_stat VALUES ('opd_record (S/O/A/P) ที่เขียน', (SELECT count(*) FROM opd_ins));

-- ══ PASS D · the advice box ════════════════════════════════════════════════════════
WITH ins AS (
  INSERT INTO opd_visit_advice (clinic_code, visit_id, hn, body, version, last_base_version,
                                updated_by_name, created_at, updated_at)
  SELECT {{C}}, nv.visit_id, n.hn, n.advice, 1, 0, {{MARK}},
         (n.d::timestamp AT TIME ZONE 'Asia/Bangkok'), (n.d::timestamp AT TIME ZONE 'Asia/Bangkok')
    FROM opd_s_note n JOIN opd_note_visit nv ON nv.src_id = n.src_id
   WHERE coalesce(n.advice, '') <> ''
  ON CONFLICT (clinic_code, visit_id) DO NOTHING
  RETURNING 1)
INSERT INTO opd_stat SELECT 'คำแนะนำที่เขียน', count(*) FROM ins;

-- ══ PASS E · vital signs, bound through the V2 visit they were recorded against ═══
CREATE TEMP TABLE opd_vital_visit ON COMMIT DROP AS
SELECT DISTINCT ON (t.src_id) t.src_id, vm.id AS visit_id, vm."HN"::text AS hn,
       vm.visit_date, vm."VN"::text AS vn
  FROM opd_s_vital t
  JOIN opd_alloc a ON a.kind = 0 AND a.d = t.d AND a.vn_src = t.vn_src
  JOIN visit_master vm ON vm.clinic_code = {{C}} AND vm.visit_date = a.d AND vm."VN" = a.vn
 ORDER BY t.src_id, a.src_id;
INSERT INTO opd_stat VALUES ('สัญญาณชีพที่ไม่ได้ผูก (visit ของ V2 เคยนำเข้าแล้ว)',
  (SELECT count(*) FROM opd_s_vital t WHERE NOT EXISTS (SELECT 1 FROM opd_vital_visit vv WHERE vv.src_id = t.src_id)));

WITH ins AS (
  INSERT INTO vital_sign (clinic_code, visit_id, "HN", {{VITAL_COLS}}, recorded_at)
  SELECT {{C}}, vv.visit_id, vv.hn, {{VITAL_SEL}}, (t.d::timestamp AT TIME ZONE 'Asia/Bangkok')
    FROM opd_s_vital t JOIN opd_vital_visit vv ON vv.src_id = t.src_id
   WHERE t.has_measure
  ON CONFLICT (clinic_code, visit_id) DO NOTHING
  RETURNING 1)
INSERT INTO opd_stat SELECT 'สัญญาณชีพที่เขียน', count(*) FROM ins;

-- ══ PASS F · the vital form's symptom text, into the S box ════════════════════════
-- Appends only to an opd_record THIS run created; a note a doctor wrote in V4 is left
-- exactly as it is. Grouped per visit first: ON CONFLICT DO UPDATE cannot touch one row
-- twice in a single command (measured on C1's first dry run).
WITH up AS (
  INSERT INTO opd_record (clinic_code, "HN", "VN", visit_date, soap_s, version, created_at, updated_at)
  SELECT {{C}}, g.hn, g.vn, g.visit_date, g.body, 1,
         (g.visit_date::timestamp AT TIME ZONE 'Asia/Bangkok'), (g.visit_date::timestamp AT TIME ZONE 'Asia/Bangkok')
    FROM (SELECT vv.visit_id, vv.hn, vv.vn, vv.visit_date,
                 string_agg(t.symptom, E'\n\n' ORDER BY t.src_id) AS body
            FROM opd_s_vital t JOIN opd_vital_visit vv ON vv.src_id = t.src_id
           WHERE coalesce(t.symptom, '') <> ''
           GROUP BY vv.visit_id, vv.hn, vv.vn, vv.visit_date) g
  ON CONFLICT (clinic_code, visit_date, "VN") DO UPDATE
     SET soap_s = CASE WHEN coalesce(opd_record.soap_s, '') = '' THEN excluded.soap_s
                       ELSE opd_record.soap_s || E'\n\n' || excluded.soap_s END,
         updated_at = excluded.updated_at
   WHERE opd_record.id IN (SELECT id FROM opd_ins)
  RETURNING 1)
INSERT INTO opd_stat SELECT 'อาการเบื้องต้นที่ลงช่อง S', count(*) FROM up;

DO $opd_report$
DECLARE r record;
BEGIN
  FOR r IN SELECT k, n FROM opd_stat ORDER BY k LOOP
    RAISE NOTICE 'OPD note · % : %', r.k, r.n;
  END LOOP;
END $opd_report$;

`
