package legacyimport

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The five money guards, emitted as SQL inside the same transaction as the rows.
//
// They exist because this import writes money WITHOUT going through /v3/checkout, which
// is where V4 normally enforces these rules. Skipping checkout was the right call (see
// money.go for why), but it means the invariants have to be re-stated somewhere — and
// the only place that can enforce them against what was ACTUALLY written is the script
// itself.
//
// One runs BEFORE any insert, because a clash is cheaper to report than to undo:
//
//	4  document numbers must not already belong to a different patient
//
// One runs BETWEEN the patient phase and the money phase:
//
//	5  every bill's HN must exist in patient
//
// Guard 5 used to sit with guard 4 at the top of the file, and that made a full import
// impossible. It asks whether a patient exists, and in a run that imports patients AND
// money the patients are written by the same script 2,800 lines further down — so the
// guard read an empty table and refused. Measured on a real export: 4,372 HNs reported
// missing while 6,439 patient inserts sat below the guard waiting to create them.
//
// Moving it after the patient phase keeps every property that mattered: the patients of
// THIS run are already in the transaction, no money row has been written yet, and a
// money-only run still gets checked against whatever the target already holds.
//
// Three run AFTER, checking the rows that landed:
//
//	1  Σ receipt_payment.amount = receipt.paid_amount        (per receipt)
//	2  Σ receipt.paid_amount    = invoice.paid_amount        (per invoice)
//	3  Σ invoice_item net − bill_discount ≥ invoice.paid_amount
//
// A guard that fires RAISEs, so the whole transaction rolls back. That is the point: a
// partially imported set of financial documents is worse than none, because the clinic
// cannot tell which numbers to trust.
//
// Guards 1–3 should be unreachable — money.go derives paid_amount and bill_discount
// from the receipts rather than copying them, and drops any bill whose totals disagree
// before rendering. They are kept anyway: "should be unreachable" is a claim about code,
// and the money is worth checking against the database instead.

// buildMoneyPreflightGuards renders guard 4 (a and b).
//
// These two only ever look at rows that were ALREADY at the target, so the top of the
// script is the right place: nothing this run writes can change the answer.
func buildMoneyPreflightGuards(clinic string, invoiceHN, receiptHN map[string]string) string {
	var b strings.Builder

	if len(invoiceHN) > 0 {
		b.WriteString(fmt.Sprintf(`-- GUARD 4a: เลขที่ใบแจ้งหนี้ต้องไม่ถูกใช้โดยคนไข้คนอื่นอยู่แล้ว
-- invoice มี UNIQUE (clinic_code, no) — เลขที่ซ้ำกับเอกสารของคนไข้คนอื่นคือเลขที่ชนกันจริง
-- ปฏิเสธทั้งสคริปต์ ส่วนเลขที่ซ้ำกับ HN เดิมถือว่าเป็นการรันซ้ำ และถูกข้ามด้วย NOT EXISTS
DO $guard$
DECLARE
    clash text;
BEGIN
    SELECT string_agg(format('เลขที่ %%s (ในระบบเป็นของ HN %%s / ในไฟล์เป็นของ HN %%s)',
                             want.no, COALESCE(inv.hn, '(ไม่มี)'), want.hn), '; ')
      INTO clash
      FROM (VALUES %s) AS want(no, hn)
      JOIN invoice inv ON inv.clinic_code = %s AND inv.no = want.no
     WHERE COALESCE(inv.hn, '') <> want.hn;

    IF clash IS NOT NULL THEN
        RAISE EXCEPTION 'invoice: เลขที่เอกสารชนกับใบที่มีอยู่ของคนไข้คนอื่น — ปฏิเสธทั้งสคริปต์: %%', clash;
    END IF;
END $guard$;

`, sqlValuesPairs(invoiceHN), lit(clinic)))
	}

	if len(receiptHN) > 0 {
		b.WriteString(fmt.Sprintf(`-- GUARD 4b: เลขที่ใบเสร็จต้องไม่ถูกใช้โดยคนไข้คนอื่นอยู่แล้ว (receipt มี UNIQUE (clinic_code, receipt_no))
DO $guard$
DECLARE
    clash text;
BEGIN
    SELECT string_agg(format('เลขที่ %%s (ในระบบเป็นของ HN %%s / ในไฟล์เป็นของ HN %%s)',
                             want.no, COALESCE(r.hn, '(ไม่มี)'), want.hn), '; ')
      INTO clash
      FROM (VALUES %s) AS want(no, hn)
      JOIN receipt r ON r.clinic_code = %s AND r.receipt_no = want.no
     WHERE COALESCE(r.hn, '') <> want.hn;

    IF clash IS NOT NULL THEN
        RAISE EXCEPTION 'receipt: เลขที่ใบเสร็จชนกับใบที่มีอยู่ของคนไข้คนอื่น — ปฏิเสธทั้งสคริปต์: %%', clash;
    END IF;
END $guard$;

`, sqlValuesPairs(receiptHN), lit(clinic)))
	}

	return b.String()
}

