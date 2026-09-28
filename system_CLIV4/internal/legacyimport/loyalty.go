package legacyimport

// แต้มสะสมของคนไข้ (mris.points → public.loyalty_account).
//
// WHAT THE SOURCE LOOKS LIKE
//
//	{ _id, total_point: NumberDecimal("3"), hn: "016400544", created_at, updated_at }
//
// One document per patient — except it is NOT one. Two exports measured: the first
// reference clinic holds 1,238 documents for 1,216 distinct HN, and Anjali (C6C936C) holds
// 6,234 for 6,154. So some patients carry two or three documents, while V4's
// loyalty_account is UNIQUE (clinic_code, hn) and can hold exactly one row.
//
// # WHICH DOCUMENT IS THE BALANCE (corrected 28 Sep 2026)
//
// The FIRST document of an HN, in export order. Not the sum, not the largest, not the
// newest.
//
// This reverses the 15 Sep 2026 decision to SUM every document. That decision was taken
// while reading only the data; it was wrong about V2, and the correction comes from V2's
// own source:
//
//	nodejs/service/helpers/point.js
//	  usePointInPayment: PointSummary.findOne({hn})  → checks total_point, then
//	                     PointSummary.updateOne({hn}, {$set: {total_point: ...}})
//	app/Http/Controllers/PointController.php:243     → returns that total_point to the screen
//
// findOne and updateOne both touch the FIRST matching document and ignore the rest. So V2
// maintains and displays one document per HN; any others are leftovers the clinic has
// never seen and can never spend. Summing them credits a patient with points that were
// never on their screen.
//
// Measured on Anjali's live database before the change: 49 HN own more than one document,
// and 2 of them (016400498, 016400039) carry a first document of 0 beside a stale one
// holding 17,000. SUM would have credited 34,000 points nobody was ever shown.
//
// The export's order is the order the shell iterates, verified rather than assumed: for
// all 48 multi-document HN in Anjali's export, the first document in the file is the one
// the live findOne returns. (One single-document HN differed — its balance had simply
// changed in the four days between the dump and the check.)
//
// Every later document is still reported, with both numbers, so an operator can see what
// was left behind instead of having to trust this comment.
//
// # DECISIONS TAKEN BY THE CLINIC OWNER, recorded because each one is money
//
//   - 28 Sep 2026 — WRITE ONLY THE HN THAT HOLD A BALANCE. This replaces the 15 Sep rule
//     of importing every HN including the zeros. V4 opens a loyalty_account on the first
//     movement that awards something (see awardPoints in internal/controller/billing),
//     never on a look, so a row at zero is a row the running app would not have written.
//     Anjali: 6,154 HN in the export, 546 of them holding a balance.
//   - 15 Sep 2026 — point_logs is NOT imported. loyalty_txn therefore stays empty and the
//     balances arrive with no ledger behind them. Migration 0238 states the opposite intent
//     for that table ("Append-only. Never updated, never deleted: a balance nobody can
//     explain is worse than no balance at all"), so this is a deliberate exception, not an
//     oversight — it is written into the generated script as well so whoever reads the SQL
//     sees it. Reconfirmed 28 Sep after finding the reason it cannot be a ledger anyway:
//     V2's returnPointDeleteBill() runs PointLog.deleteMany({payment_id}) when a bill is
//     deleted, so the movement history is destroyed by design and cannot close to a balance.
//   - 15 Sep 2026 — loyalty_policy is NOT written. Without a policy row baht_per_point is
//     absent and the imported points cannot be redeemed until the clinic sets the rate on
//     screen. The owner asked for exactly that: carry the balances over, decide the rate
//     later. Note what else that switch does — loyaltyRates.Earns() is policy.Active alone,
//     so turning it on also starts awarding catalog_service.loyalty_points. See the credit
//     guard in catalog.go.
//
// # DECISIONS TAKEN HERE (implementation, reversible)
//
//   - patient_id is filled from `patient`, not left NULL. It carries a real FK
//     (loyalty_account_patient_id_fkey) and an index the app reads
//     (idx_loyalty_account_patient), and every row the running app writes has it set.
//   - An HN with no `patient` row gets NO account. The FK would refuse a wrong id and a
//     balance owned by nobody is worse than a balance that arrives with the patients on
//     the next run. The count is reported.
//   - A non-integer total refuses the whole build rather than being rounded.
//     loyalty_account.balance is `integer`, and quietly turning 2.5 points into 2 is
//     deciding what a patient owns. A negative FIRST document refuses too, because
//     CHECK (balance >= 0) would reject it. A negative value in a later document only
//     warns: V2 never shows it, so it cannot be this row's balance.
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

