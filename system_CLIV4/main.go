// system_CLIV4 turns a clinic's legacy Dr.Ease V2 export into ONE reviewable .sql
// file for V4. It never connects to a database.
//
// Why a file and not a direct write: the artifact can be read, diffed and kept as
// evidence before anything changes, and applying it stays a deliberate act by a person
// who can see exactly what they are about to run.
//
//	system_CLIV4 -clinic C9D4865 -owner wanaka_admin \
//	  -users users.sql -doctor doctor.sql -room clinic.sql \
//	  -patient patient.sql -appointment appointment.sql -address address.ods \
//	  -out ./out
//
// Then, after reading the file:
//
//	psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f out/import_C9D4865.sql
//
// The clinic must ALREADY EXIST in V4 (created through the admin flow so it gets its
// subscription, settings, rules and counters). The generated script verifies that and
// aborts if it does not.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"system_cliv4/internal/legacydump"
	"system_cliv4/internal/legacyimport"
)

func main() {
	var (
		clinic = flag.String("clinic", "", "clinic_code ปลายทาง (ต้องมีอยู่ในระบบ V4 แล้ว) — จำเป็น")
		owner  = flag.String("owner", "", "legacy users.code ของบัญชีที่จะเป็น owner — จำเป็นเมื่อส่ง -users")
		out    = flag.String("out", ".", "โฟลเดอร์ปลายทางของไฟล์ .sql")

		users       = flag.String("users", "", "ไฟล์ dump ตาราง users (.sql)")
		doctor      = flag.String("doctor", "", "ไฟล์ dump ตาราง doctor (.sql)")
		room        = flag.String("room", "", "ไฟล์ dump ตาราง clinic ของระบบเก่า = master ของห้อง (.sql)")
		patient     = flag.String("patient", "", "ไฟล์ dump ตาราง patient (.sql)")
		address     = flag.String("patient_address", "", "ไฟล์ dump ตาราง patient_address (.sql)")
		appointment = flag.String("appointment", "", "ไฟล์ dump ตาราง appointment (.sql)")

		mongo = flag.String("mongo", "", "ไฟล์ dump MongoDB (.js) — แคตตาล็อก: category, items, listheals, course_items")
		page  = flag.String("page", "", "ไฟล์ dump MongoDB ของ mris.page (.js) — หน้าเวชระเบียน + รูปแนบ · ออกเป็น manifest ไม่ใช่ SQL")

		combined = flag.String("sql", "", "ไฟล์ dump MySQL รวมทุกตารางไว้ไฟล์เดียว — ระบบหาตาราง users/doctor/clinic/patient/appointment ให้เอง (ถ้าระบุช่องเฉพาะด้านบนด้วย ช่องนั้นชนะ)")

		only = flag.String("only", "", "นำเข้าเฉพาะบางกลุ่ม คั่นด้วย comma (เว้นว่าง = ทั้งหมด) — ใช้เมื่อปลายทางมีข้อมูลบางส่วนอยู่แล้ว")

		// Branch mapping. The V4 side allocates branch codes when the clinic is created
		// (POST /v3/import/clinics returns source_code → branch_code); this is how that
		// answer gets back into the script. Without it every row goes to one branch, which
		// is what every import before multi-branch support did.
		branchMap = flag.String("branch-map", "",
			"จับคู่รหัสสาขาเดิมกับสาขาใน V4 เช่น 01=BR00001,02=BR00002 (เว้นว่าง = ใช้สาขาเดียว)")
		defaultBranch = flag.String("default-branch", "",
			"สาขาที่ใช้กับแถวที่ไม่มีสาขา หรือรหัสที่ไม่อยู่ใน -branch-map (เว้นว่าง = ตัวแรกของ -branch-map)")

		// Per-SITE, not per-clinic: each site of a multi-site tenant runs its own V2 and
		// issues the same running numbers, while V4 keeps one number space per clinic.
		docNoMarker = flag.String("doc-no-branch-marker", "",
			"เลขหลักเดียว 1-9 แทรกเป็นตัวที่ 3 ของเลขที่เอกสารเดิม (670017358 → 672017358 · C670017358 → C672017358) — ใช้เมื่อนำเข้าหลายสาขาที่ระบบเก่าออกเลขชุดเดียวกัน (เว้นว่าง = ใช้เลขเดิม)")

		// V2 keeps one stock list; V4 has two kinds of stock and treats them differently at
		// the till. Default stays 'drug' so no existing command line changes meaning — see
		// Input.StockItemType for the measured reason this had to become an option.
		stockItemType = flag.String("stock-item-type", "drug",
			"ประเภทของรหัสในคลัง: drug (ยา ต้องมีฉลาก 5 ช่องก่อนขาย) หรือ goods (สินค้า ขายได้ทันที) — ดัมพ์ V2 ไม่มีข้อมูลฉลากเลย ถ้าคลังไม่ใช่ยาจริงให้ใช้ goods")

		// Read-only mode. Writes nothing and needs no -clinic, because it answers the
		// question that comes BEFORE a clinic exists: how many branches does this tenant
		// have, do they belong to one company, and what would splitting them cost.
		surveyBranches = flag.Bool("survey-branches", false, "สำรวจสาขาจากไฟล์ dump แล้วรายงาน (อ่านอย่างเดียว ไม่สร้างไฟล์ ไม่ต้องมี -clinic)")
		surveyJSON     = flag.Bool("json", false, "ใช้กับ -survey-branches: พิมพ์ผลเป็น JSON")
	)
	flag.Parse()

	if *surveyBranches {
		in := legacyimport.Input{
			RoomDump:        mustRead("room", *room),
			PatientDump:     mustRead("patient", *patient),
			AppointmentDump: mustRead("appointment", *appointment),
			MongoDump:       mustRead("mongo", *mongo),
		}
		if body := mustRead("sql", *combined); len(body) > 0 {
			assignCombined(&in, body)
		}
		if len(in.BranchDump) == 0 && len(in.PatientDump) == 0 && len(in.MongoDump) == 0 {
			fail("ต้องมีไฟล์อย่างน้อยหนึ่ง (-sql หรือ -patient / -mongo) เพื่อสำรวจสาขา")
		}
		s, err := legacyimport.SurveyBranches(in)
		if err != nil {
			fail(err.Error())
		}
		if *surveyJSON {
			body, jerr := json.MarshalIndent(s, "", "  ")
			if jerr != nil {
				fail(fmt.Sprintf("แปลงผลเป็น JSON ไม่ได้: %v", jerr))
			}
			fmt.Println(string(body))
			return
		}
		fmt.Print(s.Text())
		return
	}

	if strings.TrimSpace(*clinic) == "" {
		fail("ต้องระบุ -clinic (clinic_code ที่สร้างไว้ในระบบแล้ว)")
	}

	parsedBranchMap, err := parseBranchMap(*branchMap)
	if err != nil {
		fail(err.Error())
	}

	in := legacyimport.Input{
		ClinicCode:        strings.TrimSpace(*clinic),
		OwnerUsername:     strings.TrimSpace(*owner),
		Groups:            splitCSV(*only),
		BranchMap:         parsedBranchMap,
		DefaultBranch:     strings.TrimSpace(*defaultBranch),
		DocNoBranchMarker: strings.TrimSpace(*docNoMarker),
		StockItemType:     strings.TrimSpace(*stockItemType),
		UsersDump:         mustRead("users", *users),
		DoctorDump:        mustRead("doctor", *doctor),
		RoomDump:          mustRead("room", *room),
		PatientDump:       mustRead("patient", *patient),
		AddressDump:       mustRead("patient_address", *address),
		AppointmentDump:   mustRead("appointment", *appointment),

		MongoDump: mustRead("mongo", *mongo),
		PageDump:  mustRead("page", *page),
	}

	// One combined dump instead of one file per table. A phpMyAdmin export of the whole
	// database holds every table, and the parser selects by table name, so the same
	// bytes can serve every entity. Explicit per-entity flags still win, which keeps a
	// mixed set of exports workable.
	if body := mustRead("sql", *combined); len(body) > 0 {
		found, unsupported := describeCombined(body)
		if len(found) == 0 {
			fail(fmt.Sprintf("ไฟล์ -sql ไม่มีตารางที่รองรับเลย (ต้องมีอย่างน้อยหนึ่ง: %s)",
				strings.Join(combinedTableNames(), ", ")))
		}
		assignCombined(&in, body)
		fmt.Println("ไฟล์ SQL รวม — ตารางที่พบและจะใช้:")
		for _, f := range found {
			fmt.Printf("  %s\n", f)
		}
		if len(unsupported) > 0 {
			fmt.Println("ตารางที่อยู่ในไฟล์แต่ยังไม่รองรับ (ไม่ถูกนำเข้า):")
			for _, u := range unsupported {
				fmt.Printf("  %s\n", u)
			}
		}
		fmt.Println()
	}

	script, rep, err := legacyimport.BuildScript(in)
	if err != nil {
		fail(err.Error())
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fail(fmt.Sprintf("สร้างโฟลเดอร์ %s ไม่ได้: %v", *out, err))
	}
	path := filepath.Join(*out, fmt.Sprintf("import_%s.sql", in.ClinicCode))
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		fail(fmt.Sprintf("เขียนไฟล์ %s ไม่ได้: %v", path, err))
	}

	// The image copy plan is a SECOND artifact, written only when a -page dump was
	// supplied. It is JSON rather than SQL because one column (photos.file_size) can
	// only be read off the image files, which arrive separately as a tar — the caller
	// holding those files fills it in. See internal/legacyimport/page.go.
	manifestPath := ""
	if rep.Photos != nil {
		manifestPath = filepath.Join(*out, fmt.Sprintf("photos_%s.json", in.ClinicCode))
		body, merr := json.MarshalIndent(rep.Photos, "", "  ")
		if merr != nil {
			fail(fmt.Sprintf("สร้าง manifest รูปไม่ได้: %v", merr))
		}
		if err := os.WriteFile(manifestPath, append(body, '\n'), 0o644); err != nil {
			fail(fmt.Sprintf("เขียนไฟล์ %s ไม่ได้: %v", manifestPath, err))
		}
	}

	// The report goes to stdout so the operator sees what the file will do without
	// opening it, and so a CI/UI caller can capture it.
	fmt.Printf("เขียนแล้ว: %s\n", path)
	if manifestPath != "" {
		fmt.Printf("เขียนแล้ว: %s  (%d รูป — แผนคัดลอกไฟล์ ไม่ใช่ SQL)\n", manifestPath, len(rep.Photos.Files))
	}
	fmt.Println()
	fmt.Printf("clinic_code : %s\n", rep.ClinicCode)
	if in.OwnerUsername != "" {
		fmt.Printf("owner       : %s\n", in.OwnerUsername)
	}
	fmt.Println("\nสิ่งที่จะถูกเขียน")
	for _, name := range rep.Order {
		e, ok := rep.Entities[name]
		if !ok || e == nil {
			continue
		}
		line := fmt.Sprintf("  %-16s %5d statement(s) จาก %d แถวต้นทาง", name, e.Statements, e.SourceRows)
		if len(e.Skipped) > 0 {
			var parts []string
			for reason, n := range e.Skipped {
				parts = append(parts, fmt.Sprintf("%s: %d", reason, n))
			}
			line += "  [ข้าม " + strings.Join(parts, "; ") + "]"
		}
		fmt.Println(line)
	}
	if len(rep.Warnings) > 0 {
		fmt.Println("\n⚠ คำเตือน — อ่านก่อนรัน")
		for _, w := range rep.Warnings {
			fmt.Println("  " + w)
		}
	}
	if len(rep.Notes) > 0 {
		fmt.Println("\nบันทึก")
		for _, n := range rep.Notes {
			fmt.Println("  " + n)
		}
	}
	fmt.Printf("\nขั้นต่อไป (ตรวจไฟล์ก่อน แล้วจึงรัน):\n  psql \"$DATABASE_URL\" -v ON_ERROR_STOP=1 -f %s\n", path)
	// Not a footnote: the same file run by the application role instead of the owner still
	// imports correctly, but every ANALYZE is skipped with a WARNING and the run goes from
	// minutes to hours. An operator who does not know that reads the WARNING as harmless.
	fmt.Println("  ⚠ ต้องรันด้วย role เจ้าของตาราง (role เดียวกับที่รัน migration) ไม่ใช่ role ของแอป")
	fmt.Println("    เพราะสคริปต์สั่ง ANALYZE ก่อนแต่ละเฟส ซึ่งต้องมีสิทธิ์ owner (PostgreSQL 16 ยังไม่มีสิทธิ์ MAINTAIN)")
	fmt.Println("    ถ้ารันด้วย role ของแอป ANALYZE จะถูกข้ามพร้อม WARNING แล้วช้าจากไม่กี่สิบนาทีเป็นหลายชั่วโมง")
}

