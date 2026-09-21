package legacyimport

import (
	"regexp"
	"strings"
	"testing"
)

// Translating a person from the legacy dump to the account V4 issues.
//
// This is the change with the worst failure mode in the whole importer. users.id is a PRIMARY
// KEY across the platform, not per clinic, so the numbers in a tenant's dump belong to
// whoever reached them first — measured on the shared test database, 36 of one export's 112
// accounts. Writing them through meant either overwriting another clinic's login or, under
// ON CONFLICT DO NOTHING, skipping the account while patient.user_insert, clinic_user,
// clinic_staff and doctor all attached to the stranger who owned that number. None of that
// raises an error. It just records the wrong person as having registered a patient.
//
// So the assertions below are about what the SQL says, not about whether it runs.

func usersFixture() []byte {
	return []byte("INSERT INTO `users` (`id`, `code`, `name`, `email`, `password`, `active`) VALUES\n" +
		"(1,   'admin',  'ผู้ดูแล',  'admin',            '$2y$hash1', 1),\n" +
		"(356, 'beer',   'บีร์',    'beer',             '$2y$hash2', 1),\n" +
		"(391, 'sorawit','สรวิศ',   'sorawit',          '$2y$hash3', 1),\n" +
		"(634, 'nurse',  'พยาบาล',  'nurse@clinic.com', '$2y$hash4', 1);\n")
}

func buildUsersSQL(t *testing.T, in Input) (mapSQL, usersSQL string, rep *Report) {
	t.Helper()
	rows := parseUsers(t, in.UsersDump)
	rep = NewReport(in.ClinicCode)
	var err error
	if mapSQL, err = buildUserMap(rows, in, rep); err != nil {
		t.Fatalf("buildUserMap: %v", err)
	}
	if usersSQL, err = buildUsers(rows, in, rep); err != nil {
		t.Fatalf("buildUsers: %v", err)
	}
	return mapSQL, usersSQL, rep
}

func TestUsersInsertDoesNotCarryTheLegacyID(t *testing.T) {
	_, usersSQL, _ := buildUsersSQL(t, Input{ClinicCode: "MBU1", UsersDump: usersFixture()})

	// OVERRIDING SYSTEM VALUE is the tell: it only exists to force a value into an IDENTITY
	// column. Its presence means the legacy number is being written.
	if strings.Contains(usersSQL, "OVERRIDING SYSTEM VALUE") {
		t.Error("the users insert still forces the legacy id into the identity column")
	}
	if strings.Contains(usersSQL, "INSERT INTO users (id,") {
		t.Error("the users insert still names the id column")
	}
	// A re-run has to be a no-op, and email is the only unique key left to hang that on.
	if !strings.Contains(usersSQL, "ON CONFLICT (email) DO NOTHING") {
		t.Errorf("expected the re-run guard on email:\n%s", usersSQL)
	}
}

func TestUserMapKeepsAFreeLoginNameExactlyAsItWas(t *testing.T) {
	mapSQL, _, _ := buildUsersSQL(t, Input{ClinicCode: "MBU1", UsersDump: usersFixture()})

	// The decision is made in SQL because whether a name is taken is only knowable against
	// the target. Both outcomes must be present for every row: the original when it is free,
	// the suffixed form when it is not.
	for _, want := range []string{
		// TEMP is the guarantee that matters and it is unchanged: PostgreSQL drops the table
		// when psql disconnects, so a run that dies half way leaves nothing in the database.
		"CREATE TEMP TABLE import_user_map",
		// PRESERVE ROWS, not DROP. The script commits at every group boundary now (a single
		// transaction over all 19 target tables exhausts max_locks_per_transaction), and
		// ON COMMIT DROP would take this table away at the first of those commits — after
		// which the six places that translate a person through it read a relation that no
		// longer exists.
		"ON COMMIT PRESERVE ROWS",
		"legacy_email",           // kept so the renames can be counted after the database decides
		"'admin'",                // kept when free
		"'admin.MBU1@gmail.com'", // used when taken — no branch table here, so the clinic code
		"'nurse@clinic.com'",     // a real address is left alone when free
		"'nurse.MBU1@gmail.com'", // and still gets the same treatment when taken
	} {
		if !strings.Contains(mapSQL, want) {
			t.Errorf("map is missing %q:\n%s", want, mapSQL)
		}
	}
	// The CASE must compare against OTHER clinics only, or a re-run of this clinic would see
	// its own row and start suffixing names that were already fine.
	if !strings.Contains(mapSQL, "COALESCE(u.clinic_code, '') <> 'MBU1'") {
		t.Errorf("the collision test is not scoped to other clinics:\n%s", mapSQL)
	}
}

