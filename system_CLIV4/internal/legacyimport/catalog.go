package legacyimport

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"system_cliv4/internal/mongodump"
)

// Catalog import: the Mongo half of a clinic's export (dbmongo/*.js) onto V4's
// canonical catalog spine.
//
// WHAT MAPS WHERE
//
//	category      → catalog_category
//	items         → items + catalog_item(item_type='drug') + slot_stock (one per lot)
//	listheals     → catalog_item(item_type='service') + catalog_service
//	course_items  → catalog_item(item_type='course') + catalog_course + catalog_course_line
//
// `catalog_item` is the identity every other table points at, so it is written for
// every sellable thing — a row in `items` alone cannot be sold.
//
// The legacy `listheal` tables are NOT written. They are frozen rollback evidence
// (see docs/catalog-service-retirement.md); services live in catalog_item +
// catalog_service now.
//
// MONEY stays out of this file: bills / payment_summary / course_bills are a later
// phase with decisions of their own.

// Catalog entity names, in write order. catalog_category and the services come first
// because the rows after them resolve foreign keys by looking them up.
const (
	EntCatalogCategory   = "catalog_category"
	EntItems             = "items"
	EntCatalogDrug       = "catalog_item(drug)"
	EntSlotStock         = "slot_stock"
	EntCatalogService    = "catalog_item(service)"
	EntCatalogCourse     = "catalog_item(course)"
	EntCatalogCourseLine = "catalog_course_line"
)

var catalogOrder = []string{
	EntCatalogCategory,
	EntCatalogService,
	EntItems,
	EntCatalogDrug,
	EntSlotStock,
	EntCatalogCourse,
	EntCatalogCourseLine,
}

// V4 catalog_item.status (internal/controller/catalog/status.go).
const (
	statusDeleted int = 0
	statusActive  int = 1
	statusDraft   int = 2
)

// catalogStatus mirrors catalogStatusForItem in the app: draft when there is no
// selling price, or when a drug's label is incomplete. Draft means receivable into
// stock but NOT sellable — the locked rule for incomplete drug data.
func catalogStatus(itemType string, sellPrice string, labelComplete bool) int {
	if !positiveMoney(sellPrice) {
		return statusDraft
	}
	if itemType == "drug" && !labelComplete {
		return statusDraft
	}
	return statusActive
}

// mongoCatalog is one parsed export, indexed for lookup.
type mongoCatalog struct {
	items        []mongodump.Doc
	listheals    []mongodump.Doc
	courseItems  []mongodump.Doc
	categoryName map[string]string // _id → name
	kindName     map[string]string // _id → name
	itemTypeName map[string]string // _id → name
	itemUnitName map[string]string // _id → name
	// No stockBranch index here on purpose. Where a lot lives is decided by
	// branch.stock_id in the SQL `branch` table (see stockOwners), not by the
	// stocks.stock_branch string — keeping a second, weaker source of the same answer is
	// how the two drift.
	healCode map[string]string // listheals _id → code (LHxxx)

	// The money half of the same file. Parsed here so a 26 MB export is read ONCE;
	// mapped in money.go, which is where the decisions about it live.
	bills    []mongodump.Doc
	payments []mongodump.Doc

	// courseBills is the customers' remaining course entitlement — what they paid for
	// and have not used yet. Mapped in coursebought.go.
	courseBills []mongodump.Doc

	// courseCode is course_items _id → the code MINTED for it (CS001…). It only helps an
	// entitlement find its course when the CATALOG was imported by this same tool; a
	// target whose catalog came from somewhere else has different codes, which is why
	// resolveCourseCatalogID falls back to matching by name.
	courseCode map[string]string
	// healName is listheals _id → display name, for course_bought_items.name (NOT NULL).
	healName map[string]string
	// courseName is course_items _id → its catalog name. This is what lets an
	// entitlement find the SAME course at a target whose codes do not line up.
	courseName map[string]string
}

func indexNames(cols map[string][]mongodump.Doc, name, field string) map[string]string {
	out := map[string]string{}
	for _, d := range cols[name] {
		id := docObjectID(d, "_id")
		v := d.Text(field)
		if id != "" && v != "" {
			out[id] = v
		}
	}
	return out
}

