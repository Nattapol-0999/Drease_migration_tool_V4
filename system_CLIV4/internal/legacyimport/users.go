package legacyimport

import (
	"fmt"
	"strconv"
	"strings"

	"system_cliv4/internal/legacydump"
)

// buildUserMap emits the TEMP table that every later reference to a person goes through.
//
// WHY A MAP AND NOT THE LEGACY id
// users.id is a PRIMARY KEY across the WHOLE platform, not per clinic, so the numbers in a
// tenant's dump belong to whoever reached them first. Measured on the shared test database:
// of 112 accounts in one export, 36 landed on ids already held by other clinics — 19 by a
// tenant with real data since 2016. Writing them through would have needed
// OVERRIDING SYSTEM VALUE and either overwritten another clinic's login or, under
// ON CONFLICT DO NOTHING, skipped the account while everything pointing at it silently
// attached to the stranger who owned that number.
//
// So V4 issues the ids and this map translates. legacy_id is what the dump's other tables
// carry; new_id is filled once the accounts exist. Every reference resolves through it, which
// keeps the translation in ONE place rather than repeating a join at six call sites.
//
// WHY THE EMAIL IS DECIDED IN SQL
// users.email is UNIQUE platform-wide too, and these legacy values are not addresses — they
// are login names ('admin', 'beer', 'wanaka'). Two of them were already taken by another
// clinic. Whether a given name collides is only knowable against the target, so the CASE
// runs at load time: a name that is free is kept exactly as it was, and only a colliding one
// gets the clinic code folded in. Nobody's login changes unless it had to.
//
// The suffixed form was chosen by the clinic owner on 11 Sep 2026 (see DECISIONS.md, which
// also records the risk accepted: gmail.com is not a domain we own, and password reset does
// not require a verified address).
func buildUserMap(rows []legacydump.Row, in Input, rep *Report) (string, error) {
	var s sqlBuilder
	s.comment("legacy users.id → the id V4 issues · and the email each account ends up with")
	// legacy_email is kept beside the decided one so the RENAMES CAN BE REPORTED. Go cannot
	// count them: whether a name is taken is a fact about the target, known only once the
	// CASE has run. Comparing the two columns after the load gives the real number instead of
	// a guess, and it is also what an operator needs when a staff member cannot log in.
	s.addf(`CREATE TEMP TABLE import_user_map (
    legacy_id    bigint PRIMARY KEY,
    legacy_email text NOT NULL,
    email        text NOT NULL,
    new_id       bigint
) ON COMMIT DROP;

`)

	tr := truncations{}
	values := make([]string, 0, len(rows))
	for _, r := range rows {
		id := r.Get("id")
		email := r.Get("email")
		if id == "" || email == "" {
			continue // buildUsers refuses these; the map simply has nothing to say about them
		}
		// users.email is varchar(255). Clipped HERE because this is the only place the value
		// that reaches the column is decided — clipping it in buildUsers would report a
		// truncation that never happened and let the untruncated form into the map.
		email, cut := clip(email, 255)
		tr.note("email", cut)
		suffixed, cut := clip(suffixedEmail(email, in.ClinicCode), 255)
		tr.note("email (รูปที่เติมรหัสคลินิก)", cut)
		// Free name → keep it. Taken name → the suffixed form. Decided per row, at load time.
		values = append(values, fmt.Sprintf(
			"    (%s, %s, CASE WHEN EXISTS (SELECT 1 FROM users u WHERE lower(u.email) = lower(%s)\n"+
				"                                 AND COALESCE(u.clinic_code, '') <> %s)\n"+
				"                  THEN %s ELSE %s END)",
			id, lit(email), lit(email), lit(in.ClinicCode), lit(suffixed), lit(email)))
	}
	if len(values) == 0 {
		return "", nil
	}
	s.addf("INSERT INTO import_user_map (legacy_id, legacy_email, email) VALUES\n%s;\n\n",
		strings.Join(values, ",\n"))

	if sum := tr.summary(); sum != "" {
		rep.warnf("users: truncated %s", sum)
	}
	// Stated as a RULE with no count. An earlier version reported a number here and it was
	// always the row count — every account has a suffixed form available, so comparing the two
	// forms in Go said nothing about collisions. The real list is raised as a NOTICE by the
	// script, after the database has decided.
	rep.notef("users: ชื่อล็อกอินที่ซ้ำกับคลินิกอื่นจะถูกเปลี่ยนเป็น <ชื่อ>_%s@gmail.com เฉพาะตัวที่ซ้ำจริง "+
		"— ตัวที่ไม่ซ้ำใช้ค่าเดิมทุกตัว · รายชื่อที่ถูกเปลี่ยนจริงจะขึ้นเป็น NOTICE ตอนรันสคริปต์", in.ClinicCode)
	return s.String(), nil
}

