// Mapping policy for the whole package lives in this file.
//
// Only clinic_code, owner_username and the dump files come from the caller; every rule
// below is fixed, so there is ONE place to look when the next clinic needs a different
// one. The values were settled while migrating the first clinics and each carries the
// reason it is what it is — a rule with no reason is a rule nobody can safely change.
package legacyimport

import "strings"

// Hardcoded mapping decisions. Only clinic_code / clinic_name and the dump files
// themselves come from the request; everything below is fixed policy.
const (
	// BranchCode is stamped on every row that needs a branch. The legacy
	// appointment.branch_code column is NULL in every row and the legacy room master
	// only ever uses '01', so there is one branch per migrated clinic.
	BranchCode = "01"

	// MainStockBranchCode is where stock that belongs to NO branch goes. It is V4's
	// reserved inventory location (internal/controller/stock/handler.go), deliberately
	// absent from the clinic's branch table, which is why it consumes no branch quota and
	// why the stock page keeps it out of the "รวมทุกสาขา" total.
	//
	// The legacy side has the same idea: the SQL `branch` table names each branch's stock
	// through branch.stock_id, and the one stock no branch claims is the central warehouse
	// (`stocks.stock_branch = 'center'` in the export this was built from — 566 of 3,350
	// lots). The link is what decides this, not the string: 'center' is a label one tenant
	// happened to use, while stock_id is a real reference.
	MainStockBranchCode = "MAIN_STOCK"

	// RoleKeyOwner / RoleKeyStaff are the built-in role_presets keys assigned during
	// import. The legacy dump carries no role information, so exactly ONE account —
	// the one whose legacy `code` equals Input.OwnerUsername — becomes the owner and
	// every other account becomes counter_staff. The role's numeric id is resolved by
	// key at write time (rolePresetIDByKey), never hardcoded: a clinic can carry its
	// own custom presets whose ids differ, and the built-in row is keyed, not numbered.
	RoleKeyOwner = "owner"
	RoleKeyStaff = "counter_staff"

	// RoleKeyDoctor is the clinic_staff role every imported doctor must carry.
	// validate_appointment_assignment refuses a doctor_code whose doctor row does not
	// reach a clinic_staff row with this role, so it is a requirement, not a label.
	RoleKeyDoctor = "doctor"

	// CourseCodePrefix is the prefix for course codes minted during import. The legacy
	// course_items collection carries no code of its own, so one is generated per course
	// in dump order (CS001, CS002, …) — deterministic, so the same export always yields
	// the same codes and a re-run updates rather than duplicates.
	//
	// It must NOT be "LC": the legacy listheals collection already uses LC and LH for
	// services, and reusing LC would silently overwrite real services with courses.
	// Minting is collision-checked against the service codes anyway, but the prefix is
	// kept distinct so a person reading a code knows which kind of thing it is.
	CourseCodePrefix = "CS"

	// RecodeDuplicateServiceCodes decides what happens when the legacy listheals
	// collection carries the SAME code twice (two services claiming one identity).
	//
	// true  — the later row is moved to the next free number in its own prefix and the
	//         change is reported. Nothing is lost. This follows the precedent set for
	//         this clinic by the earlier gen_listheal run (LC033 → LC093).
	// false — the whole run is refused, matching the rule used for duplicate HNs.
	RecodeDuplicateServiceCodes = true

	// There is no KeepLegacyUserIDs any more, and the reason it existed is worth keeping.
	//
	// It preserved users.id from the dump via OVERRIDING SYSTEM VALUE, because
	// patient.user_insert points at those numbers and fresh ids would have silently
	// re-attributed every imported patient. That was true, and it is exactly why the id was
	// hard to give up.
	//
	// What made it untenable: users.id is a primary key across the WHOLE platform, not per
	// clinic. Measured on the shared test database, 36 of one export's 112 accounts landed on
	// ids other tenants had already taken — 19 of them a clinic with real data since 2016.
	// Preserving the number meant either overwriting somebody else's login or skipping the
	// account and letting everything that referenced it attach to a stranger.
	//
	// V4 issues the ids now and import_user_map translates, so patient.user_insert (and
	// clinic_user, clinic_staff, doctor.user_id, doctor.staff_id) resolve the person instead
	// of trusting a number. See buildUserMap.

	// SkipLegacyAgeColumn drops the legacy patient."Age": the values are strings cut
	// mid-character ('36 ปี 11 เด') and derived anyway — V4 computes age from
	// "Birthday".
	SkipLegacyAgeColumn = true

	// ZeroMeasurementToNull turns a legacy weight/height of 0 into NULL (0 means
	// "never measured" in the source).
	ZeroMeasurementToNull = true

	// StaffNameFallsBackToUsername fills clinic_staff.name from the login name when
	// the legacy users.name is empty, so the Settings list never shows a blank row.
	StaffNameFallsBackToUsername = true

	// ── money policy ─────────────────────────────────────────────────────────
	// Every rule below was decided by the clinic owner. They are money rules, so
	// each one names WHY it is what it is: a wrong value here is not a cosmetic
	// defect, it is a number the clinic will bill or chase a patient for.

	// MoneyUseBranchDocNumbers picks which legacy running number becomes the V4
	// document number. The legacy export carries TWO of each:
	//
	//	bills.invoice_no          / bills.invoice_branch_no
	//	payment_summary.receipt_no / payment_summary.receipt_branch_no
	//
	// false = the clinic-wide number (invoice_no / receipt_no). V4 has one column
	// per document, so the other number has nowhere to go and is reported as not
	// imported rather than dropped silently. Flip this if the clinic recognises the
	// branch-scoped number as "the" document number instead.
	MoneyUseBranchDocNumbers = false

	// MoneyTolerance is the rounding slack when checking totals, in baht. V4's
	// own AddInvoicePayment uses 0.01 when it decides full_paid vs partially_paid
	// (internal/db/query/invoice.sql), so the same slack is used here — a stricter
	// check here than the app applies would reject bills V4 itself calls settled.
	MoneyTolerance = 0.01

	// MoneyVATRate is 0 because the source says so: every bills.price_vat in the
	// export is Decimal128("0") and price_ex_vat equals price_total. The clinic was
	// not issuing VAT invoices in V2, so inventing a rate here would fabricate a tax
	// figure. Reported as "no VAT data in source" rather than assumed.
	MoneyVATRate = "0"

	// ── address policy ───────────────────────────────────────────────────────

	// AddressImportInactive decides what happens to a legacy address row whose
	// active flag is '0' but which has a fully typed address in it.
	//
	// true  — import it. Nothing the clinic typed is thrown away.
	// false — skip it, and report how many were skipped.
	//
	// What active='0' MEANT in V2 is not recorded anywhere in the export, and the rows
	// are not empty — so this is a judgement about patient data, not a technicality.
	// Importing is the reversible direction: a row that should not be there can be
	// deleted, while one that was never imported has to be re-derived from the dump.
	AddressImportInactive = true

	// AddressInactiveAsOther would file an inactive address under address_type='other'
	// to keep it distinguishable from the one in use.
	//
	// It is FALSE, decided against the target's actual contents: the one inactive row in
	// this export belongs to an HN that ALREADY has a 'home' address there, and it is the
	// same address. Filing it as 'other' would have produced two rows for one address —
	// a distinction bought at the price of a duplicate the clinic would have to explain.
	AddressInactiveAsOther = false

	// (see the var block below for DocNoBranchMarker, which is per-run, not policy)

	// AddressFillExistingRows adds a FILL-ONLY update beside each address insert.
	//
	// The target already holds these rows from an earlier pipeline, but incomplete: all
	// twelve carry province / district / sub-district as NAMES with province_id,
	// amphure_id and district_id NULL on every one, and half have no postcode. Names
	// without ids are why the register form's province→amphure→district cascade cannot
	// preselect anything: it matches on ids.
	//
	// The update is COALESCE(NULLIF(col,''), new, col) per column, so it can only turn an
	// empty column into a value. A column that already holds something is left exactly as
	// it is — including something a human edited after the earlier import.
	AddressFillExistingRows = true
)