// combinedTables is what a single whole-database export can supply, mapped to the
// entity it feeds. The legacy `clinic` table is the ROOM master, which is why it does
// not feed anything called clinic.
// Each entry carries the field it fills, so adding a table here cannot get out of step
// with anything else. It used to be a name/assign pair matched positionally against a
// second slice of pointers, and adding `branch` to one and not the other panicked at
// runtime — the kind of break that a single list makes impossible.
var combinedTables = []struct {
	table  string
	entity string
	field  func(*legacyimport.Input) *[]byte
}{
	{"users", "users + clinic_user + clinic_staff", func(in *legacyimport.Input) *[]byte { return &in.UsersDump }},
	{"doctor", "doctor", func(in *legacyimport.Input) *[]byte { return &in.DoctorDump }},
	{"clinic", "room (master ของห้อง)", func(in *legacyimport.Input) *[]byte { return &in.RoomDump }},
	{"patient", "patient", func(in *legacyimport.Input) *[]byte { return &in.PatientDump }},
	{"patient_address", "patient_address (ที่อยู่คนไข้)", func(in *legacyimport.Input) *[]byte { return &in.AddressDump }},
	{"appointment", "appointment", func(in *legacyimport.Input) *[]byte { return &in.AppointmentDump }},
	// Not imported — read only by -survey-branches, which is why the entity column says
	// so out loud. See knownUnsupported below for the same fact from the other side.
	{"branch", "ทะเบียนสาขา (อ่านเพื่อสำรวจเท่านั้น ไม่นำเข้า)", func(in *legacyimport.Input) *[]byte { return &in.BranchDump }},
}

