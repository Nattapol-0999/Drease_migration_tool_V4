package legacyimport

import (
	"strings"
	"testing"

	"system_cliv4/internal/legacydump"
)

// Naming the owner account.
//
// owner_username is matched against legacy `users`.`code` — not a name, not a username column,
// not an email. That is not guessable from the field label, and when the value is wrong the
// import stops at the last step, after the upload and the survey and the provisioning.
//
// The refusal used to say only how many accounts the dump held ("not found among the 112
// account(s)"), which left the operator to search a multi-megabyte dump by hand for a column
// they had no reason to know about. It now lists the codes that would have worked.

func usersDump(rows ...string) []byte {
	return []byte("INSERT INTO `users` (`id`, `code`, `name`, `lastname`, `active`) VALUES\n" +
		strings.Join(rows, ",\n") + ";\n")
}

func parseUsers(t *testing.T, dump []byte) []legacydump.Row {
	t.Helper()
	_, rows, err := legacydump.Parse(string(dump), "users")
	if err != nil {
		t.Fatalf("parse users: %v", err)
	}
	return rows
}

func TestOwnerCandidatesListsOnlyCodesThatWouldBeAccepted(t *testing.T) {
	// The row with no id is skipped by hasOwner, so offering its code would send the operator
	// straight back into the same refusal.
	rows := parseUsers(t, usersDump(
		"(1, '1010', 'ก', 'ก', 1)",
		"(2, '1002', 'ข', 'ข', 1)",
		"('', '9999', 'ไม่มี id', '', 1)",
		"(4, '', 'ไม่มี code', '', 1)",
	))

	got := ownerCandidates(rows)
	if strings.Join(got, ",") != "1002,1010" {
		t.Errorf("candidates = %v, want [1002 1010] — sorted numerically, no id-less or code-less row",
			got)
	}
	for _, offered := range got {
		if !hasOwner(rows, offered) {
			t.Errorf("candidate %q would be refused by hasOwner — the list must only hold codes that work",
				offered)
		}
	}
	if hasOwner(rows, "9999") {
		t.Error("hasOwner accepted a row with no id; ownerCandidates and hasOwner have drifted apart")
	}
}

func TestOwnerCandidatesSortsNumbersAsNumbers(t *testing.T) {
	// These are staff numbers in every export seen. Sorted as strings, 1002 lands after 10.
	rows := parseUsers(t, usersDump(
		"(1, '1010', 'a', '', 1)",
		"(2, '10', 'b', '', 1)",
		"(3, '1002', 'c', '', 1)",
		"(4, '9', 'd', '', 1)",
	))
	if got := strings.Join(ownerCandidates(rows), ","); got != "9,10,1002,1010" {
		t.Errorf("order = %s, want 9,10,1002,1010", got)
	}
}

func TestOwnerCandidatesIsDeterministicAndDeduplicated(t *testing.T) {
	rows := parseUsers(t, usersDump(
		"(1, '1010', 'a', '', 1)",
		"(2, '1010', 'a again', '', 1)",
		"(3, '1002', 'b', '', 1)",
	))
	first := strings.Join(ownerCandidates(rows), ",")
	if first != "1002,1010" {
		t.Fatalf("candidates = %s, want 1002,1010 with the duplicate collapsed", first)
	}
	for i := 0; i < 8; i++ {
		if got := strings.Join(ownerCandidates(rows), ","); got != first {
			t.Fatalf("run %d gave %s, first run gave %s", i, got, first)
		}
	}
}

func TestOwnerRefusalNamesTheColumnAndTheCandidates(t *testing.T) {
	// The whole point: an operator reading this must not have to open the dump.
	rows := parseUsers(t, usersDump(
		"(1, '1010', 'a', '', 1)",
		"(2, '1002', 'b', '', 1)",
	))
	msg := describeOwnerCandidates(rows)
	for _, want := range []string{"1002", "1010"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not offer %q: %s", want, msg)
		}
	}

	_, _, _, err := BuildAll(Input{
		ClinicCode:    "MBOWN1",
		OwnerUsername: "1096",
		UsersDump: usersDump(
			"(1, '1010', 'a', '', 1)",
			"(2, '1002', 'b', '', 1)",
		),
	}, NewReport("MBOWN1"))
	if err == nil {
		t.Fatal("an owner_username absent from the dump must refuse the build")
	}
	got := err.Error()
	for _, want := range []string{"1096", "code", "users", "1002", "1010"} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal does not mention %q — the operator has to open the dump without it:\n  %s",
				want, got)
		}
	}
}

