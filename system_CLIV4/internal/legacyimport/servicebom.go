package legacyimport

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"system_cliv4/internal/mongodump"
)

/*
Service recipes: mris.listheal_medicine onto catalog_item_child with relation_kind='bom'.

# WHAT THE SOURCE LOOKS LIKE

Measured on Anjali's V2 (24 Oct 2026), four documents in all:

	{ _id, listheal_id: ObjectId(→ listheals._id),
	  medicine: [ { _id, item_id: ObjectId(→ items._id), name, unit, numoftime: 1, fixed: false } ],
	  created_at, updated_at }

One document per service. item_id names one `items` DOCUMENT, and V2 keeps one of those per
(code × stock room), so it is resolved to its CODE — the SKU V4 holds once.

# WHAT V4 DOES WITH AN EDGE

A bom edge is a stock rule, not a description. Every time the service is billed or used out
of a course, checkout draws each material from the shelf (FEFO), and it refuses the WHOLE
bill when one of them cannot be drawn: a material that is missing or switched off, or one
the shelf cannot cover unless the clinic turned on stock_autofill_on_short
(billing/stock_routing.go, catalog/bom_broken.go). So an edge is written only where V4's own
recipe editor (replaceServiceBOMTx) would have accepted it:

  - the parent is that service; the child a drug/goods SKU that is live (status 1 or 2, no
    code_conflict) AND whose items row is active. The editor refuses anything else, and
    checkout reads a switched-off material as missing, which would make the service
    unsellable;
  - qty > 0 with at most two decimals (catalog_item_child.qty numeric(12,2), CHECK qty > 0);
  - one row per material (uniq_catalog_item_child, and the editor refuses a duplicate). The
    first one wins, the rule the course lines keep (owner's decision 3ข, 24 Oct 2026).

qty is numoftime, the amount of that material one use of the service takes (1 cc of filler
for one filler session). `fixed` has no V4 counterpart and is not used; the report counts it
so a human can check what it meant.

# A SERVICE THAT ALREADY HAS A RECIPE IN V4 IS LEFT ALONE

One statement per service, written only while that service has no bom edge at all. An
insert-only edge would re-add, on every re-run, a material the clinic has since removed on
screen, and a re-added material is a stock draw nobody asked for. The NOT EXISTS sits in the
same statement as the rows, so a service's materials land together or not at all.

Imported by the owner's decision 4 of 24 Oct 2026 (source: mris.listheals for the service,
mris.listheal_medicine for the drugs bound to it).
*/

// bomMaterial is one planned edge of a recipe.
type bomMaterial struct {
	code  string
	qty   string
	order int
}

