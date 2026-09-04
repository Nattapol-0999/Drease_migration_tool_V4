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
}

// BuildAll parses the supplied files and renders the SQL for each entity, without
// touching a database. The returned map is keyed by the Ent* constants; userRows is
// handed back so the caller can emit the cross-clinic collision guard from the same
// parse instead of reading the dump twice.
func BuildAll(in Input, rep *Report) (batches map[string]string, userRows []legacydump.Row, money moneyBatches, err error) {
	if strings.TrimSpace(in.ClinicCode) == "" {
		return nil, nil, money, fmt.Errorf("clinic_code is required")
	}

	out := map[string]string{}

	if len(in.UsersDump) > 0 {
		_, userRows, err = legacydump.Parse(string(in.UsersDump), "users")
		if err != nil {
			return nil, nil, money, fmt.Errorf("users dump: %w", err)
		}
	}

	var doctorRows []legacydump.Row
	if len(in.DoctorDump) > 0 {
		_, doctorRows, err = legacydump.Parse(string(in.DoctorDump), "doctor")
		if err != nil {
			return nil, nil, money, fmt.Errorf("doctor dump: %w", err)
		}
	}

	if len(userRows) > 0 {
		// Exactly one account becomes the owner; the caller names it and it must be
		// present, or the clinic would have nobody who can administer it.
		if strings.TrimSpace(in.OwnerUsername) == "" {
			return nil, nil, money, fmt.Errorf("owner_username is required when a users dump is provided")
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
	if len(doctorRows) > 0 {
		if out[EntDoctor], err = buildDoctor(doctorRows, in, rep); err != nil {
			return nil, nil, money, err
		}
	}

	if len(in.RoomDump) > 0 {
		// The legacy room master lives in a table called `clinic`.
		_, roomRows, err := legacydump.Parse(string(in.RoomDump), "clinic")
		if err != nil {
			return nil, nil, money, fmt.Errorf("room dump: %w", err)
		}
		if out[EntRoom], err = buildRoom(roomRows, in, rep); err != nil {
			return nil, nil, money, err
		}
	}

	if len(in.PatientDump) > 0 {
		_, patientRows, err := legacydump.Parse(string(in.PatientDump), "patient")
		if err != nil {
			return nil, nil, money, fmt.Errorf("patient dump: %w", err)
		}
		if out[EntPatient], err = buildPatient(patientRows, in, rep); err != nil {
			return nil, nil, money, err
		}
	}

	if len(in.AddressDump) > 0 {
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

	if len(in.AppointmentDump) > 0 {
		_, apptRows, err := legacydump.Parse(string(in.AppointmentDump), "appointment")
		if err != nil {
			return nil, nil, money, fmt.Errorf("appointment dump: %w", err)
		}
		bookable := buildBookableDoctors(doctorRows, userRows)
		if out[EntAppointment], err = buildAppointment(apptRows, bookable, in, rep); err != nil {
			return nil, nil, money, err
		}
	}

	// The Mongo half: catalog, then the money that references it.
	if len(in.MongoDump) > 0 {
		mc, err := parseMongoCatalog(in.MongoDump)
		if err != nil {
			return nil, nil, money, err
		}
		out[EntCatalogCategory] = buildCatalogCategory(mc, in, rep)
		if out[EntCatalogService], err = buildCatalogServices(mc, in, rep); err != nil {
			return nil, nil, money, err
		}
		itemsSQL, drugSQL, slotSQL, err := buildItems(mc, in, rep)
		if err != nil {
			return nil, nil, money, err
		}
		out[EntItems], out[EntCatalogDrug], out[EntSlotStock] = itemsSQL, drugSQL, slotSQL
		if out[EntCatalogCourse], out[EntCatalogCourseLine], err = buildCourses(mc, in, rep); err != nil {
			return nil, nil, money, err
		}

		// The money half of the same file. It runs last because invoice_item resolves
		// catalog_item_id by code, so the catalog statements above must be in the
		// script before it — the SQL is applied top-down in one transaction.
		if money, err = buildMoney(mc, in, rep); err != nil {
			return nil, nil, money, err
		}
		out[EntInvoice] = money.invoice
		out[EntInvoiceItem] = money.invoiceItem
		out[EntReceipt] = money.receipt
		out[EntReceiptPayment] = money.receiptPayment
		out[EntInvoiceSettle] = money.settle

		// Course entitlements last: they point at the invoice lines the money block just
		// wrote, and at the course codes the catalog block minted.
		purge, bought, boughtItems, cerr := buildCourseBought(mc, money, in, rep)
		if cerr != nil {
			return nil, nil, money, cerr
		}
		out[EntCourseBoughtPurge] = purge
		out[EntCourseBought] = bought
		out[EntCourseBoughtItems] = boughtItems
	}

	if len(out) == 0 {
		return nil, nil, money, fmt.Errorf("no input files: supply at least one of users, doctor, room, patient, address, appointment, mongo")
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