// buildMoneyPatientGuard renders guard 5, and is emitted between the patient phase and
// the money phase — never at the top of the script. See the package comment: at the top
// it read a table the same run had not filled yet and refused every full import.
func buildMoneyPatientGuard(clinic string, hns map[string]bool) string {
	if len(hns) == 0 {
		return ""
	}
	return fmt.Sprintf(`-- GUARD 5: ทุกบิลต้องมีคนไข้อยู่จริงในคลินิกนี้
-- invoice.hn เป็นข้อความ ไม่มี FK ไปหา patient — บิลที่อ้าง HN ที่ไม่มีตัวตนจะกลายเป็น
-- ยอดค้างที่หาเจ้าของไม่ได้ และไม่มีใครเห็นในประวัติคนไข้คนใด
--
-- ด่านนี้อยู่ตรงนี้ ไม่ใช่หัวไฟล์ เพราะคนไข้ของรอบนี้ถูกเขียนไปแล้วข้างบน ส่วนบิลยังไม่ถูกเขียน
-- ถ้าย้ายขึ้นไปหัวไฟล์ มันจะอ่านตาราง patient ที่ยังว่างแล้วปฏิเสธการนำเข้าที่ถูกต้องทุกครั้ง
DO $guard$
DECLARE
    missing text;
    n       int;
BEGIN
    SELECT count(*), string_agg(want.hn, ', ' ORDER BY want.hn)
      INTO n, missing
      FROM (SELECT DISTINCT unnest(ARRAY[%s]) AS hn) want
     WHERE NOT EXISTS (
         SELECT 1 FROM patient p WHERE p.clinic_code = %s AND p."HN" = want.hn
     );

    IF n > 0 THEN
        RAISE EXCEPTION 'invoice: มี %% HN ในบิลที่ไม่มีคนไข้ในคลินิกนี้ — นำเข้ากลุ่ม patients มาด้วย หรือนำเข้าคนไข้ให้ครบก่อน แล้วรันอีกครั้ง: %%', n, left(missing, 2000);
    END IF;
END $guard$;

`, sqlTextArray(hns), lit(clinic))
}

