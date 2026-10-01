package legacyimport

import (
	"fmt"
	"sort"
	"strings"
)

// The declared width of every varchar column this importer writes, and the invariant that
// no generated literal may exceed one.
//
// # WHY THIS FILE EXISTS — measured, not guessed
//
// kvk-clinic (C52AEF6), 1 October 2026. One receipt out of 19,311 carried a 26,286-
// character payment_remark and the renderer put it into receipt.detail, which is
// varchar(255). The script was 104 MB; psql ran for 109 seconds and then stopped on
//
//	ERROR: value too long for type character varying(255)
//
// The artifact is 27 transactions, not one (max_locks_per_transaction — see script.go), so
// the groups before the money phase had already committed: 2,057 patients, 19,313 invoices
// and 29,267 invoice lines were in, receipts and everything after them were not, and the
// tenant had to be deleted before another attempt. The widths were knowable the whole
// time; nothing checked them.
//
// So the check runs HERE, on the assembled text, before the file is handed over. A width
// that does not fit is a defect in this tool, and the place to find it is while the script
// is still a string in memory — not after an operator has waited two minutes and lost a
// half-loaded tenant.
//
// # WHERE THESE NUMBERS COME FROM
//
// information_schema.columns on the V4 schema, read 1 Oct 2026. Only `character varying`
// columns appear. A `text` or `jsonb` column is deliberately ABSENT rather than listed
// with a large number: receipt.note really does hold the whole 26,286-character remark and
// appointment.doc holds a JSON blob, and neither has a limit to check against.
//
// To refresh after a migration changes a width:
//
//	SELECT table_name, column_name, character_maximum_length
//	  FROM information_schema.columns
//	 WHERE table_schema='public' AND data_type='character varying'
//	   AND character_maximum_length IS NOT NULL
//	 ORDER BY table_name, column_name;
//
// A width declared here NARROWER than the database would refuse a script the database
// would have accepted, so these numbers are copied, never estimated.
var columnWidths = map[string]map[string]int{
	"appointment": {
		"booking_group_id": 36, "branch_code": 32, "clinic_code": 32, "confirm_status": 16,
		"doctor_code": 64, "heal_id": 24, "heal_name": 255, "hn": 20, "mongo_id": 24,
		"patient_name": 255, "reschedule_status": 16, "room_id": 64, "status": 20,
		"tel": 32, "transfer_receipt": 255,
	},
	"catalog_category":    {"clinic_code": 32},
	"catalog_channel":     {"channel_key": 32, "clinic_code": 32},
	"catalog_course":      {"clinic_code": 32},
	"catalog_course_line": {"clinic_code": 32, "line_kind": 16},
	"catalog_item": {
		"brand_color": 9, "card_style": 24, "clinic_code": 32, "code": 64,
		"item_type": 16, "legacy_source_key": 32, "sku": 16, "source_table": 24,
		"tax_item_kind": 24, "tax_treatment": 24, "tax_treatment_source": 16,
	},
	"catalog_item_channel":     {"clinic_code": 32},
	"catalog_item_child":       {"clinic_code": 32, "relation_kind": 16},
	"catalog_item_sku_counter": {"clinic_code": 32, "item_type": 16},
	"catalog_service":          {"clinic_code": 32, "service_kind_key": 32},
	"clinic_staff": {
		"clinic_code": 32, "email": 255, "name": 255, "password_hash": 255,
		"tel": 64, "username": 255,
	},
	"clinic_user": {"clinic_code": 32},
	"course_bought": {
		"branch_code": 32, "clinic_code": 32, "code": 64,
		"estimated_cost_state": 24, "hn": 20, "status": 16,
	},
	"course_bought_items": {"clinic_code": 32, "code": 64, "kind": 16, "unit": 32},
	"doctor": {
		"blade": 64, "branch_code": 32, "clinic_code": 32, "code": 64, "color": 32,
		"df_type": 32, "doctor_active": 8, "doctor_code": 64, "doctor_name": 255,
		"doctorname_en": 255, "doctorname_th": 255, "doctornickname": 255,
		"employment_type": 16, "eng_name": 255, "medical_license": 32,
		"profession": 24, "tha_name": 255,
	},
	"invoice": {
		"branch_code": 32, "clinic_code": 32, "hn": 20, "no": 32, "status": 16, "vn": 64,
	},
	"invoice_item": {
		"clinic_code": 32, "code": 64, "doctor": 32, "kind": 16,
		"source_key": 32, "source_ref": 64, "tax_treatment": 24,
	},
	"items": {
		"base_unit": 32, "category": 64, "clinic_code": 32, "code": 64,
		"dosage_form": 64, "dose_code": 64, "dose_type": 64, "generic_name_th": 200,
		"name": 255, "name_en": 255, "package_unit": 32, "source_key": 32,
		"strength": 64, "unit": 32,
	},
	"loyalty_account": {"clinic_code": 32, "hn": 32},
	"loyalty_policy":  {"clinic_code": 32},
	"patient": {
		"Age": 16, "Braces_ID": 64, "Company": 255, "Firstname": 128, "HN": 20,
		"ID_Card": 32, "Lastname": 128, "Nickname": 128, "Prefixname": 32, "Tel": 32,
		"active": 8, "blood": 8, "branch_code": 20, "career": 128, "clinic_code": 32,
		"confirm_check": 8, "contact_name": 255, "contact_name_2": 255,
		"contact_phone": 32, "contact_phone_2": 32, "contact_relation": 64,
		"department": 128, "division": 128, "email": 255, "employee_code": 64,
		"firstname_en": 128, "gender": 16, "height": 16, "history_surgery_date": 32,
		"hospital": 255, "identification_number": 32, "internal_tel": 32,
		"lastname_en": 128, "line_id": 64, "line_oa_group_id": 64,
		"marital_status": 32, "message_group": 64, "old_hn": 64, "passport_no": 32,
		"patient_insure": 64, "patient_middlename": 128, "patient_nationality": 64,
		"patient_race": 64, "patient_source": 64, "patient_source_know_us": 255,
		"patient_source_other": 255, "patient_source_promotion": 255,
		"patient_star": 16, "patient_status": 32, "patient_vip_level": 16,
		"person_segment": 16, "profile_picture": 500, "rank_id": 64, "religion": 64,
		"tax_id": 32, "utm_campaign": 255, "utm_medium": 255, "utm_source": 255,
		"weight": 16,
	},
	"receipt": {
		"branch_code": 32, "clinic_code": 32, "created_by": 64, "detail": 255,
		"doctor_code": 64, "hn": 20, "receipt_no": 32,
	},
	"receipt_payment": {
		"approve_code": 64, "clinic_code": 32, "insurer": 64, "method": 32, "reference": 64,
	},
	"room":      {"branch_code": 32, "clinic_code": 32, "code": 64, "color": 16},
	"sequences": {"clinic_code": 32, "prefix": 8, "type": 32},
	"slot_stock": {
		"branch_code": 16, "clinic_code": 32, "code": 64, "company_medicine": 255,
		"item_code": 64, "item_id": 24, "mongo_id": 24,
	},
	"stock_movement": {
		"HN": 20, "branch_code": 16, "clinic_code": 32, "item_code": 64,
		"item_name": 200, "lot_code": 64, "rate_code": 32, "reason": 64,
		"unit": 32, "user_id": 32,
	},
	"users": {
		"avatar_path": 255, "clinic_code": 32, "email": 255, "locale": 8,
		"name": 255, "name_en": 255, "password": 255, "remember_token": 100,
	},
}

