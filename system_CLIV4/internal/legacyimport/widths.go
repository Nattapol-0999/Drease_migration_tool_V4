package legacyimport

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// What every column this importer writes can physically hold, and the invariant that no
// generated value may exceed it.
//
// WHY THIS FILE EXISTS — three failures of one shape
//
// Every import that died this week died the same way: a value out of V2 that does not fit
// the V4 column, discovered only when psql was already running.
//
//	1 Oct  C52AEF6   a 26,286-character remark into receipt.detail varchar(255)
//	2 Oct  ปิ่นลักษณ์  two bills claiming one invoice.no (unique index)
//	2 Oct  CBE8806   a stock lot of 99,999,999,999,999 into slot_stock.amount numeric(14,2)
//
// The first one is what this file was originally built for, and it covered varchar only.
// CBE8806 walked straight through it: `numeric field overflow`, three lots out of 206,
// and because the script commits in 27 transactions the tenant was left half-loaded with
// 231 patients, 533 appointments and 188 catalogue rows in and no stock at all.
//
// So the check now covers every kind of column that has a ceiling:
//
//	character varying(n)  — n CHARACTERS, which is what Postgres counts
//	numeric(p,s)          — |value| < 10^(p-s)
//	smallint / integer    — the platform ranges
//
// `text` and `jsonb` are deliberately ABSENT: receipt.note really does hold the whole
// 26,286-character remark and appointment.doc holds a JSON blob, and neither has a limit.
//
// IT REPORTS EVERYTHING, NOT THE FIRST THING
//
// Refusing on the first violation is what turned one clinic into three round trips of
// pull, create, generate, import, fail, delete. The scan collects every violation and the
// refusal groups them by column with counts and examples, so one run names every value
// that has to be dealt with.
//
// WHERE THE NUMBERS COME FROM
//
// information_schema on the V4 schema, read 2 Oct 2026. To refresh after a migration:
//
//	SELECT table_name, column_name, data_type,
//	       character_maximum_length, numeric_precision, numeric_scale
//	  FROM information_schema.columns
//	 WHERE table_schema='public'
//	 ORDER BY table_name, column_name;
//
// A limit declared here TIGHTER than the database would refuse a script the database
// would have accepted, so these numbers are copied, never estimated.

// columnWidths is the declared width of every varchar column this importer writes.
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
	// Read from production (migration 728) on 24 Oct 2026, when the course register started
	// being written.
	"courses": {"clinic_code": 32, "code": 64, "group_key": 32, "source_key": 32},
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

// numericLimit is a numeric(p,s) column. intDigits is p-s: how many digits may stand to
// the left of the decimal point, which is the whole of what overflows.
type numericLimit struct {
	precision int
	scale     int
}

func (n numericLimit) intDigits() int { return n.precision - n.scale }

// columnNumeric is every numeric(p,s) column this importer writes.
//
// The tightest ones are the ones to watch: receipt.vat_rate and
// receipt.card_charge_percent are numeric(6,2), so anything from 10,000 up overflows.
var columnNumeric = map[string]map[string]numericLimit{
	"appointment":         {"deposit_amount": {14, 2}, "heal_price": {14, 2}},
	"catalog_course_line": {"qty": {12, 2}},
	"catalog_item":        {"cost": {12, 2}, "list_price": {12, 2}, "sell_price": {12, 2}},
	"catalog_item_child":  {"qty": {12, 2}},
	"course_bought": {
		"estimated_material_cost": {14, 2}, "net_sales_value": {14, 2}, "price": {12, 2},
	},
	"course_bought_items": {
		"per_course": {12, 2}, "qty_total": {12, 2},
		"ref_price_snapshot": {18, 2}, "used": {12, 2},
	},
	"course_bought_usage": {"material_cost": {14, 2}, "sessions_used": {12, 2}},
	"courses":             {"price": {12, 2}},
	"invoice":             {"bill_discount": {12, 2}, "paid_amount": {12, 2}},
	"invoice_item":        {"discount": {12, 2}, "price": {12, 2}},
	"items":               {"cost": {12, 2}, "min_alert": {12, 2}, "price": {12, 2}},
	"loyalty_policy":      {"baht_per_point": {12, 4}},
	"receipt": {
		"card_charge": {12, 2}, "card_charge_percent": {6, 2}, "deposit_apply": {12, 2},
		"discount": {12, 2}, "gift_card_apply": {12, 2}, "paid_amount": {12, 2},
		"point_apply": {12, 2}, "vat_amount": {12, 2}, "vat_rate": {6, 2},
	},
	"receipt_payment": {"amount": {12, 2}},
	"slot_stock": {
		"amount": {14, 2}, "amount_total": {14, 2}, "cost_lot": {14, 2},
		"cost_per_unit": {14, 4}, "disbursement": {14, 2}, "remain": {14, 2},
	},
	"stock_movement": {
		"qty_delta": {10, 2}, "remain_after": {14, 2},
		"remain_before": {14, 2}, "unit_price": {12, 2},
	},
}