// buildServiceBOM renders the recipes. It needs the services' WRITTEN codes, so it runs
// after buildCatalogServices, which re-codes duplicates in mc.healCode.
func buildServiceBOM(mc *mongoCatalog, in Input, rep *Report) string {
	if len(mc.listhealMedicine) == 0 {
		return ""
	}
	e := rep.entity(EntCatalogServiceBOM)
	e.SourceRows = len(mc.listhealMedicine)

	itemCodeByID := map[string]string{}
	for _, it := range mc.items {
		if id := docObjectID(it, "_id"); id != "" {
			if c := it.Text("code"); c != "" {
				itemCodeByID[id] = c
			}
		}
	}

	// The newest document per service, when a service has more than one: it is the recipe
	// V2 showed last. Ties keep the later one in the file, so the choice is stable.
	type recipeDoc struct {
		doc mongodump.Doc
		at  time.Time
	}
	newest := map[string]recipeDoc{}
	withService := 0
	for _, d := range mc.listhealMedicine {
		svcID := docObjectID(d, "listheal_id")
		if svcID == "" {
			e.skip("เอกสารไม่มี listheal_id")
			continue
		}
		withService++
		at, ok := docTime(d, "updated_at")
		if !ok {
			at, _ = docTime(d, "created_at")
		}
		if prev, seen := newest[svcID]; seen && at.Before(prev.at) {
			continue
		}
		newest[svcID] = recipeDoc{doc: d, at: at}
	}
	superseded := withService - len(newest)

	ids := make([]string, 0, len(newest))
	for id := range newest {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var s sqlBuilder
	clinicLit := lit(in.ClinicCode)
	var plannedCodes []string
	services, materials := 0, 0
	noService, noItem, badQty, duplicate, fixed, empty := 0, 0, 0, 0, 0, 0
	var rounded []string

	for _, svcID := range ids {
		d := newest[svcID].doc
		svcCode := mc.healCode[svcID]
		if svcCode == "" {
			noService++
			e.skip("listheal_id ไม่ตรงบริการใน listheals")
			continue
		}
		var mats []bomMaterial
		seen := map[string]bool{}
		for _, m := range docArray(d, "medicine") {
			if docBool(m, "fixed") {
				fixed++
			}
			code := itemCodeByID[docObjectID(m, "item_id")]
			if code == "" {
				noItem++
				e.skip("วัสดุ: item_id ไม่ตรง items ใด")
				continue
			}
			qty, roundedFrom, ok := bomQty(m.Text("numoftime"))
			if !ok {
				badQty++
				e.skip("วัสดุ: numoftime ไม่ใช่จำนวนที่มากกว่า 0")
				continue
			}
			if seen[code] {
				duplicate++
				e.skip("วัสดุซ้ำในสูตรเดียวกัน — ใช้แถวแรก")
				continue
			}
			seen[code] = true
			if roundedFrom != "" {
				rounded = append(rounded, fmt.Sprintf("%s → %s %s เป็น %s", svcCode, code, roundedFrom, qty))
			}
			mats = append(mats, bomMaterial{code: code, qty: qty, order: len(mats)})
		}
		if len(mats) == 0 {
			empty++
			e.skip("สูตรไม่มีวัสดุที่ใช้ได้")
			continue
		}
		services++
		materials += len(mats)
		plannedCodes = append(plannedCodes, svcCode)

		rows := make([]string, len(mats))
		names := make([]string, len(mats))
		for i, m := range mats {
			rows[i] = fmt.Sprintf("(%s, %s, %d)", lit(m.code), m.qty, m.order)
			names[i] = fmt.Sprintf("%s ×%s", m.code, m.qty)
		}
		s.comment("%s · %s → %s", svcCode, oneLine(mc.healName[svcID]), strings.Join(names, " · "))
		s.addf(`INSERT INTO catalog_item_child
    (clinic_code, parent_item_id, child_item_id, qty, sort_order, relation_kind, created_at)
SELECT %[1]s, svc.id, mat.id, v.qty, v.ord, 'bom', now()
  FROM (SELECT id FROM catalog_item
         WHERE clinic_code = %[1]s AND code = %[2]s AND item_type = 'service'
           AND status IN (1, 2) AND NOT code_conflict
         ORDER BY id LIMIT 1) svc
 CROSS JOIN (VALUES %[3]s) AS v(code, qty, ord)
  JOIN LATERAL (
       SELECT ci.id FROM catalog_item ci
         JOIN items i ON i.clinic_code = ci.clinic_code AND i.id = ci.source_id AND i.active
        WHERE ci.clinic_code = %[1]s AND ci.code = v.code AND ci.source_table = 'items'
          AND ci.item_type IN ('drug', 'goods') AND ci.status IN (1, 2) AND NOT ci.code_conflict
        ORDER BY ci.id LIMIT 1) mat ON true
 WHERE mat.id <> svc.id
   AND NOT EXISTS (SELECT 1 FROM catalog_item_child b
                    WHERE b.clinic_code = %[1]s AND b.parent_item_id = svc.id
                      AND b.relation_kind = 'bom');

`, clinicLit, lit(svcCode), strings.Join(rows, ", "))
	}

	if len(plannedCodes) > 0 {
		quoted := make([]string, len(plannedCodes))
		for i, c := range plannedCodes {
			quoted[i] = lit(c)
		}
		// Said by the database after the fact: which of the planned edges actually landed is
		// decided at load time (a material switched off in V4, a recipe already set on screen).
		s.addf(`DO $bom$
DECLARE n_edges bigint; n_services bigint;
BEGIN
    SELECT count(*), count(DISTINCT b.parent_item_id) INTO n_edges, n_services
      FROM catalog_item_child b
      JOIN catalog_item p ON p.clinic_code = b.clinic_code AND p.id = b.parent_item_id
     WHERE b.clinic_code = %[1]s AND b.relation_kind = 'bom' AND p.item_type = 'service'
       AND p.code = ANY (ARRAY[%[2]s]::text[]);
    RAISE NOTICE 'สูตรวัสดุของบริการ: มีอยู่ %% บริการ · %% วัสดุ (แผนจาก V2 %d บริการ · %d วัสดุ) — ส่วนที่ขาดคือวัสดุที่ V4 ปิดใช้งานหรือไม่ใช่สต็อก หรือบริการที่มีสูตรใน V4 อยู่ก่อนแล้วจึงไม่แตะ',
        n_services, n_edges;
END $bom$;

`, clinicLit, strings.Join(quoted, ", "), services, materials)
	}

	e.Statements = s.stmts

	rep.notef("catalog_item_child(bom): สูตรวัสดุของบริการจาก listheal_medicine — %d บริการ · %d วัสดุ · "+
		"จำนวน = numoftime (ปริมาณต่อการใช้บริการ 1 ครั้ง) · ลงเฉพาะวัสดุที่เป็นยา/เวชภัณฑ์ที่เปิดใช้งาน "+
		"และเฉพาะบริการที่ยังไม่มีสูตรใน V4 · ข้าม [ไม่ตรงบริการ: %d, item_id ไม่ตรง items: %d, จำนวนไม่ถูกต้อง: %d, "+
		"วัสดุซ้ำ: %d, สูตรว่าง: %d, เอกสารเก่าที่ถูกแทน: %d]",
		services, materials, noService, noItem, badQty, duplicate, empty, superseded)
	if services > 0 {
		rep.warnf("catalog_item_child(bom): หลังนำเข้า บริการ %d รายการที่มีสูตรจะตัดวัสดุออกจากคลังทุกครั้งที่รับชำระหรือตัดคอร์ส — "+
			"ถ้าวัสดุไม่พอ และคลินิกไม่ได้เปิดกฎ 'ตัดใช้ได้แม้สต็อกไม่พอ' (stock_autofill_on_short) V4 จะรับชำระบิลนั้นไม่ได้ทั้งใบ",
			services)
	}
	if fixed > 0 {
		rep.notef("catalog_item_child(bom): %d วัสดุตั้ง fixed=true ใน V2 — V4 ไม่มีค่าที่ตรงกันจึงไม่ได้ใช้ · ตรวจว่าใน V2 หมายถึงอะไร", fixed)
	}
	if len(rounded) > 0 {
		rep.warnf("catalog_item_child(bom): %d วัสดุมีจำนวนทศนิยมเกิน 2 ตำแหน่ง — ปัดแบบเดียวกับที่ Postgres ปัด: %s",
			len(rounded), strings.Join(capList(rounded, 25), " · "))
	}
	return s.String()
}

// bomQty reads numoftime as a quantity for catalog_item_child.qty (numeric(12,2), > 0).
// A value with more than two decimals is rounded exactly as the column would round it, and
// the original text is returned so the report can name it.
func bomQty(text string) (qty, roundedFrom string, ok bool) {
	t := strings.TrimSpace(text)
	if t == "" || !validNumeric(t) || !positiveMoney(t) {
		return "", "", false
	}
	if _, over := decimalsBeyondScale(t, 2); over {
		r, rok := roundMoneyToScale(t, 2)
		if !rok || !positiveMoney(r) {
			return "", "", false
		}
		return r, t, true
	}
	// Normalise "+1" and similar to a plain literal; validNumeric accepts a leading sign.
	if f, err := strconv.ParseFloat(t, 64); err != nil || f <= 0 {
		return "", "", false
	}
	return strings.TrimPrefix(t, "+"), "", true
}