// widthViolation is one literal that does not fit the column it is written to.
type widthViolation struct {
	table  string
	column string
	width  int
	got    int
	sample string
}

// widthCheckStats is what the scan saw, so "it found nothing" can be told apart from
// "it understood nothing".
type widthCheckStats struct {
	Statements int // INSERTs whose shape was read
	Skipped    int // INSERTs whose shape was not read, or whose arity disagreed
	Values     int // string literals actually compared against a declared width
}

// maxWidthViolationsReported bounds the error text. One violation is a bug to fix; fifty
// of them are usually the same bug, and printing all of them buries the first.
const maxWidthViolationsReported = 8

// checkScriptWidths refuses a script that Postgres would reject for width.
func checkScriptWidths(script string, rep *Report) error {
	violations, st := scanScriptWidths(script)
	if rep != nil {
		rep.notef("ตรวจความกว้างคอลัมน์: อ่านคำสั่ง INSERT ได้ %d คำสั่ง · เทียบค่าข้อความ %d ค่า · "+
			"ข้ามรูปแบบที่อ่านไม่ออก %d (ข้ามแล้วไม่ปฏิเสธ เพราะรูปแบบใหม่ที่ตัวตรวจไม่รู้จักไม่ควรบล็อกการนำเข้าจริง)",
			st.Statements, st.Values, st.Skipped)
	}
	if len(violations) == 0 {
		return nil
	}
	return fmt.Errorf("%s", describeWidthViolations(violations))
}