// columnIntegers is every smallint/integer column this importer writes. These overflow
// too, and a legacy id or a count copied straight through is exactly how.
var columnIntegers = map[string]map[string]string{
	"appointment":              {"booking_sequence": "integer", "seq": "integer"},
	"catalog_course":           {"buffet_count": "integer", "days": "integer", "loyalty_points": "integer", "periods": "integer"},
	"catalog_course_line":      {"sort_order": "integer"},
	"catalog_item":             {"status": "smallint"},
	"catalog_item_child":       {"sort_order": "integer"},
	"catalog_item_sku_counter": {},
	"catalog_service":          {"followup_days": "integer", "loyalty_points": "integer"},
	"course_bought": {
		"buffet_count": "integer", "financial_snapshot_version": "integer",
		"periods_total": "integer", "periods_used": "integer", "qty": "integer",
	},
	"course_bought_usage": {"material_cost_version": "integer"},
	"courses": {
		"buffet_count": "integer", "days": "integer", "months": "integer",
		"periods": "integer", "point": "integer", "status": "smallint",
	},
	"invoice_item":    {"qty": "integer", "sort_order": "integer", "tax_rate_bps": "integer"},
	"items":           {"units_per_package": "integer"},
	"loyalty_account": {"balance": "integer"},
	"loyalty_policy":  {"min_redeem": "integer", "referral_points": "integer"},
	"patient":         {"Braces_ID_int": "integer", "old_hn_int": "integer", "user_insert": "integer"},
	"photos":          {"created_by": "integer", "file_size": "integer", "visit_id": "integer"},
	"receipt":         {"installment_no": "integer", "point_earned": "integer", "point_spent": "integer"},
	"receipt_payment": {"payment_index": "smallint"},
	"sequences":       {"counter": "integer"},
}

// intBounds are the platform ranges. Values are compared as big.Int against these, so a
// legacy id with twenty digits cannot wrap on its way to being checked.
var intBounds = map[string]struct{ lo, hi string }{
	"smallint": {"-32768", "32767"},
	"integer":  {"-2147483648", "2147483647"},
}

// limitKind says which ceiling a value broke, so the refusal can explain itself.
type limitKind string

