package legacyimport

import "fmt"

/*
buildCatalogSKUIssue hands every catalog row this import wrote its machine-issued
`catalog_item.sku`, and parks the per-(clinic, item_type) counter above the highest
number it gave out.

# WHY THIS EXISTS

`catalog_item.sku` (migration 0367) is the PREFIX-NNNNNN code staff read out loud,
print and search by: SVC-000001, MED-000001, CRS-000001, PRD-000001, VCH-000001. It
is machine-issued and never user-editable, and V4 has exactly one writer for it —
catalog.CreateSKU in internal/controller/catalog/sku.go, which bumps
`catalog_item_sku_counter` and formats the result.

This import is not that writer. upsertCatalogItem in catalog.go INSERTs catalog_item
with an explicit column list that has no `sku` in it, so every imported row landed
with sku = NULL and the counter table had no row for the tenant at all. Migration
0367's own backfill could not cover them either: it numbered the rows that existed
when it ran, and these arrive long after.

Measured on prod 2026-09-22, before this was added:

	C2EC1E3  2,503 catalog rows  0 with sku
	C9D4865    359 catalog rows  0 with sku

while all five clinics that predate the CLI sat at 100%. Both had to be repaired by
hand (20_backfill_sku_C2EC1E3_C9D4865.sql); this makes that script unnecessary for
the next tenant.

# WHAT IT BREAKS WHEN IT IS MISSING

Nothing financial. No table holds sku as a foreign key — migration 0367 forbids that
precisely so the column can be renumbered — so selling, stock movement and billing
are unaffected. What the clinic loses is the SKU column on the catalogue screen
(blank on every row, repo.go reads coalesce(ci.sku,”)) and SKU search, which is one
of the five fields the catalogue search box sweeps.

# WHY A SET-BASED PASS INSTEAD OF WRITING sku IN upsertCatalogItem

upsertCatalogItem renders one INSERT per row out of literals; it holds no counter and
knows nothing about the rows around it. Issuing there would mean a counter round-trip
inside each of a few thousand statements, plus a rule for what the ON CONFLICT DO
UPDATE branch should do with a code the row already has. One ordered pass after the
rows are in is what migration 0367 does, is replay-safe, and numbers by
catalog_item.id — real import order, which for a V2 export means ST000001 → MED-000001,
LH001 → SVC-000001, CS001 → CRS-000001.

# EVERY STATUS IS NUMBERED

0 deleted, 1 active, 2 draft alike, because uniq_catalog_item_clinic_sku covers every
status and a sku may never be reused. A soft-deleted row without one would otherwise
be handed a number that a live row already carries the day someone undeletes it.

# REPLAY AND CONCURRENCY

Only `sku IS NULL` rows are touched, so a second run of the same script updates zero
rows. start_at is one above BOTH the counter row and the highest sku already present,
so a code minted through the app between two runs can never be duplicated, and
GREATEST on conflict means the counter can only move forward — replaying an older
export cannot rewind it and start handing out numbers that are already in use.

`catalog_item_sku_counter` carries the tenant_isolation policy, so this must run
inside a transaction that has already issued `SET LOCAL app.clinic_code`, which every
group transaction in this script does. Without it the policy would match no rows and
the INSERT would write nothing while psql still reported success.

No schema guard: the tool already reads catalog_item.tax_treatment (migration 0410),
so a target that can run this import necessarily has 0367.
*/
func buildCatalogSKUIssue(clinic string) string {
	return fmt.Sprintf(`-- ── ออกรหัส SKU ให้รายการที่เพิ่งนำเข้า (catalog_item.sku) ──────────────
-- SKU คือรหัสที่เครื่องออกให้ คนแก้ไม่ได้ ใช้พูด/พิมพ์/ค้นหา (SVC- PRD- MED- CRS- VCH-)
-- การนำเข้าเขียน catalog_item ตรง ไม่ผ่าน catalog.CreateSKU → ถ้าไม่ทำข้อนี้ ช่อง SKU
-- ในหน้าแค็ตตาล็อกว่างทุกแถว และค้นด้วย SKU ไม่เจอ (ไม่กระทบการขาย/สต๊อก/บิล)
-- เรียงเลขตาม catalog_item.id = ลำดับนำเข้าจริง → ST000001→MED-000001, LH001→SVC-000001
-- ให้เลขทุกสถานะรวมที่ลบแล้ว(0)/ฉบับร่าง(2) เพราะ sku ห้ามใช้ซ้ำข้ามสถานะ
-- รันซ้ำได้: แตะเฉพาะแถวที่ sku ว่าง และ GREATEST ทำให้ตัวนับเดินหน้าอย่างเดียว
WITH base AS (
    SELECT ci.clinic_code, ci.item_type,
           GREATEST(
               COALESCE(max(substring(ci.sku from 5)::bigint), 0),
               COALESCE(max(c.counter), 0)
           ) AS start_at
      FROM catalog_item ci
      LEFT JOIN catalog_item_sku_counter c
             ON c.clinic_code = ci.clinic_code
            AND c.item_type   = ci.item_type
     WHERE ci.clinic_code = %[1]s
     GROUP BY 1, 2
), numbered AS (
    SELECT ci.id, ci.item_type,
           b.start_at + row_number() OVER (
               PARTITION BY ci.item_type ORDER BY ci.id) AS n
      FROM catalog_item ci
      JOIN base b ON b.clinic_code = ci.clinic_code
                 AND b.item_type   = ci.item_type
     WHERE ci.clinic_code = %[1]s
       AND ci.sku IS NULL
)
UPDATE catalog_item ci
   SET sku = CASE numbered.item_type
                 WHEN 'service'   THEN 'SVC'
                 WHEN 'goods'     THEN 'PRD'
                 WHEN 'drug'      THEN 'MED'
                 WHEN 'course'    THEN 'CRS'
                 WHEN 'gift_card' THEN 'VCH'
             END || '-' ||
             CASE WHEN numbered.n <= 999999
                  THEN lpad(numbered.n::text, 6, '0')
                  ELSE numbered.n::text END
  FROM numbered
 WHERE ci.id = numbered.id;

-- ปักตัวนับไว้เหนือเลขสูงสุดที่เพิ่งแจก เลขถัดไปที่หน้าจอออกให้ (counter + 1) จึงไม่ชน
INSERT INTO catalog_item_sku_counter (clinic_code, item_type, counter)
SELECT clinic_code, item_type, max(substring(sku from 5)::bigint)
  FROM catalog_item
 WHERE clinic_code = %[1]s
   AND sku IS NOT NULL
 GROUP BY 1, 2
ON CONFLICT (clinic_code, item_type)
DO UPDATE SET counter = GREATEST(catalog_item_sku_counter.counter, EXCLUDED.counter),
              updated_at = now();

-- ไม่ยอมจบแบบเงียบ: เหลือ NULL แม้แถวเดียว = ประเภทที่ยังไม่มี prefix ในแผนที่ข้างบน
-- (CASE ไม่มี ELSE → ได้ NULL → sku ยังว่าง) ต้องดังตรงนี้ ไม่ใช่ไปเจอตอนเปิดหน้าจอ
DO $sku$
DECLARE
    leftover bigint;
    types    text;
BEGIN
    SELECT count(*), coalesce(string_agg(DISTINCT item_type, ', '), '')
      INTO leftover, types
      FROM catalog_item
     WHERE clinic_code = %[1]s AND sku IS NULL;
    IF leftover > 0 THEN
        RAISE EXCEPTION 'ออกรหัส SKU ไม่ครบ: เหลือ %% แถวที่ยังไม่มี sku (item_type: %%) — เพิ่ม prefix ของประเภทนี้ใน catalogsku.go และ migration 0367 ก่อน',
            leftover, types;
    END IF;
END $sku$;

`, lit(clinic))
}