// scanScriptWidths measures without deciding, so a test can assert on what was read as
// well as on what was refused.
//
// # FAIL CLOSED ON A VIOLATION, FAIL OPEN ON A PARSE
//
// A statement whose shape this does not understand is COUNTED and skipped, never reported
// as a violation. Treating "I could not read this" as "this is wrong" would let a new
// statement shape block a real import, which is a worse failure than the one being
// prevented. The skipped count is surfaced so a shape that stops being understood is
// visible rather than silent.
func scanScriptWidths(script string) ([]widthViolation, widthCheckStats) {
	// Comments and dollar-quoted bodies are blanked first: an entity header or a plpgsql
	// block can contain the words "INSERT INTO" without being a statement this owns.
	// Blanking preserves length, so offsets into the result still address the original.
	masked := maskNonSQL(script)

	var violations []widthViolation
	var st widthCheckStats

	for i := 0; ; {
		at := strings.Index(masked[i:], "INSERT INTO ")
		if at < 0 {
			break
		}
		i = i + at + len("INSERT INTO ")

		table, pos, ok := readIdent(masked, i)
		if !ok {
			st.Skipped++
			continue
		}
		cols, valueLists, ok := readInsertShape(masked, pos)
		if !ok {
			st.Skipped++
			continue
		}
		st.Statements++
		i = pos

		widths := columnWidths[table]
		if len(widths) == 0 {
			continue // no varchar column here, or a temp table such as import_user_map
		}
		for _, vals := range valueLists {
			if len(vals) != len(cols) {
				// Columns and values disagree, so there is no mapping to trust. Counted
				// as a skip rather than guessed at.
				st.Skipped++
				continue
			}
			for n, col := range cols {
				limit, limited := widths[col]
				if !limited {
					continue
				}
				text, isLit := literalText(vals[n])
				if !isLit {
					continue // NULL, a number, now(), a cast, a subquery — nothing to measure
				}
				st.Values++
				// Postgres counts varchar(n) in CHARACTERS, so this must too: a Thai
				// character is three bytes, and counting bytes would refuse values that fit.
				if got := len([]rune(text)); got > limit {
					violations = append(violations, widthViolation{
						table: table, column: col, width: limit, got: got,
						sample: sampleForError(text),
					})
				}
			}
		}
	}
	return violations, st
}

// sampleForError keeps the message readable: the head of the value, on one line.
func sampleForError(s string) string {
	s = oneLine(s)
	r := []rune(s)
	if len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return s
}

