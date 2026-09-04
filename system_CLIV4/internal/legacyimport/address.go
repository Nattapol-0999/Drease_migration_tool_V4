package legacyimport

import (
	"fmt"
	"strings"

	"system_cliv4/internal/legacydump"
)

// Patient addresses: the legacy `patient_address` table onto V4's `patient_address`.
//
// WHY THIS WAS ONCE DECLARED IMPOSSIBLE, AND WHY THAT WAS WRONG
//
// This table was cut from an earlier phase on the reasoning that "the legacy rows store
// province / district / sub-district as numeric ids while V4 stores names, so nothing
// could be written without lookups nobody asked for". Both halves of that were false:
//
//   - V4's patient_address stores BOTH: province_id / amphure_id / district_id AND
//     province / district / sub_district as text (migration 0041).
//   - The ids resolve, because migration 0340 seeded _provinces / _amphures / _districts
//     into V4 from this very V2 dump with the ORIGINAL ids preserved. Its own header says
//     the reason out loud: "patient_address.*_id all reference them — the V2 patient
//     import must resolve to the same ids."
//
// Verified against the live database before this file was written:
//
//	province_id 2      -> สมุทรปราการ
//	amphure_id  54     -> บางพลี
//	district_id 110302 -> บางแก้ว, zip 10540
//
// which matches the free-text address the same legacy row carries, field for field.
//
// THE ONE FIELD THAT NEEDED UNPACKING
//
// address_sub_district holds TWO values joined by a semicolon: '110302;10540'. The first
// is the sub-district and the second its postcode. It looks like a packed composite key
// and was read as one for a while — but the legacy `_districts.id` is itself a
// varchar(6) official area code, so the first half IS the id, not a code needing
// translation. Splitting on ';' is all it takes.
//
// WHAT THE EXPORT ACTUALLY CONTAINS
//
// 559 rows, of which TWELVE hold an address. The legacy register form inserted a row for
// every patient whether or not anybody typed into it, so 542 are blank placeholders and 5
// are stubs the form left behind before a patient was saved (HN '00'). That is not a
// parsing failure; it is what the clinic recorded.
//
// WHY THIS ENTITY ALSO UPDATES, WHEN THE REST OF THE TOOL ONLY INSERTS
//
// Those same twelve HNs are ALREADY at the target, written by an earlier pipeline that
// carried the names and left every province_id / amphure_id / district_id NULL, plus half
// the postcodes. An insert-only pass would therefore write nothing at all and change
// nothing. So each insert is followed by a FILL-ONLY update: every column is
// COALESCE(NULLIF(col,''), new, col), which can only turn an empty column into a value
// and can never overwrite one — not even one a human corrected by hand after the earlier
// import.

// Address entity names. The fill pass is its own entity so the report can say how much
// of the work was new rows and how much was completing rows that already existed.
const (
	EntPatientAddress     = "patient_address"
	EntPatientAddressFill = "patient_address(เติมช่องที่ว่าง)"
)

var addressOrder = []string{EntPatientAddress, EntPatientAddressFill}

// addressRow is one legacy row resolved into the SQL expressions both passes share, so
// the INSERT and the fill UPDATE can never disagree about a value.
type addressRow struct {
	hn          string
	addressType string

	addressNo string
	village   string
	moo       string
	soi       string
	road      string

	provinceID string
	amphureID  string
	districtID string

	province    string
	district    string
	subDistrict string

	zip       string
	createdAt string
	updatedAt string
}

