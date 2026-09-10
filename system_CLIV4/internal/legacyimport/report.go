package legacyimport

import (
	"fmt"
	"sort"
	"strings"
)

// Entity names, in the order the generated script must apply them: a later entity
// references rows an earlier one creates (clinic_user/clinic_staff have real foreign
// keys to users; appointment references hn / doctor_code / room_id as plain strings).
//
// There is deliberately no `clinic` entity: the tenant is created through the normal
// V4 admin flow before an import runs, and BuildScript only verifies that it exists.
const (
	EntUsers       = "users"
	EntClinicUser  = "clinic_user"
	EntClinicStaff = "clinic_staff"
	EntDoctor      = "doctor"
	EntRoom        = "room"
	EntPatient     = "patient"

	EntAppointment = "appointment"
)

// order is the fixed statement order inside the generated script. The catalog block
// runs after the tenant's people and places: nothing in it depends on them, but a
// reviewer reads the file top-down and the clinic's own records come first.
// The money block is LAST because it depends on both of the blocks before it:
// invoice.hn must reach a patient row, and invoice_item.catalog_item_id is resolved by
// looking up the catalog code.
// The entitlement block is after money because it points at the invoice lines the money
// block writes.
// photoOrder is last and carries NO SQL: the photos entity renders a copy manifest
// instead (see page.go). It is in this list only so the report prints it alongside the
// rest; script.go skips any entity with no statements.
var order = append(append(append(append(append([]string{
	EntUsers, EntClinicUser, EntClinicStaff,
	EntDoctor, EntRoom, EntPatient,
}, addressOrder...), []string{EntAppointment}...),
	append(catalogOrder, moneyOrder...)...), courseBoughtOrder...), photoOrder...)

// EntityOrder returns a copy of the order, for callers that render entities without
// holding a Report. A copy, not the slice: the order is policy, and a caller must not
// be able to reshuffle the script by mutating what it was handed.
func EntityOrder() []string { return append([]string(nil), order...) }

// Groups let an operator import PART of an export. That is not a convenience: a target
// that already holds some of this data needs the missing pieces only, and writing the
// rest would overwrite records someone has since edited by hand.
//
// A group is all-or-nothing so a half-written relationship cannot be produced.
var entityGroups = map[string][]string{
	"people":   {EntUsers, EntClinicUser, EntClinicStaff, EntDoctor},
	"rooms":    {EntRoom},
	"patients": {EntPatient},
	// addresses is separate from patients on purpose: a target can already hold the
	// patients and want only the addresses (which is exactly this clinic's case).
	"addresses":    {EntPatientAddress, EntPatientAddressFill},
	"appointments": {EntAppointment},
	"catalog": {
		EntCatalogCategory, EntCatalogService, EntItems, EntCatalogDrug,
		EntCatalogCourse, EntCatalogCourseLine,
	},
	"stock": {EntSlotStock},
	// money is all-or-nothing for a reason no other group has: an invoice without its
	// items is a bill for nothing, and a receipt without its payment channels is money
	// with no source. Splitting it would produce financial documents that contradict
	// themselves, so the four tables plus the settle pass move together.
	"money": {
		EntInvoice, EntInvoiceItem, EntReceipt, EntReceiptPayment, EntInvoiceSettle,
	},
	// entitlements is the ONLY group that deletes before it writes (the clinic owner
	// chose to replace a half-finished earlier import rather than patch it). The purge
	// travels WITH the rows that replace it, in the same transaction, so the balances can
	// never be missing — either both happen or neither does.
	"entitlements": {
		EntCourseBoughtPurge, EntCourseBought, EntCourseBoughtItems,
	},
	// photos produces no SQL, so selecting it changes nothing in the .sql artifact — it
	// is here so `-only photos` is not rejected as an unknown group when an operator
	// wants the image manifest and nothing else.
	"photos": {EntPhoto},
}

