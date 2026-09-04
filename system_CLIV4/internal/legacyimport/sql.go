package legacyimport

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Shared literal helpers. Every value that reaches Postgres goes through these, so
// quoting and width clipping are decided in exactly one place.

// lit renders a Postgres string literal, doubling embedded single quotes. Backslashes
// are left alone: standard_conforming_strings is on (default since PostgreSQL 9.1)
// and the importer sets it explicitly on its transaction, so a backslash in legacy
// text stays a backslash.
func lit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// nullLit renders NULL for an empty string, a quoted literal otherwise.
func nullLit(s string) string {
	if s == "" {
		return "NULL"
	}
	return lit(s)
}

// clip trims a value to a varchar(n) width by CHARACTER, which is what Postgres
// counts. Clipping bytes would split a Thai character and corrupt the text. max 0
// means unlimited (a text column).
func clip(s string, max int) (string, bool) {
	if max <= 0 {
		return s, false
	}
	r := []rune(s)
	if len(r) <= max {
		return s, false
	}
	return string(r[:max]), true
}

// legacyTS parses a legacy naive datetime as UTC. The dumps set
// time_zone = "+00:00", so the stored values are UTC despite carrying no offset.
func legacyTS(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// tsLit renders a legacy datetime as a UTC timestamptz literal, or the given
// fallback SQL expression when it is absent or unparseable.
func tsLit(s, fallback string) string {
	t, ok := legacyTS(s)
	if !ok {
		return fallback
	}
	return lit(t.UTC().Format("2006-01-02 15:04:05-07"))
}

// dateLit renders a legacy "DD/MM/YYYY" birthday as a date literal. Anything that
// does not parse becomes NULL and is reported by the caller.
func dateLit(s string) (string, bool) {
	if s == "" {
		return "NULL", true
	}
	t, err := time.Parse("02/01/2006", s)
	if err != nil {
		return "NULL", false
	}
	return lit(t.Format("2006-01-02")), true
}

// combineDateTime merges a legacy date ("2025-12-04") and clock ("09:00:00") into one
// UTC timestamptz literal. An empty clock counts as midnight.
func combineDateTime(date, clock string) (string, *time.Time, bool) {
	if date == "" {
		return "NULL", nil, false
	}
	if clock == "" {
		clock = "00:00:00"
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", date+" "+clock, time.UTC)
	if err != nil {
		return "NULL", nil, false
	}
	return lit(t.UTC().Format("2006-01-02 15:04:05-07")), &t, true
}

// intLit renders an integer literal, or NULL when the value is empty or not a number.
func intLit(s string) (string, bool) {
	if s == "" {
		return "NULL", true
	}
	if _, err := strconv.ParseInt(s, 10, 64); err != nil {
		return "NULL", false
	}
	return s, true
}

// numLit renders a legacy double as a TEXT literal (V4 stores weight/height as
// varchar). A value of 0 becomes NULL — see ZeroMeasurementToNull.
func numLit(s string) (string, bool) {
	if s == "" {
		return "NULL", true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return "NULL", false
	}
	if f == 0 && ZeroMeasurementToNull {
		return "NULL", true
	}
	return lit(strconv.FormatFloat(f, 'f', -1, 64)), true
}

// oneLine keeps generated SQL comments on a single line so embedded newlines from
// legacy notes cannot break out of a "--" comment.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.ReplaceAll(s, "\n", " ")
}

// sqlBuilder accumulates the statements for one entity.
type sqlBuilder struct {
	b     strings.Builder
	stmts int
}

func (s *sqlBuilder) addf(format string, args ...any) {
	fmt.Fprintf(&s.b, format, args...)
	s.stmts++
}

func (s *sqlBuilder) comment(format string, args ...any) {
	fmt.Fprintf(&s.b, "-- "+format+"\n", args...)
}

func (s *sqlBuilder) String() string { return s.b.String() }

// joinCols renders a column list for an INSERT.
func joinCols(cols []string) string { return strings.Join(cols, ", ") }
