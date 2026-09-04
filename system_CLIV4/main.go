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

		combined = flag.String("sql", "", "ไฟล์ dump MySQL รวมทุกตารางไว้ไฟล์เดียว — ระบบหาตาราง users/doctor/clinic/patient/appointment ให้เอง (ถ้าระบุช่องเฉพาะด้านบนด้วย ช่องนั้นชนะ)")

		only = flag.String("only", "", "นำเข้าเฉพาะบางกลุ่ม คั่นด้วย comma (เว้นว่าง = ทั้งหมด) — ใช้เมื่อปลายทางมีข้อมูลบางส่วนอยู่แล้ว")
	)
	flag.Parse()

	if strings.TrimSpace(*clinic) == "" {
		fail("ต้องระบุ -clinic (clinic_code ที่สร้างไว้ในระบบแล้ว)")
	}

	in := legacyimport.Input{
		ClinicCode:      strings.TrimSpace(*clinic),
		OwnerUsername:   strings.TrimSpace(*owner),
		Groups:          splitCSV(*only),
		UsersDump:       mustRead("users", *users),
		DoctorDump:      mustRead("doctor", *doctor),
		RoomDump:        mustRead("room", *room),
		PatientDump:     mustRead("patient", *patient),
		AddressDump:     mustRead("patient_address", *address),
		AppointmentDump: mustRead("appointment", *appointment),

		MongoDump: mustRead("mongo", *mongo),
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

	// The report goes to stdout so the operator sees what the file will do without
	// opening it, and so a CI/UI caller can capture it.
	fmt.Printf("เขียนแล้ว: %s\n\n", path)
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
}

// combinedTables is what a single whole-database export can supply, mapped to the
// entity it feeds. The legacy `clinic` table is the ROOM master, which is why it does
// not feed anything called clinic.
var combinedTables = []struct {
	table  string
	entity string
	assign func(*legacyimport.Input, []byte)
}{
	{"users", "users + clinic_user + clinic_staff", func(in *legacyimport.Input, b []byte) { in.UsersDump = b }},
	{"doctor", "doctor", func(in *legacyimport.Input, b []byte) { in.DoctorDump = b }},
	{"clinic", "room (master ของห้อง)", func(in *legacyimport.Input, b []byte) { in.RoomDump = b }},
	{"patient", "patient", func(in *legacyimport.Input, b []byte) { in.PatientDump = b }},
	{"patient_address", "patient_address (ที่อยู่คนไข้)", func(in *legacyimport.Input, b []byte) { in.AddressDump = b }},
	{"appointment", "appointment", func(in *legacyimport.Input, b []byte) { in.AppointmentDump = b }},
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

// assignCombined points every entity that has no explicit file at the combined dump.
func assignCombined(in *legacyimport.Input, body []byte) {
	current := []*[]byte{
		&in.UsersDump, &in.DoctorDump, &in.RoomDump,
		&in.PatientDump, &in.AddressDump, &in.AppointmentDump,
	}
	for i, c := range combinedTables {
		if len(*current[i]) == 0 {
			c.assign(in, body)
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