const (
	kindWidth   limitKind = "varchar"
	kindNumeric limitKind = "numeric"
	kindInteger limitKind = "integer"

	// kindScale is a value with MORE decimal places than its numeric(p,s) column keeps.
	//
	// The same silent rounding as kindFraction, one column over. numericIntegerDigits
	// counts only the digits left of the point, because that is what overflows — so a
	// price of 3.571428572 into numeric(12,2) passed every check in this file, and
	// Postgres stored 3.57.
	//
	// Measured on master-clinic (C9C89A4) 6 Oct 2026: bill 680028253 carried
	// 28 x 3.571428572, worth 100.000000016 at the source and 99.96 in the column. Its
	// receipt had collected ฿350 against a bill the database then thought was worth
	// ฿349.96, and guard 3 refused a 241,181-line script over four satang — after the
	// invoice, its items and its receipts had all been committed.
	//
	// planBillLines now folds a readable over-precise price into the line total, so this
	// is the backstop for everything it cannot fold and for every OTHER numeric column
	// this importer writes.
	kindScale limitKind = "numeric-scale"

	// kindFraction is a value with a fractional part written into a column that holds
	// whole numbers only.
	//
	// WHY IT IS A VIOLATION AND NOT A ROUNDING (owner's decision, 5 Oct 2026)
	//
	// Postgres does not refuse 0.2 into an integer column. It rounds, commits, and says
	// nothing. Measured on pinluck-clinic: six of 29,337 bill lines carried a fractional
	// bills.list[].amount, invoice_item.qty took them as 0, 1, 1, 1, 2 — and four
	// invoices ended up worth less than the receipts under them had collected while two
	// ended up worth MORE, showing patients ฿1,000 and ฿315 of debt that never existed.
	// The whole clinic was in production before anything complained.
	//
	// The range check above could not see it: integerFits deliberately compares only the
	// integer part, because a wide legacy id must not be called out of range over its
	// decimals. So nothing in this file looked at whether the value was whole.
	//
	// It covers every integer column this importer writes, not just the one that broke —
	// sort_order, tax_rate_bps, point_earned, point_spent, installment_no, payment_index,
	// file_size, visit_id, counter, Braces_ID_int, old_hn_int, user_insert, created_by.
	// Any of them would have been rounded just as quietly.
	kindFraction limitKind = "integer-fraction"
)

// limitViolation is one generated value that does not fit its column.
type limitViolation struct {
	table  string
	column string
	kind   limitKind
	limit  string // human form of the ceiling, e.g. "varchar(255)" or "numeric(14,2)"
	got    string // what was measured: "26286 characters" / "14 digits"
	sample string
}

// maxColumnsReported bounds the refusal text. Violations are grouped per column first, so
// this is a count of COLUMNS, not of rows: one bad column with 4,000 rows is one entry.
const maxColumnsReported = 12

// maxSamplesPerColumn is how many offending values are shown per column.
const maxSamplesPerColumn = 3

// limitCheckStats is what the scan saw, so "it found nothing" can be told apart from
// "it understood nothing".
type limitCheckStats struct {
	Statements int // INSERTs whose shape was read
	Skipped    int // INSERTs whose shape was not read, or whose arity disagreed
	Strings    int // string literals compared against a width
	Numbers    int // numeric literals compared against a precision or a range
}

// checkScriptColumnLimits refuses a script that Postgres would reject for a column limit.
func checkScriptColumnLimits(script string, rep *Report) error {
	violations, st := scanScriptColumnLimits(script)
	if rep != nil {
		rep.notef("ตรวจขีดจำกัดคอลัมน์: อ่านคำสั่ง INSERT ได้ %d คำสั่ง · เทียบข้อความ %d ค่า · เทียบตัวเลข %d ค่า · "+
			"ข้ามรูปแบบที่อ่านไม่ออก %d (ข้ามแล้วไม่ปฏิเสธ เพราะรูปแบบใหม่ที่ตัวตรวจไม่รู้จักไม่ควรบล็อกการนำเข้าจริง)",
			st.Statements, st.Strings, st.Numbers, st.Skipped)
	}
	if len(violations) == 0 {
		return nil
	}
	return fmt.Errorf("%s", describeLimitViolations(violations))
}