// groupDependsOn records which group must already be present at the TARGET for another
// to make sense. It is advisory: the script still runs, but the report says so.
//
// money is the exception — its dependency on patients is NOT advisory. Guard 5 refuses
// the import when a bill's HN has no patient row, because a bill nobody owns is an
// amount owed by nobody.
var groupDependsOn = map[string][]string{
	// addresses is NOT advisory: every address INSERT is gated on the patient existing,
	// so an address whose patient is absent writes nothing (and is reported).
	"addresses":    {"patients"},
	"appointments": {"people", "rooms", "patients"},
	"stock":        {"catalog"},
	"money":        {"patients", "catalog"},
	// entitlements resolves catalog_item_id from the MINTED course code and
	// invoice_item_id from the sale line, so both must already be at the target.
	"entitlements": {"patients", "catalog", "money"},
}

// GroupNames lists the selectable groups, sorted for a stable UI and help text.
func GroupNames() []string {
	out := make([]string, 0, len(entityGroups))
	for g := range entityGroups {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// entitiesForGroups resolves group names to entity names, rejecting unknown groups
// rather than silently importing less than the caller asked for.
func entitiesForGroups(groups []string) (map[string]bool, error) {
	want := map[string]bool{}
	for _, g := range groups {
		g = strings.ToLower(strings.TrimSpace(g))
		if g == "" {
			continue
		}
		ents, ok := entityGroups[g]
		if !ok {
			return nil, fmt.Errorf("ไม่รู้จักกลุ่ม %q — เลือกได้: %s", g, strings.Join(GroupNames(), ", "))
		}
		for _, e := range ents {
			want[e] = true
		}
	}
	return want, nil
}

// EntityResult is what one entity produced.
type EntityResult struct {
	// SourceRows is how many rows the dump held.
	SourceRows int `json:"source_rows"`
	// Statements is how many INSERT statements were generated.
	Statements int `json:"statements"`
	// Skipped explains rows the mapping deliberately left out.
	Skipped map[string]int `json:"skipped,omitempty"`
}

// Report is the whole generation run: what was read, what was written, and every
// decision worth a human's attention before the script is applied.
type Report struct {
	ClinicCode string                   `json:"clinic_code"`
	Entities   map[string]*EntityResult `json:"entities"`
	// Order lets a client render the entities in script order without sorting.
	Order    []string `json:"order"`
	Warnings []string `json:"warnings,omitempty"`
	Notes    []string `json:"notes,omitempty"`

	// Photos is the image copy plan, present only when a -page dump was supplied. It is
	// NOT part of the .sql artifact: photos.file_size can only be read off the image
	// files themselves, which arrive separately (see page.go).
	Photos *PhotoManifest `json:"photos,omitempty"`
}

// NewReport starts a report for one clinic.
func NewReport(clinic string) *Report {
	return &Report{ClinicCode: clinic, Entities: map[string]*EntityResult{}, Order: EntityOrder()}
}

func (r *Report) entity(name string) *EntityResult {
	if e, ok := r.Entities[name]; ok {
		return e
	}
	e := &EntityResult{Skipped: map[string]int{}}
	r.Entities[name] = e
	return e
}

func (r *Report) warnf(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

func (r *Report) notef(format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

// skip records a deliberately dropped row under a reason label.
func (e *EntityResult) skip(reason string) {
	if e.Skipped == nil {
		e.Skipped = map[string]int{}
	}
	e.Skipped[reason]++
}

// truncations is a small helper the entity builders share to report clipped values
// once per column instead of once per row.
type truncations map[string]int

func (t truncations) note(col string, clipped bool) {
	if clipped {
		t[col]++
	}
}

func (t truncations) summary() string {
	if len(t) == 0 {
		return ""
	}
	cols := make([]string, 0, len(t))
	for c := range t {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, fmt.Sprintf("%s×%d", c, t[c]))
	}
	return strings.Join(parts, ", ")
}