func TestOwnerRefusalIsBoundedButSaysHowManyWereWithheld(t *testing.T) {
	// A refusal that scrolls its own explanation off the screen has not helped anybody.
	rows := make([]string, 0, 40)
	for i := 1; i <= 40; i++ {
		rows = append(rows, "("+itoa(i)+", '"+itoa(1000+i)+"', 'x', '', 1)")
	}
	parsed := parseUsers(t, usersDump(rows...))
	msg := describeOwnerCandidates(parsed)

	shown := strings.Count(msg, "10")
	if shown == 0 {
		t.Fatalf("no codes listed at all: %s", msg)
	}
	if !strings.Contains(msg, "และอีก 20 รหัส") {
		t.Errorf("message must say how many were withheld, not trail off — a reader cannot tell whether theirs was in the part cut:\n  %s",
			msg)
	}
	if strings.Contains(msg, "1040") {
		t.Errorf("the list is not bounded; it printed all 40 codes:\n  %s", msg)
	}
}

func TestOwnerRefusalWhenNoAccountCouldEverBeOwner(t *testing.T) {
	// Every row lacks an id. "Pick one of these: (nothing)" is not an answer, so say what is
	// actually wrong with the file.
	rows := parseUsers(t, usersDump("('', '1010', 'a', '', 1)", "('', '1002', 'b', '', 1)"))
	msg := describeOwnerCandidates(rows)
	if !strings.Contains(msg, "ไม่มีบัญชีที่ใช้เป็น owner ได้") {
		t.Errorf("message should say the file has no usable account, not offer an empty list: %s", msg)
	}
}

// itoa keeps the fixture readable without pulling strconv into the test's imports.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// The import itself can only ever produce ONE owner. What it cannot see is what the
// target already holds, and because clinic_user is written insert-only (so a re-run
// never rewrites a membership an operator narrowed by hand) a clinic that arrived with
// extra owners keeps them.
//
// C9D4865 was found holding 19 owner memberships while its users screen printed 7 - the
// screen reads clinic_staff.role_id, the permission layer reads
// clinic_user.role_preset_id. Every one of those 19 could edit settings, void bills and
// manage users, and nobody noticed until somebody counted. So the script now counts, at
// the target, after the writes.
func TestClinicUserWarnsWhenTheTargetKeepsExtraOwners(t *testing.T) {
	out, _, _, err := BuildAll(Input{
		ClinicCode:    "MBOWN2",
		OwnerUsername: "admin",
		UsersDump:     usersFixture(),
	}, NewReport("MBOWN2"))
	if err != nil {
		t.Fatalf("BuildAll: %v", err)
	}
	sql := out[EntClinicUser]

	// Exactly one owner is written and the rest are counter_staff. This is the half the
	// tool controls.
	if n := strings.Count(sql, "key = 'owner'"); n < 2 {
		t.Errorf("the owner role is referenced %d time(s); want the insert plus the guard", n)
	}
	if !strings.Contains(sql, RoleKeyStaff) {
		t.Errorf("the other accounts are not written as %s", RoleKeyStaff)
	}

	// The guard reads the TARGET and warns rather than refusing: the import is correct,
	// and 44 clinics legitimately run on two owners today, so refusing would strand a
	// migration over somebody else's configuration.
	if !strings.Contains(sql, "RAISE WARNING") {
		t.Error("no warning is emitted when the target keeps more than one owner")
	}
	if strings.Contains(sql, "RAISE EXCEPTION 'คลินิก %% มีเจ้าของ") {
		t.Error("the owner count must warn, not refuse")
	}
	// It has to count the BUILT-IN owner role only: a clinic may define its own role and
	// call it anything, and role_presets.clinic_code IS NULL is what separates them.
	if !strings.Contains(sql, "rp.clinic_code IS NULL") {
		t.Error("the guard counts clinic-defined roles as owners too")
	}
	// The message has to explain why the screen disagrees, or the operator reads the
	// number as a bug in the tool.
	for _, want := range []string{"clinic_staff.role_id", "clinic_user.role_preset_id"} {
		if !strings.Contains(sql, want) {
			t.Errorf("the warning does not mention %s, so the screen/permission split stays hidden", want)
		}
	}
}

// The warning has to be CONDITIONAL. A guard that fires on every import becomes noise
// every operator learns to ignore, and then the one clinic with 19 owners scrolls past
// with everything else.
func TestClinicUserGuardOnlyWarnsAboveOneOwner(t *testing.T) {
	out, _, _, err := BuildAll(Input{
		ClinicCode:    "MBOWN3",
		OwnerUsername: "admin",
		UsersDump:     usersFixture(),
	}, NewReport("MBOWN3"))
	if err != nil {
		t.Fatalf("BuildAll: %v", err)
	}
	sql := out[EntClinicUser]
	// The guard is always present; what must be conditional is the warning it raises.
	if !strings.Contains(sql, "IF n > 1 THEN") {
		t.Error("the guard does not gate its warning on there being more than one owner")
	}
}