// buildPatientAddress maps the legacy address rows.
//
// The three area NAMES are not carried over from the legacy row — they are read at write
// time from V4's own _provinces / _amphures / _districts by id. The legacy row does carry
// names, but as free text a receptionist typed, so the seeded reference data is the more
// trustworthy source and it cannot drift from the ids stored beside it.
func buildPatientAddress(rows []legacydump.Row, in Input, rep *Report) (insertSQL, fillSQL string, err error) {
	e := rep.entity(EntPatientAddress)
	eFill := rep.entity(EntPatientAddressFill)
	e.SourceRows = len(rows)
	tr := truncations{}

	var (
		empty, noHN, dupHN  int
		inactiveWithContent int
		activeWithContent   int
		badID               int
		noArea              int
		droppedCurrent      int
		zipFromPacked       int
	)

	plans := make([]addressRow, 0, 16)
	seen := map[string]bool{}

	for n, r := range rows {
		hn := strings.TrimSpace(r.Get("HN"))
		content := addressHasContent(r)

		// A row with no HN cannot be filed against anybody. The legacy table also holds
		// placeholder rows the register form created before a patient was saved (HN '00'),
		// which the length test catches — an HN in this clinic is 9 digits, so anything
		// shorter is a stub, not a person.
		if hn == "" || len(hn) < 5 {
			if content {
				rep.warnf("patient_address row %d: HN %q ใช้ไม่ได้ แต่แถวนี้มีที่อยู่อยู่ — ไม่นำเข้า ต้องดูด้วยมือ", n+1, hn)
			}
			noHN++
			e.skip("HN ว่างหรือเป็นแถวค้างจากฟอร์ม (เช่น '00')")
			continue
		}
		if !content {
			empty++
			e.skip("ทุกช่องที่อยู่ว่าง (ต้นทางสร้างแถวไว้แต่ไม่เคยกรอก)")
			continue
		}

		addressType := "home"
		if !legacyRowActive(r) {
			inactiveWithContent++
			if !AddressImportInactive {
				e.skip("active='0' (ปิดไว้ในระบบเก่า)")
				continue
			}
			if AddressInactiveAsOther {
				addressType = "other"
			}
		} else {
			activeWithContent++
		}

		// V4 has no unique index here, so the dedup key is chosen: one address of each
		// type per patient. A second row of the same type in one file would overwrite the
		// first silently, so it is refused instead.
		key := hn + "\x00" + addressType
		if seen[key] {
			dupHN++
			e.skip("HN ซ้ำในไฟล์ (ที่อยู่ประเภทเดียวกันมากกว่าหนึ่งแถว)")
			rep.warnf("patient_address: HN %s มีที่อยู่ประเภท %q มากกว่าหนึ่งแถวในไฟล์ — นำเข้าแถวแรก ข้ามที่เหลือ", hn, addressType)
			continue
		}
		seen[key] = true

		provinceID, okP := intLit(r.Get("address_province"))
		amphureID, okA := intLit(r.Get("address_district"))
		districtRaw, zipPacked := splitSubDistrict(r.Get("address_sub_district"))
		districtID, okD := intLit(districtRaw)
		if !okP || !okA || !okD {
			badID++
		}
		if provinceID == "NULL" && amphureID == "NULL" && districtID == "NULL" {
			noArea++
		}

		// zip: the dedicated column first, then the half packed into
		// address_sub_district, then the sub-district's own postcode from V4's reference
		// data. Never invented.
		zip := strings.TrimSpace(r.Get("address_postcode"))
		if zip == "" && zipPacked != "" {
			zip = zipPacked
			zipFromPacked++
		}
		zipLit := "NULL"
		if zip != "" {
			v, cut := clip(zip, 16)
			tr.note("zip_code", cut)
			zipLit = lit(v)
		} else if districtID != "NULL" {
			zipLit = fmt.Sprintf("(SELECT zip_code::text FROM _districts WHERE id = %s)", districtID)
		}

		if strings.TrimSpace(r.Get("address_current")) != "" {
			droppedCurrent++
		}

		plans = append(plans, addressRow{
			hn:          hn,
			addressType: addressType,
			addressNo:   addrText(r, "address_no", 255, tr, "address_no"),
			village:     addrText(r, "address_vilabe_name", 255, tr, "village"),
			moo:         addrText(r, "address_moo", 64, tr, "moo"),
			soi:         addrText(r, "address_street", 255, tr, "soi"),
			road:        addrText(r, "address_road", 255, tr, "road"),
			provinceID:  provinceID,
			amphureID:   amphureID,
			districtID:  districtID,
			province:    areaName("_provinces", provinceID),
			district:    areaName("_amphures", amphureID),
			subDistrict: areaName("_districts", districtID),
			zip:         zipLit,
			createdAt:   tsLit(r.Get("created_at"), "now()"),
			updatedAt:   tsLit(firstNonEmpty(r.Get("updated_at"), r.Get("created_at")), "now()"),
		})
	}

	insertSQL = renderAddressInsert(plans, in, e)
	if AddressFillExistingRows {
		fillSQL = renderAddressFill(plans, in, eFill)
		eFill.SourceRows = len(plans)
	}

	rep.notef("patient_address: ชื่อจังหวัด/อำเภอ/ตำบล ไม่ได้คัดลอกจากต้นทาง แต่อ่านจาก _provinces/_amphures/_districts ของ V4 ด้วย id — ตารางอ้างอิงนั้นถูก seed จาก dump ชุดนี้เอง (migration 0340) จึงไม่มีทางเพี้ยนจาก id ที่เก็บคู่กัน")
	rep.notef("patient_address: address_sub_district ในต้นทางเก็บสองค่าคั่นด้วย ';' (เช่น '110302;10540') — ส่วนหน้าคือ id ตำบล ส่วนหลังคือรหัสไปรษณีย์")
	if activeWithContent > 0 || inactiveWithContent > 0 {
		rep.notef("patient_address: ต้นทางมีที่อยู่จริง %d แถวจาก %d — active='1' %d แถว · active='0' %d แถว",
			activeWithContent+inactiveWithContent, len(rows), activeWithContent, inactiveWithContent)
	}
	if inactiveWithContent > 0 && AddressImportInactive && !AddressInactiveAsOther {
		rep.notef("patient_address: %d แถวที่ active='0' นำเข้าเป็น address_type='home' เหมือนแถวอื่น — HN นั้นมีที่อยู่ 'home' อยู่ในปลายทางแล้วและเป็นที่อยู่เดียวกัน แยกเป็น 'other' จะได้ที่อยู่ซ้ำสองแถว",
			inactiveWithContent)
	}
	if AddressFillExistingRows {
		rep.warnf("patient_address: มีคำสั่ง UPDATE เติมช่องที่ว่างของแถวที่มีอยู่แล้ว (province_id / amphure_id / district_id / zip_code ฯลฯ) — เขียนเฉพาะช่องที่ว่างจริง ช่องที่มีค่าอยู่แล้วไม่ถูกแตะแม้แต่ช่องเดียว")
	}
	if empty > 0 {
		rep.notef("patient_address: ข้าม %d แถวที่ไม่ได้กรอกที่อยู่เลย (ต้นทางสร้างแถวไว้ให้คนไข้ทุกคน)", empty)
	}
	if noHN > 0 {
		rep.notef("patient_address: ข้าม %d แถวที่ HN ใช้ไม่ได้ (ว่าง หรือเป็นแถวค้างจากฟอร์ม เช่น '00')", noHN)
	}
	if dupHN > 0 {
		rep.warnf("patient_address: %d แถวเป็น HN ที่ซ้ำประเภทเดียวกัน — นำเข้าแถวแรกของแต่ละ HN เท่านั้น", dupHN)
	}
	if badID > 0 {
		rep.warnf("patient_address: %d แถวมี id จังหวัด/อำเภอ/ตำบล ที่ไม่ใช่ตัวเลข — ช่องนั้นเป็น NULL และชื่อพื้นที่จะว่างตามไป", badID)
	}
	if noArea > 0 {
		rep.warnf("patient_address: %d แถวไม่มี id พื้นที่เลย (มีแต่เลขที่บ้าน/ถนน) — นำเข้าได้ แต่ค้นหาตามจังหวัด/อำเภอจะไม่เจอ", noArea)
	}
	if zipFromPacked > 0 {
		rep.notef("patient_address: %d แถวไม่มีค่าใน address_postcode จึงใช้รหัสไปรษณีย์ที่อยู่ครึ่งหลังของ address_sub_district", zipFromPacked)
	}
	if droppedCurrent > 0 {
		rep.notef("patient_address: %d แถวมี address_current (ที่อยู่แบบเต็มบรรทัดที่ระบบเก่าประกอบไว้) — ไม่นำเข้า เพราะ V4 ไม่มีคอลัมน์รับ และข้อมูลย่อยทุกช่องถูกนำเข้าครบแล้ว จึงประกอบใหม่ได้",
			droppedCurrent)
	}
	rep.notef("patient_address: ID_Card ในตารางนี้ไม่นำเข้า — เลขบัตรอยู่ที่ตาราง patient แล้ว ไม่เก็บซ้ำในที่อยู่")
	if sum := tr.summary(); sum != "" {
		rep.warnf("patient_address: ตัดค่าที่ยาวเกินคอลัมน์ %s", sum)
	}
	return insertSQL, fillSQL, nil
}