// DocNoBranchMarker is the one value in this file that is NOT fixed policy: it differs per
// SITE of a multi-site tenant, so the caller sets it through Input.DocNoBranchMarker and
// BuildAll publishes it here. It lives beside the policy constants because it changes the
// same thing they do — what goes into invoice.no / receipt.receipt_no.
//
// Empty (the default) means "carry the source number verbatim", which is every
// single-site import ever run by this tool. See applyDocNoBranchMarker in money.go.
var DocNoBranchMarker string

// billImportable decides what happens to one legacy bill, by its status. Decided by
// the clinic owner against the real counts in the export (1,405 bills):
//
//	full_paid  1257  → import (a completed sale)
//	send         10  → import as an UNPAID debt. The bill was issued to the patient
//	                   and the money never arrived; dropping it hides revenue the
//	                   clinic can still collect.
//	delete      133  → skip. Voided/deleted in V2; importing it would resurrect a
//	                   cancelled charge.
//	waiting       5  → skip. Never issued — a bill still being assembled at the
//	                   counter, not a document anyone owes.
//
// An UNKNOWN status is skipped, not guessed: a status this tool has never seen could
// mean anything, and the wrong guess either invents or erases a charge.
//
// partially_paid / watting: added for the-gloss ฉะเชิงเทรา (BR00001) on 2026-09-09 by
// the clinic owner (K.Art), against that export's real counts (partially_paid 334,
// watting 18).
//   - partially_paid → import. It is a real, meaningful status: the patient paid part of
//     the bill. invoice.status/paid_amount are DERIVED from the receipts that get
//     imported (renderSettle), so a partial payment lands as 'partially_paid' on its own
//     and Guard 3 (no OVER-payment) still passes because paid < total is allowed.
//   - watting → import. It is the source's misspelling of "waiting"; the owner confirmed
//     these are real bills, not drafts. A watting bill that carries no line items is still
//     dropped by the existing empty-bill skip before it can reach Guard 3b, so only ones
//     with real lines are written. "waiting" (correctly spelled) stays a skip — the owner
//     did not ask to import those.
func billImportable(status string) (importIt bool, skipReason string) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "full_paid", "send", "partially_paid", "watting":
		return true, ""
	case "delete":
		return false, "bills.status=delete (ถูกลบ/ยกเลิกในระบบเก่า)"
	case "waiting":
		return false, "bills.status=waiting (ยังไม่ออกบิลจริง)"
	default:
		return false, "bills.status=" + status + " (สถานะที่เครื่องมือนี้ไม่รู้จัก — ไม่เดา)"
	}
}