func describeWidthViolations(v []widthViolation) string {
	sort.Slice(v, func(a, b int) bool {
		if v[a].table != v[b].table {
			return v[a].table < v[b].table
		}
		if v[a].column != v[b].column {
			return v[a].column < v[b].column
		}
		return v[a].got > v[b].got
	})
	var b strings.Builder
	fmt.Fprintf(&b, "ไม่สร้างไฟล์: มีค่า %d จุดยาวเกินความกว้างคอลัมน์ปลายทาง — psql จะหยุดกลางทาง "+
		"และเพราะสคริปต์ถูกแบ่งเป็นหลายทรานแซกชัน กลุ่มที่ commit ไปแล้วจะค้างอยู่ในฐาน "+
		"(ต้องลบคลินิกก่อนลองใหม่) จึงปฏิเสธตั้งแต่ตอนนี้\n", len(v))
	for n, x := range v {
		if n >= maxWidthViolationsReported {
			fmt.Fprintf(&b, "  … และอีก %d จุด\n", len(v)-maxWidthViolationsReported)
			break
		}
		fmt.Fprintf(&b, "  %s.%s เป็น varchar(%d) แต่ได้ค่ายาว %d ตัวอักษร: %q\n",
			x.table, x.column, x.width, x.got, x.sample)
	}
	b.WriteString("แก้ที่ตัวสร้าง: ค่าที่ลงคอลัมน์ varchar ต้องผ่าน clip(value, n) ให้ตรงความกว้าง " +
		"แล้วรายงานการตัดผ่าน truncations เหมือน entity อื่น (ดู money.go · patient.go)")
	return b.String()
}

// ── a very small amount of SQL reading ───────────────────────────────────────────
//
// Enough to find the values of an INSERT and no more. Everything here gives up rather
// than guesses: see the note on scanScriptWidths.

// maskNonSQL blanks line comments and dollar-quoted bodies, preserving length so offsets
// into the result also address the original.
func maskNonSQL(s string) string {
	out := []byte(s)
	i, n := 0, len(s)
	for i < n {
		switch {
		case s[i] == '\'':
			// A literal. Comment and dollar-quote openers inside it mean nothing.
			i = skipLiteral(s, i)
		case s[i] == '-' && i+1 < n && s[i+1] == '-':
			for i < n && s[i] != '\n' {
				out[i] = ' '
				i++
			}
		case s[i] == '$':
			tag, after, ok := readDollarTag(s, i)
			if !ok {
				i++
				continue
			}
			end := strings.Index(s[after:], tag)
			if end < 0 {
				// Unterminated: blank to the end rather than read a body as statements.
				for ; i < n; i++ {
					out[i] = ' '
				}
				continue
			}
			stop := after + end + len(tag)
			for ; i < stop; i++ {
				out[i] = ' '
			}
		default:
			i++
		}
	}
	return string(out)
}

// readDollarTag reads a `$$` or `$tag$` opener at i, returning the tag and the offset
// just past it.
func readDollarTag(s string, i int) (tag string, after int, ok bool) {
	if i >= len(s) || s[i] != '$' {
		return "", i, false
	}
	j := i + 1
	for j < len(s) && isIdentByte(s[j]) {
		j++
	}
	if j >= len(s) || s[j] != '$' {
		return "", i, false
	}
	return s[i : j+1], j + 1, true
}

func isIdentByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// readIdent reads a (possibly double-quoted) identifier at i.
func readIdent(s string, i int) (name string, next int, ok bool) {
	i = skipSpace(s, i)
	if i >= len(s) {
		return "", i, false
	}
	if s[i] == '"' {
		j := strings.IndexByte(s[i+1:], '"')
		if j < 0 {
			return "", i, false
		}
		return s[i+1 : i+1+j], i + j + 2, true
	}
	j := i
	for j < len(s) && isIdentByte(s[j]) {
		j++
	}
	if j == i {
		return "", i, false
	}
	return s[i:j], j, true
}

func skipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\n' || s[i] == '\t' || s[i] == '\r') {
		i++
	}
	return i
}

