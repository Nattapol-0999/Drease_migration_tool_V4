package legacyimport

import (
	"encoding/json"
	"fmt"
	"math"
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
	EntStockMovement     = "stock_movement"
	EntCatalogService    = "catalog_item(service)"
	EntCatalogCourse     = "catalog_item(course)"
	EntCatalogCourseLine = "catalog_course_line"
	EntCatalogPublish    = "catalog_item_channel"
)

var catalogOrder = []string{
	EntCatalogCategory,
	EntCatalogService,
	EntItems,
	EntCatalogDrug,
	EntSlotStock,
	// After the lots, because it resolves each lot's id from slot_stock.mongo_id.
	EntStockMovement,
	EntCatalogCourse,
	EntCatalogCourseLine,
	// Last on purpose: it reads back the ids of the catalog_item rows everything above wrote.
	EntCatalogPublish,
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

	// points is one balance per patient (mris.points), mapped in loyalty.go. pointLogs is
	// the movement history behind it; it is parsed only to be COUNTED, because the clinic
	// owner chose not to import it and a number in the report is how that omission stays
	// visible instead of looking like the collection was never there.
	points    []mongodump.Doc
	pointLogs []mongodump.Doc
	// linePatients is mris.line_patients — one document per paired LINE account,
	// mapped in linebind.go. Same database as points and page, so it arrives in the
	// same dump and needs no flag of its own.
	linePatients []mongodump.Doc
	// pairLogs is mris.pair_logs — the EVENT log V2 writes beside those pairings
	// (CREATE / DELETE / SET_ACTIVE per user_code + line_id). It is the SECOND source:
	// read only when line_patients is absent or empty, by the owner's rule of 6 Oct 2026
	// (line_patients first, pair_logs second, neither = skip the group). See
	// bindsFromPairLogs for how a current pairing is reconstructed from it.
	pairLogs []mongodump.Doc

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
		points:      byName["points"],
		pointLogs:   byName["point_logs"],

		linePatients: byName["line_patients"],
		pairLogs:     byName["pair_logs"],
		courseCode:   map[string]string{},
		courseName:   map[string]string{},
		healName:     map[string]string{},
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
	// line_patients is on this list because a dump taken for `-only linebind` holds
	// nothing else, and the old guard rejected it as unsupported while naming five
	// collections the operator had not asked for. pair_logs is on it for the same reason:
	// a box with no line_patients can still carry its pairings in the log alone.
	if len(mc.items) == 0 && len(mc.listheals) == 0 && len(mc.courseItems) == 0 &&
		len(mc.bills) == 0 && len(mc.points) == 0 && len(mc.linePatients) == 0 &&
		len(mc.pairLogs) == 0 {
		return nil, fmt.Errorf("mongo dump ไม่มี collection ที่รองรับเลย (ต้องมีอย่างน้อยหนึ่ง: items, listheals, course_items, bills, points, line_patients, pair_logs)")
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
//
// # TWO SOURCES, ONE SPINE
//
// The export splits categories across two collections: `category` names the ones a
// SERVICE carries, `item_types` names the ones a DRUG carries ("ยา" / "อุปกรณ์" /
// "เวชภัณฑ์"). Only the first used to be written here, while buildItems went on passing
// the item_types name to categoryIDByName — which is a subselect, and a subselect that
// finds nothing is not an error. So every imported drug landed with category_id = NULL
// and the catalogue screen had nothing to group them by, silently.
//
// V4 has no such split. catalog.ResolveCategoryID (internal/controller/catalog/sku.go)
// takes whatever name the caller has, looks it up case/space-insensitively, and CREATES
// the row when it is missing — for a drug from the stock page exactly as for a service.
// One spine is the app's own shape, not a convenience adopted here.
//
// Dedupe is case/space-insensitive because the target index is on lower(btrim(name)):
// "ยา" and "ยา " are one category there, so emitting both would waste a sort_order slot
// and make which one wins depend on map iteration order.
func buildCatalogCategory(mc *mongoCatalog, in Input, rep *Report) string {
	seen := map[string]bool{}
	names := make([]string, 0, len(mc.categoryName)+len(mc.itemTypeName))
	for _, src := range []map[string]string{mc.categoryName, mc.itemTypeName} {
		for _, n := range src {
			key := strings.ToLower(strings.TrimSpace(n))
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			names = append(names, n)
		}
	}
	sort.Strings(names) // deterministic output — sort_order below is the index
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
	if drugCats := distinctValues(mc.itemTypeName); len(drugCats) > 0 {
		rep.notef("catalog_category: รวมหมวดของยา/เวชภัณฑ์จาก item_types เข้ากองเดียวกับหมวดบริการด้วย (%s) — "+
			"ก่อนหน้านี้เขียนแค่หมวดบริการ ทำให้ catalog_item.category_id ของยาเป็น NULL ทุกแถวโดยไม่มี error "+
			"(categoryIDByName เป็น subselect · หาไม่เจอ = NULL ไม่ใช่ข้อผิดพลาด) · V4 เองก็ใช้กองเดียว "+
			"(catalog.ResolveCategoryID สร้างหมวดให้เมื่อยังไม่มี ไม่แยกว่าเป็นของยาหรือบริการ)",
			strings.Join(drugCats, ", "))
	}
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
//
// WHY source_id IS IN THE DO UPDATE AND source_table / source_key ARE NOT
//
// source_id is repaired FILL-ONLY — COALESCE(existing, new) — because a row that already
// resolved keeps the id it has, while a row left NULL by an earlier run gets one. That
// NULL is not hypothetical: the subselect in buildItems used to carry `AND active`, so
// every SKU switched off in V2 arrived with source_id NULL, and the trigger
// catalog_capture_legacy_source_key (migration 0301) then took its `source_id IS NULL`
// branch and set legacy_source_key NULL too. Both columns matter downstream — the
// checkout safety check joins items ON i.id = ci.source_id and reads a missing join as
// "not sellable", and a doctor-fee rule binds a bill line through legacy_source_key. The
// old DO UPDATE touched neither, so re-running the import could not fix it either.
//
// Filling source_id is enough to fix both: the trigger fires on UPDATE as well, sees
// source_table='items' with a non-NULL source_id, and copies items.source_key across.
//
// source_table stays out because changing it would re-point provenance at a different
// table, and source_key stays out because re-minting the key of a row a fee rule already
// points at is how that rule silently stops matching.
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
              source_id  = COALESCE(catalog_item.source_id, EXCLUDED.source_id),
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
	// Services whose legacy `point` turned out to be stored-value credit rather than a
	// loyalty rate. Collected so the report can name each one instead of stating a count.
	var creditLike []string
	// Services the arithmetic let through while the NAME says stored value. Reported for a
	// human to judge, never refused here.
	var nameSuspect []string
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

		// แต้มสะสมต่อรายการ. V2's `point` is not always points — see
		// serviceLoyaltyPoints. A value that is really stored-value credit is dropped to 0
		// and named in the report.
		points, credit := serviceLoyaltyPoints(d.Text("point"), price)
		switch {
		case credit:
			creditLike = append(creditLike,
				fmt.Sprintf("%s %s (ราคา %s → point %d)", code, oneLine(nameTh), moneyLit(price), intFromMoney(d.Text("point"))))
		case points > 0 && looksLikeStoredValueName(nameTh):
			// Passed the arithmetic but the name says stored value. Reported, not refused:
			// see looksLikeStoredValueName for why the two cannot be told apart from data.
			nameSuspect = append(nameSuspect,
				fmt.Sprintf("%s %s (ราคา %s → point %d)", code, oneLine(nameTh), moneyLit(price), points))
		}

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
			boolLit(docBool(d, "isDoctorFee")), points,
			catalogItemIDByCode(in.ClinicCode, code))
	}

	e.Statements = s.stmts
	if draft > 0 {
		rep.warnf("catalog_item(service): %d รายการไม่มีราคา → status=draft (ขายไม่ได้จนกว่าจะใส่ราคา)", draft)
	}
	if len(creditLike) > 0 {
		rep.warnf("catalog_service: %d รายการมี point ที่ไม่น้อยกว่าราคาขาย — เป็นวงเงิน/เครดิตบาท ไม่ใช่แต้มสะสม "+
			"จึงนำเข้าเป็น 0 (มติเจ้าของคลินิก 28 ก.ย. 2569) · คลินิกตั้งแต้มที่ถูกต้องเองในจอได้: %s",
			len(creditLike), strings.Join(capList(creditLike, 25), " · "))
	}
	if len(nameSuspect) > 0 {
		rep.warnf("catalog_service: %d รายการที่ชื่อบอกว่าเป็นวงเงิน/เครดิต แต่ point น้อยกว่าราคา จึง**นำเข้าตามต้นทาง** — "+
			"แยกจากตัวเลขไม่ได้ว่าเป็นอัตราแต้มหรือเครดิตส่วนเพิ่ม ต้องให้คนตรวจ ถ้าผิดให้แก้ในจอก่อนเปิด loyalty_policy: %s",
			len(nameSuspect), strings.Join(capList(nameSuspect, 25), " · "))
	}
	if kinds := distinctValues(mc.kindName); len(kinds) > 0 {
		rep.notef("listheals: legacy kind (%s) ไม่ได้ import — V4 ไม่มีแถว service_kind ให้อ้าง จึงปล่อย NULL ไม่เดา",
			strings.Join(kinds, ", "))
	}
	return s.String(), nil
}