// knownUnsupported are tables an operator will reasonably expect to be imported when
// they hand over a whole-database export. Saying "present but not read" out loud is the
// difference between a limitation and a silent data loss.
var knownUnsupported = map[string]string{
	"patient_diag": "การวินิจฉัย — เวชระเบียนยังไม่อยู่ในเฟสนี้",
	"branch":       "สาขา — V4 ใช้ตาราง clinic เป็นทะเบียนสาขา สร้างตอนตั้งคลินิก",
	"clinic_room":  "ชนิดห้อง (lookup) — V4 ไม่มีตารางเทียบ",
}

func combinedTableNames() []string {
	out := make([]string, 0, len(combinedTables))
	for _, c := range combinedTables {
		out = append(out, c.table)
	}
	return out
}

// describeCombined reports which supported tables the export holds (with row counts)
// and which recognisable-but-unsupported ones are in there too.
func describeCombined(body []byte) (found, unsupported []string) {
	rowsByTable := map[string]int{}
	for _, t := range legacydump.Tables(string(body)) {
		rowsByTable[strings.ToLower(t.Name)] = t.Rows
	}
	for _, c := range combinedTables {
		if n, ok := rowsByTable[c.table]; ok && n > 0 {
			found = append(found, fmt.Sprintf("%-14s %6d แถว → %s", c.table, n, c.entity))
		}
	}
	for table, why := range knownUnsupported {
		if n, ok := rowsByTable[table]; ok && n > 0 {
			unsupported = append(unsupported, fmt.Sprintf("%-14s %6d แถว · %s", table, n, why))
		}
	}
	sort.Strings(unsupported)
	return found, unsupported
}

