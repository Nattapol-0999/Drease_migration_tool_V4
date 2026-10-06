// Package legacyimport turns the legacy Dr.Ease V2 exports (a phpMyAdmin MySQL dump
// and a mongosh catalog dump) into V4 INSERT statements.
//
// It NEVER touches a database. It renders SQL text that a human reviews and runs with
// psql, which is what makes this tool safe to hand to an operator: the artifact is
// readable and diffable before anything is written, and running it is a deliberate act.
//
// The per-table mapping decisions were settled while migrating the first clinic
// (Wanaka) and are recorded as constants in config.go — ONE place to look when the
// next clinic needs a different rule.
//
// SCOPE (six uploaded files → nine tables):
//
//	users       → users + clinic_user + clinic_staff
//	doctor      → doctor
//	clinic      → room            (the legacy room master is a table named `clinic`)
//	patient     → patient
//	appointment → appointment
//
// patient_address is deliberately NOT imported: the legacy table stores province,
// district and sub-district as numeric ids (and packs a postcode into the same field),
// while V4 keeps names — so nothing could be written without resolving lookups the
// clinic has not asked for. It is reported as present-but-skipped rather than ignored.
//
// The tenant row itself is NOT created here: the clinic must already exist in V4
// (created through the normal admin flow so it gets its subscription, settings, rules
// and counters), and BuildScript emits a guard that aborts when it does not.
package legacyimport

import (
	"fmt"
	"strings"

	"system_cliv4/internal/legacydump"
)

// Input is one import request: the target tenant plus whichever dump files the caller
// supplied. An empty file means "skip that entity", so a clinic can be migrated in
// stages.
type Input struct {
	// ClinicCode is the tenant every generated row is stamped with. The clinic must
	// already exist in V4 — see BuildScript's guard.
	ClinicCode string
	// OwnerUsername is the legacy `users.code` (login name) of the ONE account that
	// becomes the clinic owner. Every other imported account becomes counter_staff.
	// Required whenever a users dump is supplied; a users dump with no matching row is
	// rejected, because a clinic with no owner cannot be administered.
	OwnerUsername string

	// Groups limits the script to part of the export (see entityGroups). Empty means
	// everything. Use it when the target already holds some of this data: importing the
	// rest would overwrite rows someone may have edited since.
	Groups []string

	// MongoDump is the dbmongo/*.js half of the export. It carries the catalog —
	// category, items (+ their stock lots), listheals (services), course_items — and
	// the sales history: bills and payment_summary.
	//
	// course_bills is still NOT read: it is the customers' remaining course
	// entitlement, and the target already holds rows for it, so importing it needs a
	// reconciliation nobody has asked for yet.
	MongoDump []byte

	UsersDump   []byte // legacy `users`
	DoctorDump  []byte // legacy `doctor`
	RoomDump    []byte // legacy `clinic` — the room master, NOT clinic data
	PatientDump []byte // legacy `patient`
	// AddressDump is the legacy `patient_address` table. The area ids in it resolve
	// against V4's own _provinces/_amphures/_districts, which migration 0340 seeded from
	// this same V2 dump with the original ids kept.
	AddressDump []byte

	AppointmentDump []byte // legacy `appointment`
	// BranchDump is the legacy `branch` table — the tenant's branch registry, with the
	// legal entity and tax id of each one. Nothing is imported from it: V4 keeps branches
	// as rows in `clinic`. It is read by SurveyBranches so an operator can see the real
	// branch layout (and whether the branches belong to different companies) before
	// choosing how to map it.
	BranchDump []byte

	// BranchMap turns a legacy branch code ("01".."08") into the V4 branch_code that
	// POST /v3/import/clinics allocated for it ("BR00001"…). Empty keeps the original
	// single-branch behaviour: every row is stamped with BranchCode, which is what the
	// first tenants needed and what every existing import produced.
	//
	// It exists because V4 owns branch identity — branch_code comes from a sequence and
	// carries the branch_no every HN at that branch keeps for life — so the legacy code
	// cannot be written through.
	BranchMap map[string]string
	// DefaultBranch is used for rows with no branch, and for a legacy code that is not
	// in BranchMap (the export has a few typos). Empty picks the first entry of
	// BranchMap, or BranchCode when there is no map. Never blank in the output, because
	// branch_code is NOT NULL on patient, invoice, receipt, room, doctor and slot_stock.
	DefaultBranch string

	// DocNoBranchMarker is a single digit ("2".."9") stamped into the third character of
	// every legacy V2 document number this run writes (YY0NNNNNN → YY<marker>NNNNNN, and
	// CYY0NNNNNN → CYY<marker>NNNNNN). Empty — the default — carries the source number
	// verbatim, which is what a single-site tenant needs.
	//
	// It exists because each site of a multi-site tenant runs its own copy of V2 and
	// therefore issues the SAME running numbers, while V4 keeps one number space per
	// clinic. The generated script also treats the number as the document's identity
	// (dedup is `WHERE NOT EXISTS (… AND no = '<number>')`), so without a per-site marker
	// the second site's import either collides with the first site's documents or silently
	// re-inserts rows that are already there. See applyDocNoBranchMarker in money.go.
	DocNoBranchMarker string

	/*
		StockItemType decides what the stock collection becomes in the catalogue: "drug" or
		"goods". Empty means "drug", which is what every import before 22 Sep 2026 did, so an
		existing command line behaves exactly as it always has.

		WHY IT IS A CHOICE AND NOT A RULE

		V2 has one stock list. V4 has two kinds of sellable stock and they are not interchangeable
		at the till: nonSellableCatalogCodes (billing/helpers.go) refuses an item_type='drug' row
		whose five legal label fields are not all filled in, and a V2 export carries none of them —
		measured, 0 of 1,901 items rows have generic_name_th, strength, dosage_form, indication or
		warning. So importing the whole list as 'drug' produces a clinic that cannot sell anything
		out of its own stock room, which is what happened to the-gloss: 365 SKUs with stock on hand
		and 0 sellable, repaired afterwards by hand with scripts 16 and 17 that moved 331 of them
		to 'goods'.

		What it is NOT is a way to decide what counts as a medicine. That question belongs to the
		clinic and its pharmacist, and the answer differs per row: the same list holds Botox and
		Restylane next to cotton wool, cannulas and cleansing milk. This flag only lets the person
		running the import say which default is less wrong for the clinic in front of them, and
		'drug' stays the default precisely so nobody gets a sellable Botox by accident.

		Whichever is chosen, stock behaves identically — both types draw from the same lots
		through the same code. The difference is the label gate at checkout, and nothing else.
	*/
	StockItemType string

	// PageDump is the mongo export of `mris.page` — the patient record pages and the
	// images attached to them. It is a SEPARATE flag from MongoDump because it is a
	// different database (mris, not the catalog one) and because it is the only input
	// whose output is a copy manifest rather than SQL (see page.go).
	PageDump []byte
}