// paymentImportable decides what happens to one legacy payment_summary row.
//
//	active  1258 → import
//	delete   104 → skip (the payment was voided in V2)
func paymentImportable(status string) (importIt bool, skipReason string) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "active":
		return true, ""
	case "delete":
		return false, "payment_summary.status=delete (การรับเงินถูกยกเลิก)"
	default:
		return false, "payment_summary.status=" + status + " (สถานะที่เครื่องมือนี้ไม่รู้จัก — ไม่เดา)"
	}
}

// invoiceItemKind maps the legacy bill line type onto invoice_item.kind.
//
// The V4 values are the ones production actually holds — "procedure", "supply",
// "course" (internal/controller/billing/helpers_test.go pins them, because the stock
// dispatch routes on this column: a misrouted line either deducts inventory that
// never left the shelf or leaves real inventory uncounted).
//
//	operative  1358 → procedure  (หัตถการ — performed, not taken off a shelf)
//	medicine    781 → supply     (ยา/เวชภัณฑ์ — physical stock)
//	course      652 → course     (drawn down when USED, not when sold)
//
// known=false means the type is not in the export this mapping was built against;
// the caller reports it instead of filing it under a kind that changes stock.
//
// "other" was added for the-gloss ศรีราชา on 2026-09-09 by the clinic owner (K.Art), and
// the reason it had to be decided rather than left unknown is worth recording: dropping an
// unknown LINE silently breaks its whole BILL. balanceBills refuses any bill whose line sum
// no longer equals its header total, which is correct — but it meant 255 bills worth
// ฿5,664,228 were refused, and with them 263 course entitlements that hung off those bills
// (a patient then shows ฿0 purchased and no course, with nothing on screen explaining why).
//
// What "other" holds in this export, counted: ปากกามอนจา / ปากกาวีโก (GLP-1 pens, ~110
// lines), ตรวจเลือด (89), NAD+, G5, ตรวจสุขภาพ. The majority are physical goods handed to
// the patient, so 'supply' is the owner's call. The lab/checkup lines land there too and
// are the known cost of one rule over per-name rules — a rule nobody can maintain is worse
// than a rule that is slightly coarse.
//
// This does NOT move stock for the imported history: the money import writes
// invoice/invoice_item/receipt only, never stock_movement, so kind here affects how the
// line reads and reports, not a back-dated inventory deduction.
func invoiceItemKind(legacyType string) (kind string, known bool) {
	switch strings.ToLower(strings.TrimSpace(legacyType)) {
	case "operative":
		return "procedure", true
	case "medicine":
		return "supply", true
	case "course":
		return "course", true
	case "other":
		return "supply", true
	default:
		return "", false
	}
}

