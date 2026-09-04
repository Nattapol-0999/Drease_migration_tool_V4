// Package applysql runs a generated .sql artifact against a real database, over ssh.
//
// WHY SSH AND psql ON THE SERVER, not a Postgres driver here:
//   - nothing extra has to be installed on the operator's machine
//   - the database credentials never leave the server they belong to
//   - psql is the same thing a human would type, so what the tool does and what an
//     operator would do by hand are the same act — there is no second code path that
//     could behave differently on the day it matters
//
// The SQL is handed over on STDIN. It is never written to a file on the server and
// never placed in a command argument, so a dump full of patient names does not end up
// in /tmp or in anybody's process list.
package applysql

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Target is where an import goes. The field names match the environment variables the
// system_importV4 web UI already reads, so ONE file can configure both and there is no
// second place for the two to disagree about which database is "dev".
type Target struct {
	SSHHost string
	SSHUser string
	SSHPort string
	SSHKey  string

	DBHost     string
	DBPort     string
	DBName     string
	DBUser     string
	DBPassword string

	// DockerContainer is non-empty when Postgres runs inside docker on the server, in
	// which case psql is reached through `docker exec` instead of directly.
	DockerContainer string

	// Label is shown before the confirmation prompt ("dev", "prod"). It has no effect on
	// where anything goes — it exists so the operator can SEE where they are pointed
	// before they type the clinic code.
	Label string
}

// envKeys maps each setting to its variable name. Kept as data so the "what is missing"
// message can name the exact key the operator has to add.
var envKeys = struct {
	sshHost, sshUser, sshPort, sshKey      string
	dbHost, dbPort, dbName, dbUser, dbPass string
	dockerContainer, label                 string
}{
	sshHost: "IMPORT_SSH_HOST",
	sshUser: "IMPORT_SSH_USER",
	sshPort: "IMPORT_SSH_PORT",
	sshKey:  "IMPORT_SSH_KEY",

	dbHost: "IMPORT_DB_HOST",
	dbPort: "IMPORT_DB_PORT",
	dbName: "IMPORT_DB_NAME",
	dbUser: "IMPORT_DB_USER",
	dbPass: "IMPORT_DB_PASSWORD",

	dockerContainer: "IMPORT_DB_DOCKER_CONTAINER",
	label:           "IMPORT_TARGET_LABEL",
}

// ReadTarget builds a Target from an optional KEY=VALUE file plus the real environment.
//
// The file is read first and the process environment wins over it, which is the order
// that lets an operator keep a checked-in dev file and still redirect one run with a
// variable. A missing file is only an error when the caller ASKED for one: reading the
// environment alone is a valid way to configure this.
//
// missing lists the variables that still have no value; the caller refuses to run while
// it is non-empty, because a half-configured target is how a run lands somewhere nobody
// intended.
func ReadTarget(path string) (t Target, missing []string, err error) {
	fromFile := map[string]string{}
	if strings.TrimSpace(path) != "" {
		fromFile, err = parseEnvFile(path)
		if err != nil {
			return Target{}, nil, err
		}
	}

	get := func(key string) string {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		return strings.TrimSpace(fromFile[key])
	}
	// getRaw keeps an EXPLICIT empty value distinguishable from an absent one, which
	// matters for IMPORT_DB_DOCKER_CONTAINER: empty means "Postgres runs directly", and
	// that is a real answer, not a missing one.
	orDefault := func(key, def string) string {
		if v := get(key); v != "" {
			return v
		}
		return def
	}

	t = Target{
		SSHHost: get(envKeys.sshHost),
		SSHUser: orDefault(envKeys.sshUser, "ubuntu"),
		SSHPort: orDefault(envKeys.sshPort, "22"),
		SSHKey:  get(envKeys.sshKey),

		DBHost:     orDefault(envKeys.dbHost, "localhost"),
		DBPort:     orDefault(envKeys.dbPort, "5432"),
		DBName:     get(envKeys.dbName),
		DBUser:     orDefault(envKeys.dbUser, "postgres"),
		DBPassword: get(envKeys.dbPass),

		DockerContainer: get(envKeys.dockerContainer),
		Label:           get(envKeys.label),
	}

	if t.SSHHost == "" {
		missing = append(missing, envKeys.sshHost)
	}
	if t.SSHKey == "" {
		missing = append(missing, envKeys.sshKey)
	}
	if t.DBName == "" {
		missing = append(missing, envKeys.dbName)
	}
	if t.DBPassword == "" {
		missing = append(missing, envKeys.dbPass)
	}
	sort.Strings(missing)
	return t, missing, nil
}

// Describe is a short, NON-SECRET description for the confirmation prompt. The password
// is never part of it — this string is meant to be read aloud, pasted into a ticket and
// kept in a terminal scrollback.
func (t Target) Describe() string {
	where := fmt.Sprintf("%s@%s", t.SSHUser, t.SSHHost)
	if t.SSHPort != "" && t.SSHPort != "22" {
		where += ":" + t.SSHPort
	}
	db := fmt.Sprintf("%s @ %s:%s (user %s)", t.DBName, t.DBHost, t.DBPort, t.DBUser)
	if t.DockerContainer != "" {
		db += " ในคอนเทนเนอร์ " + t.DockerContainer
	}
	if t.Label != "" {
		return fmt.Sprintf("%s · %s · %s", t.Label, where, db)
	}
	return fmt.Sprintf("%s · %s", where, db)
}

// MissingHint turns the missing list into something an operator can act on, including
// where the web UI keeps the same values.
func MissingHint(missing []string) string {
	return fmt.Sprintf(
		"ยังตั้งค่าปลายทางไม่ครบ — ขาด %s\n"+
			"  ตั้งเป็น environment variable หรือใส่ในไฟล์ KEY=VALUE แล้วชี้ด้วย -target <ไฟล์>\n"+
			"  ถ้าเคยตั้งไว้ให้หน้าเว็บ system_importV4 แล้ว ใช้ไฟล์เดิมได้เลย: -target ..\\system_importV4\\.env.local",
		strings.Join(missing, ", "))
}

// parseEnvFile reads the KEY=VALUE shape a .env file uses: blank lines and # comments
// are skipped, an `export ` prefix is tolerated, and one layer of surrounding quotes is
// removed. Anything more (variable expansion, multi-line values) is deliberately NOT
// supported — this reads a settings file, it does not run a shell.
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("อ่านไฟล์ตั้งค่าปลายทาง %s ไม่ได้: %w", path, err)
	}
	defer f.Close()

	out := map[string]string{}
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		line := strings.TrimSpace(strings.TrimSuffix(s.Text(), "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if key != "" {
			out[key] = value
		}
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("อ่านไฟล์ตั้งค่าปลายทาง %s ไม่จบ: %w", path, err)
	}
	return out, nil
}