// BuildAll parses the supplied files and renders the SQL for the entities the caller asked
// for, without touching a database. The returned map is keyed by the Ent* constants;
// userRows is handed back so the caller can emit the cross-clinic collision guard from the
// same parse instead of reading the dump twice.
//
// # WHY THE GROUP FILTER IS APPLIED HERE AND NOT AFTERWARDS
//
// It used to build everything and let BuildScript drop what was not selected, so the report
// could still say what the export contained. That cost more than it was worth: a mapping
// that REFUSES is how this tool protects a clinic, and a refusal about an unselected group
// stopped the whole run.
//
// Found on a real export: its `users` table has eight rows whose email column holds a
// person's name, and those names collide. users.email is UNIQUE across the whole table in
// V4, so buildUsers refuses — correctly. But because everything was built first, that
// refusal also blocked `-only catalog,stock`, which does not write a single user. The
// operator was told to fix eight email addresses before they could import a price list,
// and only after a 79-second parse of a 719 MB dump.
//
// So: a group that was not asked for is not built, and cannot fail the run. The tradeoff is
// that the report no longer counts rows for tables nobody selected, which is the smaller
// loss — the operator already knows what they chose to leave out.
//
// PARSING IS SEPARATE FROM BUILDING, and has to be. Several selections need rows from a
// table they do not write:
//
//	appointment  needs users + doctor to know which doctor_code V4 will accept
//	             (buildBookableDoctors); without them every appointment silently loses
//	             its doctor, which looks like imported data and is not
//	appointment   needs patient to know each HN's branch (branchByHN)
//	entitlements needs the money plan to point at the invoice lines it sells
func BuildAll(in Input, rep *Report) (batches map[string]string, userRows []legacydump.Row, money moneyBatches, err error) {
	if strings.TrimSpace(in.ClinicCode) == "" {
		return nil, nil, money, fmt.Errorf("clinic_code is required")
	}

	// The doc-number marker is read deep inside the money mapping (billDocNo /
	// receiptDocNo), which sees one document at a time and no Input. Publishing it once
	// here keeps the rule in Input where the caller sets it, instead of threading it
	// through every money helper. One run = one clinic = one marker.
	DocNoBranchMarker = strings.TrimSpace(in.DocNoBranchMarker)
	if DocNoBranchMarker != "" {
		if len(DocNoBranchMarker) != 1 || DocNoBranchMarker[0] < '1' || DocNoBranchMarker[0] > '9' {
			return nil, nil, money, fmt.Errorf("doc-no-branch-marker ต้องเป็นเลขหลักเดียว 1-9 (ได้รับ %q)", in.DocNoBranchMarker)
		}
		rep.notef("เลขที่เอกสารจากระบบเก่าถูกแทรกเลขสาขา %q เป็นตัวที่ 3 (YY0NNNNNN → YY%sNNNNNN · CYY0NNNNNN → CYY%sNNNNNN) — เพราะแต่ละสาขาของระบบเก่าออกเลขชุดเดียวกัน แต่ V4 ใช้เลขร่วมกันทั้งคลินิก · เลขที่ไม่ใช่รูปแบบเดิมจะไม่ถูกแตะ",
			DocNoBranchMarker, DocNoBranchMarker, DocNoBranchMarker)
	}

	// Validated here, next to the other per-run choice, and BEFORE any group runs: a typo has
	// to stop the run rather than quietly produce a catalogue of the wrong kind. Rejecting an
	// unknown value is the whole point — silently falling back to 'drug' would hand somebody a
	// clinic that cannot sell its stock and no reason why.
	switch strings.TrimSpace(in.StockItemType) {
	case "", "drug":
		in.StockItemType = "drug"
	case "goods":
		// Said as a WARNING, not a note. It changes what the till will accept for every SKU in
		// the stock room, and the operator has to be able to see that they asked for it.
		rep.warnf("รหัสสต็อกทั้งหมดถูกนำเข้าเป็น goods (สินค้า) ไม่ใช่ drug (ยา) ตามที่สั่งด้วย " +
			"-stock-item-type=goods — ขายได้ทันทีโดยไม่ต้องกรอกฉลากยา 5 ช่อง · " +
			"ถ้าในคลังมียาจริง ต้องเลื่อนรายการนั้นขึ้นเป็น drug แล้วกรอกฉลากก่อนจ่ายยา")
	default:
		return nil, nil, money, fmt.Errorf(
			"-stock-item-type ต้องเป็น drug หรือ goods (ได้รับ %q)", in.StockItemType)
	}

	// An empty selection means everything, which is what an operator who passes no -only
	// expects. Unknown group names are rejected here rather than silently importing less.
	want := map[string]bool{}
	all := len(in.Groups) == 0
	if !all {
		if want, err = entitiesForGroups(in.Groups); err != nil {
			return nil, nil, money, err
		}
	}
	wants := func(names ...string) bool {
		if all {
			return true
		}
		for _, n := range names {
			if want[n] {
				return true
			}
		}
		return false
	}

	out := map[string]string{}

	// ── parse ───────────────────────────────────────────────────────────────────
	//
	// Read only what something selected actually needs. Parsing a 719 MB dump nobody asked
	// about is minutes of an operator's afternoon and, on the deployed box, most of its RAM.

	// EntPatient is in this list because patient.user_insert is a legacy users.id and is now
	// translated through import_user_map — so a patients-only run needs the users dump PARSED
	// (to build the map) even though it writes no accounts. Leaving it out produced a script
	// with thousands of lookups into a table that was never created, which psql only rejects
	// after the whole file has been uploaded and started. BuildScript re-checks that
	// invariant on the assembled text, so forgetting -users is refused rather than emitted.
	needUsers := wants(EntUsers, EntClinicUser, EntClinicStaff, EntAppointment, EntPatient)
	needDoctor := wants(EntDoctor, EntClinicStaff, EntAppointment)

	if len(in.UsersDump) > 0 && needUsers {
		_, userRows, err = legacydump.Parse(string(in.UsersDump), "users")
		if err != nil {
			return nil, nil, money, fmt.Errorf("users dump: %w", err)
		}
	}

	var doctorRows []legacydump.Row
	if len(in.DoctorDump) > 0 && needDoctor {
		_, doctorRows, err = legacydump.Parse(string(in.DoctorDump), "doctor")
		if err != nil {
			return nil, nil, money, fmt.Errorf("doctor dump: %w", err)
		}
	}

	// ── people ──────────────────────────────────────────────────────────────────

	if len(userRows) > 0 && wants(EntUsers, EntClinicUser, EntClinicStaff) {
		// Exactly one account becomes the owner; the caller names it and it must be
		// present, or the clinic would have nobody who can administer it. Only asked for
		// when accounts are actually being written — importing a price list does not need
		// to know who owns the clinic.
		if strings.TrimSpace(in.OwnerUsername) == "" {
			return nil, nil, money, fmt.Errorf("owner_username is required when importing the people group")
		}
		if !hasOwner(userRows, in.OwnerUsername) {
			// The candidates are named because the operator's only other route to them is
			// grepping a multi-megabyte dump for a column whose name is not guessable:
			// owner_username matches legacy `users`.`code`, not a name or an email.
			return nil, nil, money, fmt.Errorf(
				"owner_username %q ไม่มีในไฟล์ users (มี %d บัญชี) — owner_username คือคอลัมน์ `code` ของตาราง users ในระบบเดิม ไม่ใช่ชื่อหรืออีเมล · %s",
				in.OwnerUsername, len(userRows), describeOwnerCandidates(userRows))
		}
		if out[EntUsers], err = buildUsers(userRows, in, rep); err != nil {
			return nil, nil, money, err
		}
		// The fill belongs immediately after the accounts land and before anything resolves
		// a person, so it rides on the end of the users batch rather than relying on where a
		// separate entity would be ordered.
		out[EntUsers] += buildUserMapFill(in.ClinicCode, true)
		if out[EntClinicUser], err = buildClinicUser(userRows, in, rep); err != nil {
			return nil, nil, money, err
		}
		if out[EntClinicStaff], err = buildClinicStaff(userRows,
			telByUserID(doctorRows), doctorUserIDsFrom(doctorRows),
			extraDoctorStaffRows(doctorRows), in, rep); err != nil {
			return nil, nil, money, err
		}
	}
	if len(doctorRows) > 0 && wants(EntDoctor) {
		if out[EntDoctor], err = buildDoctor(doctorRows, in, rep); err != nil {
			return nil, nil, money, err
		}
	}

	// ── appointments: PARSED here, BUILT further down ────────────────────────────
	//
	// The parse has to happen before rooms, because a room's status depends on whether an
	// appointment books it: V4 refuses an appointment naming a room with status 0, so a
	// room the source closed but the history still references must be imported active
	// (see buildRoom's `booked` parameter). The BUILD stays below, after patients, because
	// an appointment takes its branch from the patient's.
	var apptRows []legacydump.Row
	if len(in.AppointmentDump) > 0 && wants(EntAppointment) {
		if _, apptRows, err = legacydump.Parse(string(in.AppointmentDump), "appointment"); err != nil {
			return nil, nil, money, fmt.Errorf("appointment dump: %w", err)
		}
	}

	// ── rooms ───────────────────────────────────────────────────────────────────
	//
	// Parsed when appointments are selected even if rooms are not: V4 derives an
	// appointment's branch from its room, so the only way to report a booking whose branch
	// is about to be overwritten is to know what branch each room is at.
	var branchOfRoom map[string]string

	if len(in.RoomDump) > 0 && wants(EntRoom, EntAppointment) {
		// The legacy room master lives in a table called `clinic`.
		_, roomRows, err := legacydump.Parse(string(in.RoomDump), "clinic")
		if err != nil {
			return nil, nil, money, fmt.Errorf("room dump: %w", err)
		}
		branchOfRoom = branchByRoom(roomRows, in)
		if wants(EntRoom) {
			// An empty set when appointments are not part of this run, which keeps every
			// source status as-is — and means a rooms-only run followed by an
			// appointments-only run can still hit the trigger. Reported rather than guessed
			// at, because reopening rooms nothing books would be a change made for no reason.
			booked := roomsWithAppointments(apptRows)
			if len(booked) == 0 && !wants(EntAppointment) {
				rep.notef("room: รอบนี้ไม่ได้นำเข้านัด จึงคงสถานะห้องตามต้นทางทุกห้อง — ถ้าจะนำเข้านัดรอบหลัง ห้องที่ปิดอยู่จะทำให้นัดที่อ้างถึงมันล้มทั้งไฟล์")
			}
			if out[EntRoom], err = buildRoom(roomRows, booked, in, rep); err != nil {
				return nil, nil, money, err
			}
		}
	}

	// ── patients ────────────────────────────────────────────────────────────────
	//
	// Parsed when appointments are selected even if patients are not: an appointment takes
	// its branch from the patient's, and guessing it instead would file the booking at a
	// branch the patient has never been to.
	var branchByPatient map[string]string
	// knownHN is nil when the patient dump was not read at all. nil and empty mean
	// different things downstream: nil is "this run cannot tell whether a patient exists",
	// empty is "it can, and there are none".
	var knownHN map[string]bool

	// The money groups are listed here as well, even though they write no patient row: a
	// bill names its patient by HN with no foreign key behind it, so planBills has to be
	// able to tell whether that patient exists. Without the dump it cannot, and the SQL
	// guard is left to check against the live table instead.
	if len(in.PatientDump) > 0 &&
		wants(EntPatient, EntAppointment, EntInvoice, EntCourseBought, EntLoyaltyAccount) {
		// The column list is kept, not discarded: buildPatient needs it to tell a file with
		// a few unusable rows apart from a file whose HN column is spelled differently.
		patientCols, patientRows, err := legacydump.Parse(string(in.PatientDump), "patient")
		if err != nil {
			return nil, nil, money, fmt.Errorf("patient dump: %w", err)
		}
		branchByPatient = branchByHN(patientRows)
		knownHN = patientHNSet(patientRows)
		if wants(EntPatient) {
			if out[EntPatient], err = buildPatient(patientCols, patientRows, in, rep); err != nil {
				return nil, nil, money, err
			}
		}
	}

	if len(in.AddressDump) > 0 && wants(EntPatientAddress, EntPatientAddressFill) {
		_, addrRows, err := legacydump.Parse(string(in.AddressDump), "patient_address")
		if err != nil {
			return nil, nil, money, fmt.Errorf("patient_address dump: %w", err)
		}
		addrSQL, fillSQL, aerr := buildPatientAddress(addrRows, in, rep)
		if aerr != nil {
			return nil, nil, money, aerr
		}
		out[EntPatientAddress] = addrSQL
		out[EntPatientAddressFill] = fillSQL
	}

	if len(apptRows) > 0 {
		bookable := buildBookableDoctors(doctorRows, userRows)
		if out[EntAppointment], err = buildAppointment(apptRows, bookable, branchByPatient, branchOfRoom, in, rep); err != nil {
			return nil, nil, money, err
		}
	}

	// ── the Mongo half: catalog, then the money that references it ──────────────

	needCatalog := wants(EntCatalogCategory, EntCatalogService, EntItems, EntCatalogDrug,
		EntCatalogCourse, EntCatalogCourseLine)
	needStock := wants(EntSlotStock)
	needMoney := wants(EntInvoice, EntInvoiceItem, EntReceipt, EntReceiptPayment, EntInvoiceSettle)
	needEntitlements := wants(EntCourseBoughtPurge, EntCourseBought, EntCourseBoughtItems,
		EntCourseBoughtUsage)
	needLoyalty := wants(EntLoyaltyAccount)
	needLineBind := wants(EntLineoaGroup, EntPatientLineBinding, EntPatientLineID)

	if len(in.MongoDump) > 0 &&
		(needCatalog || needStock || needMoney || needEntitlements || needLoyalty || needLineBind) {
		mc, err := parseMongoCatalog(in.MongoDump)
		if err != nil {
			return nil, nil, money, err
		}
		if needCatalog {
			out[EntCatalogCategory] = buildCatalogCategory(mc, in, rep)
			if out[EntCatalogService], err = buildCatalogServices(mc, in, rep); err != nil {
				return nil, nil, money, err
			}
			if out[EntCatalogCourse], out[EntCatalogCourseLine], err = buildCourses(mc, in, rep); err != nil {
				return nil, nil, money, err
			}
			// Publishing comes AFTER both builders, and is set-based rather than per row: it
			// selects the catalog_item rows those two just wrote. Ordering is not a style
			// choice here — the ids it needs are issued by the INSERTs above.
			out[EntCatalogPublish] = buildCatalogPublish(in, rep)
		}
		// items, catalog_item(drug) and slot_stock come out of ONE pass over the items
		// collection, so catalog and stock cannot be built independently of each other.
		// Whichever was asked for is kept; the other is dropped rather than re-parsed.
		if needCatalog || needStock {
			itemsSQL, drugSQL, slotSQL, moveSQL, ierr := buildItems(mc, in, rep)
			if ierr != nil {
				return nil, nil, money, ierr
			}
			if needCatalog {
				out[EntItems], out[EntCatalogDrug] = itemsSQL, drugSQL
			}
			if needStock {
				// The lots and their receive ledger travel together: V4 never opens a lot
				// without a stock_movement row, so splitting them would produce inventory
				// with no arrival history — see buildItems.
				out[EntSlotStock] = slotSQL
				out[EntStockMovement] = moveSQL
			}
		}

		// The money half of the same file. It runs last because invoice_item resolves
		// catalog_item_id by code, so the catalog statements above must be in the
		// script before it — the SQL is applied top-down, group by group, and the
		// catalog group commits before the money group runs.
		//
		// Built for entitlements too even when money itself was not selected: an
		// entitlement points at the invoice line that sold it, and that reference comes
		// out of this plan. Only the SQL is withheld.
		if needMoney || needEntitlements {
			if money, err = buildMoney(mc, in, knownHN, rep); err != nil {
				return nil, nil, money, err
			}
			if needMoney {
				out[EntInvoice] = money.invoice
				out[EntInvoiceItem] = money.invoiceItem
				out[EntReceipt] = money.receipt
				out[EntReceiptPayment] = money.receiptPayment
				out[EntInvoiceSettle] = money.settle
			}
		}

		if needEntitlements {
			purge, bought, boughtItems, boughtUsage, cerr := buildCourseBought(mc, money, in, rep)
			if cerr != nil {
				return nil, nil, money, cerr
			}
			out[EntCourseBoughtPurge] = purge
			out[EntCourseBought] = bought
			out[EntCourseBoughtItems] = boughtItems
			out[EntCourseBoughtUsage] = boughtUsage
		}

		// Point balances. Independent of everything above — it reads its own collection
		// and resolves patient_id at the target — so it is built last and refuses only
		// when the dump has no `points` collection at all.
		if needLoyalty {
			if len(mc.points) == 0 {
				return nil, nil, money, fmt.Errorf(
					"เลือกกลุ่ม loyalty แต่ mongo dump ไม่มี collection \"points\" — ไม่มีแต้มให้นำเข้า")
			}
			if out[EntLoyaltyAccount], err = buildLoyalty(mc, in, rep); err != nil {
				return nil, nil, money, err
			}
		}

		// LINE bindings. Independent of everything above for the same reason loyalty is:
		// its own collection, and every id it needs is resolved at the target. Refuses
		// only when the dump has no `line_patients` collection at all — a clinic that
		// never used LINE pairing in V2 has nothing here, and the export script treats a
		// count of 0 as absent.
		if needLineBind {
			if len(mc.linePatients) == 0 {
				return nil, nil, money, fmt.Errorf(
					"เลือกกลุ่ม linebind แต่ mongo dump ไม่มี collection \"line_patients\" — " +
						"ไม่มีการผูก LINE ให้นำเข้า (คลินิกที่ไม่เคยใช้การผูก LINE ใน V2 จะไม่มี collection นี้)")
			}
			group, binding, pointer, lerr := buildLineBind(mc, in, rep)
			if lerr != nil {
				return nil, nil, money, lerr
			}
			out[EntLineoaGroup] = group
			out[EntPatientLineBinding] = binding
			out[EntPatientLineID] = pointer
		}
	}

	// Patient page images. This one hangs off the report rather than `out`: it produces
	// no SQL, because photos.file_size is only knowable from the image files themselves.
	if len(in.PageDump) > 0 && wants(EntPhoto) {
		man, perr := buildPhotos(in.PageDump, in, rep)
		if perr != nil {
			return nil, nil, money, perr
		}
		rep.Photos = man
	}

	if len(out) == 0 && rep.Photos == nil {
		// Two different mistakes, and telling them apart is the difference between
		// "attach a file" and "you picked groups this file has nothing for".
		if !all {
			return nil, nil, money, fmt.Errorf(
				"ไฟล์ที่แนบมาไม่มีข้อมูลของกลุ่มที่เลือก (%s) — ตรวจว่าเลือกกลุ่มถูก หรือแนบไฟล์ที่มีตารางของกลุ่มนั้น",
				strings.Join(in.Groups, ", "))
		}
		return nil, nil, money, fmt.Errorf("no input files: supply at least one of users, doctor, room, patient, address, appointment, mongo, page")
	}
	return out, userRows, money, nil
}

// The identity-sequence fix is gone.
//
// It advanced users' sequence past ids written with OVERRIDING SYSTEM VALUE, or the next
// account the app created collided on the primary key. Nothing overrides the sequence now —
// V4 issues every id — so advancing it would skip a block of numbers for no reason, and on a
// shared database it would move a counter other tenants also draw from.