var addressCols = []string{
	"clinic_code", `"HN"`, "address_type",
	"address_no", "village", "moo", "soi", "road",
	"sub_district", "district", "province",
	"province_id", "amphure_id", "district_id",
	"zip_code", "country", "created_at", "updated_at",
}

func renderAddressInsert(plans []addressRow, in Input, e *EntityResult) string {
	var s sqlBuilder
	for _, p := range plans {
		vals := []string{
			lit(in.ClinicCode), lit(p.hn), lit(p.addressType),
			p.addressNo, p.village, p.moo, p.soi, p.road,
			p.subDistrict, p.district, p.province,
			p.provinceID, p.amphureID, p.districtID,
			p.zip, lit("Thailand"), p.createdAt, p.updatedAt,
		}
		// EXISTS on patient is not decoration: patient_address has no foreign key to
		// patient, so a row whose HN never made it into this clinic would be an address
		// no screen can reach and no report can attribute.
		s.comment("HN %s · %s", p.hn, p.addressType)
		s.addf(`INSERT INTO patient_address (%s)
SELECT %s
WHERE EXISTS (
    SELECT 1 FROM patient p WHERE p.clinic_code = %s AND p."HN" = %s
)
  AND NOT EXISTS (
    SELECT 1 FROM patient_address a
     WHERE a.clinic_code = %s AND a."HN" = %s AND a.address_type = %s
);

`, joinCols(addressCols), joinCols(vals),
			lit(in.ClinicCode), lit(p.hn),
			lit(in.ClinicCode), lit(p.hn), lit(p.addressType))
	}
	e.Statements = s.stmts
	return s.String()
}