// parseMongoCatalog reads the export and indexes the lookup collections. Nothing is
// mapped here; that is buildCatalog*'s job.
func parseMongoCatalog(body []byte) (*mongoCatalog, error) {
	cols, err := mongodump.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("mongo dump: %w", err)
	}
	byName := map[string][]mongodump.Doc{}
	for _, c := range cols {
		byName[c.Name] = append(byName[c.Name], c.Docs...)
	}

	mc := &mongoCatalog{
		items:        byName["items"],
		listheals:    byName["listheals"],
		courseItems:  byName["course_items"],
		categoryName: indexNames(byName, "category", "name"),
		kindName:     indexNames(byName, "kind", "name"),
		itemTypeName: indexNames(byName, "item_types", "name"),
		itemUnitName: indexNames(byName, "item_units", "name"),

		healCode:    map[string]string{},
		bills:       byName["bills"],
		payments:    byName["payment_summary"],
		courseBills: byName["course_bills"],
		courseCode:  map[string]string{},
		courseName:  map[string]string{},
		healName:    map[string]string{},
	}
	for _, d := range mc.listheals {
		id := docObjectID(d, "_id")
		if id == "" {
			continue
		}
		if code := d.Text("code"); code != "" {
			mc.healCode[id] = code
		}
		if n := firstNonEmpty(d.Text("name_th"), d.Text("name_en")); n != "" {
			mc.healName[id] = n
		}
	}
	if len(mc.items) == 0 && len(mc.listheals) == 0 && len(mc.courseItems) == 0 && len(mc.bills) == 0 {
		return nil, fmt.Errorf("mongo dump ไม่มี collection ที่รองรับเลย (ต้องมีอย่างน้อยหนึ่ง: items, listheals, course_items, bills)")
	}
	return mc, nil
}

// docObjectID reads a field that must be an ObjectId, as hex text.
func docObjectID(d mongodump.Doc, path string) string {
	v, ok := d.Get(path)
	if !ok || v == nil {
		return ""
	}
	if oid, isOID := v.(mongodump.ObjectID); isOID {
		return string(oid)
	}
	return ""
}

