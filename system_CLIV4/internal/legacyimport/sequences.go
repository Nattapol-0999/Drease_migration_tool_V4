package legacyimport

import "fmt"

/*
buildSequenceBump advances the per-clinic running-number counters past the codes this
import just wrote.

WHY THIS EXISTS

V4 mints a new code by bumping `sequences` and formatting the result: a new course is
`CS%05d` from sequences.type='course', a new service is `LH%05d` from type='listheal'
(see coursesetting/repo.go and the room/branch equivalents). The import writes codes
WITHOUT going through those paths — it carries the V2 code verbatim for services and
mints CS001.. for courses — so the counters stayed at whatever the target already had,
which for a fresh tenant is nothing at all.

Measured on two real imports before this was added: C2EC1E3 held CS001–CS560 and
LH001–LH663 with no `course` or `listheal` row in `sequences` at all, and C9D4865 held
CS001–CS106 / LH001–LH104 the same way. The next course created on screen would have
been offered CS00001 — a number that reads as if the catalogue had been reset, next to
560 courses that were already there. Both clinics had to be repaired by hand
(19_bump_sequences_C2EC1E3_C9D4865.sql); this makes that script unnecessary.

Not a collision, to be exact about the risk: CS00001 (five digits) and CS001 (three) are
different strings, so `catalog_item`'s unique code index never fired. The damage was to
the numbering people read, not to the data.

WHAT IT DOES NOT TOUCH, AND WHY

  - catalog_import_drug (MED) / catalog_import_goods (PRD) — the code series V4 issues for
    stock items. The imported stock codes are V2's `ST000001..`, a different prefix
    entirely, so there is no sequence to continue. Settled with the clinic owner on
    2026-09-22: the ST codes stay as they are and new items keep taking MED/PRD.
  - requisition (RQ) — no requisition documents come from V2.
  - room / doctor / branch — already advanced by the people phase.

HOW THE VALUE IS CHOSEN

Read back out of `catalog_item` with max() rather than passed in from Go. The counter
then reflects what is ACTUALLY at the target, including rows from an earlier run of this
same import, and the statement stays correct if it is replayed. GREATEST on conflict means
it can only ever move forward — a re-run of an older export cannot rewind a counter and
start handing out numbers that are already on documents.

BumpSequenceWithPrefix increments BEFORE returning, so storing max(n) makes the next code
max(n)+1.

`sequences` carries the tenant_isolation policy, so this must run inside a transaction
that has already issued `SET LOCAL app.clinic_code` — which every group transaction in
this script does. Without it the policy would match no rows and the INSERT would write
nothing while psql still reported success.
*/
func buildSequenceBump(clinic string) string {
	type series struct {
		seqType string
		prefix  string
		note    string
	}
	all := []series{
		{"course", "CS", "คอร์สที่สร้างใหม่ในหน้าจอ (coursesetting/repo.go · CS%05d)"},
		{"listheal", "LH", "บริการที่สร้างใหม่ในหน้าจอ (LH%05d)"},
	}

	out := "-- ── เลื่อนเลขวิ่งให้พ้นรหัสที่เพิ่งนำเข้า ──────────────────────────────\n" +
		"-- ไม่ทำข้อนี้ = รายการที่สร้างใหม่ในหน้าจอเริ่มนับ 1 ใหม่ (CS00001) ทั้งที่มี CS001.. อยู่แล้ว\n" +
		"-- อ่านค่าจาก catalog_item เองด้วย max() และใช้ GREATEST ตอนชน → รันซ้ำได้ เลขไม่ถอยหลัง\n" +
		"-- ไม่แตะ catalog_import_drug (MED) / catalog_import_goods (PRD) ตามมติเจ้าของ 2026-09-22:\n" +
		"-- รหัส ST ที่นำเข้าคงไว้แบบเดิม ของที่เพิ่มใหม่ปล่อยให้ V4 ออก MED/PRD ตามการออกแบบ\n"

	for _, s := range all {
		out += fmt.Sprintf(`
-- %s
INSERT INTO sequences (clinic_code, type, prefix, counter, updated_at)
SELECT %s, %s, %s, max((regexp_replace(code, '^%s', ''))::int), now()
  FROM catalog_item
 WHERE clinic_code = %s AND code ~ '^%s[0-9]+$'
HAVING count(*) > 0
ON CONFLICT (clinic_code, type)
DO UPDATE SET counter = GREATEST(sequences.counter, EXCLUDED.counter), updated_at = now();
`, s.note, lit(clinic), lit(s.seqType), lit(s.prefix), s.prefix, lit(clinic), s.prefix)
	}

	return out + "\n"
}
