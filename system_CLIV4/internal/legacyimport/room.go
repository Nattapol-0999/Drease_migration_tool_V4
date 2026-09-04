package legacyimport

import (
	"fmt"
	"strconv"

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
func buildRoom(rows []legacydump.Row, in Input, rep *Report) (string, error) {
	e := rep.entity(EntRoom)
	e.SourceRows = len(rows)
	tr := truncations{}

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
		branch := r.Get("branch_code")
		if branch == "" {
			branch = BranchCode
		}
		name := r.Get("tha_name")
		if name == "" {
			name = r.Get("eng_name")
		}
		if name == "" {
			return "", fmt.Errorf("room row %d (code=%s): no name, but room.name is NOT NULL", n+1, code)
		}
		status := "1"
		if r.Get("active") == "0" {
			status = "0"
			e.skip("soft-deleted in the source (status 0)")
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
	// A dropped column only stays harmless while it holds one value for every row.
	for col, vals := range uniform {
		if len(vals) > 1 {
			rep.warnf("room: dropped column %q has %d distinct values in this dump — it is NOT uniform any more, so information is being lost", col, len(vals))
		}
	}
	return s.String(), nil
}