// scanScriptColumnLimits measures without deciding, so a test can assert on what was read
// as well as on what was refused.
//
// # FAIL CLOSED ON A VIOLATION, FAIL OPEN ON A PARSE
//
// A statement whose shape this does not understand is COUNTED and skipped, never reported
// as a violation, and so is any value that is not a bare literal — an expression, a cast,
// a subquery. Treating "I could not read this" as "this is wrong" would let a new
// statement shape block a real import, which is worse than the bug being prevented.
func scanScriptColumnLimits(script string) ([]limitViolation, limitCheckStats) {
	// Comments and dollar-quoted bodies are blanked first: an entity header or a plpgsql
	// block can contain the words "INSERT INTO" without being a statement this owns.
	// Blanking preserves length, so offsets into the result still address the original.
	masked := maskNonSQL(script)

	var violations []limitViolation
	var st limitCheckStats

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
		nums := columnNumeric[table]
		ints := columnIntegers[table]
		if len(widths) == 0 && len(nums) == 0 && len(ints) == 0 {
			continue // nothing on this table has a ceiling to check
		}
		for _, vals := range valueLists {
			if len(vals) != len(cols) {
				// Columns and values disagree, so there is no mapping to trust.
				st.Skipped++
				continue
			}
			for n, col := range cols {
				v := vals[n]

				if max, limited := widths[col]; limited {
					if text, isLit := literalText(v); isLit {
						st.Strings++
						// Postgres counts varchar(n) in CHARACTERS, so this must too: a
						// Thai character is three bytes, and counting bytes would refuse
						// values that fit.
						if got := len([]rune(text)); got > max {
							violations = append(violations, limitViolation{
								table: table, column: col, kind: kindWidth,
								limit:  fmt.Sprintf("varchar(%d)", max),
								got:    fmt.Sprintf("%d ตัวอักษร", got),
								sample: sampleForError(text),
							})
						}
					}
					continue
				}

				if lim, limited := nums[col]; limited {
					if digits, text, isNum := numericIntegerDigits(v); isNum {
						st.Numbers++
						if digits > lim.intDigits() {
							violations = append(violations, limitViolation{
								table: table, column: col, kind: kindNumeric,
								limit: fmt.Sprintf("numeric(%d,%d) — ค่าสูงสุดต้องน้อยกว่า 10^%d",
									lim.precision, lim.scale, lim.intDigits()),
								got:    fmt.Sprintf("%d หลักหน้าจุดทศนิยม", digits),
								sample: sampleForError(text),
							})
						}
						// And the other end of the same column. Postgres does not refuse
						// 3.571428572 into numeric(12,2) either: it rounds to the scale
						// and commits. See the note on kindScale.
						if extra, over := decimalsBeyondScale(text, lim.scale); over {
							violations = append(violations, limitViolation{
								table: table, column: col, kind: kindScale,
								limit: fmt.Sprintf("numeric(%d,%d) — เก็บทศนิยมได้ %d ตำแหน่ง",
									lim.precision, lim.scale, lim.scale),
								got: fmt.Sprintf("มีทศนิยม %d ตำแหน่ง (Postgres จะปัดเป็น %s)",
									extra, roundedToScaleForError(text, lim.scale)),
								sample: sampleForError(text),
							})
						}
					}
					continue
				}

				if kind, limited := ints[col]; limited {
					if _, text, isNum := numericIntegerDigits(v); isNum {
						st.Numbers++
						if b, ok := intBounds[kind]; ok && !integerFits(text, b.lo, b.hi) {
							violations = append(violations, limitViolation{
								table: table, column: col, kind: kindInteger,
								limit:  fmt.Sprintf("%s — ช่วงที่รับได้ %s ถึง %s", kind, b.lo, b.hi),
								got:    "เกินช่วง",
								sample: sampleForError(text),
							})
						}
						// An integer column cannot hold a fraction, and Postgres does not
						// say so: it rounds on the cast and commits. See the note on
						// kindFraction for the import that proved this the expensive way.
						if frac, fractional := fractionalPart(text); fractional {
							violations = append(violations, limitViolation{
								table: table, column: col, kind: kindFraction,
								limit:  fmt.Sprintf("%s — เก็บได้แต่จำนวนเต็ม", kind),
								got:    fmt.Sprintf("มีเศษ .%s (Postgres จะปัดให้เป็น %s)", frac, roundedForError(text)),
								sample: sampleForError(text),
							})
						}
					}
				}
			}
		}
	}
	return violations, st
}

// numericIntegerDigits reads a value that is EXACTLY one numeric literal and returns how
// many digits stand left of the decimal point. Anything else — an expression, a cast, a
// quoted string, now() — is not something to measure, and saying so is how this avoids
// inventing violations.
func numericIntegerDigits(v string) (digits int, text string, ok bool) {
	s := strings.TrimSpace(v)
	if s == "" {
		return 0, "", false
	}
	body := strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	if body == "" {
		return 0, "", false
	}
	intPart, fracPart := body, ""
	if dot := strings.IndexByte(body, '.'); dot >= 0 {
		intPart, fracPart = body[:dot], body[dot+1:]
	}
	if intPart == "" && fracPart == "" {
		return 0, "", false
	}
	for _, part := range []string{intPart, fracPart} {
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return 0, "", false
			}
		}
	}
	// Leading zeros are not significant digits: 0000123 is three digits wide.
	trimmed := strings.TrimLeft(intPart, "0")
	return len(trimmed), s, true
}

