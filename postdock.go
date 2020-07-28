// postdock package runs db-related commands either inside a docker container
// or pulls and runs them inside a docker container. Example: postgres-11.8-alpine.
// All docker commands are run with --rm, which means they are removed after exit.
//
// FYI, some functions use postgres as a database name. This is intentional since
// the database your're trying to access may not exist yet. postgres is the default
// database before other databases have been created. As a consumer of this package,
// the dbName _your_ database.
//
// Note, this package constructs raw queries from the Options struct and passes them to
// psql or pg_dump. It is unlikely you will expose this outside your system, but be warned
// about the usage of fmt.Sprintf. If you're unsure what this means, please read about
// prepared statements and sql injection.
package postdock

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/bitfield/script"
)

var (
	ErrDBNotExist = errors.New("db does not exists")
)

type Options struct {
	DockerImage   string
	DockerNetwork string
	dockerVolume  string

	DBName     string
	DBHost     string
	DBUser     string
	DBPassword string

	Debug bool
}

func (o Options) isValid(dbName string) error {
	if dbName == "" {
		return errors.New("postdock: required option: db name")
	}

	if o.DBHost == "" {
		return errors.New("postdock: required option: db host")
	}
	if o.DBUser == "" {
		return errors.New("postdock: required option: db user")
	}
	if o.DBPassword == "" {
		return errors.New("postdock: required option: db password")
	}

	if o.DockerImage == "" {
		return errors.New("postdock: required option: docker base image (ex: postgres:11.7-alpine")
	}

	return nil
}

func Create(dbName string, opt Options) error {
	if err := opt.isValid(dbName); err != nil {
		return err
	}

	q := fmt.Sprintf("SELECT EXISTS ( SELECT usename FROM pg_catalog.pg_user WHERE usename = '%s');", opt.DBUser)
	cmd := psql("postgres", q, opt)
	out, err := run(cmd, opt)
	if err != nil {
		return err
	}
	exists, err := strconv.ParseBool(out)
	if err != nil {
		return err
	}
	if !exists {
		q = fmt.Sprintf("CREATE USER %s WITH PASSWORD '%s';", opt.DBUser, opt.DBPassword)
		cmd := psql("postgres", q, opt)
		out, err := run(cmd, opt)
		if err != nil {
			return err
		}
		log.Printf("[%s]: successfully created user:%s", out, opt.DBUser)
	}

	// Only continue creating a DB if one does not already exists, but do not fail otherwise, this function
	// should be idempotent.
	if err := Exists(dbName, opt); err == nil {
		log.Printf("skipping creating existing database:%s", dbName)
		return nil
	}

	q = fmt.Sprintf("CREATE DATABASE %s ENCODING 'UTF-8' LC_COLLATE='en_US.UTF-8' LC_CTYPE='en_US.UTF-8' TEMPLATE template0 OWNER %s;",
		dbName, opt.DBUser)
	cmd = psql("postgres", q, opt)
	out, err = run(cmd, opt)
	if err != nil {
		return err
	}
	log.Printf("[%s]: successfully created database:%s", out, dbName)

	// Yes, we could do this in one shot. But let's keep it readable and extensible. Hit the DB 4 times and
	// log each command out.
	queries := []string{
		"GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO %s",
		"GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public TO %s",
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL PRIVILEGES ON TABLES TO %s",
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL PRIVILEGES ON SEQUENCES TO %s",
	}

	for _, in := range queries {
		q := fmt.Sprintf(in, opt.DBUser)
		cmd := psql(dbName, q, opt)
		out, err = run(cmd, opt)
		if err != nil {
			return err
		}
		log.Printf("[%s]: successfully applied PRIVILEGES to user:%s on db:%s", out, opt.DBUser, dbName)
	}

	return nil
}

func Exists(dbName string, opt Options) error {
	if err := opt.isValid(dbName); err != nil {
		return err
	}

	q := fmt.Sprintf("SELECT EXISTS ( SELECT datname FROM pg_database WHERE datname = '%s')", dbName)
	cmd := psql("postgres", q, opt)
	out, err := run(cmd, opt)
	if err != nil {
		return err
	}
	exists, err := strconv.ParseBool(out)
	if err != nil {
		return err
	}
	if exists {
		log.Printf("skipping creating db:%s exists", dbName)
		return nil
	}

	return fmt.Errorf("%s: %w", dbName, ErrDBNotExist)
}

func Terminate(dbName string, opt Options) error {
	if err := opt.isValid(dbName); err != nil {
		return err
	}

	q := fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s';", dbName)
	cmd := psql("postgres", q, opt)
	out, err := run(cmd, opt)
	if err != nil {
		return err
	}

	log.Printf("[%s]: terminate db:%s errors:%v", out, dbName, err)

	return nil
}