// renderAddressFill completes rows that already exist at the target.
//
// Every assignment is COALESCE(NULLIF(col,”), new, col) — three arguments, and the third
// is what makes this safe to state plainly:
//
//	col has a value            -> NULLIF returns it, kept
//	col empty, source has one  -> the source value is written
//	col empty, source empty    -> falls back to col, so the row is not touched at all
//
// It CANNOT overwrite. Re-running it changes nothing, which is why it does not break the
// idempotence the rest of this tool relies on.
func renderAddressFill(plans []addressRow, in Input, e *EntityResult) string {
	if len(plans) == 0 {
		return ""
	}
	text := func(col, newExpr string) string {
		return fmt.Sprintf("%s = COALESCE(NULLIF(a.%s, ''), %s, a.%s)", col, col, newExpr, col)
	}
	num := func(col, newExpr string) string {
		return fmt.Sprintf("%s = COALESCE(a.%s, %s)", col, col, newExpr)
	}

	var s sqlBuilder
	for _, p := range plans {
		sets := []string{
			text("address_no", p.addressNo),
			text("village", p.village),
			text("moo", p.moo),
			text("soi", p.soi),
			text("road", p.road),
			text("sub_district", p.subDistrict),
			text("district", p.district),
			text("province", p.province),
			num("province_id", p.provinceID),
			num("amphure_id", p.amphureID),
			num("district_id", p.districtID),
			text("zip_code", p.zip),
			text("country", lit("Thailand")),
			"updated_at = now()",
		}
		s.comment("เติมช่องที่ว่างของ HN %s · %s (ไม่แตะช่องที่มีค่าแล้ว)", p.hn, p.addressType)
		s.addf(`UPDATE patient_address a
   SET %s
 WHERE a.clinic_code = %s AND a."HN" = %s AND a.address_type = %s;

`, strings.Join(sets, ",\n       "), lit(in.ClinicCode), lit(p.hn), lit(p.addressType))
	}
	e.Statements = s.stmts
	return s.String()
}

// addrText renders one text column, recording truncation under the TARGET name so the
// report names the column an operator would look at.
func addrText(r legacydump.Row, source string, max int, tr truncations, target string) string {
	v := strings.TrimSpace(r.Get(source))
	if v == "" {
		return "NULL"
	}
	clipped, cut := clip(v, max)
	tr.note(target, cut)
	return lit(clipped)
}

// areaName resolves an area name from V4's reference data by id, or NULL when there is no
// id. The lookup happens on the server so the name always agrees with the id beside it —
// copying the legacy free-text name would let the two disagree.
func areaName(table, idExpr string) string {
	if idExpr == "NULL" {
		return "NULL"
	}
	return fmt.Sprintf("(SELECT name_th FROM %s WHERE id = %s)", table, idExpr)
}

// splitSubDistrict unpacks 'districtId;zip'. A value with no ';' is taken as the id
// alone, which is what rows written before the postcode was appended look like.
func splitSubDistrict(v string) (districtID, zip string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", ""
	}
	id, rest, found := strings.Cut(v, ";")
	if !found {
		return strings.TrimSpace(v), ""
	}
	return strings.TrimSpace(id), strings.TrimSpace(rest)
}

// addressHasContent reports whether the row holds an address at all. The legacy register
// form inserted one row per patient up front, so most rows are placeholders.
func addressHasContent(r legacydump.Row) bool {
	for _, f := range []string{
		"address_no", "address_moo", "address_vilabe_name", "address_street",
		"address_road", "address_province", "address_district", "address_sub_district",
		"address_postcode", "address_current",
	} {
		if strings.TrimSpace(r.Get(f)) != "" {
			return true
		}
	}
	return false
}

// legacyRowActive reads the legacy enum('1','0'). Absent counts as active, matching the
// column's own DEFAULT '1'.
func legacyRowActive(r legacydump.Row) bool {
	v := strings.TrimSpace(r.Get("active"))
	return v == "" || v == "1"
}