// serviceLoyaltyPoints decides what reaches catalog_service.loyalty_points, and reports
// whether the source value was refused.
//
// # WHY THE SOURCE NUMBER CANNOT BE TRUSTED AS POINTS
//
// V2's listheal.point is used for two unrelated things and nothing in the document says
// which. Some clinics put a loyalty rate there; others put the STORED-VALUE CREDIT a
// prepaid package hands back. The second kind is unmistakable in the data because the
// credit is always at least the price — the package's whole selling point is getting more
// back than you paid. Measured across the five clinics on production (28 Sep 2026):
//
//	point >= price : 33 rows, every one named วงเงิน / Member / VIP / Package แถม —
//	                 "VIP 500,000 ได้รับ 550,000", "วงเงิน 50,000ใช้ได้60,000"
//	point <  price : 5 rows, all plausible rates — "รายการรักษา A3 (มีแต้มสะสม)"
//	                 5,000 → 500, "Anti - Aging" 10,000 → 10
//
// Importing the first kind as loyalty points is not a cosmetic error. awardPoints
// (internal/controller/billing/service.go) sums loyalty_points over the invoice and awards
// it once the bill is settled in full, and the ONLY gate on that is policy.Active — there
// is no per-bill ceiling anywhere. Anjali (C6C936C) imported this way carried 4,603,000
// points across 19 Member packages, so the day the clinic turned the scheme on, one
// 600,000 baht package would have awarded 750,000 points. Those rows had to be cleared by
// hand on 28 Sep 2026; this guard is why it does not happen again.
//
// The owner's rule (28 Sep 2026): carry `point` only when it is smaller than the price.
// Anything else arrives as 0 and is named in the report, so the clinic sets a real rate on
// screen rather than inheriting a number that means something else.
//
// A price that is absent or zero cannot be compared, so it falls on the refusing side.
// Those rows are status=draft anyway (catalogStatus makes a service with no price a draft)
// and cannot be sold until someone prices them, which is the same moment they would set
// the points.
func serviceLoyaltyPoints(pointText, priceText string) (points int, creditLike bool) {
	p := intFromMoney(pointText)
	if p <= 0 {
		return 0, false
	}
	// moneyFloat is the package's check-only parser: it returns 0 for anything it cannot
	// read, which lands unpriced and malformed rows on the refusing side together.
	price := moneyFloat(priceText)
	if price <= 0 {
		return 0, true
	}
	if float64(p) >= math.Round(price) {
		return 0, true
	}
	return p, false
}

// storedValueNameHints are the words V2's clinics put in the name of a prepaid package.
// Latin ones are matched case-insensitively; the Thai ones have no case.
var storedValueNameHints = []string{
	"credit", "member", "vip", "package",
	"วงเงิน", "แถม", "มัดจำ", "เครดิต",
}

// looksLikeStoredValueName reports whether a service is NAMED like a prepaid package.
//
// # WHY THIS ONLY WARNS
//
// serviceLoyaltyPoints catches the credit that is at least the price, which is what a
// "จ่าย 600,000 ได้ 750,000" package always looks like. It cannot catch a BONUS credit
// smaller than the price, because "ซื้อ 20,000 ได้เครดิตเพิ่ม 1,000" and "20,000 บาท ได้
// 1,000 แต้ม" are the same two numbers. Anjali's export holds exactly that: LH491 "Beauty
// Credit" priced 20,000 with point 1,000 passes the arithmetic, while its siblings LC942
// to LC946 — the same product family, same wording — were priced 0 and were caught.
//
// A name is weaker evidence than a price, so it does not refuse anything: a clinic really
// can name a loyalty item "รายการรักษา A3 (มีแต้มสะสม)" or set 1 point on "VIP50000", and
// both exist on production today. Refusing on a word would delete an intention the clinic
// did state. Warning names the row and leaves the judgement with the person reading the
// report, before they turn loyalty_policy.active on.
func looksLikeStoredValueName(name string) bool {
	n := strings.ToLower(name)
	for _, hint := range storedValueNameHints {
		if strings.Contains(n, hint) {
			return true
		}
	}
	return false
}