func TestSuffixedEmailShape(t *testing.T) {
	// The shape the clinic owner chose on 21 Sep 2026: <name>.<clinic slug>@gmail.com, replacing
	// the <name>_<CLINIC CODE> form chosen on 11 Sep. Both the dot and keeping gmail.com are the
	// owner's call, taken with the consequence on the record: Gmail ignores dots in a local
	// part, so this address shares a mailbox with its dotless spelling.
	for _, c := range []struct{ in, suffix, want string }{
		{"admin", "the-gloss-clinic-cco", "admin.the-gloss-clinic-cco@gmail.com"},
		{"beer", "the-gloss-clinic-cco", "beer.the-gloss-clinic-cco@gmail.com"},
		// The fallback suffix is the clinic code, so that shape has to keep working too.
		{"admin", "C8B173E", "admin.C8B173E@gmail.com"},
		// An address keeps its local part and loses its domain: if the original had been
		// usable, it would not have collided in a way worth preserving.
		{"a@b.com", "MBU1", "a.MBU1@gmail.com"},
	} {
		got, cut := suffixedEmail(c.in, c.suffix)
		if got != c.want {
			t.Errorf("suffixedEmail(%q, %q) = %q, want %q", c.in, c.suffix, got, c.want)
		}
		if cut {
			t.Errorf("suffixedEmail(%q, %q) reported a trim on a name that fits", c.in, c.suffix)
		}
	}

	// The local part is trimmed to leave room for the suffix, the domain AND the ".9" rung, so
	// no candidate on the ladder can overflow varchar(255) halfway up.
	got, cut := suffixedEmail(strings.Repeat("x", 300), "the-gloss-clinic-cco")
	if !cut {
		t.Error("a 300-character login was not reported as trimmed")
	}
	if len(got)+len(".9") > loginNameMax {
		t.Errorf("no room left for the .9 rung: len=%d", len(got))
	}
}

func TestEveryReferenceResolvesThroughTheMapRatherThanTheLegacyNumber(t *testing.T) {
	// One build, every table checked: a rule that got clinic_user right by getting patient
	// wrong would pass a narrower test.
	in := Input{
		ClinicCode:    "MBU1",
		OwnerUsername: "admin",
		UsersDump:     usersFixture(),
		PatientDump: []byte("INSERT INTO `patient` (`HN`, `Firstname`, `branch_code`, `user_insert`) VALUES\n" +
			"('0169001', 'ก', '01', 391),\n" +
			"('0169002', 'ข', '01', 0),\n" +
			"('0169003', 'ค', '01', 'x');\n"),
		DoctorDump: []byte("INSERT INTO `doctor` (`doctor_code`, `doctor_name`, `doctor_user_id`, `doctor_active`) VALUES\n" +
			"('D1', 'หมอหนึ่ง', 634, '1');\n"),
	}
	batches, _, _, err := BuildAll(in, NewReport(in.ClinicCode))
	if err != nil {
		t.Fatalf("BuildAll: %v", err)
	}

	resolve := "SELECT m.new_id FROM import_user_map m WHERE m.legacy_id ="
	for _, c := range []struct{ ent, name string }{
		{EntClinicUser, "clinic_user.user_id"},
		{EntClinicStaff, "clinic_staff.user_id"},
		{EntPatient, "patient.user_insert"},
		{EntDoctor, "doctor.user_id / staff_id"},
	} {
		sqlText := batches[c.ent]
		if sqlText == "" {
			t.Fatalf("%s produced no SQL", c.ent)
		}
		if !strings.Contains(sqlText, resolve) {
			t.Errorf("%s does not resolve the person through the map:\n%s", c.name, head(sqlText, 700))
		}
	}

	// patient row 2 links to user 0 and row 3 to 'x'. Neither is a person, and guessing one
	// would put a name against a registration nobody made.
	patients := batches[EntPatient]
	if strings.Contains(patients, "legacy_id = 0") {
		t.Error("a user_insert of 0 was treated as a link")
	}

	// The fill has to sit between the accounts and anything reading them.
	users := batches[EntUsers]
	if !strings.Contains(users, "UPDATE import_user_map") {
		t.Errorf("the users batch does not record the ids V4 issued:\n%s", head(users, 400))
	}
	if strings.Index(users, "INSERT INTO users") > strings.Index(users, "UPDATE import_user_map") {
		t.Error("the fill runs before the accounts are written")
	}
}

