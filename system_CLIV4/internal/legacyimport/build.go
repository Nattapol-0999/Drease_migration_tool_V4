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

	needUsers := wants(EntUsers, EntClinicUser, EntClinicStaff, EntAppointment)
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
			return nil, nil, money, fmt.Errorf("owner_username %q not found among the %d account(s) in the users dump",
				in.OwnerUsername, len(userRows))
		}
		if out[EntUsers], err = buildUsers(userRows, in, rep); err != nil {
			return nil, nil, money, err
		}
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

	// ── rooms ───────────────────────────────────────────────────────────────────

	if len(in.RoomDump) > 0 && wants(EntRoom) {
		// The legacy room master lives in a table called `clinic`.
		_, roomRows, err := legacydump.Parse(string(in.RoomDump), "clinic")
		if err != nil {
			return nil, nil, money, fmt.Errorf("room dump: %w", err)
		}
		if out[EntRoom], err = buildRoom(roomRows, in, rep); err != nil {
			return nil, nil, money, err
		}
	}

	// ── patients ────────────────────────────────────────────────────────────────
	//
	// Parsed when appointments are selected even if patients are not: an appointment takes
	// its branch from the patient's, and guessing it instead would file the booking at a
	// branch the patient has never been to.
	var branchByPatient map[string]string

	if len(in.PatientDump) > 0 && wants(EntPatient, EntAppointment) {
		_, patientRows, err := legacydump.Parse(string(in.PatientDump), "patient")
		if err != nil {
			return nil, nil, money, fmt.Errorf("patient dump: %w", err)
		}
		branchByPatient = branchByHN(patientRows)
		if wants(EntPatient) {
			if out[EntPatient], err = buildPatient(patientRows, in, rep); err != nil {
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

	if len(in.AppointmentDump) > 0 && wants(EntAppointment) {
		_, apptRows, err := legacydump.Parse(string(in.AppointmentDump), "appointment")
		if err != nil {
			return nil, nil, money, fmt.Errorf("appointment dump: %w", err)
		}
		bookable := buildBookableDoctors(doctorRows, userRows)
		if out[EntAppointment], err = buildAppointment(apptRows, bookable, branchByPatient, in, rep); err != nil {
			return nil, nil, money, err
		}
	}

	// ── the Mongo half: catalog, then the money that references it ──────────────

	needCatalog := wants(EntCatalogCategory, EntCatalogService, EntItems, EntCatalogDrug,
		EntCatalogCourse, EntCatalogCourseLine)
	needStock := wants(EntSlotStock)
	needMoney := wants(EntInvoice, EntInvoiceItem, EntReceipt, EntReceiptPayment, EntInvoiceSettle)
	needEntitlements := wants(EntCourseBoughtPurge, EntCourseBought, EntCourseBoughtItems)

	if len(in.MongoDump) > 0 && (needCatalog || needStock || needMoney || needEntitlements) {
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
		}
		// items, catalog_item(drug) and slot_stock come out of ONE pass over the items
		// collection, so catalog and stock cannot be built independently of each other.
		// Whichever was asked for is kept; the other is dropped rather than re-parsed.
		if needCatalog || needStock {
			itemsSQL, drugSQL, slotSQL, ierr := buildItems(mc, in, rep)
			if ierr != nil {
				return nil, nil, money, ierr
			}
			if needCatalog {
				out[EntItems], out[EntCatalogDrug] = itemsSQL, drugSQL
			}
			if needStock {
				out[EntSlotStock] = slotSQL
			}
		}

		// The money half of the same file. It runs last because invoice_item resolves
		// catalog_item_id by code, so the catalog statements above must be in the
		// script before it — the SQL is applied top-down in one transaction.
		//
		// Built for entitlements too even when money itself was not selected: an
		// entitlement points at the invoice line that sold it, and that reference comes
		// out of this plan. Only the SQL is withheld.
		if needMoney || needEntitlements {
			if money, err = buildMoney(mc, in, rep); err != nil {
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
			purge, bought, boughtItems, cerr := buildCourseBought(mc, money, in, rep)
			if cerr != nil {
				return nil, nil, money, cerr
			}
			out[EntCourseBoughtPurge] = purge
			out[EntCourseBought] = bought
			out[EntCourseBoughtItems] = boughtItems
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

// usersSequenceFix advances users' identity sequence past the imported ids. WITHOUT
// this the next user created through the app gets a low id and collides on the
// primary key.
func usersSequenceFix() string {
	return "-- advance the identity sequence past the preserved ids\n" +
		"SELECT setval(pg_get_serial_sequence('users', 'id'), (SELECT max(id) FROM users));\n\n"
}