// loyaltyBalance is one patient's balance as V2 shows it, plus whatever else the export
// held for that HN so the report can name what was left behind.
type loyaltyBalance struct {
	hn string
	// balance is the FIRST document's total_point — the number V2 reads and displays.
	balance int64
	// docs counts every document of this HN, including the first.
	docs int
	// ignored holds the later documents' values, in file order. Non-empty means the export
	// carried leftovers V2 does not use.
	ignored []int64
}

// disagrees reports whether any ignored document holds a different number from the
// balance. Leftovers that happen to repeat the same value change nothing and are not worth
// an operator's attention; ones that differ are.
func (b *loyaltyBalance) disagrees() bool {
	for _, v := range b.ignored {
		if v != b.balance {
			return true
		}
	}
	return false
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

		if b := byHN[hn]; b != nil {
			// A later document. V2's findOne never reaches it, so it is recorded for the
			// report and takes no part in the balance.
			b.docs++
			b.ignored = append(b.ignored, v)
			continue
		}
		byHN[hn] = &loyaltyBalance{hn: hn, balance: v, docs: 1}
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
		s          sqlBuilder
		tr         = truncations{}
		written    int
		zero       int
		multi      int
		total      int64
		maxPer     int64
		negative   []string
		leftBehind []string
		negLater   []string
	)

	for _, hn := range hns {
		b := byHN[hn]
		if b.balance < 0 {
			negative = append(negative, fmt.Sprintf("%s (%d)", hn, b.balance))
			continue
		}
		if b.docs > 1 {
			multi++
			if b.disagrees() {
				leftBehind = append(leftBehind,
					fmt.Sprintf("%s (ยอดที่ใช้ %d · ใบที่เหลือ %s)", hn, b.balance, joinInts(b.ignored)))
			}
			for _, v := range b.ignored {
				if v < 0 {
					negLater = append(negLater, fmt.Sprintf("%s (%d)", hn, v))
				}
			}
		}
		if b.balance == 0 {
			// Owner's rule, 28 Sep 2026: no row for a patient who has nothing saved.
			zero++
			e.skip("ยอดแต้มเป็น 0 — ไม่เปิดบัญชี (มติเจ้าของคลินิก 28 ก.ย. 2569)")
			continue
		}

		written++
		total += b.balance
		if b.balance > maxPer {
			maxPer = b.balance
		}

		hnLit, cut := clip(hn, 32)
		tr.note("hn", cut)

		c := lit(in.ClinicCode)
		h := lit(hnLit)

		if b.docs > 1 {
			s.comment("hn %s — %d แต้ม (ต้นทางมี %d เอกสาร ใช้ใบแรกตามที่ V2 อ่าน · ใบที่เหลือ %s)",
				hn, b.balance, b.docs, joinInts(b.ignored))
		} else {
			s.comment("hn %s — %d แต้ม", hn, b.balance)
		}
		// patient_id read in SQL at write time, exactly like photos.branch_code: the id is
		// the target's, this side has never seen it, and uq_patient_clinic_hn makes the
		// lookup match one row or none.
		s.addf(`INSERT INTO loyalty_account (clinic_code, hn, balance, patient_id, updated_at)
SELECT %s, %s, %d, p.id, now()
  FROM patient p
 WHERE p.clinic_code = %s AND p."HN" = %s
ON CONFLICT (clinic_code, hn) DO NOTHING;`, c, h, b.balance, c, h)
	}

	if len(negative) > 0 {
		// CHECK (balance >= 0) would refuse it anyway; failing here names every HN
		// instead of stopping at whichever row Postgres reached first.
		return "", fmt.Errorf(
			"loyalty_account: %d hn มีแต้มติดลบในเอกสารใบแรก ซึ่ง CHECK (balance >= 0) ปฏิเสธ — %s · "+
				"ต้นทางมี use_point ที่หักเกินยอด ต้องให้เจ้าของคลินิกตัดสินว่าจะปรับเป็น 0 หรือแก้ที่ต้นทาง",
			len(negative), strings.Join(negative, ", "))
	}

	if written == 0 {
		return "", fmt.Errorf(
			"mris.points มี %d hn แต่ยอดเป็น 0 ทั้งหมด — ไม่มีบัญชีแต้มให้เปิด (มติ 28 ก.ย. 2569: ไม่เปิดบัญชีให้ยอด 0)",
			len(hns))
	}

	// Stated in the script itself, not only in the report: whoever reads this SQL later
	// has to be able to see what was left out on purpose.
	s.comment("")
	s.comment("ยอดต่อ hn = total_point ของเอกสารใบแรก ซึ่งเป็นตัวเลขเดียวกับที่หน้าจอ V2 แสดง")
	s.comment("(V2 อ่านด้วย PointSummary.findOne({hn}) จึงเห็นเฉพาะใบแรก ใบที่เหลือเป็นของค้างที่ลูกค้าไม่เคยเห็น)")
	s.comment("hn ที่ยอดเป็น 0 ไม่ถูกเปิดบัญชี %d ราย (มติเจ้าของคลินิก 28 ก.ย. 2569)", zero)
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
        RAISE NOTICE 'loyalty_account: มี %% แถว จากที่ต้องการ %d แถว — ส่วนที่ขาดคือ HN ที่ยังไม่มีในตาราง patient (loyalty_account.patient_id มี FK) · นำเข้ากลุ่ม patients ก่อนแล้วรอบถัดไปจะครบ', made;
    ELSE
        RAISE NOTICE 'loyalty_account: มีครบ %% แถว', made;
    END IF;
    RAISE NOTICE 'loyalty_account: แต้มรวมในฐาน %% แต้ม', pts;