// buildItems maps the Mongo `items` collection onto three tables.
//
// One SKU can appear several times in the export, once per stock location, so the
// collection is folded: the first document defines the SKU (items + catalog_item) and
// EVERY document becomes a slot_stock lot keyed by its Mongo _id.
func buildItems(mc *mongoCatalog, in Input, rep *Report) (itemsSQL, catalogSQL, slotSQL, moveSQL string, err error) {
	br := newBranchResolver(in)
	defer br.report(rep, "slot_stock")
	if len(mc.items) == 0 {
		return "", "", "", "", nil
	}
	eItems := rep.entity(EntItems)
	eCatalog := rep.entity(EntCatalogDrug)
	eSlot := rep.entity(EntSlotStock)
	eMove := rep.entity(EntStockMovement)
	eItems.SourceRows = len(mc.items)
	eSlot.SourceRows = len(mc.items)

	// Which branch owns which stock, from the SQL side (branch.stock_id). Read before the
	// loop because every lot needs it, and a broken branch table must stop the build rather
	// than silently make every lot look central.
	stockOwner, oerr := stockOwners(in)
	if oerr != nil {
		return "", "", "", "", oerr
	}

	// One value for the whole stock collection, resolved by BuildAll (which also rejects an
	// unknown one), so every row below agrees and the report can name it once.
	stockType := in.StockItemType
	if stockType == "" {
		stockType = "drug"
	}

	// Which of a code's documents speaks for the SKU. Must be decided before the loop
	// writes anything, because the answer can be a document the loop has not reached yet.
	masterOf := chooseMasterDocs(mc.items, stockOwner, in)

	var sItems, sCatalog, sSlot, sMove sqlBuilder
	firstSeen := map[string]bool{}
	noBranch, draft := 0, 0
	// mainStockLots counts lots in stocks no branch claims; untickedLots counts, per legacy
	// branch, the lots dropped because that branch is not part of this clinic.
	mainStockLots := 0
	untickedLots := map[string]int{}
	// movesWritten is the receive-ledger rows emitted beside the lots. Counted separately
	// from the lots themselves so "lot written but no movement" is visible as a mismatch in
	// the report rather than as an empty stock-in screen months later.
	movesWritten := 0
	// Quantities cut to fit their column, kept so the report can name every one. See
	// clampnumeric.go for the rule and for what it costs.
	var clamps qtyClamps

	for n, d := range mc.items {
		code := d.Text("code")
		if code == "" {
			return "", "", "", "", fmt.Errorf("items doc %d: ไม่มี code (ใช้เป็นคีย์)", n+1)
		}
		mongoID := docObjectID(d, "_id")
		name := firstNonEmpty(joinName(d.Text("name1"), d.Text("name2")), code)
		unit := mc.itemUnitName[docObjectID(d, "unit")]
		category := mc.itemTypeName[docObjectID(d, "type")]
		price := d.Text("price")
		cost := d.Text("cost")
		// Quantities are read once, clamped once, and used for every column that carries
		// them (lot value, received amount, on-hand, and the receive movement) so the four
		// can never disagree. See clampNonNegativeMoney for why V2 hands out negatives.
		qty := clampNonNegativeMoney(d.Text("quantity"))
		qtyTotal := clampNonNegativeMoney(d.Text("quantity_total"))

		if masterOf[code] == mongoID && !firstSeen[code] {
			firstSeen[code] = true

			nameLit, _ := clip(name, 255)
			unitLit, _ := clip(unit, 64)
			catLit, _ := clip(category, 64)

			// Drug label fields stay EMPTY on purpose. The export has no
			// generic_name/strength/dosage_form/indication/warning, and filling them
			// from a product name would be inventing clinical text. Empty keeps the
			// SKU in draft, which is exactly "receivable, not sellable".
			// source_key IS DELIBERATELY NOT LISTED.
			//
			// This used to write 'mongo:items:'+code and it made the SECOND clinic
			// importable-once-ever. Migration 0115 declares the column as
			//
			//	source_key varchar(32) NOT NULL DEFAULT md5(random()||clock_timestamp())
			//	CREATE UNIQUE INDEX uniq_items_source_key ON items (source_key)
			//
			// — a globally unique opaque surrogate, unique across items/courses/listheal
			// together, because doctor-fee rules bind a bill line to a catalog row by it and
			// those three tables reuse the same integer ids. The V2 stock code is not that:
			// 'ST000001' is the first SKU in EVERY V2 database. So the second tenant to
			// import hit
			//
			//	ERROR: duplicate key value violates unique constraint "uniq_items_source_key"
			//	DETAIL: Key (source_key)=(mongo:items:ST000001) already exists.
			//
			// on a row that belonged to a completely different clinic. Omitting the column
			// lets the declared default mint a distinct key per row, which is the contract
			// the index is enforcing. Provenance is not lost: items.code still holds the V2
			// code verbatim, and the ON CONFLICT below already treats (clinic_code, code) as
			// this row's identity.
			//
			// The DO UPDATE deliberately leaves source_key out too — a re-import must not
			// re-mint the key of a row a fee rule may already point at.
			// WHY THIS IS NOT `ON CONFLICT ... DO UPDATE`
			//
			// It was, and that is how one tenant ended up with 1,256 junk rows.
			// uniq_items_clinic_code is PARTIAL — `ON items (clinic_code, code) WHERE
			// active` — so it is the only conflict target available, and an INACTIVE row
			// never violates it. Every re-run therefore inserted another copy of every
			// switched-off SKU. Measured at C2EC1E3: 645 real codes, 1,901 rows, 356 codes
			// holding 4–6 rows each, all of them inactive, created on the day of the last
			// re-run. Those duplicates are what makes `code` ambiguous for anything that
			// resolves a SKU by code.
			//
			// NOT EXISTS + UPDATE covers both halves with no index to depend on: the row is
			// created once and refreshed thereafter, whether V2 has it switched on or off.
			// Both statements run in the catalog group's transaction, so there is no window
			// between them.
			//
			// `active` and `source_key` are deliberately absent from the UPDATE. active is
			// the clinic's own switch and catalog_item.status is derived from it — a re-import
			// must not flip what somebody turned on in V4. source_key is the immutable
			// identity a doctor-fee rule may already point at (see the note below).
			// min_alert is written as 0, and it is written EXPLICITLY.
			//
			// The column defaults to 5 (migration 0068), but both of V4's own intake doors
			// send 0: Stock.CreateItem passes whatever the form holds — 0 when untouched —
			// and GuidedReceive hardcodes pgkit.Numeric(0). Leaving the default meant every
			// imported SKU arrived with a reorder threshold of 5 that nobody chose, so the
			// clinic's first look at its stock page showed low-stock warnings invented by a
			// migration. The export says nothing about reorder levels; 0 claims nothing.
			//
			// It is deliberately absent from the UPDATE below, for the same reason `active`
			// is: once a human has set a threshold in V4, a re-import must not reset it.
			sItems.comment("%s · %s", code, oneLine(name))
			sItems.addf(`INSERT INTO items
    (clinic_code, code, name, name_en, category, unit, cost, price, min_alert, active,
     created_at, updated_at)
SELECT %s, %s, %s, NULL, %s, %s, %s, %s, 0, %s,
       %s, now()
 WHERE NOT EXISTS (SELECT 1 FROM items WHERE clinic_code = %s AND code = %s);

UPDATE items SET
    name       = %s,
    category   = %s,
    unit       = %s,
    cost       = %s,
    price      = %s,
    updated_at = now()
 WHERE clinic_code = %s AND code = %s;

`, lit(in.ClinicCode), lit(code), lit(nameLit),
				defaultLit(catLit, "general"), defaultLit(unitLit, "ชิ้น"),
				moneyLit(cost), moneyLit(price), boolLit(docBool(d, "active")),
				tsLit(d.Text("createdAt"), "now()"),
				lit(in.ClinicCode), lit(code),
				lit(nameLit), defaultLit(catLit, "general"), defaultLit(unitLit, "ชิ้น"),
				moneyLit(cost), moneyLit(price),
				lit(in.ClinicCode), lit(code))

			// Every SKU needs a catalog_item or it cannot be sold at all. The whole collection
			// gets ONE item_type, chosen per run through -stock-item-type and defaulting to
			// 'drug' — see Input.StockItemType for why it is a choice and not a rule. It is
			// one value for the whole list because the export offers nothing trustworthy to
			// split it on: V2's own category calls a cannula ยา, and 258 of 645 codes at one
			// clinic disagree with themselves across rows.
			//
			// STATUS FOLLOWS items.active, AND NOTHING ELSE — settled with the clinic owner
			// on 15 Sep 2026, replacing catalogStatus("drug", price, false).
			//
			// The old rule asked two questions the export cannot answer. It wanted a selling
			// price, and 316 of this tenant's 398 SKUs carry 0 in V2 while the stock screen
			// still lists them as ใช้งาน; it also wanted the five label fields
			// (generic_name/strength/dosage_form/indication/warning), and the export has none
			// of them for any SKU — measured, 0 of 398. So every drug arrived as draft: not
			// sellable, absent from the item picker, and no amount of correct data in the
			// dump could have changed that. The owner's answer is that V2's own switch is the
			// authority: what the stock screen calls ใช้งาน is what V4 should offer.
			//
			// On this tenant that is 270 active (ยา 147 · อุปกรณ์ 81 · อื่นๆ 42) and 128 kept
			// as draft. Draft still means receivable into stock and visible to staff, so a
			// SKU switched off in V2 loses nothing but the ability to be sold.
			//
			// The price is no longer consulted at all: an item with no price sells at 0 and
			// the counter can type one, which is a smaller problem than an item that cannot
			// be dispensed.
			itemActive := docBool(d, "active")
			status := statusActive
			if !itemActive {
				status = statusDraft
				draft++
			}
			sCatalog.comment("%s · %s → %s", code, oneLine(name), stockType)
			sCatalog.addf("%s", upsertCatalogItem(in.ClinicCode, code, stockType, nameLit, "",
				category, price, cost, status, "items",
				// NO `AND active` HERE. That one word cost two tenants their provenance.
				//
				// The subquery used to read `... AND code = Y AND active LIMIT 1`, so a SKU
				// that V2 had switched off matched nothing and the row landed with
				// source_id = NULL. That is not a cosmetic gap:
				// trg_catalog_capture_legacy_source_key (migrations 0296/0301) reads `items`
				// THROUGH source_id, so it then forces legacy_source_key to NULL as well,
				// and nonSellableCatalogCodes (billing/helpers.go) refuses a drug whose
				// items row is missing BEFORE it looks at the label. The SKU could not be
				// made sellable by any amount of correct label data — the pharmacist's work
				// would have had nowhere to land.
				//
				// Measured on prod 2026-09-22, matching with no exceptions:
				//
				//	clinic     drug+goods   source_id NULL
				//	C2EC1E3           645              450   (= the 450 inactive codes)
				//	C6C936C           566              221   (= the 221 inactive codes)
				//
				// Both were repaired by hand (22_link_items_source_id.sql).
				//
				// ORDER BY active DESC, id ASC makes the pick deterministic where several
				// items rows share a code: the row V2 still has switched on, else the
				// oldest — which is the one THIS import wrote first, not a duplicate from a
				// later re-run. No code anywhere has two active rows (the partial unique
				// index uniq_items_clinic_code forbids it), so the first clause never has a
				// tie to break.
				fmt.Sprintf("(SELECT id FROM items WHERE clinic_code = %s AND code = %s ORDER BY active DESC, id ASC LIMIT 1)",
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
			return "", "", "", "", fmt.Errorf("items doc %d (%s): build doc: %w", n+1, code, derr)
		}
		// cost_lot and cost_per_unit are DIFFERENT numbers and used to be given the same
		// one. `items.cost` is the cost of ONE unit — measured on the-gloss-cct's export,
		// of the 42 SKUs carrying a cost every single one sits at or below twice its
		// selling price (AMK 189 vs 250 at qty 10 · Collaju 16,999 vs 16,999 at qty 3) and
		// none looks like a lot total. V4 agrees by construction: GuidedReceive writes
		// CostLot: pgkit.Numeric(row.UnitCost * row.Qty) beside CostPerUnit: row.UnitCost.
		//
		// So writing the unit cost into cost_lot understated the value of every multi-unit
		// lot in the clinic — a 52-unit box of Semaglutide was valued at one pen. The
		// number also leaves the stock page: ListCourseCatalogCompositionForPurchase
		// estimates a course's material cost from the lot's cost_per_unit, and
		// stock/analytics reads the lot value.
		//
		// The multiplication is handed to Postgres rather than done in Go. Every other
		// money value in this file reaches the database as source digits for the same
		// reason (moneyLit's contract): float64 has no exact ฿0.10, and numeric × numeric
		// in Postgres is exact.
		// V2 lets a quantity be typed in wider than the column it has to land in — a
		// field of nines, 99999999999999 units of a face mask. Postgres rejects it mid
		// file, and because the script commits per group that leaves the clinic half
		// imported. The owner's rule (ก1, 2 Oct 2026) is to cut it to what each column
		// holds, which is NOT the same number for slot_stock as for the movement below.
		slotAmount, cutAmount := clampToColumn("slot_stock", "amount", qty)
		slotTotal, cutTotal := clampToColumn("slot_stock", "amount_total", qtyTotal)
		clamps.record(code, mongoID, "slot_stock.amount/remain", qty, slotAmount, cutAmount)
		clamps.record(code, mongoID, "slot_stock.amount_total", qtyTotal, slotTotal, cutTotal)

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
			lotCostExpr(cost, slotAmount), moneyLit(cost),
			slotAmount, slotAmount,
			tsLit(d.Text("store"), "now()"), tsLitNullable(d.Text("expire")),
			slotTotal, lit(doc),
			lit(in.ClinicCode), lit(branch))

		// The receive that put this lot on the shelf.
		//
		// WHY A LEDGER ROW IS NOT OPTIONAL
		//
		// V4 never opens a lot without one: both doors that create stock — ReceiveLot and
		// GuidedReceive — call InsertStockReceipt in the same transaction as InsertSlotLot
		// (reason='receive', positive qty_delta, slot_id/lot_code naming the lot). The
		// import wrote the lot and not the movement, so an imported clinic had inventory
		// that had never arrived: "รายงานการรับเข้า" was empty, stock/movements showed
		// nothing, and the integrity check could not reconcile on-hand against its own
		// history. Migration 0209 records the reason plainly — 44 of 213 lot×branch pairs
		// in demo already disagree with slot_stock "because not every writer has always
		// gone through a movement". This is that writer.
		//
		// created_at is the lot's own first_date, NOT now(): the goods arrived when V2 says
		// they arrived, and stamping the import date would put years of deliveries on one
		// afternoon and make every stock-in report wrong. user_id stays NULL because no
		// person did this — naming a staff member would be a lie the ledger keeps forever.
		//
		// slot_id is resolved through slot_stock.mongo_id, which migration 0035 declares
		// UNIQUE, so the subquery can match at most one lot. It is a SELECT rather than a
		// literal because slot_stock.id is issued by the database one statement earlier.
		//
		// The NOT EXISTS keys on (clinic_code, slot_id, reason) — one lot receives once, so
		// a re-run adds nothing. It cannot key on the lot alone: a withdrawal from the same
		// lot is also a stock_movement row and must not be mistaken for this one.
		moveName, _ := clip(name, 200)
		moveUnit, _ := clip(unit, 32)
		// qty_delta is numeric(10,2) while slot_stock.amount is numeric(14,2), so the
		// same source quantity is cut to a SMALLER figure here. That is the cost of ก1
		// the owner accepted: for a clamped lot the receive ledger will not add up to
		// what the shelf says. Every such lot is named in the report.
		moveQty, cutMove := clampToColumn("stock_movement", "qty_delta", qty)
		clamps.record(code, mongoID, "stock_movement.qty_delta", qty, moveQty, cutMove)
		sMove.comment("%s · รับเข้า lot %s → คลัง %s", code, mongoID, branch)
		sMove.addf(`INSERT INTO stock_movement
    (item_code, item_name, branch_code, clinic_code, qty_delta, unit,
     reason, note, slot_id, lot_code, created_at)
SELECT %s, %s, %s, %s, %s, %s,
       'receive', 'migrate v2', s.id, %s, %s
  FROM slot_stock s
 WHERE s.mongo_id = %s
   AND NOT EXISTS (
       SELECT 1 FROM stock_movement m
        WHERE m.clinic_code = %s
          AND m.slot_id     = s.id
          AND m.reason      = 'receive'
   );

`, lit(code), lit(moveName), lit(branch), lit(in.ClinicCode),
			moveQty, nullLit(moveUnit),
			lit(mongoID), tsLit(d.Text("store"), "now()"),
			lit(mongoID), lit(in.ClinicCode))
		movesWritten++
	}

	eItems.Statements = sItems.stmts
	eCatalog.Statements = sCatalog.stmts
	eCatalog.SourceRows = len(firstSeen)
	eSlot.Statements = sSlot.stmts

	// Counted as "how many did V2 switch off", because that is now the only thing that
	// decides it. The old message blamed a missing price and the five label fields, which
	// stopped being the rule on 15 Sep 2026 — leaving it would send the operator looking for
	// data to fill in that would change nothing.
	if draft > 0 {
		rep.notef("catalog_item(drug): %d SKU เป็น status=draft เพราะระบบเดิมปิดใช้งานไว้ (items.active=false) — รับเข้าคลังและเห็นในรายการได้ แต่ขายไม่ได้จนกว่าจะเปิดใช้งานในหน้าตั้งค่า · ที่เหลือเข้าเป็น status=1 ตามที่ระบบเดิมบอกว่าใช้งาน", draft)
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
	// Last thing in the drug batch, so it runs in the same transaction as the rows it
	// judges and a failure takes them with it. See itemsProvenanceGuard.
	sCatalog.addf("%s", itemsProvenanceGuard(in.ClinicCode))
	eCatalog.Statements = sCatalog.stmts

	eMove.SourceRows = movesWritten
	eMove.Statements = sMove.stmts
	if movesWritten != sSlot.stmts {
		// Cannot happen while both are written in the same iteration, and said out loud
		// anyway: a lot with no receive row is inventory that never arrived, which is
		// exactly the state this entity was added to end.
		rep.warnf("stock_movement: เขียน %d แถว แต่ล็อตมี %d แถว — ล็อตที่ไม่มีบรรทัดรับเข้าคือของที่โผล่มาในคลังโดยไม่มีประวัติ",
			movesWritten, sSlot.stmts)
	}
	if movesWritten > 0 {
		rep.notef("stock_movement: เขียนบรรทัดรับเข้า (reason='receive') ให้ทุกล็อต %d แถว — V4 ไม่เคยเปิดล็อตโดยไม่มีบรรทัดนี้ "+
			"(ReceiveLot และ GuidedReceive เรียก InsertStockReceipt ในทรานแซกชันเดียวกับล็อต) · "+
			"วันที่ใช้ first_date ของล็อตเดิม ไม่ใช่วันที่ import จึงไม่ยัดของหลายปีมากองวันเดียว · "+
			"user_id เป็น NULL เพราะไม่มีคนกดรับ · ถ้าไม่มีแถวพวกนี้ รายงานรับเข้าและหน้าความเคลื่อนไหวสต็อกจะว่างทั้งที่มีของ",
			movesWritten)
	}

	rep.notef("items: %d เอกสาร → %d SKU (ที่เหลือคือล็อตของ code เดิม → slot_stock)", len(mc.items), len(firstSeen))
	rep.notef("slot_stock: คลังของแต่ละสาขามาจาก branch.stock_id ในตาราง branch (ไม่ได้อ่านจากสตริง stocks.stock_branch) · "+
		"พบสาขาที่มีคลังผูกไว้ %d สาขา", len(stockOwner))
	rep.notef("slot_stock: cost_lot = ต้นทุนต่อหน่วย × จำนวนในล็อต (คนละค่ากับ cost_per_unit) ตามที่ V4 คำนวณใน GuidedReceive — " +
		"เดิมใส่ค่าเดียวกันทั้งสองช่อง ทำให้มูลค่าคลังของล็อตที่มีหลายหน่วยต่ำกว่าความจริง")
	clamps.report(rep)
	return sItems.String(), sCatalog.String(), sSlot.String(), sMove.String(), nil
}

// lotCostExpr renders slot_stock.cost_lot: the cost of the WHOLE lot, which is the unit
// cost times the quantity in it.
//
// The product is rendered as SQL so Postgres multiplies exact numerics — the same reason
// moneyLit hands source digits straight through instead of parsing them into a float64.
// The two short-circuits are not an optimisation: they keep a zero-cost or zero-quantity
// lot rendering as the literal `0` that every existing generated file already carries, so
// a diff between two runs shows only the lots whose value actually changed.
// A product too wide for cost_lot is capped in SQL rather than in Go, because the
// multiplication itself happens in Postgres: a quantity cut to 999999999999.99 times any
// real unit cost overflows numeric(14,2) at run time, where no literal check can see it.
// LEAST is only emitted when the digits cannot be shown to fit, so every lot whose value
// was already safe renders exactly as it did before and two runs still diff cleanly.
func lotCostExpr(unitCost, qty string) string {
	c, q := moneyLit(unitCost), moneyLit(qty)
	if c == "0" || q == "0" {
		return "0"
	}
	product := fmt.Sprintf("(%s::numeric * %s::numeric)", c, q)
	if lim, ok := columnNumeric["slot_stock"]["cost_lot"]; ok && !productFits(c, q, lim) {
		return fmt.Sprintf("LEAST(%s, %s::numeric)", product, numericCeiling(lim))
	}
	return product
}

/*
itemsProvenanceGuard refuses the import when a ยา/เวชภัณฑ์ row has no provenance.

THE RULE (owner, 22 Sep 2026): for an imported drug/goods SKU, catalog_item.source_id
and catalog_item.legacy_source_key must never be empty.

# WHY IT IS A REFUSAL AND NOT A WARNING

A blank source_id is not a missing nicety, it is a SKU that can never be sold. The
chain, all of it silent:

 1. source_id NULL
 2. trg_catalog_capture_legacy_source_key (0296/0301) reads `items` through source_id,
    so it sets legacy_source_key to NULL too
 3. nonSellableCatalogCodes (billing/helpers.go) refuses a drug when `i.id IS NULL` —
    BEFORE it looks at a single label field

So the SKU is unsellable, and no amount of correct label data fixes it. It went
unnoticed through two tenants (C2EC1E3 450 rows, C6C936C 221) precisely because every
layer reported success: the import finished, psql printed no warning, the catalogue
listed the row, and only the cashier ever found out.

WHAT IT CHECKS

  - source_id present on every drug/goods row of this clinic
  - legacy_source_key present on the same set — checked separately rather than assumed
    from the first, because it is written by a TRIGGER. If that trigger is ever dropped
    or altered, the first check passes and this one catches it.
  - the link points at an items row of the SAME clinic and the SAME code. A link to
    another product's row would attach one drug's label to another drug's bill, which
    is worse than no link at all and is invisible on screen.

It reads catalog_item back rather than trusting the generator, so it also covers rows
an earlier run left behind. `items` is written in an earlier group that has already
committed, so the subquery can see it.
*/
func itemsProvenanceGuard(clinic string) string {
	return fmt.Sprintf(`-- ── ด่าน: ยา/เวชภัณฑ์ ต้องมี source_id และ legacy_source_key ─────────────
-- ว่างแม้แถวเดียว = SKU นั้นขายไม่ได้ตลอดไป และกรอกฉลากยาก็ไม่ช่วย เพราะประตูคิดเงิน
-- ปฏิเสธที่ "หาแถว items ไม่เจอ" ก่อนจะดูฉลาก · เคยหลุดมาสองคลินิกเพราะทุกชั้นรายงานว่าสำเร็จ
DO $prov$
DECLARE no_id bigint; no_key bigint; wrong_target bigint; sample text;
BEGIN
    SELECT count(*) INTO no_id
      FROM catalog_item
     WHERE clinic_code = %[1]s AND item_type IN ('drug','goods') AND source_id IS NULL;
    IF no_id > 0 THEN
        SELECT string_agg(code, ', ' ORDER BY code) INTO sample
          FROM (SELECT code FROM catalog_item
                 WHERE clinic_code = %[1]s AND item_type IN ('drug','goods')
                   AND source_id IS NULL
                 ORDER BY code LIMIT 10) s;
        RAISE EXCEPTION 'ยา/เวชภัณฑ์ %% รายการไม่มี source_id — ขายไม่ได้ตลอดไปและกรอกฉลากยาก็ไม่ช่วย · ตัวอย่างรหัส: %%',
            no_id, sample;
    END IF;

    -- เขียนโดย trigger ไม่ใช่โดยสคริปต์นี้ จึงตรวจแยก: ถ้า trigger ถูกถอดหรือแก้ ด่านบนจะผ่านแต่ด่านนี้จับได้
    SELECT count(*) INTO no_key
      FROM catalog_item
     WHERE clinic_code = %[1]s AND item_type IN ('drug','goods') AND legacy_source_key IS NULL;
    IF no_key > 0 THEN
        RAISE EXCEPTION 'ยา/เวชภัณฑ์ %% รายการไม่มี legacy_source_key — trg_catalog_capture_legacy_source_key ไม่ทำงาน (migration 0296/0301) ตรวจว่า trigger ยังอยู่',
            no_key;
    END IF;

    -- ชี้ผิดตัวอันตรายกว่าไม่ชี้: ฉลากของสินค้าหนึ่งจะไปติดบิลของอีกสินค้า และมองไม่เห็นบนหน้าจอ
    SELECT count(*) INTO wrong_target
      FROM catalog_item ci
     WHERE ci.clinic_code = %[1]s AND ci.item_type IN ('drug','goods')
       AND NOT EXISTS (SELECT 1 FROM items i
                        WHERE i.id = ci.source_id
                          AND i.clinic_code = ci.clinic_code
                          AND i.code = ci.code);
    IF wrong_target > 0 THEN
        RAISE EXCEPTION 'ยา/เวชภัณฑ์ %% รายการชี้ไปที่แถว items ของคลินิกอื่นหรือรหัสอื่น', wrong_target;
    END IF;
END $prov$;

`, lit(clinic))
}

// slotDoc keeps the lot's source fields that V4 has no column for, so the row can be
// traced back to the export.
// chooseMasterDocs decides, per stock code, WHICH of its `items` documents supplies the
// catalogue row: the name, category, unit, cost, price and the active flag that becomes
// catalog_item.status.
//
// # WHY A CODE HAS SEVERAL DOCUMENTS AT ALL
//
// In V2 the `items` collection is not a product register, it is a row per (code × stock
// room), each holding its own quantity AND its own price. On the pb-plasticsurgery export:
// 817 codes across 1,287 documents, 467 of them present in both rooms.
//
// # WHAT THIS USED TO DO, AND WHAT IT COST
//
// The first document in dump order won. mongoexport orders by _id, and an ObjectId begins
// with its creation time, so the room created in 2020 (MainStock, stock_branch=center)
// came before the room created in 2024 (the branch's own) nearly every time. The branch
// room is where the stock actually sits and where staff price it, so a SKU priced only
// there arrived in V4 at 0: ผ้าคาดผม 350→0 with 42 in hand, ผ้ารัดหน้า L/M/XL 350→0,
// Augmentin 25→0 with 916 in hand. Five SKUs the clinic sells every day, unsellable or
// free, with nothing in any log to say why.
//
// THE RULE (owner, 23 Sep 2026): the branch room's document wins, unconditionally.
//
// Unconditionally is the owner's explicit choice over two softer variants that were
// offered and declined, and it has two consequences measured on this same export:
//
//	ST000770 Skinvive     MainStock 15,900 · branch room 0  → the SKU arrives at 0
//	ST000728 Dermalogica  branch room's document is blank   → name/category/unit come
//	                                                          from nothing
//
// Neither is a bug in this function. The name falls back to the code (firstNonEmpty on
// name1/name2 → code), category to 'general' and unit to 'ชิ้น', so the row is still
// usable; it just carries less than the other document knew. Recorded here because the
// next person to read a 0 price on Skinvive deserves to find the decision rather than
// guess at a defect.
//
// A code present in only ONE room is untouched by all of this: 350 of the 817 codes are
// MainStock-only, 8 of them priced, and every one keeps what it had.
//
// A room belonging to a branch this import is NOT bringing over does not get to speak
// either — its lots are dropped (they arrive with that branch's own clinic), so letting
// it set the price here would price this clinic's catalogue from another company's shelf.
func chooseMasterDocs(docs []mongodump.Doc, stockOwner map[string]string, in Input) map[string]string {
	firstInFile := map[string]string{}
	fromBranchRoom := map[string]string{}

	for _, d := range docs {
		code := strings.TrimSpace(d.Text("code"))
		if code == "" {
			continue
		}
		id := docObjectID(d, "_id")
		if id == "" {
			continue
		}
		if _, seen := firstInFile[code]; !seen {
			firstInFile[code] = id
		}
		legacyBranch, ownedByBranch := stockOwner[docObjectID(d, "stock")]
		if !ownedByBranch || !knowsBranch(in, legacyBranch) {
			continue
		}
		// First branch room wins when a tenant has several: dump order is stable, so two
		// runs of the same export pick the same document. Which of two branches should
		// price a shared SKU is a question no export answers, and inventing an answer per
		// run would make the same import produce different catalogues.
		if _, seen := fromBranchRoom[code]; !seen {
			fromBranchRoom[code] = id
		}
	}

	out := make(map[string]string, len(firstInFile))
	for code, id := range firstInFile {
		if branchDoc, ok := fromBranchRoom[code]; ok {
			out[code] = branchDoc
			continue
		}
		out[code] = id
	}
	return out
}

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
	zeroQty := 0
	// How many catalog_item_child edges were written beside the course lines. Counted and
	// reported because a course whose contents are invisible on screen looks like an import
	// that lost data, and this number is what proves the projection was written too.
	childEdges := 0

	// medicien_id → items.code, so a course med resolves to its drug SKU the same way a
	// service line resolves through healCode. Built once per run.
	itemCodeByID := map[string]string{}
	for _, it := range mc.items {
		if id := docObjectID(it, "_id"); id != "" {
			if c := it.Text("code"); c != "" {
				itemCodeByID[id] = c
			}
		}
	}
	medEmitted, medNoSvc, medNoItem, medZeroQty := 0, 0, 0, 0
	// medChildEdges is counted apart from childEdges (the service lines') because the two
	// answer different questions and the report says so separately: a service edge missing
	// means the course looks empty on screen, a medication edge missing means a paying
	// customer silently does not get the drugs.
	medChildEdges := 0

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

				// The SAME edge, written a second time into catalog_item_child.
				//
				// WHY BOTH TABLES
				//
				// V4 keeps a course's contents in two places and the query files say which is
				// which: catalog_course_line carries "canonical course composition identities"
				// (course_lines.sql), catalog_item_child is "the flattened price/allocation
				// projection" (itemtax.sql, courses.sql). Writing only the first is what left
				// 523 imported courses looking EMPTY on screen while 545 lines sat in the
				// database — courses.sql joins catalog_item_child, not catalog_course_line.
				//
				// It is not only the screen. catalog_item_child also feeds:
				//   · course_bought.sql   — finds the service to deduct when a course is used
				//   · itemtax.sql         — the weight each line gets when tax is allocated
				//   · billing.sql         — what a package contains at invoice time
				// So the omission is a billing and tax problem, not a cosmetic one.
				//
				// relation_kind = 'composition' (the column default, and what every existing
				// row in the database uses for a course parent: 201 rows on the clinic
				// imported before this, 3 on the app's own test tenant). 'bom' is the other
				// allowed value and means a service's materials, which this is not.
				//
				// qty and sort_order are copied from the same source values as the line above,
				// so the two tables cannot disagree — verified on the reference clinic, where
				// all 201 pairs match on qty.
				sLine.addf(`INSERT INTO catalog_item_child
    (clinic_code, parent_item_id, child_item_id, qty, sort_order, relation_kind, created_at)
SELECT %s, %s, %s, %s, %d, 'composition', now()
WHERE %s IS NOT NULL AND %s IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM catalog_item_child ch
       WHERE ch.clinic_code = %s
         AND ch.parent_item_id = %s
         AND ch.child_item_id  = %s
  );

`, lit(in.ClinicCode), catalogItemIDByCode(in.ClinicCode, code),
					catalogItemIDByCode(in.ClinicCode, childCode), qty, i,
					catalogItemIDByCode(in.ClinicCode, code),
					catalogItemIDByCode(in.ClinicCode, childCode),
					lit(in.ClinicCode), catalogItemIDByCode(in.ClinicCode, code),
					catalogItemIDByCode(in.ClinicCode, childCode))
				eLine.SourceRows++
				childEdges++
			}
		}

		// list_medicien → catalog_course_line rows of kind 'medication'. Each entry
		// DOES name its service: heal_id is the treatment the med is used under
		// (= parent_service_catalog_item_id, which V4 requires NOT NULL for a med line),
		// and medicien_id is the drug. Both resolve to their catalog_item by code, the
		// same way a service line resolves through healCode. (An earlier version wrongly
		// believed the source omitted the service and dropped every med line.)
		if med, ok := d.Get("list_medicien"); ok {
			if arr, isArr := med.([]any); isArr {
				for mi, raw := range arr {
					line, isDoc := raw.(mongodump.Doc)
					if !isDoc {
						continue
					}
					svcCode := mc.healCode[docObjectID(line, "heal_id")]
					childCode := itemCodeByID[docObjectID(line, "medicien_id")]
					if svcCode == "" {
						medNoSvc++
						eLine.skip("ยาในคอร์ส: heal_id ไม่ตรงบริการใน listheals")
						continue
					}
					if childCode == "" {
						medNoItem++
						eLine.skip("ยาในคอร์ส: medicien_id ไม่ตรง items ใด")
						continue
					}
					qty := intLitDefault(line.Text("qty_use"), 1)
					if q, cerr := strconv.Atoi(qty); cerr != nil || q <= 0 {
						medZeroQty++
						eLine.skip("ยาในคอร์ส: qty_use เป็น 0 หรือน้อยกว่า")
						continue
					}
					courseID := catalogItemIDByCode(in.ClinicCode, code)
					childID := catalogItemIDByCode(in.ClinicCode, childCode)
					svcID := catalogItemIDByCode(in.ClinicCode, svcCode)
					sLine.comment("%s → ยา %s ใต้บริการ %s ×%s", code, childCode, svcCode, qty)
					sLine.addf(`INSERT INTO catalog_course_line
    (clinic_code, course_catalog_item_id, child_catalog_item_id, line_kind,
     parent_service_catalog_item_id, qty, sort_order, created_at, updated_at)
SELECT %s, %s, %s, 'medication', %s, %s, %d, now(), now()
WHERE %s IS NOT NULL AND %s IS NOT NULL AND %s IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM catalog_course_line l
       WHERE l.clinic_code = %s
         AND l.course_catalog_item_id = %s
         AND l.child_catalog_item_id  = %s
         AND l.line_kind = 'medication'
         AND COALESCE(l.parent_service_catalog_item_id, 0) = COALESCE(%s, 0)
  );

`, lit(in.ClinicCode), courseID, childID, svcID, qty, 1000+mi,
						courseID, childID, svcID,
						lit(in.ClinicCode), courseID, childID, svcID)

					// THE SAME EDGE INTO catalog_item_child — decided 22 Sep 2026, replacing
					// the earlier rule that a drug line got no child row.
					//
					// That rule was argued from the course EDITOR: catalog_item_child is a
					// flat parent→child edge with nowhere to record "used under service X",
					// and the screen that lists a course's drugs (ListCourseMedications)
					// reads catalog_course_line. Both statements are still true. What they
					// miss is what happens when the course is SOLD.
					//
					// At sale time V4 does not read catalog_course_line at all.
					// billing/service.go calls ListCourseCatalogCompositionForPurchase
					// (query/course_bought.sql), which joins catalog_item_child ON
					// relation_kind='composition' and nothing else, then maps a child of
					// item_type 'drug'/'goods' to kind='medication' when it writes
					// course_bought_items. So without this row the customer pays for the
					// course, receives its treatments as entitlements, and receives NONE of
					// its drugs — and a course whose only contents are drugs fails the whole
					// payment on `len(children) == 0` with
					// "ยังสร้างสิทธิคอร์สไม่ได้: … ไม่มีรายการย่อยที่พร้อมใช้ในแค็ตตาล็อก".
					//
					// Which service the drug belongs under is not lost: that is precisely
					// what catalog_course_line above keeps. Two tables, two questions.
					//
					// uniq_catalog_item_child is (parent_item_id, child_item_id,
					// relation_kind) (migration 0291), so one drug used under two services in
					// the same course yields ONE edge — first line wins, the second is
					// skipped by the NOT EXISTS and its qty is not added. That is the same
					// outcome 11_fix_course_meds_C2EC1E3.sql already produced in production,
					// so replaying this against that clinic changes nothing.
					//
					// The self guard is load-bearing: catalog_item_child_self_chk (0262)
					// refuses parent = child, and a 23514 raised here aborts the ENTIRE
					// import, taking every other table with it.
					sLine.addf(`INSERT INTO catalog_item_child
    (clinic_code, parent_item_id, child_item_id, qty, sort_order, relation_kind, created_at)
SELECT %s, %s, %s, %s, %d, 'composition', now()
WHERE %s IS NOT NULL AND %s IS NOT NULL
  AND %s <> %s
  AND NOT EXISTS (
      SELECT 1 FROM catalog_item_child ch
       WHERE ch.clinic_code = %s
         AND ch.parent_item_id = %s
         AND ch.child_item_id  = %s
  );

`, lit(in.ClinicCode), courseID, childID, qty, 1000+mi,
						courseID, childID,
						courseID, childID,
						lit(in.ClinicCode), courseID, childID)

					eLine.SourceRows++
					medEmitted++
					medChildEdges++
				}
			}
		}
	}

	eCourse.Statements = sCourse.stmts
	eLine.Statements = sLine.stmts

	rep.notef("catalog_item(course): ต้นทางไม่มีรหัสคอร์ส จึง mint ให้เป็น %s001.. ตามลำดับในไฟล์ (deterministic · เลี่ยงรหัสที่บริการใช้อยู่)",
		CourseCodePrefix)
	rep.notef("catalog_course_line: เขียน %d บรรทัดคู่กับ catalog_item_child อีก %d แถว (relation_kind='composition') — "+
		"V4 เก็บองค์ประกอบคอร์สสองที่: catalog_course_line เป็นตัวตน ส่วน catalog_item_child เป็นตัวที่จอรายการคอร์ส "+
		"การตัดใช้คอร์ส การกระจายภาษี และการออกบิลอ่าน · เขียนแค่ตารางแรกคอร์สจะขึ้นจอว่างเปล่าทั้งที่ข้อมูลอยู่ในฐาน",
		childEdges, childEdges)
	if len(unresolved) > 0 {
		total := 0
		for _, v := range unresolved {
			total += v
		}
		rep.warnf("catalog_course_line: %d บรรทัดอ้าง heal_id ที่ไม่มีใน listheals (%d id) — ไม่ import บรรทัดนั้น คอร์สจะมีของไม่ครบ",
			total, len(unresolved))
	}
	if medEmitted > 0 || medNoSvc > 0 || medNoItem > 0 || medZeroQty > 0 {
		rep.notef("catalog_course_line: เขียนยาในคอร์ส (line_kind='medication') %d บรรทัด · parent_service มาจาก heal_id ใน list_medicien · ข้าม [heal_id ไม่ตรงบริการ: %d, medicien_id ไม่ตรง items: %d, qty<=0: %d]",
			medEmitted, medNoSvc, medNoItem, medZeroQty)
	}
	if medChildEdges > 0 {
		rep.notef("catalog_item_child: เขียนยาในคอร์สเป็น edge ของคอร์สอีก %d แถว (relation_kind='composition') — "+
			"ตอนขายคอร์สจริง V4 อ่าน catalog_item_child ไม่ได้อ่าน catalog_course_line "+
			"(ListCourseCatalogCompositionForPurchase → course_bought_items) ถ้าไม่มีแถวพวกนี้ "+
			"ลูกค้าจ่ายเงินแล้วได้สิทธิ์แต่บริการ ไม่ได้ยา · และคอร์สที่มีแต่ยาจะจ่ายเงินไม่ผ่านทั้งใบ · "+
			"ยาตัวเดียวที่อยู่ใต้หลายบริการในคอร์สเดียวกันจะได้ edge เดียว (แถวแรกชนะ)",
			medChildEdges)
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

/*
buildCatalogPublish turns the storefront switch ON for the services and courses just imported.

# WHAT THE SWITCH IS

The "หน้าร้าน" column on the catalog screen is catalog_item_channel.published for the clinic's
`online` channel (migration 0263). Before this, every imported item showed it OFF, and not
because anything was set to false: THE ROWS DID NOT EXIST. Measured on agafar-clinic —

	catalog_channel rows       0
	catalog_item_channel rows  0

— so the screen was rendering the column default. 0263 created the `online` channel only for
clinics that already had catalog_item rows when it ran, which no clinic imported afterwards did.
That is why the channel is created here too, not just the publish rows.

SCOPE: SERVICE AND COURSE ONLY

Owner's decision, 18 Sep 2026 (option A of three). Drugs are deliberately left unpublished —
`drug` is 398 rows on that same clinic of which 128 are draft, and a pharmacy line on a public
page is a different conversation from a treatment or a package.

Status is NOT filtered. A draft row (status 2) that is published still does not sell, because
draft is what withholds selling; filtering here would mean the operator turning a draft into
an active item silently gets a different storefront answer than the one they set.

⚠️ TAX IS EMPTY ON EVERY ROW THIS PUBLISHES

Measured: tax_treatment and tax_item_kind are blank on 703/703 services and 523/523 courses —
which is the "ยังไม่ระบุภาษี" badge on screen. Publishing puts those prices in front of a
customer who can buy them, and the receipt that follows is built from tax fields nobody has
filled in. The owner was asked directly and chose to publish; the count is reported on EVERY
import instead of being written into a comment nobody reads.
*/
func buildCatalogPublish(in Input, rep *Report) string {
	e := rep.entity(EntCatalogPublish)
	var s sqlBuilder
	c := lit(in.ClinicCode)

	s.comment("เปิดหน้าร้านออนไลน์ให้บริการและคอร์สที่นำเข้า (owner เคาะ 18 ก.ย. 2569)")
	s.comment("ยาไม่ถูกเปิด — เป็นการตัดสินใจแยก")

	// The channel first. ON CONFLICT keyed on (clinic_code, channel_key) makes a re-import a
	// no-op rather than a duplicate-key failure, and it also covers the clinic that migration
	// 0263 did create one for.
	s.addf(`INSERT INTO catalog_channel (clinic_code, channel_key, label)
VALUES (%s, 'online', 'หน้าร้านออนไลน์')
ON CONFLICT (clinic_code, channel_key) DO NOTHING;

`, c)

	// One statement for the whole catalog rather than one per item. 1,226 rows on the clinic
	// measured, and a per-row form would also have to resolve the id it does not know yet.
	//
	// ON CONFLICT (item_id, channel_id) DO NOTHING, not DO UPDATE: if a row is already there,
	// somebody has since decided its publish state on screen, and an import must not overrule
	// a human who has touched it.
	s.addf(`INSERT INTO catalog_item_channel (clinic_code, item_id, channel_id, published, published_at)
SELECT ci.clinic_code, ci.id, ch.id, true, now()
  FROM catalog_item ci
  JOIN catalog_channel ch
    ON ch.clinic_code = ci.clinic_code AND ch.channel_key = 'online'
 WHERE ci.clinic_code = %s
   AND ci.item_type IN ('service', 'course')
ON CONFLICT (item_id, channel_id) DO NOTHING;

`, c)

	// Said by the database after the fact, not guessed in Go: how many went up, and how many of
	// them have no tax answer. The generator cannot know either number — the rows are selected
	// at load time — so this is a NOTICE rather than a report line.
	s.addf(`DO $publish$
DECLARE
    n_pub int;
    n_notax int;
BEGIN
    SELECT count(*) INTO n_pub
      FROM catalog_item_channel cic
      JOIN catalog_item ci ON ci.id = cic.item_id
      JOIN catalog_channel ch ON ch.id = cic.channel_id AND ch.channel_key = 'online'
     WHERE cic.clinic_code = %s AND cic.published
       AND ci.item_type IN ('service', 'course');

    SELECT count(*) INTO n_notax
      FROM catalog_item_channel cic
      JOIN catalog_item ci ON ci.id = cic.item_id
      JOIN catalog_channel ch ON ch.id = cic.channel_id AND ch.channel_key = 'online'
     WHERE cic.clinic_code = %s AND cic.published
       AND ci.item_type IN ('service', 'course')
       AND (coalesce(ci.tax_treatment, '') = '' OR coalesce(ci.tax_item_kind, '') = '');

    -- The doubled percent below is Go's escape for a literal one. Postgres then sees a single
    -- percent and treats it as its own RAISE placeholder; without the doubling, go vet reads
    -- it as a bad format verb and the package will not build.
    RAISE NOTICE 'หน้าร้าน: เปิดขายออนไลน์ %% รายการ (บริการ+คอร์ส)', n_pub;
    IF n_notax > 0 THEN
        RAISE NOTICE 'หน้าร้าน: ⚠ %% รายการยังไม่ระบุภาษี แต่ถูกเปิดขายแล้ว — ต้องกรอกภาษีก่อนออกใบกำกับ', n_notax;
    END IF;
END $publish$;

`, c, c)

	e.Statements = s.stmts
	rep.warnf("หน้าร้าน: บริการและคอร์สที่นำเข้าจะถูกเปิดขายออนไลน์ทันที (owner เคาะ) · " +
		"ยาไม่ถูกเปิด · ข้อมูลภาษีของรายการที่นำเข้าว่างทั้งหมด ต้องกรอกก่อนออกใบกำกับภาษี — " +
		"จำนวนจริงจะขึ้นเป็น NOTICE ตอนรันสคริปต์")
	return s.String()
}