// fractionalPart returns the digits after the decimal point when they are not all zero.
//
// Trailing zeros do not count: 2.00 is the whole number two written long, and calling that
// a violation would refuse every money-shaped literal that happens to feed an integer
// column. .005 does count — it is the half-satang that Postgres would round away.
func fractionalPart(text string) (digits string, fractional bool) {
	s := strings.TrimSpace(text)
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return "", false
	}
	frac := s[dot+1:]
	if strings.Trim(frac, "0") == "" {
		return "", false
	}
	return frac, true
}

// roundedForError shows what the column would have ended up holding, so the refusal names
// the damage rather than only the input. Same rule as the database: half away from zero.
func roundedForError(text string) string {
	if q, ok := qtyAsWritten(text); ok {
		return q.String()
	}
	return "(อ่านค่าไม่ออก)"
}

// decimalsBeyondScale returns how many decimal places a literal really has, when that is
// more than the column keeps.
//
// Trailing zeros do not count, for the same reason they do not count in fractionalPart:
// 3.570 into numeric(12,2) is the same number the column will hold, and calling that a
// violation would refuse every money literal written long.
func decimalsBeyondScale(text string, scale int) (decimals int, over bool) {
	s := strings.TrimSpace(text)
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return 0, false
	}
	frac := strings.TrimRight(s[dot+1:], "0")
	if len(frac) <= scale {
		return len(frac), false
	}
	return len(frac), true
}

// roundedToScaleForError shows what a numeric(p,s) column will hold, so the refusal names
// the stored value next to the source one.
func roundedToScaleForError(text string, scale int) string {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(text))
	if !ok {
		return "(อ่านค่าไม่ออก)"
	}
	return r.FloatString(scale)
}

// integerFits compares a numeric literal against a column's range exactly, using big.Int
// so a twenty-digit legacy id cannot wrap on its way to being checked. A literal with a
// fractional part is rejected as out of range only when its integer part is — whether it
// is whole at all is kindFraction's question, checked separately.
func integerFits(text, lo, hi string) bool {
	s := text
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		s = s[:dot]
	}
	s = strings.TrimPrefix(s, "+")
	if s == "" || s == "-" {
		return true // nothing to compare; treated as unreadable rather than wrong
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return true
	}
	loV, _ := new(big.Int).SetString(lo, 10)
	hiV, _ := new(big.Int).SetString(hi, 10)
	return v.Cmp(loV) >= 0 && v.Cmp(hiV) <= 0
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