// buildUserMapFill records the ids V4 actually issued, so later tables can resolve a person.
//
// Scoped to this clinic: two tenants can hold the same email only if one of them was
// suffixed, and matching without the clinic_code would let a stranger's row win the join.
//
// strict is on when THIS run writes the accounts. Then every mapped row must resolve, and one
// that does not is a refusal: it means the account was not written, and every table pointing
// at that person would land NULL or on somebody else. When the run only reads the users dump
// (a patients-only import resolving against accounts a previous run created), an unresolved
// row is left alone — a clinic whose staff were never migrated is a legitimate state, and
// refusing would block the import over people nobody asked for.
func buildUserMapFill(clinic string, strict bool) string {
	fill := fmt.Sprintf(`-- the ids V4 issued, so every reference below can resolve a person
UPDATE import_user_map m
   SET new_id = u.id
  FROM users u
 WHERE lower(u.email) = lower(m.email)
   AND COALESCE(u.clinic_code, '') = %s;

-- Which logins actually had to change. Raised here and not reported by the generator
-- because the collision is a fact about the target: the CASE above has just decided it.
-- The operator needs this list — these are the people whose old login stops working.
DO $renamed$
DECLARE
    changed text;
    n       int;
BEGIN
    SELECT count(*), string_agg(format('%%s → %%s', m.legacy_email, m.email), ', ' ORDER BY m.legacy_email)
      INTO n, changed
      FROM import_user_map m WHERE m.email <> m.legacy_email;
    IF n > 0 THEN
        RAISE NOTICE 'users: เปลี่ยนชื่อล็อกอิน %% บัญชี เพราะชื่อเดิมเป็นของคลินิกอื่น: %%', n, changed;
    ELSE
        RAISE NOTICE 'users: ไม่มีชื่อล็อกอินที่ต้องเปลี่ยน — ทุกบัญชีใช้ค่าเดิม';
    END IF;
END $renamed$;

`, lit(clinic))
	if !strict {
		return fill + `-- Not every account has to resolve here: this run does not write users, so a legacy
-- reference to somebody who was never imported simply stays NULL.

`
	}
	return fill + `-- GUARD: every mapped account must have resolved, because this run wrote them all.
-- An unresolved row means the insert was skipped — normally because its email belongs to
-- another clinic — and anything pointing at that person would land NULL or on a stranger.
DO $guard$
DECLARE
    orphans text;
BEGIN
    SELECT string_agg(format('legacy_id=%s email=%s', m.legacy_id, m.email), '; ')
      INTO orphans
      FROM import_user_map m WHERE m.new_id IS NULL;
    IF orphans IS NOT NULL THEN
        RAISE EXCEPTION 'users: มีบัญชีที่หา id ใหม่ไม่ได้หลังนำเข้า — ปฏิเสธทั้งสคริปต์: %', orphans;
    END IF;
END $guard$;

`
}

// userIDByLegacy is the expression every table uses instead of the legacy number.
//
// A subquery rather than a join so it drops into a VALUES list unchanged. import_user_map is
// keyed on legacy_id, so this is an index lookup, and the guard above has already proved
// new_id is not NULL for every row in it.
func userIDByLegacy(legacyID string) string {
	return fmt.Sprintf("(SELECT m.new_id FROM import_user_map m WHERE m.legacy_id = %s)", legacyID)
}

// suffixedEmail folds the clinic code into a login name so it can coexist with another
// tenant's identical one.
//
// `admin` → `admin_C8B173E@gmail.com`. A value that already looks like an address keeps its
// local part and gains the code the same way, so `a@b.com` → `a_C8B173E@gmail.com` — the
// domain is replaced deliberately: if the original address were usable it would not have
// collided in a way anyone wanted to keep.
func suffixedEmail(email, clinic string) string {
	local := email
	if at := strings.IndexByte(email, '@'); at > 0 {
		local = email[:at]
	}
	return fmt.Sprintf("%s_%s@gmail.com", local, clinic)
}

// buildUsers maps the legacy `users` table onto V4 `public.users`.
//
// The id is NOT carried over — see buildUserMap. Rows take the email the map decided, so a
// login name that was free stays exactly as it was and only a colliding one is rewritten.
//
// ON CONFLICT (email) DO NOTHING makes a re-run of the same clinic a no-op. It cannot hide a
// cross-tenant collision: the map suffixes those before they reach here, and the fill guard
// refuses the script if any account failed to resolve afterwards.
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
		pass, cut := clip(r.Get("password"), 255)
		tr.note("password", cut)
		token, cut := clip(r.Get("remember_token"), 100)
		tr.note("remember_token", cut)

		// The email comes from the map, not from this row: the map is where the collision
		// rule lives, and reading it in one place keeps the two from ever disagreeing.
		// buildUserMap is also where the 255-character limit is applied, for the same reason.
		s.addf(`INSERT INTO users (name, email, password, remember_token, created_at, updated_at, clinic_code)
SELECT %s, m.email, %s, %s, %s, %s, %s
  FROM import_user_map m WHERE m.legacy_id = %s
ON CONFLICT (email) DO NOTHING;

`, lit(name), lit(pass), nullLit(token),
			tsLit(r.Get("created_at"), "NULL"), tsLit(r.Get("updated_at"), "NULL"),
			lit(in.ClinicCode), id)
	}

	e.Statements = s.stmts
	if sum := tr.summary(); sum != "" {
		rep.warnf("users: truncated %s", sum)
	}
	if minID > 0 {
		rep.notef("users: ids เดิม %d..%d ไม่ถูกนำมาใช้ — V4 ออกเลขให้เอง แล้วแปลผ่าน import_user_map "+
			"(ids เดิมเป็น primary key ระดับแพลตฟอร์ม จึงเป็นของคลินิกที่ไปถึงเลขนั้นก่อน)", minID, maxID)
	}
	return s.String(), nil
}

// No buildUsersSequenceFix any more.
//
// It existed to setval() the identity sequence past ids written with OVERRIDING SYSTEM VALUE,
// or the next account created through the app would collide on the primary key. Nothing
// overrides the sequence now, so advancing it would only skip a block of numbers for no
// reason — and doing it on a shared database would move a counter other tenants also draw
// from.