func Drop(dbName string, opt Options) error {
	if err := Terminate(dbName, opt); err != nil {
		return err
	}

	q := fmt.Sprintf("DROP DATABASE IF EXISTS %s;", dbName)
	cmd := psql("postgres", q, opt)
	out, err := run(cmd, opt)
	if err != nil {
		return err
	}

	log.Printf("[%s]: drop db:%s", out, dbName)

	return nil
}

// Import from a sql file, where file must be relative to the current
// working directory. Exmaple, sql file can be of the format:
// data/schema/schema.sql, /data/schema/schema.sql or ./data/schema/schema.sql
func Import(dbName string, sqlFile string, opt Options) error {
	if sqlFile == "" {
		return errors.New("required option: sql file to import")
	}

	// terminate is called by drop.

	if err := Drop(dbName, opt); err != nil {
		return err
	}
	if err := Create(dbName, opt); err != nil {
		return err
	}

	file := strings.TrimPrefix(sqlFile, ".")
	file = strings.TrimPrefix(file, "/")
	dir, _ := filepath.Split(file)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	opt.dockerVolume = fmt.Sprintf("%s:/%s", absDir, dir)

	// As far as the container or psql is concerned, sqlFile is just a
	// path to a file. The docker volume ensure the file makes
	// it into the container.
	cmd := psqlFile(dbName, sqlFile, opt)
	out, err := run(cmd, opt)
	if err != nil {
		return err
	}

	log.Printf("[%s]: successfully imported into db:%s from file:%s",
		out, dbName, sqlFile)

	return nil
}

// SchemaDump does a schema-only only pg_dump, cleans out specific lines and
// writes the output to a file or returns the string. One or the other, but not both.
// If file is empty string, the output is returned to the caller.
func SchemaDump(dbName string, outputFile string, opt Options) (string, error) {
	if err := opt.isValid(dbName); err != nil {
		return "", err
	}

	cmd := fmt.Sprintf("pg_dump -h %s -p 5432 -U %s %s --schema-only",
		opt.DBHost, opt.DBUser, dbName)

	out, err := run(cmd, opt)
	if err != nil {
		return "", err
	}

	p := script.Echo(out).
		Reject(`ALTER DEFAULT PRIVILEGES`).
		Reject(`OWNER TO`).
		RejectRegexp(regexp.MustCompile(`^--`)).
		RejectRegexp(regexp.MustCompile(`^REVOKE`)).
		RejectRegexp(regexp.MustCompile(`^COMMENT ON`)).
		RejectRegexp(regexp.MustCompile(`^SET`)).
		RejectRegexp(regexp.MustCompile(`^GRANT`)).Exec("cat -s")

	if outputFile != "" {
		if _, err := p.WriteFile(outputFile); err != nil {
			return "", err
		}
		// TODO(mf): ye we could return the string as well.
		return "", nil
	}

	return p.String()
}

func inDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return false
}

// psql is a helper function that takes a sql query and builds a psql
// command against the given database. It can be passed directly to run.
func psql(dbName string, query string, o Options) string {
	return fmt.Sprintf("psql -h %s -d %s -U %s -p 5432 -v ON_ERROR_STOP=1 -t -c %q",
		o.DBHost, dbName, o.DBUser, query)
}

func psqlFile(dbName string, fileName string, o Options) string {
	return fmt.Sprintf("psql -h %s -d %s -U %s -p 5432 -v ON_ERROR_STOP=1 --file=%s",
		o.DBHost, dbName, o.DBUser, fileName)
}

func run(cmd string, o Options) (string, error) {
	// Inside a docker container we expect the command name to be available.
	if inDocker() {
		s, err := script.Exec(cmd).String()
		if err != nil {
			return "", err
		}

		return strings.TrimSpace(s), nil
	}

	// Pull the image silently.
	if err := dockerPull(o.DockerImage); err != nil {
		return "", err
	}

	var network string
	if o.DockerNetwork != "" {
		network = fmt.Sprintf("--network=%s", o.DockerNetwork)
	}
	var vol string
	if o.dockerVolume != "" {
		vol = fmt.Sprintf("--volume %s", o.dockerVolume)
	}
	// docker run [OPTIONS] IMAGE [COMMAND] [ARG...]
	e := fmt.Sprintf("docker run --rm %s %s %s sh -c %q",
		network, vol, o.DockerImage, cmd)

	if o.Debug {
		log.Printf("raw docker command:\n%s", e)
	}

	exec := script.Exec(e)
	n := exec.ExitStatus()
	if n > 0 {
		exec.SetError(nil)
		out, _ := exec.String()
		return "", fmt.Errorf("raw: %s", out)
	}

	out, err := exec.String()
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(out), nil
}

func dockerPull(imageName string) error {
	p := script.Exec("docker pull -q " + imageName)
	if p.ExitStatus() > 0 {
		p.SetError(nil)
		out, _ := p.String()
		return fmt.Errorf("raw: %s", out)
	}

	return nil
}
