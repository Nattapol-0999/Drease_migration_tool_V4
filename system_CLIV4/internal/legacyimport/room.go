package legacyimport

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"system_cliv4/internal/legacydump"
)

// buildRoom maps the legacy `clinic` table — which despite its name is the ROOM master
// (Doctor Room, ห้องหัตถการ, Head Spa 1..3, …) — onto V4 `public.room`.
//
// room.code KEEPS the legacy code ('01'..'10'): it is the value appointment.room_id
// stores, and room.sql's ListRooms literally selects `code AS room_id`. Re-issuing
// codes would orphan every legacy appointment.
//
// Idempotency comes from the partial unique index uniq_room_clinic_code
// (clinic_code, code) WHERE status = 1 — the predicate is repeated in ON CONFLICT so
// Postgres can infer the index. Legacy active 1/0 maps to status 1/0 (0 is how
// SoftDeleteRoom marks a removed room) and room_number maps to sort_order.
//
// Dropped: the legacy id (nothing references it — appointments reference `code`),
// eng_name (V4 room has a single `name`), type, room_type, df_type,
// send_notification, hide_in_home. In the first clinic's data every one of those held
// a single identical value across all rows, so nothing was lost; the builder reports
// when that is no longer true.
//
// booked is the set of legacy room codes the appointment dump actually books (see
// roomsWithAppointments). A room in that set is imported ACTIVE even when the source
// closed it, because V4's validate_appointment_assignment resolves an appointment's
// branch from `room WHERE status = 1` and RAISES when it finds nothing — so a closed
// room does not merely look closed, it takes every appointment ever booked in it down
// with the whole import. Measured on the first multi-branch tenant: 4 of 8 rooms were
// closed with the branch they belonged to, and 345 of 8,833 appointments referenced
// them. Empty set (no appointment dump in this run) keeps every source status as-is.
func buildRoom(rows []legacydump.Row, booked map[string]bool, in Input, rep *Report) (string, error) {
	e := rep.entity(EntRoom)
	e.SourceRows = len(rows)
	tr := truncations{}
	br := newBranchResolver(in)
	defer br.report(rep, "room")
	var reopened []string

	uniform := map[string]map[string]bool{}
	for _, c := range []string{"type", "room_type", "df_type", "send_notification", "hide_in_home"} {
		uniform[c] = map[string]bool{}
	}

	var s sqlBuilder
	for n, r := range rows {
		code := r.Get("code")
		if code == "" {
			return "", fmt.Errorf("room row %d: empty code (it is the room_id appointments reference)", n+1)
		}
		// Translated, not copied: the source value is the legacy branch code and V4 owns
		// its own (see branchResolver).
		branch := br.resolve(r.Get("branch_code"))
		name := r.Get("tha_name")
		if name == "" {
			name = r.Get("eng_name")
		}
		if name == "" {
			return "", fmt.Errorf("room row %d (code=%s): no name, but room.name is NOT NULL", n+1, code)
		}
		status := "1"
		if r.Get("active") == "0" {
			if booked[code] {
				// Kept ACTIVE on purpose. The alternative — status 0 — is not "the room
				// shows as closed": the trigger refuses every appointment that names it,
				// and clearing room_id instead makes the same trigger set branch_code to
				// NULL, filing the booking at no branch at all. Reopening a room the
				// clinic had closed is a visible, reversible change; losing the history
				// is neither.
				reopened = append(reopened, fmt.Sprintf("%s (%s)", code, oneLine(name)))
			} else {
				status = "0"
				e.skip("soft-deleted in the source (status 0)")
			}
		}
		sortOrder := "0"
		if v := r.Get("room_number"); v != "" {
			if _, err := strconv.Atoi(v); err == nil {
				sortOrder = v
			} else {
				rep.warnf("room %s: room_number %q is not a number — sort_order 0", code, v)
			}
		}
		for c := range uniform {
			uniform[c][r.Get(c)] = true
		}

		codeLit, cut := clip(code, 64)
		tr.note("code", cut)
		branchLit, cut := clip(branch, 32)
		tr.note("branch_code", cut)
		color, cut := clip(r.Get("color"), 16)
		tr.note("color", cut)

		s.comment("room %s · %s", code, oneLine(name))
		s.addf(`INSERT INTO room (clinic_code, branch_code, code, name, color, sort_order, status, created_at, updated_at)
VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s)
ON CONFLICT (clinic_code, code) WHERE status = 1
DO UPDATE SET branch_code = EXCLUDED.branch_code,
              name        = EXCLUDED.name,
              color       = EXCLUDED.color,
              sort_order  = EXCLUDED.sort_order,
              updated_at  = now();

`, lit(in.ClinicCode), lit(branchLit), lit(codeLit), lit(name), lit(color),
			sortOrder, status,
			tsLit(r.Get("create_at"), "now()"), tsLit(r.Get("updated_at"), "now()"))
	}

	e.Statements = s.stmts
	if sum := tr.summary(); sum != "" {
		rep.warnf("room: truncated %s", sum)
	}
	// Loud, and by name: the clinic closed these rooms and the import is overriding that.
	// It has to be in the artifact's header, because the only other way to notice is to
	// open the booking screen and find a room that should not be there.
	if len(reopened) > 0 {
		sort.Strings(reopened)
		rep.warnf("room: %d ห้องที่ระบบเดิมปิดไว้ ถูกนำเข้าเป็นห้องที่ใช้งานได้ (status=1) เพราะมีนัดในไฟล์อ้างถึง — V4 ปฏิเสธนัดที่อ้างห้องปิด (validate_appointment_assignment) ฉะนั้นถ้าเก็บสถานะเดิมไว้ นัดเหล่านั้นจะพาการนำเข้าล้มทั้งไฟล์ · ห้องเหล่านี้จะโผล่ในจอจองคิวด้วย ปิดเองได้ที่ตั้งค่าห้องหลังนำเข้าเสร็จ · %s",
			len(reopened), strings.Join(reopened, ", "))
	}
	// A dropped column only stays harmless while it holds one value for every row.
	for col, vals := range uniform {
		if len(vals) > 1 {
			rep.warnf("room: dropped column %q has %d distinct values in this dump — it is NOT uniform any more, so information is being lost", col, len(vals))
		}
	}
	return s.String(), nil
}

// roomsWithAppointments is the set of legacy room codes the appointment dump books.
//
// The legacy column is called `clinic_code` and is NOT a clinic: its values are the room
// master's codes ('01'..'10'), which is the same misnaming buildAppointment documents and
// maps to room_id. Read from the same column here so the two cannot drift apart.
//
// Returns legacy codes, matching room.code, because the room master keeps the legacy code
// as its own (see buildRoom's header) — no branch translation is involved.
func roomsWithAppointments(rows []legacydump.Row) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		if code := strings.TrimSpace(r.Get("clinic_code")); code != "" {
			out[code] = true
		}
	}
	return out
}

// branchByRoom maps a legacy room code to the V4 branch_code that room is imported at.
//
// It resolves through a branchResolver of its own and deliberately never calls report():
// buildRoom already accounts for every one of these rows, and a second voice counting the
// same rows would double every number in the report. This one exists only so
// buildAppointment can compare what it wrote against where the trigger will actually file
// the booking.
func branchByRoom(rows []legacydump.Row, in Input) map[string]string {
	br := newBranchResolver(in)
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		code := strings.TrimSpace(r.Get("code"))
		if code == "" {
			continue
		}
		out[code] = br.resolve(r.Get("branch_code"))
	}
	return out
}