// buildCatalogCategory writes the category master. Idempotent on the unique index
// (clinic_code, lower(trim(name))) WHERE status = 1.
func buildCatalogCategory(mc *mongoCatalog, in Input, rep *Report) string {
	names := make([]string, 0, len(mc.categoryName))
	for _, n := range mc.categoryName {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic output
	if len(names) == 0 {
		return ""
	}

	e := rep.entity(EntCatalogCategory)
	e.SourceRows = len(names)
	var s sqlBuilder
	for i, n := range names {
		nameLit, _ := clip(n, 255)
		s.addf(`INSERT INTO catalog_category (clinic_code, name, sort_order, status, created_at, updated_at)
VALUES (%s, %s, %d, 1, now(), now())
ON CONFLICT (clinic_code, lower(btrim(name))) WHERE status = 1 DO NOTHING;

`, lit(in.ClinicCode), lit(nameLit), i)
	}
	e.Statements = s.stmts
	return s.String()
}

// categoryIDByName resolves catalog_category.id in SQL: its id is generated by the
// database, so the reference cannot be known while rendering the file.
func categoryIDByName(clinic, name string) string {
	if strings.TrimSpace(name) == "" {
		return "NULL"
	}
	return fmt.Sprintf(
		"(SELECT id FROM catalog_category WHERE clinic_code = %s AND lower(btrim(name)) = lower(btrim(%s)) AND status = 1 LIMIT 1)",
		lit(clinic), lit(name))
}

// catalogItemIDByCode resolves catalog_item.id for a code, the same way.
func catalogItemIDByCode(clinic, code string) string {
	return fmt.Sprintf(
		"(SELECT id FROM catalog_item WHERE clinic_code = %s AND code = %s AND status IN (1,2) AND NOT code_conflict LIMIT 1)",
		lit(clinic), lit(code))
}

// upsertCatalogItem renders the canonical catalog_item upsert. Every sellable thing
// goes through here so there is one shape to review.
//
// sourceTable/sourceID tie a drug back to its `items` row the way the app's own
// CreateSKU does; services and courses carry the default 'catalog'.
func upsertCatalogItem(clinic, code, itemType, nameTh, nameEn, categoryName,
	sellPrice, cost string, status int, sourceTable, sourceIDExpr, createdAt string) string {
	if sourceTable == "" {
		sourceTable = "catalog"
	}
	if sourceIDExpr == "" {
		sourceIDExpr = "NULL"
	}
	if createdAt == "" {
		createdAt = "now()"
	}
	return fmt.Sprintf(`INSERT INTO catalog_item
    (clinic_code, code, item_type, name_th, name_en, category_id,
     list_price, sell_price, cost, source_table, source_id, code_conflict, status,
     created_at, updated_at)
VALUES (%s, %s, %s, %s, %s, %s,
        %s, %s, %s, %s, %s, false, %d,
        %s, now())
ON CONFLICT (clinic_code, code) WHERE status IN (1, 2) AND NOT code_conflict
DO UPDATE SET item_type  = EXCLUDED.item_type,
              name_th    = EXCLUDED.name_th,
              name_en    = EXCLUDED.name_en,
              category_id = COALESCE(EXCLUDED.category_id, catalog_item.category_id),
              list_price = EXCLUDED.list_price,
              sell_price = EXCLUDED.sell_price,
              cost       = EXCLUDED.cost,
              status     = EXCLUDED.status,
              updated_at = now();

`, lit(clinic), lit(code), lit(itemType), lit(nameTh), nullLit(nameEn),
		categoryIDByName(clinic, categoryName),
		moneyLit(sellPrice), moneyLit(sellPrice), moneyLit(cost),
		lit(sourceTable), sourceIDExpr, status, createdAt)
}

// buildCatalogServices maps listheals onto catalog_item(service) + catalog_service.
func buildCatalogServices(mc *mongoCatalog, in Input, rep *Report) (string, error) {
	if len(mc.listheals) == 0 {
		return "", nil
	}
	e := rep.entity(EntCatalogService)
	e.SourceRows = len(mc.listheals)

	// healCode maps a listheal's ObjectId to the code actually WRITTEN, which differs
	// from the source code for a re-coded duplicate. Course lines resolve their child
	// through this map, so a course keeps pointing at the right service.
	healCodeOverride := mc.healCode

	// Every code in the collection, so a duplicate can be moved to a number that is
	// genuinely free rather than to the next one that merely looks free.
	used := map[string]bool{}
	for _, d := range mc.listheals {
		if c := d.Text("code"); c != "" {
			used[c] = true
		}
	}

	var s sqlBuilder
	seen := map[string]bool{}
	draft := 0
	for n, d := range mc.listheals {
		code := d.Text("code")
		if code == "" {
			return "", fmt.Errorf("listheals doc %d: ไม่มี code (ใช้เป็นคีย์)", n+1)
		}
		if seen[code] {
			// Two services claiming one code. Refusing loses the whole import; silently
			// merging would make one service disappear into the other. Re-coding keeps
			// both and says so.
			if !RecodeDuplicateServiceCodes {
				return "", fmt.Errorf("listheals: code %q ซ้ำในไฟล์ — (clinic_code, code) ต้องไม่ซ้ำ", code)
			}
			next := nextFreeCode(code, used)
			if next == "" {
				return "", fmt.Errorf("listheals: code %q ซ้ำ และหารหัสว่างถัดไปของ prefix นี้ไม่ได้", code)
			}
			rep.warnf("listheals: code %q ซ้ำในไฟล์ — ย้ายรายการที่ซ้ำ (%s) ไปเป็น %q · รหัสนี้ต่างจากต้นทาง ต้องแจ้งคลินิก",
				code, oneLine(firstNonEmpty(d.Text("name_th"), d.Text("name_en"))), next)
			used[next] = true
			code = next
		}
		seen[code] = true
		healCodeOverride[docObjectID(d, "_id")] = code

		nameTh := firstNonEmpty(d.Text("name_th"), d.Text("name_en"), code)
		nameEn := d.Text("name_en")
		if nameEn == "-" {
			nameEn = ""
		}
		price := d.Text("cost") // legacy `cost` on a service is the price charged
		status := catalogStatus("service", price, true)
		if status == statusDraft {
			draft++
		}
		category := mc.categoryName[docObjectID(d, "category")]

		nameThLit, _ := clip(nameTh, 500)
		nameEnLit, _ := clip(nameEn, 500)
		s.comment("%s · %s", code, oneLine(nameTh))
		s.addf("%s", upsertCatalogItem(in.ClinicCode, code, "service", nameThLit, nameEnLit,
			category, price, "0", status, "catalog", "NULL", tsLit(d.Text("createdAt"), "now()")))

		// catalog_service carries the service policy. service_kind stays NULL: the
		// legacy `kind` is free text ("หัตถการ"/"Lab"/"อื่นๆ") with no V4 kind row to
		// point at, and inventing one would put a guess in the policy table.
		s.addf(`INSERT INTO catalog_service
    (catalog_item_id, clinic_code, doctor_fee_eligible, followup_enabled, followup_days,
     loyalty_points, service_kind_id, service_kind_key, created_at, updated_at)
SELECT %s, %s, %s, false, 0, %d, NULL, NULL, now(), now()
WHERE %s IS NOT NULL
ON CONFLICT (catalog_item_id) DO UPDATE SET
    doctor_fee_eligible = EXCLUDED.doctor_fee_eligible,
    loyalty_points      = EXCLUDED.loyalty_points,
    updated_at          = now();

`, catalogItemIDByCode(in.ClinicCode, code), lit(in.ClinicCode),
			boolLit(docBool(d, "isDoctorFee")), intFromMoney(d.Text("point")),
			catalogItemIDByCode(in.ClinicCode, code))
	}

	e.Statements = s.stmts
	if draft > 0 {
		rep.warnf("catalog_item(service): %d รายการไม่มีราคา → status=draft (ขายไม่ได้จนกว่าจะใส่ราคา)", draft)
	}
	if kinds := distinctValues(mc.kindName); len(kinds) > 0 {
		rep.notef("listheals: legacy kind (%s) ไม่ได้ import — V4 ไม่มีแถว service_kind ให้อ้าง จึงปล่อย NULL ไม่เดา",
			strings.Join(kinds, ", "))
	}
	return s.String(), nil
}

// buildItems maps the Mongo `items` collection onto three tables.
//
// One SKU can appear several times in the export, once per stock location, so the
// collection is folded: the first document defines the SKU (items + catalog_item) and
// EVERY document becomes a slot_stock lot keyed by its Mongo _id.
func buildItems(mc *mongoCatalog, in Input, rep *Report) (itemsSQL, catalogSQL, slotSQL string, err error) {
	br := newBranchResolver(in)
	defer br.report(rep, "slot_stock")
	if len(mc.items) == 0 {
		return "", "", "", nil
	}
	eItems := rep.entity(EntItems)
	eCatalog := rep.entity(EntCatalogDrug)
	eSlot := rep.entity(EntSlotStock)
	eItems.SourceRows = len(mc.items)
	eSlot.SourceRows = len(mc.items)

	// Which branch owns which stock, from the SQL side (branch.stock_id). Read before the
	// loop because every lot needs it, and a broken branch table must stop the build rather
	// than silently make every lot look central.
	stockOwner, oerr := stockOwners(in)
	if oerr != nil {
		return "", "", "", oerr
	}

	var sItems, sCatalog, sSlot sqlBuilder
	firstSeen := map[string]bool{}
	noBranch, draft := 0, 0
	// mainStockLots counts lots in stocks no branch claims; untickedLots counts, per legacy
	// branch, the lots dropped because that branch is not part of this clinic.
	mainStockLots := 0
	untickedLots := map[string]int{}

	for n, d := range mc.items {
		code := d.Text("code")
		if code == "" {
			return "", "", "", fmt.Errorf("items doc %d: ไม่มี code (ใช้เป็นคีย์)", n+1)
		}
		mongoID := docObjectID(d, "_id")
		name := firstNonEmpty(joinName(d.Text("name1"), d.Text("name2")), code)
		unit := mc.itemUnitName[docObjectID(d, "unit")]
		category := mc.itemTypeName[docObjectID(d, "type")]
		price := d.Text("price")
		cost := d.Text("cost")

		if !firstSeen[code] {
			firstSeen[code] = true

			nameLit, _ := clip(name, 255)
			unitLit, _ := clip(unit, 64)
			catLit, _ := clip(category, 64)

			// Drug label fields stay EMPTY on purpose. The export has no
			// generic_name/strength/dosage_form/indication/warning, and filling them
			// from a product name would be inventing clinical text. Empty keeps the
			// SKU in draft, which is exactly "receivable, not sellable".
			sItems.comment("%s · %s", code, oneLine(name))
			sItems.addf(`INSERT INTO items
    (clinic_code, code, name, name_en, category, unit, cost, price, active,
     source_key, created_at, updated_at)
VALUES (%s, %s, %s, NULL, %s, %s, %s, %s, %s,
        %s, %s, now())
ON CONFLICT (clinic_code, code) WHERE active DO UPDATE SET
    name       = EXCLUDED.name,
    category   = EXCLUDED.category,
    unit       = EXCLUDED.unit,
    cost       = EXCLUDED.cost,
    price      = EXCLUDED.price,
    updated_at = now();

`, lit(in.ClinicCode), lit(code), lit(nameLit),
				defaultLit(catLit, "general"), defaultLit(unitLit, "ชิ้น"),
				moneyLit(cost), moneyLit(price), boolLit(docBool(d, "active")),
				lit("mongo:items:"+code), tsLit(d.Text("createdAt"), "now()"))

			// Every SKU needs a catalog_item or it cannot be sold at all. item_type is
			// 'drug' for the whole collection (locked decision).
			status := catalogStatus("drug", price, false)
			if status == statusDraft {
				draft++
			}
			sCatalog.comment("%s · %s → drug", code, oneLine(name))
			sCatalog.addf("%s", upsertCatalogItem(in.ClinicCode, code, "drug", nameLit, "",
				category, price, cost, status, "items",
				fmt.Sprintf("(SELECT id FROM items WHERE clinic_code = %s AND code = %s AND active LIMIT 1)",
					lit(in.ClinicCode), lit(code)),
				tsLit(d.Text("createdAt"), "now()")))
		}

		// One lot per document, and the whole question is which shelf it is on. Stock is
		// physical: a lot filed at the wrong branch is a box the staff there cannot find,
		// and slot_stock CHECKs branch_code is non-empty so there is no "unknown" to fall
		// back on.
		//
		// Three outcomes, from the stock the lot points at:
		//
		//  1. no stock reference at all → dropped. Nothing says where the box is.
		//  2. a stock no branch claims  → MAIN_STOCK, V4's central warehouse. Not the
		//     default branch: putting the warehouse's 566 lots on one branch's shelf both
		//     misplaces them and inflates that clinic's "รวมทุกสาขา" value, which V4
		//     deliberately computes without MAIN_STOCK.
		//  3. a stock owned by a branch → that branch, translated through the map.
		//     UNLESS the branch was not created in this clinic, in which case the lot is
		//     dropped and counted rather than parked on the default branch. A branch left
		//     out of the tick list is being imported as its OWN clinic (different legal
		//     entity, its own TIN), and its lots arrive there. Keeping a copy here would
		//     put the same physical boxes in two clinics at once.
		stockID := docObjectID(d, "stock")
		if stockID == "" {
			noBranch++
			eSlot.skip("ไม่มี stock ในเอกสาร จึงไม่รู้ว่าอยู่คลังไหน")
			continue
		}
		legacyBranch, ownedByBranch := stockOwner[stockID]
		var branch string
		switch {
		case !ownedByBranch:
			branch = MainStockBranchCode
			mainStockLots++
		case !knowsBranch(in, legacyBranch):
			untickedLots[legacyBranch]++
			eSlot.skip("คลังของสาขา " + legacyBranch + " ที่ไม่ได้เลือกนำเข้ารอบนี้")
			continue
		default:
			branch = br.resolve(legacyBranch)
		}

		doc, derr := slotDoc(d, mongoID)
		if derr != nil {
			return "", "", "", fmt.Errorf("items doc %d (%s): build doc: %w", n+1, code, derr)
		}
		sSlot.comment("%s · lot %s · คลัง %s", code, mongoID, branch)
		sSlot.addf(`INSERT INTO slot_stock
    (mongo_id, item_code, item_id, code, cost_lot, cost_per_unit, amount, remain,
     company_medicine, first_date, expire_date, amount_total, doc,
     clinic_code, branch_code, created_at, updated_at)
VALUES (%s, %s, %s, %s, %s, %s, %s, %s,
        '', %s, %s, %s, %s::jsonb,
        %s, %s, now(), now())
ON CONFLICT (mongo_id) DO NOTHING;

`, lit(mongoID), lit(code), lit(mongoID), lit(mongoID),
			moneyLit(cost), moneyLit(cost),
			moneyLit(d.Text("quantity")), moneyLit(d.Text("quantity")),
			tsLit(d.Text("store"), "now()"), tsLitNullable(d.Text("expire")),
			moneyLit(d.Text("quantity_total")), lit(doc),
			lit(in.ClinicCode), lit(branch))
	}

	eItems.Statements = sItems.stmts
	eCatalog.Statements = sCatalog.stmts
	eCatalog.SourceRows = len(firstSeen)
	eSlot.Statements = sSlot.stmts

	if draft > 0 {
		rep.warnf("catalog_item(drug): %d SKU เป็น status=draft — รับเข้าคลังได้ แต่ขายไม่ได้จนกว่าจะใส่ราคาขายและฉลากยา 5 ฟิลด์ (ต้นทางไม่มีข้อมูลนี้)", draft)
	}
	if noBranch > 0 {
		rep.warnf("slot_stock: %d ล็อตไม่รู้คลังต้นทาง จึงไม่ถูก import (slot_stock บังคับ branch_code ต้องไม่ว่าง)", noBranch)
	}
	if mainStockLots > 0 {
		rep.notef("slot_stock: %d ล็อตอยู่ในคลังที่ไม่ได้เป็นของสาขาใด → เข้าคลังหลัก %s ตามที่ V4 ใช้ · "+
			"หน้าสต็อกของ V4 ไม่นับคลังหลักในยอด \"รวมทุกสาขา\" โดยเจตนา ของจึงไม่ไปโป่งอยู่ที่สาขาใดสาขาหนึ่ง",
			mainStockLots, MainStockBranchCode)
	}
	if len(untickedLots) > 0 {
		codes := make([]string, 0, len(untickedLots))
		total := 0
		for c := range untickedLots {
			codes = append(codes, c)
		}
		sort.Strings(codes)
		parts := make([]string, 0, len(codes))
		for _, c := range codes {
			parts = append(parts, fmt.Sprintf("%s (%d ล็อต)", c, untickedLots[c]))
			total += untickedLots[c]
		}
		// A note, not a warning: this is the chosen behaviour, not a problem with the file.
		// The count is here so the second import can be checked against it — those lots
		// must appear there, and appear exactly once.
		rep.notef("slot_stock: ไม่นำเข้า %d ล็อตของสาขาที่ไม่ได้เลือกรอบนี้ — %s · "+
			"ของอยู่ที่ชั้นวางของสาขานั้นจริง ซึ่งไม่มีในคลินิกนี้ ถ้าเอามาวางที่สาขาหลักจะทั้งบอกตำแหน่งผิด "+
			"และซ้ำกับตอน import สาขานั้นเป็นคลินิกของตัวเอง",
			total, strings.Join(parts, ", "))
	}
	rep.notef("items: %d เอกสาร → %d SKU (ที่เหลือคือล็อตของ code เดิม → slot_stock)", len(mc.items), len(firstSeen))
	rep.notef("slot_stock: คลังของแต่ละสาขามาจาก branch.stock_id ในตาราง branch (ไม่ได้อ่านจากสตริง stocks.stock_branch) · "+
		"พบสาขาที่มีคลังผูกไว้ %d สาขา", len(stockOwner))
	return sItems.String(), sCatalog.String(), sSlot.String(), nil
}

// slotDoc keeps the lot's source fields that V4 has no column for, so the row can be
// traced back to the export.
func slotDoc(d mongodump.Doc, mongoID string) (string, error) {
	m := map[string]any{"migrate_source": "mongo items", "mongo_id": mongoID}
	for _, f := range []string{"name1", "name2", "vat", "alert_stock", "alert_expire", "disbursement_fee"} {
		if v := d.Text(f); v != "" {
			m[f] = v
		}
	}
	b, err := json.Marshal(m)
	return string(b), err
}

// buildCourses maps course_items onto catalog_item(course) + catalog_course, and its
// list_item[] onto catalog_course_line.
//
// The legacy collection has NO code, so one is minted per course in dump order (LC001,
// LC002, …) — deterministic, so re-running the generator produces the same codes.
func buildCourses(mc *mongoCatalog, in Input, rep *Report) (courseSQL, lineSQL string, err error) {
	if len(mc.courseItems) == 0 {
		return "", "", nil
	}
	eCourse := rep.entity(EntCatalogCourse)
	eLine := rep.entity(EntCatalogCourseLine)
	eCourse.SourceRows = len(mc.courseItems)

	var sCourse, sLine sqlBuilder
	unresolved := map[string]int{}
	skippedMed := 0
	zeroQty := 0

	// Service codes are off-limits when minting course codes.
	taken := map[string]bool{}
	for _, c := range mc.healCode {
		taken[c] = true
	}

	for n, d := range mc.courseItems {
		code := mintCourseCode(n+1, taken)
		if code == "" {
			return "", "", fmt.Errorf("course_items doc %d: หารหัสคอร์สที่ว่างไม่ได้", n+1)
		}
		taken[code] = true
		// Remember which code this course got. The entitlement pass (coursebought.go)
		// resolves catalog_item through it — the legacy collection carries no code, so
		// this map is the ONLY link between a customer's balance and its course.
		if id := docObjectID(d, "_id"); id != "" {
			mc.courseCode[id] = code
			if n := firstNonEmpty(d.Text("name_th"), d.Text("name_en")); n != "" {
				mc.courseName[id] = n
			}
		}
		nameTh := firstNonEmpty(d.Text("name_th"), d.Text("name_en"), code)
		nameEn := d.Text("name_en")
		price := d.Text("price")

		// A course with no children cannot be sold in V4 (the app keeps such a course
		// draft), so status follows price AND child count.
		lines, _ := d.Get("list_item")
		childCount := 0
		if arr, ok := lines.([]any); ok {
			childCount = len(arr)
		}
		status := catalogStatus("course", price, true)
		if childCount == 0 {
			status = statusDraft
		}

		nameThLit, _ := clip(nameTh, 500)
		nameEnLit, _ := clip(nameEn, 500)
		sCourse.comment("%s · %s (%d รายการในคอร์ส)", code, oneLine(nameTh), childCount)
		sCourse.addf("%s", upsertCatalogItem(in.ClinicCode, code, "course", nameThLit, nameEnLit,
			"", price, "0", status, "catalog", "NULL", tsLit(d.Text("created_at"), "now()")))

		sCourse.addf(`INSERT INTO catalog_course
    (catalog_item_id, clinic_code, periods, days, is_buffet, buffet_count,
     loyalty_points, doctor_fee_eligible, created_at, updated_at)
SELECT %s, %s, %s, %s, %s, 0, 0, false, now(), now()
WHERE %s IS NOT NULL
ON CONFLICT (catalog_item_id) DO UPDATE SET
    periods    = EXCLUDED.periods,
    days       = EXCLUDED.days,
    is_buffet  = EXCLUDED.is_buffet,
    updated_at = now();

`, catalogItemIDByCode(in.ClinicCode, code), lit(in.ClinicCode),
			intLitDefault(d.Text("number_of_time_total"), 1),
			intLitDefault(d.Text("dayofcourse"), 0),
			boolLit(docBool(d, "infinity_use")),
			catalogItemIDByCode(in.ClinicCode, code))

		// Course lines. Each list_item entry points at a listheal by ObjectId; V4 wants
		// the child's catalog_item, resolved by the service code.
		if arr, ok := lines.([]any); ok {
			for i, raw := range arr {
				line, isDoc := raw.(mongodump.Doc)
				if !isDoc {
					continue
				}
				healID := docObjectID(line, "heal_id")
				childCode := mc.healCode[healID] // already reflects any re-coded duplicate
				if childCode == "" {
					unresolved[healID]++
					eLine.skip("heal_id ไม่ตรงกับ listheals ใด")
					continue
				}
				// V4 refuses a course line with no quantity:
				// catalog_course_line_qty_chk CHECK (qty > 0). The source is not always
				// positive — 115 of one tenant's 1,010 course lines carry
				// number_of_time = 0 — and a single 0 aborts the ENTIRE import on that
				// constraint, taking every other table down with it.
				//
				// A line that says "0 of this service" describes nothing the customer can
				// redeem, so it is dropped and counted. Writing 1 instead was considered
				// and rejected by the clinic owner: it would hand everyone who bought that
				// course a session the clinic never sold.
				//
				// An ABSENT or unparseable value still means 1 (intLitDefault's job) —
				// "the source did not say" and "the source said none" are different facts.
				qty := intLitDefault(line.Text("number_of_time"), 1)
				if n, cerr := strconv.Atoi(qty); cerr != nil || n <= 0 {
					zeroQty++
					eLine.skip("จำนวนในคอร์สเป็น 0 หรือน้อยกว่า — V4 บังคับ qty > 0")
					continue
				}
				sLine.comment("%s → %s ×%s", code, childCode, qty)
				sLine.addf(`INSERT INTO catalog_course_line
    (clinic_code, course_catalog_item_id, child_catalog_item_id, line_kind,
     parent_service_catalog_item_id, qty, sort_order, created_at, updated_at)
SELECT %s, %s, %s, 'service', NULL, %s, %d, now(), now()
WHERE %s IS NOT NULL AND %s IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM catalog_course_line l
       WHERE l.clinic_code = %s
         AND l.course_catalog_item_id = %s
         AND l.child_catalog_item_id  = %s
  );

`, lit(in.ClinicCode), catalogItemIDByCode(in.ClinicCode, code),
					catalogItemIDByCode(in.ClinicCode, childCode), qty, i,
					catalogItemIDByCode(in.ClinicCode, code),
					catalogItemIDByCode(in.ClinicCode, childCode),
					lit(in.ClinicCode), catalogItemIDByCode(in.ClinicCode, code),
					catalogItemIDByCode(in.ClinicCode, childCode))
				eLine.SourceRows++
			}
		}

		// list_medicien would be line_kind='medication', which V4 requires to name the
		// service it belongs to (parent_service_catalog_item_id NOT NULL). The export
		// does not say which service that is, so these are reported, never guessed.
		if med, ok := d.Get("list_medicien"); ok {
			if arr, isArr := med.([]any); isArr && len(arr) > 0 {
				skippedMed += len(arr)
			}
		}
	}

	eCourse.Statements = sCourse.stmts
	eLine.Statements = sLine.stmts

	rep.notef("catalog_item(course): ต้นทางไม่มีรหัสคอร์ส จึง mint ให้เป็น %s001.. ตามลำดับในไฟล์ (deterministic · เลี่ยงรหัสที่บริการใช้อยู่)",
		CourseCodePrefix)
	if len(unresolved) > 0 {
		total := 0
		for _, v := range unresolved {
			total += v
		}
		rep.warnf("catalog_course_line: %d บรรทัดอ้าง heal_id ที่ไม่มีใน listheals (%d id) — ไม่ import บรรทัดนั้น คอร์สจะมีของไม่ครบ",
			total, len(unresolved))
	}
	if skippedMed > 0 {
		rep.warnf("catalog_course_line: %d รายการยาในคอร์ส (list_medicien) ไม่ถูก import — V4 บังคับว่ายาในคอร์สต้องระบุว่าอยู่ใต้บริการใด (parent_service) ซึ่งต้นทางไม่ได้บอก",
			skippedMed)
	}
	// Loud, because this changes what a course CONTAINS. The alternative was to write 1,
	// which the clinic owner rejected: it would give every buyer of that course a session
	// nobody sold them.
	if zeroQty > 0 {
		rep.warnf("catalog_course_line: %d บรรทัดในคอร์สระบุจำนวน 0 ครั้ง (number_of_time = 0) — ไม่ import บรรทัดนั้น เพราะ V4 บังคับ qty > 0 และ '0 ครั้ง' คือไม่มีอะไรให้ตัดสิทธิ์ · ถ้าปล่อยผ่านจะล้มทั้งไฟล์ที่ constraint catalog_course_line_qty_chk · ถ้าคลินิกยืนยันว่าบรรทัดไหนควรเป็น 1 ต้องแก้ที่ต้นทางแล้ว import ใหม่ — เครื่องมือนี้ไม่เดาจำนวนสิทธิ์ให้",
			zeroQty)
	}
	return sCourse.String(), sLine.String(), nil
}

// nextFreeCode returns the first unused code that shares base's prefix and digit
// width — LC033 with LC001..LC092 taken becomes LC093. Returns "" when the numbering
// space is exhausted, which the caller reports rather than papering over.
func nextFreeCode(base string, used map[string]bool) string {
	prefix, width := splitCode(base)
	if width == 0 {
		return ""
	}
	for n := 1; n < 100000; n++ {
		cand := fmt.Sprintf("%s%0*d", prefix, width, n)
		if !used[cand] {
			return cand
		}
	}
	return ""
}

// splitCode splits "LC033" into ("LC", 3). width 0 means the code has no numeric tail.
func splitCode(code string) (prefix string, width int) {
	i := len(code)
	for i > 0 && code[i-1] >= '0' && code[i-1] <= '9' {
		i--
	}
	return code[:i], len(code) - i
}

// mintCourseCode produces the nth course code, skipping anything the service codes
// already occupy so a course can never land on top of a service.
func mintCourseCode(n int, used map[string]bool) string {
	for i := n; i < n+100000; i++ {
		cand := fmt.Sprintf("%s%03d", CourseCodePrefix, i)
		if !used[cand] {
			return cand
		}
	}
	return ""
}

// groupSelected reports whether a group name is in the requested list.
func groupSelected(groups []string, want string) bool {
	for _, g := range groups {
		if strings.EqualFold(strings.TrimSpace(g), want) {
			return true
		}
	}
	return false
}

func distinctValues(m map[string]string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range m {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// joinName keeps both legacy name parts visible ("Allergan 1u (M)" + "100u") instead of
// filing name2 into a clinical field it may not mean.
func joinName(name1, name2 string) string {
	a, b := strings.TrimSpace(name1), strings.TrimSpace(name2)
	if a == "" {
		return b
	}
	if b == "" || strings.Contains(a, b) {
		return a
	}
	return a + " " + b
}