// describeLimitViolations groups every violation by column, so one bad column with four
// thousand rows reads as one entry with a count rather than four thousand lines.
//
// The kind is part of the grouping key, not just the column: one integer column can break
// two different rules at once — 3000000000.5 is both out of range and not whole — and
// merging those would print one entry under whichever rule happened to be seen first,
// with a count covering both. They are different problems with different fixes.
func describeLimitViolations(v []limitViolation) string {
	type group struct {
		key     string
		table   string
		column  string
		kind    limitKind
		limit   string
		count   int
		samples []string
		worst   string
	}
	byCol := map[string]*group{}
	var order []string
	for _, x := range v {
		key := x.table + "." + x.column + " · " + string(x.kind)
		g, seen := byCol[key]
		if !seen {
			g = &group{key: key, table: x.table, column: x.column, kind: x.kind, limit: x.limit}
			byCol[key] = g
			order = append(order, key)
		}
		g.count++
		if len(g.samples) < maxSamplesPerColumn {
			g.samples = append(g.samples, fmt.Sprintf("%q (%s)", x.sample, x.got))
		}
		g.worst = x.got
	}
	sort.Slice(order, func(a, b int) bool {
		if byCol[order[a]].count != byCol[order[b]].count {
			return byCol[order[a]].count > byCol[order[b]].count
		}
		return order[a] < order[b]
	})

	var b strings.Builder
	rows := 0
	for _, k := range order {
		rows += byCol[k].count
	}
	fmt.Fprintf(&b, "ไม่สร้างไฟล์: มีค่าเกินขีดจำกัดคอลัมน์ปลายทาง %d ค่า ใน %d คอลัมน์ — psql จะหยุดกลางทาง "+
		"และเพราะสคริปต์ถูกแบ่งเป็นหลายทรานแซกชัน กลุ่มที่ commit ไปแล้วจะค้างอยู่ในฐาน "+
		"(ต้องลบคลินิกก่อนลองใหม่) จึงปฏิเสธตั้งแต่ตอนนี้ · รายการทั้งหมดอยู่ข้างล่าง ไม่ใช่แค่ตัวแรก\n",
		rows, len(order))
	for n, k := range order {
		if n >= maxColumnsReported {
			fmt.Fprintf(&b, "  … และอีก %d คอลัมน์\n", len(order)-maxColumnsReported)
			break
		}
		g := byCol[k]
		fmt.Fprintf(&b, "  %s.%s เป็น %s · เกิน %d ค่า · ตัวอย่าง: %s\n",
			g.table, g.column, g.limit, g.count, strings.Join(g.samples, ", "))
	}
	b.WriteString("แก้ที่ตัวสร้าง หรือแก้ข้อมูลที่ต้นทาง: ค่าข้อความต้องผ่าน clip(value, n) ให้ตรงความกว้าง " +
		"และค่าตัวเลขที่ต้นทางใส่มาเกินจริง (เช่นพิมพ์ 9 รวด) ต้องตัดสินใจทีละเคสว่าจะตัด ตั้งศูนย์ หรือข้ามแถวนั้น " +
		"เพราะเป็นตัวเลขที่กระทบยอดเงินและยอดสต็อก")
	for _, k := range order {
		if byCol[k].kind == kindFraction {
			b.WriteString("\nค่าที่ \"มีเศษ\" เป็นเรื่องแยก: คอลัมน์นั้นเก็บจำนวนเต็มเท่านั้น และ Postgres จะไม่ปฏิเสธ " +
				"มันจะปัดแล้ว commit เงียบ ๆ ทำให้ตัวเลขในฐานไม่ใช่ตัวเลขของต้นทาง · " +
				"ถ้าเป็นจำนวนในบิล ให้แก้ที่ต้นทางเป็นจำนวนเต็มแล้วปรับราคาต่อหน่วยให้ยอดรวมเท่าเดิม " +
				"(เช่น จำนวน 0.2 ราคา 500 → จำนวน 1 ราคา 100) แล้วดึงข้อมูลใหม่")
			break
		}
	}
	for _, k := range order {
		if byCol[k].kind == kindScale {
			b.WriteString("\nค่าที่ \"มีทศนิยมเกิน\" เป็นปัญหาเดียวกันคนละคอลัมน์: Postgres ไม่ปฏิเสธ มันปัดตามทศนิยม " +
				"ที่คอลัมน์เก็บได้แล้ว commit เงียบ ๆ · ของบรรทัดในบิล เครื่องมือจะย้ายมูลค่าทั้งบรรทัดไปไว้ที่ราคา " +
				"ให้เองอยู่แล้ว (จำนวน × ราคา → จำนวน 1 ราคารวม) ที่มาถึงตรงนี้คือค่าที่ย้ายไม่ได้ " +
				"หรืออยู่ในคอลัมน์อื่นที่ไม่ใช่บรรทัดในบิล — ต้องแก้ที่ต้นทางหรือตัดสินใจทีละเคส " +
				"เพราะเป็นตัวเลขที่กระทบยอดเงินและยอดสต็อก")
			break
		}
	}
	return b.String()
}

// ── a very small amount of SQL reading ───────────────────────────────────────────
//
// Enough to find the values of an INSERT and no more. Everything here gives up rather
// than guesses: see the note on scanScriptColumnLimits.

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