func TestTheRenameCountComesFromTheDatabaseNotFromGo(t *testing.T) {
	// A number computed in Go here would always equal the row count: every account HAS a
	// suffixed form, so comparing the two forms says nothing about whether the name is taken.
	// An earlier version reported "112 accounts collide" for an export where 2 did.
	in := Input{ClinicCode: "MBU1", UsersDump: usersFixture()}
	_, _, rep := buildUsersSQL(t, in)
	counted := regexp.MustCompile(`\d+ (บัญชี|account)`)
	for _, note := range rep.Notes {
		if strings.Contains(note, "gmail.com") && counted.MatchString(note) {
			t.Errorf("the generator is putting a number on collisions it cannot know: %q", note)
		}
	}

	// The script asks the database instead, and says so either way — silence would leave an
	// operator unable to explain why somebody's login stopped working.
	fill := buildUserMapFill("MBU1", true)
	for _, want := range []string{
		"m.email <> m.legacy_email",
		"RAISE NOTICE",
		"ไม่มีชื่อล็อกอินที่ต้องเปลี่ยน",
	} {
		if !strings.Contains(fill, want) {
			t.Errorf("the fill does not report the renames (%q missing):\n%s", want, fill)
		}
	}
}

func TestUnresolvedAccountRefusesTheScriptWhenThisRunWroteThem(t *testing.T) {
	// An account that failed to insert leaves new_id NULL. Every reference to that person
	// would then be NULL — or, if the row was skipped because a stranger holds the email,
	// resolve to the stranger. Refusing is the only safe outcome, and only when this run was
	// supposed to have written them.
	strict := buildUserMapFill("MBU1", true)
	if !strings.Contains(strict, "RAISE EXCEPTION") {
		t.Errorf("a run that writes accounts must refuse on an unresolved one:\n%s", strict)
	}
	loose := buildUserMapFill("MBU1", false)
	if strings.Contains(loose, "RAISE EXCEPTION") {
		t.Errorf("a patients-only run must not be blocked over staff it never imported:\n%s", loose)
	}
	// Allowed is not the same as unremarked. Unresolved people mean patient.user_insert
	// arrives blank, and on a real patients-only run that was 111 of 112 accounts and 4,425
	// patients. Letting that pass without a number is how attribution disappears quietly.
	for _, want := range []string{"new_id IS NULL", "RAISE NOTICE", "people"} {
		if !strings.Contains(loose, want) {
			t.Errorf("a patients-only run does not report unresolved people (%q missing):\n%s", want, loose)
		}
	}
	for _, s := range []string{strict, loose} {
		if !strings.Contains(s, "COALESCE(u.clinic_code, '') = 'MBU1'") {
			t.Errorf("the fill is not scoped to this clinic, so another tenant's row could win the join:\n%s", s)
		}
	}
}

func TestCollisionGuardOnlyRefusesWhenBothEmailFormsAreTaken(t *testing.T) {
	rows := parseUsers(t, usersFixture())
	g := buildUsersCollisionGuard(rows, Input{ClinicCode: "MBU1", UsersDump: usersFixture()})

	// The whole ladder has to appear per account: the bare name, the renamed form, and the
	// prefix the .2 … .9 rungs are built from. Without the prefix this could only ask about two
	// of the places the map is willing to put the account.
	if !strings.Contains(strings.ToLower(g), "('admin', 'admin.mbu1@gmail.com', 'admin.mbu1')") {
		t.Errorf("the ladder is not carried per account, so the guard asks about fewer forms "+
			"than the map tries:\n%s", g)
	}
	if !strings.Contains(g, "generate_series(2, 9)") {
		t.Errorf("guard does not walk the same .2-.9 rungs the map does:\n%s", g)
	}

	// THE REGRESSION THIS TEST EXISTS FOR. Listing the forms and refusing if ANY of them was
	// taken rejected the whole 112-account import against the real database, because `admin` and
	// `beer` belong to another clinic -- which is precisely why the rename exists. Refusing has
	// to require every rung to be gone.
	if strings.Contains(g, "= ANY (ARRAY[") {
		t.Errorf("guard is back to refusing on a single form, which blocks every run the "+
			"rename was added to allow:\n%s", g)
	}
	if !strings.Contains(g, "unnest(ARRAY[w.bare, w.suffixed]") {
		t.Errorf("guard does not ask whether ANY rung is still free:\n%s", g)
	}

	// The same scope as the map, or the two disagree: a row of THIS clinic is a re-import, not
	// the name being taken, and counting it would make a re-run refuse itself.
	if !strings.Contains(g, "COALESCE(u.clinic_code, '') <> 'MBU1'") {
		t.Errorf("guard is not scoped to other clinics:\n%s", g)
	}

	// Checking the legacy id would refuse most runs against a shared database over numbers
	// nothing is going to write.
	if strings.Contains(g, "u.id") {
		t.Errorf("guard still checks the legacy id, which is no longer written:\n%s", g)
	}
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + " …"
}