/**
 * assignCombined points every entity that has no explicit file at the combined dump —
 * but only for tables the dump actually contains.
 *
 * The "only if present" part matters: pointing an entity at a file with no such table
 * made the run fail with "no INSERT INTO `users` found" instead of importing the tables
 * that were there. A partial export is a normal thing to hand over (one table per file,
 * or a dump trimmed to the part being re-imported), and it must not be an error.
 */
func assignCombined(in *legacyimport.Input, body []byte) {
	present := map[string]bool{}
	for _, t := range legacydump.Tables(string(body)) {
		if t.Rows > 0 {
			present[strings.ToLower(t.Name)] = true
		}
	}
	for _, c := range combinedTables {
		if !present[c.table] {
			continue
		}
		if p := c.field(in); len(*p) == 0 {
			*p = body
		}
	}
}

// mustRead reads an input file, or returns nil when the flag was not supplied. A
// supplied-but-unreadable path is fatal: silently skipping it would produce a script
// that looks complete and is not.
func mustRead(label, path string) []byte {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		fail(fmt.Sprintf("อ่านไฟล์ %s (%s) ไม่ได้: %v", label, path, err))
	}
	if len(b) == 0 {
		fail(fmt.Sprintf("ไฟล์ %s (%s) ว่าง", label, path))
	}
	return b
}

/**
 * parseBranchMap reads "01=BR00001,02=BR00002" into a map.
 *
 * Chosen over JSON because a person types this on a command line as often as a program
 * builds it, and a malformed pair has to be refused rather than half-read: a mapping with
 * one entry silently missing would file that branch's patients somewhere else.
 */
func parseBranchMap(s string) (map[string]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("-branch-map: %q ไม่ถูกรูป — ต้องเป็น รหัสเดิม=รหัสV4 เช่น 01=BR00001", pair)
		}
		if prev, dup := out[k]; dup {
			return nil, fmt.Errorf("-branch-map: รหัสเดิม %q ถูกกำหนดซ้ำ (%q แล้ว %q)", k, prev, v)
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-branch-map: ไม่มีคู่ที่ใช้ได้เลย")
	}
	return out, nil
}

// splitCSV reads a comma-separated flag into a trimmed list.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "system_CLIV4: "+msg)
	os.Exit(1)
}