// readInsertShape reads the column list and the value list(s) of an INSERT whose table
// name has just been consumed. ok is false for any shape it cannot read with certainty,
// including an INSERT with no explicit column list — there is nothing to map against.
func readInsertShape(s string, i int) (cols []string, valueLists [][]string, ok bool) {
	i = skipSpace(s, i)
	if i >= len(s) || s[i] != '(' {
		return nil, nil, false
	}
	closeIdx := matchParen(s, i)
	if closeIdx < 0 {
		return nil, nil, false
	}
	for _, c := range splitTopLevel(s[i+1 : closeIdx]) {
		name, _, got := readIdent(c, 0)
		if !got {
			return nil, nil, false
		}
		cols = append(cols, name)
	}
	if len(cols) == 0 {
		return nil, nil, false
	}

	i = skipSpace(s, closeIdx+1)
	switch {
	case hasWordAt(s, i, "VALUES"):
		i += len("VALUES")
		for {
			i = skipSpace(s, i)
			if i >= len(s) || s[i] != '(' {
				break
			}
			cl := matchParen(s, i)
			if cl < 0 {
				return nil, nil, false
			}
			valueLists = append(valueLists, splitTopLevel(s[i+1:cl]))
			i = skipSpace(s, cl+1)
			if i < len(s) && s[i] == ',' {
				i++
				continue
			}
			break
		}
		return cols, valueLists, len(valueLists) > 0

	case hasWordAt(s, i, "SELECT"):
		i += len("SELECT")
		end := selectListEnd(s, i)
		return cols, [][]string{splitTopLevel(s[i:end])}, true
	}
	return nil, nil, false
}

// selectListEnd finds where a SELECT's value list stops: the first clause keyword or
// semicolon at depth 0.
func selectListEnd(s string, i int) int {
	stops := []string{"FROM", "WHERE", "GROUP", "ORDER", "HAVING", "WINDOW",
		"UNION", "LIMIT", "OFFSET", "RETURNING", "ON"}
	depth := 0
	for i < len(s) {
		if s[i] == '\'' {
			i = skipLiteral(s, i)
			continue
		}
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ';':
			if depth == 0 {
				return i
			}
		}
		if depth == 0 {
			for _, k := range stops {
				if hasWordAt(s, i, k) {
					return i
				}
			}
		}
		i++
	}
	return len(s)
}

// hasWordAt reports whether word starts at i on a word boundary.
func hasWordAt(s string, i int, word string) bool {
	if i < 0 || i+len(word) > len(s) || s[i:i+len(word)] != word {
		return false
	}
	if i > 0 && isIdentByte(s[i-1]) {
		return false
	}
	if j := i + len(word); j < len(s) && isIdentByte(s[j]) {
		return false
	}
	return true
}

// matchParen returns the index of the ')' closing the '(' at i, or -1.
func matchParen(s string, i int) int {
	if i >= len(s) || s[i] != '(' {
		return -1
	}
	depth := 0
	for i < len(s) {
		if s[i] == '\'' {
			i = skipLiteral(s, i)
			continue
		}
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
		i++
	}
	return -1
}

// skipLiteral returns the index just past the literal starting at i.
func skipLiteral(s string, i int) int {
	i++ // opening quote
	for i < len(s) {
		if s[i] == '\'' {
			if i+1 < len(s) && s[i+1] == '\'' {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return len(s)
}

// splitTopLevel splits a comma-separated list, ignoring commas inside quotes or parens.
func splitTopLevel(s string) []string {
	var out []string
	depth, start, i := 0, 0, 0
	for i < len(s) {
		if s[i] == '\'' {
			i = skipLiteral(s, i)
			continue
		}
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
		i++
	}
	out = append(out, strings.TrimSpace(s[start:]))
	return out
}

// literalText returns the text of a value that is EXACTLY one string literal. Anything
// else — NULL, a number, a cast, a concatenation, a subquery — is not something to
// measure, and saying so is how this avoids inventing violations.
func literalText(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if len(v) < 2 || v[0] != '\'' {
		return "", false
	}
	if end := skipLiteral(v, 0); end != len(v) {
		return "", false
	}
	return strings.ReplaceAll(v[1:len(v)-1], "''", "'"), true
}
