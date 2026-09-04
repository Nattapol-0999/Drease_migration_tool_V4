package legacyimport

import (
	"fmt"
	"strconv"
	"strings"

	"system_cliv4/internal/legacydump"
)

// buildUsers maps the legacy `users` table onto V4 `public.users`.
//
// The legacy id is preserved (KeepLegacyUserIDs): patient.user_insert already points
// at those numbers. users.id is GENERATED ALWAYS AS IDENTITY, so every insert needs
// OVERRIDING SYSTEM VALUE and the identity sequence must be advanced afterwards —
// buildUsersSequenceFix does that and the importer runs it last.
//
// email is carried over verbatim even though the legacy values are usernames
// ('admin', 'wanaka'): they are what people log in with. users_email_key is UNIQUE
// across the WHOLE table, not per clinic, so a collision with an existing row aborts
// the transaction — which is the safe outcome.
//
// locale / is_platform_admin / must_change_password keep their table defaults ('th',
// false, false): the legacy schema has nothing to fill them with. The legacy password
// is a bcrypt "$2y$" hash; golang.org/x/crypto/bcrypt accepts it (its decodeVersion
// only rejects a major version above '2' and the minor letter is not part of the KDF),
// so existing passwords keep working.
func buildUsers(rows []legacydump.Row, in Input, rep *Report) (string, error) {
	e := rep.entity(EntUsers)
	e.SourceRows = len(rows)
	tr := truncations{}

	var s sqlBuilder
	var minID, maxID int64
	seenEmail := map[string]bool{}
	seenID := map[string]bool{}

	for n, r := range rows {
		id := r.Get("id")
		if id == "" {
			return "", fmt.Errorf("users row %d: empty id (it is the conflict key)", n+1)
		}
		if seenID[id] {
			return "", fmt.Errorf("users: duplicate id %q in the dump — users.id is the primary key", id)
		}
		seenID[id] = true
		email := r.Get("email")
		if email == "" {
			return "", fmt.Errorf("users row %d (id=%s): empty email, but users.email is NOT NULL UNIQUE", n+1, id)
		}
		if key := strings.ToLower(email); seenEmail[key] {
			return "", fmt.Errorf("users: duplicate email %q in the dump — users_email_key would reject it", email)
		} else {
			seenEmail[key] = true
		}
		if v, err := strconv.ParseInt(id, 10, 64); err == nil {
			if minID == 0 || v < minID {
				minID = v
			}
			if v > maxID {
				maxID = v
			}
		}

		name, cut := clip(r.Get("name"), 255)
		tr.note("name", cut)
		if name == "" {
			e.skip("empty name (imported as '')")
		}
		emailLit, cut := clip(email, 255)
		tr.note("email", cut)
		pass, cut := clip(r.Get("password"), 255)
		tr.note("password", cut)
		token, cut := clip(r.Get("remember_token"), 100)
		tr.note("remember_token", cut)

		// Insert-only (ON CONFLICT DO NOTHING): a re-run of the same clinic skips ids
		// that already landed. A cross-tenant id/email collision never reaches here —
		// Run's preflight rejects the whole run first, because DO NOTHING would
		// otherwise silently drop an account whose id belongs to another clinic.
		s.addf(`INSERT INTO users (id, name, email, password, remember_token, created_at, updated_at, clinic_code)
OVERRIDING SYSTEM VALUE
VALUES (%s, %s, %s, %s, %s, %s, %s, %s)
ON CONFLICT (id) DO NOTHING;

`, id, lit(name), lit(emailLit), lit(pass), nullLit(token),
			tsLit(r.Get("created_at"), "NULL"), tsLit(r.Get("updated_at"), "NULL"),
			lit(in.ClinicCode))
	}

	e.Statements = s.stmts
	if sum := tr.summary(); sum != "" {
		rep.warnf("users: truncated %s", sum)
	}
	if minID > 0 {
		rep.notef("users: legacy ids %d..%d preserved (identity sequence advanced after the load)", minID, maxID)
	}
	return s.String(), nil
}

// buildUsersSequenceFix advances users' identity sequence past the imported ids.
// WITHOUT this the next user created through the app gets a low id and collides on the
// primary key.
func buildUsersSequenceFix() string {
	return "-- advance the identity sequence past the preserved ids\n" +
		"SELECT setval(pg_get_serial_sequence('users', 'id'), (SELECT max(id) FROM users));\n\n"
}
