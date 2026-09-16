package legacyimport

// แต้มสะสมของคนไข้ (mris.points → public.loyalty_account).
//
// WHAT THE SOURCE LOOKS LIKE
//
//	{ _id, total_point: NumberDecimal("3"), hn: "016400544", created_at, updated_at }
//
// One document per patient — except it is NOT one: the reference clinic's export holds
// 1,238 documents for 1,216 distinct HN, so 17 patients carry two or three. V4's
// loyalty_account is UNIQUE (clinic_code, hn), so those have to become one row.
//
// DECISIONS TAKEN BY THE CLINIC OWNER (15 Sep 2026), recorded because each one is money:
//
//   - SUM every document of an HN. Measured on the reference export: SUM = 38 points,
//     LAST-per-HN = 37, MAX-per-HN = 37. The one point of difference is HN 016400544
//     carrying [1, 1]. The owner chose SUM, i.e. two documents of 1 point mean the
//     patient earned twice.
//   - IMPORT EVERY HN, including the 1,193 of 1,216 whose balance is 0. A zero row is
//     not noise: it says the patient is enrolled in the scheme and has nothing saved
//     yet, which is different from never having been enrolled.
//   - point_logs (2,467 documents: add_point 2,343 / use_point 124) is NOT imported.
//     loyalty_txn therefore stays empty and the balances arrive with no ledger behind
//     them. Migration 0238 states the opposite intent for that table ("Append-only.
//     Never updated, never deleted: a balance nobody can explain is worse than no
//     balance at all"), so this is a deliberate exception, not an oversight — it is
//     written into the generated script as well so whoever reads the SQL sees it.
//   - loyalty_policy is NOT written. Without a policy row baht_per_point is absent and
//     the imported points cannot be redeemed until the clinic sets the rate on screen.
//     The owner asked for exactly that: carry the balances over, decide the rate later.
//
// DECISIONS TAKEN HERE (implementation, reversible):
//
//   - patient_id is filled from `patient`, not left NULL. It carries a real FK
//     (loyalty_account_patient_id_fkey) and an index the app reads
//     (idx_loyalty_account_patient), and every one of the 20 rows the running app has
//     written has it set. A row with NULL there is a row the app never produces.
//   - An HN with no `patient` row gets NO account. The FK would refuse a wrong id and a
//     balance owned by nobody is worse than a balance that arrives with the patients on
//     the next run. The count is reported.
//   - A non-integer or negative total refuses the whole build rather than being rounded
//     or clamped. loyalty_account.balance is `integer` with CHECK (balance >= 0), and
//     quietly turning 2.5 points into 2 (or -1 into 0) is deciding what a patient owns.
//     Every value in the reference export is a non-negative integer, so this path exists
//     to stop a surprise, not to handle a known case.
//   - ON CONFLICT DO NOTHING, not DO UPDATE: re-running the import must not overwrite a
//     balance the clinic has since changed on screen.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// EntLoyaltyAccount is the entity name for imported point balances.
const EntLoyaltyAccount = "loyalty_account"

var loyaltyOrder = []string{EntLoyaltyAccount}

// loyaltyBalance is one patient's summed balance plus how many source documents it came
// from, so the report can say how many were merged.
type loyaltyBalance struct {
	hn   string
	sum  int64
	docs int
}