// moneyChannels maps the legacy payment_summary.payment_type sub-fields onto
// receipt_payment.method, in the order the rows are written.
//
// V4 puts no CHECK on method and labels an unknown one by echoing it back
// (paymentLabel's default branch in internal/controller/billing/helpers.go), so a
// channel V4 has no word for — sso, insurance, voucher — keeps its own name instead
// of being flattened into "other". Flattening would make three different kinds of
// money indistinguishable in the receipt history, which is the one thing a receipt
// exists to record.
var moneyChannels = []struct {
	field  string // payment_type.<field> in the legacy doc
	method string // receipt_payment.method in V4
}{
	{"payment", "cash"},
	{"payment_transfer", "transfer"},
	{"payment_credit", "credit"},
	{"payment_sso", "sso"},
	{"payment_insurance", "insurance"},
	{"voucher", "voucher"},
	{"payment_other", "other"},
}

// statusFromConfirmCheck maps the legacy appointment.confirm_check onto V4's two
// CHECK-constrained columns (mapping confirmed by the data owner):
//
//	'10' = ยังไม่ยืนยัน -> reserve / pending
//	'1'  = ยืนยัน       -> confirm / confirmed
//	'0'  = ยกเลิก       -> cancel  / cancelled
//
// The two CHECK sets are asymmetric on purpose in the schema: status uses
// 'cancel'/'postpone' while confirm_status uses 'cancelled'/'postponed'.
func statusFromConfirmCheck(confirmCheck string) (status, confirmStatus string, known bool) {
	switch confirmCheck {
	case "10":
		return "reserve", "pending", true
	case "1":
		return "confirm", "confirmed", true
	case "0":
		return "cancel", "cancelled", true
	default:
		return "reserve", "pending", false
	}
}

// ── LINE bindings (mris.line_patients → clinic_lineoa + patient_line_binding) ──────

// SkipInactiveLineBinding drops a V2 pairing whose `active` is false.
//
// It is TRUE, and the reason is consent rather than tidiness: active=false is the
// customer having unlinked, so carrying the row over would re-link somebody who asked
// not to be. Measured on the-gloss: 8 of 699.
//
// Importing them as revoked was the alternative and it does not work honestly —
// ck_plb_revocation_complete requires revoked_at AND revoked_by together, and V2
// records neither, so both would have to be invented.
//
// Owner's decision ก, 6 Oct 2026.
const SkipInactiveLineBinding = true

// LineBindVerifiedMethod is what patient_line_binding.verified_method says for a
// binding this tool carried over.
//
// It is a distinct value on purpose. The column otherwise records HOW a human proved
// they own the LINE account — 'phone_otp' and 'counter_otp' in the target today — and
// an import proved nothing. Migration 0327 set the precedent with 'phone_otp_legacy'
// for the rows it back-filled, so a migration-sourced label is an established shape and
// not a new idea.
//
// Also the only handle the generated script has on its own output: the closing report
// counts rows by this exact value. varchar(32); this is 15.
const LineBindVerifiedMethod = "v2_line_pairing"