END $loyalty$;`, lit(in.ClinicCode), written, written)

	e.Statements = s.stmts
	if sum := tr.summary(); sum != "" {
		rep.warnf("loyalty_account: truncated %s", sum)
	}

	rep.notef("loyalty_account: %d เอกสาร → %d hn · เขียน %d แถว (ยอด > 0) · แต้มรวม %d · สูงสุดต่อคน %d",
		len(mc.points), len(hns), written, total, maxPer)
	if zero > 0 {
		rep.notef("loyalty_account: ข้าม %d hn ที่ยอดเป็น 0 — ไม่เปิดบัญชี (มติเจ้าของคลินิก 28 ก.ย. 2569) "+
			"V4 เปิดบัญชีตอนได้แต้มครั้งแรก ไม่ใช่ตอนเปิดคนไข้", zero)
	}
	if multi > 0 {
		rep.notef("loyalty_account: %d hn มีหลายเอกสารในต้นทาง — ใช้ใบแรกตามที่ V2 อ่าน "+
			"(PointSummary.findOne({hn}) เห็นเฉพาะใบแรก · ไม่รวมยอด ไม่เอาค่ามากสุด)", multi)
	}
	if len(leftBehind) > 0 {
		rep.warnf("loyalty_account: %d hn มีเอกสารใบอื่นที่ยอดไม่เท่าใบแรก — ไม่ถูกนำเข้าเพราะ V2 ไม่เคยแสดงค่านั้น: %s",
			len(leftBehind), strings.Join(capList(leftBehind, 20), " · "))
	}
	if len(negLater) > 0 {
		rep.warnf("loyalty_account: %d hn มีเอกสารใบอื่นที่ยอดติดลบ — ไม่กระทบยอดที่นำเข้า แต่บอกว่าต้นทางเคยหักเกิน: %s",
			len(negLater), strings.Join(capList(negLater, 20), " · "))
	}
	if noHN > 0 {
		rep.warnf("loyalty_account: %d เอกสารไม่มี hn — ไม่นำเข้า เพราะแต้มที่ไม่รู้ว่าเป็นของใครเขียนลงตารางไม่ได้", noHN)
	}
	if len(mc.pointLogs) > 0 {
		rep.warnf("loyalty_account: point_logs %d เอกสารไม่ถูกนำเข้า (มติเจ้าของคลินิก) — loyalty_txn จะว่าง "+
			"และยอดแต้มจะไม่มีประวัติอธิบายที่มา · ต้นทางเองก็ปิดยอดไม่ได้ เพราะ returnPointDeleteBill() ของ V2 "+
			"ลบ log ทิ้งเมื่อบิลถูกลบ", len(mc.pointLogs))
	}
	rep.notef("loyalty_account: ไม่ได้เขียน loyalty_policy — แต้มที่นำเข้าจะแลกไม่ได้จนคลินิกตั้งอัตรา " +
		"(baht_per_point) และเปิด active เองในจอ · การเปิด active ยังเริ่มแจกแต้มจาก catalog_service.loyalty_points ด้วย")

	return s.String(), nil
}

// joinInts renders a list of source values for an operator to read.
func joinInts(vs []int64) string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, strconv.FormatInt(v, 10))
	}
	return strings.Join(out, ", ")
}

// capList keeps a report line readable when hundreds of rows qualify. The full count is
// always stated beside it, so nothing is hidden — only shortened.
func capList(items []string, max int) []string {
	if len(items) <= max {
		return items
	}
	out := append([]string{}, items[:max]...)
	return append(out, fmt.Sprintf("… และอีก %d รายการ", len(items)-max))
}