// buildLoyalty maps mris.points onto loyalty_account. It renders SQL directly (unlike
// page.go) because there is nothing to copy — the whole entity is one integer per patient.
func buildLoyalty(mc *mongoCatalog, in Input, rep *Report) (string, error) {
	e := rep.entity(EntLoyaltyAccount)
	e.SourceRows = len(mc.points)

	byHN := map[string]*loyaltyBalance{}
	noHN := 0
	for n, d := range mc.points {
		hn := strings.TrimSpace(d.Text("hn"))
		if hn == "" {
			// A balance with no owner cannot be written (loyalty_account.hn is NOT NULL)
			// and cannot be guessed from anything else in the document.
			e.skip("document ไม่มี hn (loyalty_account.hn เป็น NOT NULL)")
			noHN++
			continue
		}

		raw := strings.TrimSpace(d.Text("total_point"))
		if raw == "" {
			raw = "0"
		}
		// Parsed as an integer on purpose: the column is `integer`, and a fractional
		// point would have to be rounded — which is a decision about money, not a
		// formatting detail. Refused loudly instead.
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return "", fmt.Errorf(
				"points document %d (hn=%s): total_point %q ไม่ใช่จำนวนเต็ม — loyalty_account.balance เป็น integer "+
					"การปัดเศษคือการตัดสินใจเรื่องแต้มของคนไข้ ไม่ใช่เรื่องรูปแบบตัวเลข จึงไม่เดาให้",
				n+1, hn, raw)
		}

		b := byHN[hn]
		if b == nil {
			b = &loyaltyBalance{hn: hn}
			byHN[hn] = b
		}
		b.sum += v
		b.docs++
	}

	if len(byHN) == 0 {
		return "", fmt.Errorf("mris.points ไม่มีเอกสารที่นำเข้าได้เลย (ไม่มี hn ในเอกสารใด)")
	}

	// Deterministic order: the .sql artifact is meant to be diffed between two runs, and
	// map iteration is not stable.
	hns := make([]string, 0, len(byHN))
	for hn := range byHN {
		hns = append(hns, hn)
	}
	sort.Strings(hns)

	var (
		s        sqlBuilder
		tr       = truncations{}
		merged   int
		zero     int
		total    int64
		maxPer   int64
		negative []string
	)

	for _, hn := range hns {
		b := byHN[hn]
		if b.sum < 0 {
			negative = append(negative, fmt.Sprintf("%s (%d)", hn, b.sum))
			continue
		}
		if b.docs > 1 {
			merged++
		}
		if b.sum == 0 {
			zero++
		}
		total += b.sum
		if b.sum > maxPer {
			maxPer = b.sum
		}

		hnLit, cut := clip(hn, 32)
		tr.note("hn", cut)

		c := lit(in.ClinicCode)
		h := lit(hnLit)

		if b.docs > 1 {
			s.comment("hn %s — รวม %d เอกสารเป็น %d แต้ม (มติเจ้าของคลินิก: SUM)", hn, b.docs, b.sum)
		} else {
			s.comment("hn %s — %d แต้ม", hn, b.sum)
		}
		// patient_id read in SQL at write time, exactly like photos.branch_code: the id is
		// the target's, this side has never seen it, and uq_patient_clinic_hn makes the
		// lookup match one row or none.
		s.addf(`INSERT INTO loyalty_account (clinic_code, hn, balance, patient_id, updated_at)
SELECT %s, %s, %d, p.id, now()
  FROM patient p
 WHERE p.clinic_code = %s AND p."HN" = %s
ON CONFLICT (clinic_code, hn) DO NOTHING;`, c, h, b.sum, c, h)
	}

	if len(negative) > 0 {
		// CHECK (balance >= 0) would refuse it anyway; failing here names every HN
		// instead of stopping at whichever row Postgres reached first.
		return "", fmt.Errorf(
			"loyalty_account: %d hn มีแต้มรวมติดลบ ซึ่ง CHECK (balance >= 0) ปฏิเสธ — %s · "+
				"ต้นทางมี use_point ที่หักเกินยอด ต้องให้เจ้าของคลินิกตัดสินว่าจะปรับเป็น 0 หรือแก้ที่ต้นทาง",
			len(negative), strings.Join(negative, ", "))
	}

	// Stated in the script itself, not only in the report: whoever reads this SQL later
	// has to be able to see that the ledger was left out on purpose.
	s.comment("")
	s.comment("point_logs ของ V2 (%d เอกสาร) ไม่ถูกนำเข้าตามมติเจ้าของคลินิก 15 ก.ย. 2569", len(mc.pointLogs))
	s.comment("ฉะนั้น loyalty_txn จะว่าง และยอดแต้มข้างบนจะไม่มี ledger อธิบายที่มา")
	s.comment("(migration 0238 ตั้งใจให้ loyalty_txn เป็น append-only ที่อธิบายยอด — นี่คือข้อยกเว้นที่เจาะจง)")
	s.comment("loyalty_policy ก็ไม่ถูกเขียน แต้มจึงยังแลกไม่ได้จนคลินิกตั้งอัตราเองในจอ")
	s.comment("")

	// How many accounts actually landed. A shortfall means those HNs have no patient row
	// yet, and the balances did not silently vanish — they were never written and will be
	// on the next run once the patients are in.
	s.addf(`DO $loyalty$
DECLARE
    made  bigint;
    pts   bigint;
BEGIN
    SELECT count(*), coalesce(sum(balance), 0) INTO made, pts
      FROM loyalty_account WHERE clinic_code = %s;
    IF made < %d THEN
        RAISE NOTICE 'loyalty_account: มี %%s แถว จากที่ต้องการ %d แถว — ส่วนที่ขาดคือ HN ที่ยังไม่มีในตาราง patient (loyalty_account.patient_id มี FK) · นำเข้ากลุ่ม patients ก่อนแล้วรอบถัดไปจะครบ', made;
    ELSE
        RAISE NOTICE 'loyalty_account: มีครบ %%s แถว', made;
    END IF;
    RAISE NOTICE 'loyalty_account: แต้มรวมในฐาน %%s แต้ม', pts;
END $loyalty$;`, lit(in.ClinicCode), len(hns), len(hns))

	e.Statements = s.stmts
	if sum := tr.summary(); sum != "" {
		rep.warnf("loyalty_account: truncated %s", sum)
	}

	rep.notef("loyalty_account: %d เอกสาร → %d hn · แต้มรวม %d · สูงสุดต่อคน %d · ยอด 0 อยู่ %d hn",
		len(mc.points), len(hns), total, maxPer, zero)
	if merged > 0 {
		rep.notef("loyalty_account: %d hn มีหลายเอกสารในต้นทาง — รวมยอดด้วย SUM ตามมติเจ้าของคลินิก "+
			"(ทางเลือกอื่นคือใช้เอกสารล่าสุดหรือค่ามากสุด ซึ่งให้ผลต่างกัน)", merged)
	}
	if noHN > 0 {
		rep.warnf("loyalty_account: %d เอกสารไม่มี hn — ไม่นำเข้า เพราะแต้มที่ไม่รู้ว่าเป็นของใครเขียนลงตารางไม่ได้", noHN)
	}
	if len(mc.pointLogs) > 0 {
		rep.warnf("loyalty_account: point_logs %d เอกสารไม่ถูกนำเข้า (มติเจ้าของคลินิก) — loyalty_txn จะว่าง "+
			"และยอดแต้มจะไม่มีประวัติอธิบายที่มา", len(mc.pointLogs))
	}
	rep.notef("loyalty_account: ไม่ได้เขียน loyalty_policy — แต้มที่นำเข้าจะแลกไม่ได้จนคลินิกตั้งอัตรา " +
		"(baht_per_point) และเปิด active เองในจอ")

	return s.String(), nil
}