// buildMoneyVerifyGuards renders guards 1–3, which read the rows the script just wrote.
//
// overpaidInSource are invoice numbers the SOURCE already recorded with more cash than the
// bill was worth. Guard 3 excludes exactly those, by name. The alternative — dropping the
// guard, or dropping the bills — would either stop checking 17,000 good invoices or leave
// real money out of V4; naming the anomalies keeps both the check and the cash.
func buildMoneyVerifyGuards(clinic string, invoiceNos, receiptNos map[string]bool,
	overpaidInSource map[string]bool) string {
	tol := strconv.FormatFloat(MoneyTolerance, 'f', -1, 64)
	var b strings.Builder

	if len(receiptNos) > 0 {
		b.WriteString(fmt.Sprintf(`-- GUARD 1: ทุกใบเสร็จ ยอดรับเงินต้องเท่ากับผลรวมช่องทางที่จ่ายมา
-- receipt.paid_amount กับ Σ receipt_payment.amount คือคำตอบของคำถามเดียวกัน
-- ("เงินเข้ามาเท่าไร") ถ้าไม่เท่ากัน มีช่องทางหาย หรือมียอดที่ไม่มีที่มา
DO $guard$
DECLARE
    bad text;
    n   int;
BEGIN
    SELECT count(*), string_agg(format('%%s (ใบเสร็จ %%s ≠ ช่องทางรวม %%s)',
                                       r.receipt_no, r.paid_amount, COALESCE(p.total, 0)), '; ')
      INTO n, bad
      FROM receipt r
      LEFT JOIN (
          SELECT rp.receipt_id, SUM(rp.amount) AS total
            FROM receipt_payment rp
           WHERE rp.clinic_code = %s
           GROUP BY rp.receipt_id
      ) p ON p.receipt_id = r.id
     WHERE r.clinic_code = %s
       AND r.receipt_no = ANY (ARRAY[%s])
       AND abs(r.paid_amount - COALESCE(p.total, 0)) > %s;

    IF n > 0 THEN
        RAISE EXCEPTION 'receipt: %% ใบมียอดรับเงินไม่เท่ากับผลรวมช่องทางการจ่าย — ปฏิเสธทั้งสคริปต์: %%', n, left(bad, 2000);
    END IF;
END $guard$;

`, lit(clinic), lit(clinic), sqlTextArray(receiptNos), tol))
	}

	if len(invoiceNos) > 0 {
		nos := sqlTextArray(invoiceNos)
		b.WriteString(fmt.Sprintf(`-- GUARD 2: ทุกใบแจ้งหนี้ ยอดที่จ่ายแล้วต้องเท่ากับผลรวมใบเสร็จของใบนั้น
DO $guard$
DECLARE
    bad text;
    n   int;
BEGIN
    SELECT count(*), string_agg(format('%%s (ใบแจ้งหนี้ %%s ≠ ใบเสร็จรวม %%s)',
                                       inv.no, inv.paid_amount, COALESCE(r.total, 0)), '; ')
      INTO n, bad
      FROM invoice inv
      LEFT JOIN (
          SELECT rc.invoice_id, SUM(rc.paid_amount) AS total
            FROM receipt rc
           WHERE rc.clinic_code = %s
           GROUP BY rc.invoice_id
      ) r ON r.invoice_id = inv.id
     WHERE inv.clinic_code = %s
       AND inv.no = ANY (ARRAY[%s])
       AND abs(inv.paid_amount - COALESCE(r.total, 0)) > %s;

    IF n > 0 THEN
        RAISE EXCEPTION 'invoice: %% ใบมียอดที่จ่ายแล้วไม่เท่ากับผลรวมใบเสร็จ — ปฏิเสธทั้งสคริปต์: %%', n, left(bad, 2000);
    END IF;
END $guard$;

-- GUARD 3: ห้ามเก็บเงินเกินยอดที่ต้องจ่าย
-- ยอดสุทธิ = Σ(ราคา × จำนวน − ส่วนลดบรรทัด) − ส่วนลดท้ายบิล ตามสูตรของ V4 เอง
-- เกินยอด = คนไข้จ่ายเกิน หรือรายการในใบหายไป ทั้งสองกรณีต้องหยุด
--
-- ยกเว้นใบที่ต้นทางรับเงินเกินมาตั้งแต่ต้น (ระบุชื่อไว้ในรายการยกเว้นด้านล่าง ถ้ามี) —
-- ใบเหล่านั้นถูกนำเข้าตามต้นทางโดยเจตนา เพื่อให้ยอดเงินใน V4 เท่ากับ V2 และถูกรายงานไว้แล้ว
DO $guard$
DECLARE
    bad text;
    n   int;
BEGIN
    SELECT count(*), string_agg(format('%%s (จ่าย %%s > ยอดสุทธิ %%s)', inv.no, inv.paid_amount, net.total), '; ')
      INTO n, bad
      FROM invoice inv
      CROSS JOIN LATERAL (
          SELECT COALESCE(SUM(GREATEST(ii.price * ii.qty - ii.discount, 0)), 0) - inv.bill_discount AS total
            FROM invoice_item ii
           WHERE ii.clinic_code = inv.clinic_code AND ii.invoice_id = inv.id
      ) net
     WHERE inv.clinic_code = %s
       AND inv.no = ANY (ARRAY[%s])
       AND inv.paid_amount > net.total + %s%s;

    IF n > 0 THEN
        RAISE EXCEPTION 'invoice: %% ใบเก็บเงินเกินยอดสุทธิ — ปฏิเสธทั้งสคริปต์: %%', n, left(bad, 2000);
    END IF;
END $guard$;

-- GUARD 3b: ใบแจ้งหนี้ทุกใบที่นำเข้าต้องมีรายการอย่างน้อยหนึ่งบรรทัด
-- ใบเปล่าคือยอด 0 ที่หน้าจอแสดงว่า "จ่ายครบแล้ว" ทั้งที่ไม่มีอะไรถูกคิดเงิน
DO $guard$
DECLARE
    bad text;
    n   int;
BEGIN
    SELECT count(*), string_agg(inv.no, ', ' ORDER BY inv.no)
      INTO n, bad
      FROM invoice inv
     WHERE inv.clinic_code = %s
       AND inv.no = ANY (ARRAY[%s])
       AND NOT EXISTS (
           SELECT 1 FROM invoice_item ii
            WHERE ii.clinic_code = inv.clinic_code AND ii.invoice_id = inv.id
       );

    IF n > 0 THEN
        RAISE EXCEPTION 'invoice: %% ใบไม่มีรายการเลย — ปฏิเสธทั้งสคริปต์: %%', n, left(bad, 2000);
    END IF;
END $guard$;

`, lit(clinic), lit(clinic), nos, tol,
			lit(clinic), nos, tol, overpayExclusion(overpaidInSource),
			lit(clinic), nos))
	}

	return b.String()
}

// overpayExclusion renders the NOT-IN clause that keeps guard 3 from failing on bills the
// source itself recorded as over-paid. Empty when there are none, so a clean export emits
// exactly the guard it always did.
func overpayExclusion(nos map[string]bool) string {
	if len(nos) == 0 {
		return ""
	}
	list := make([]string, 0, len(nos))
	for no := range nos {
		list = append(list, no)
	}
	sort.Strings(list)
	quoted := make([]string, 0, len(list))
	for _, no := range list {
		quoted = append(quoted, lit(no))
	}
	return "\n       AND inv.no <> ALL (ARRAY[" + strings.Join(quoted, ", ") + "])"
}

// sqlValuesPairs renders a sorted `('a','b'), ('c','d')` list for a VALUES clause.
// Sorted so the same export always produces a byte-identical script.
func sqlValuesPairs(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, "("+lit(k)+", "+lit(m[k])+")")
	}
	return strings.Join(parts, ", ")
}
