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
