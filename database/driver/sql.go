package driver

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/pghq/go-tea/trail"
	"github.com/pressly/goose/v3"

	"github.com/pghq/go-ark/database"
)

var (
	// migrationFile regex match
	migrationFile = regexp.MustCompile(`^(\d+)_.+\.sql$`)

	// seedFile regex match
	seedFile = regexp.MustCompile(`^(\d+).*$`)
)

// SQL database
type SQL struct {
	backend db
}

func (d SQL) Ping(ctx context.Context) error {
	return d.backend.Ping(ctx)
}

// NewSQL Create a new SQL database
func NewSQL(dialect string, databaseURL *url.URL, opts ...database.Option) (*SQL, error) {
	config := database.ConfigWith(opts)
	db := SQL{}
	var err error
	switch dialect {
	case "postgres", "redshift":
		db.backend, err = newPostgres(dialect, databaseURL, config)
	default:
		return nil, trail.NewError("unrecognized dialect")
	}

	if err != nil {
		return nil, trail.Stacktrace(err)
	}

	if config.MigrationFS != nil && config.MigrationDirectory != "" {
		err := applyMigration(
			isLocalhost(databaseURL.Host),
			db.backend.SQL(),
			config.MigrationFS,
			dialect,
			config.MigrationTable,
			config.MigrationDirectory,
			config.SeedDirectory,
		)

		if err != nil {
			return nil, trail.Stacktrace(err)
		}
	}

	return &db, nil
}

// applyMigration applies the migration and seeds data
func applyMigration(localhost bool, db *sql.DB, dir fs.ReadDirFS, dialect, migrationTable, migrationDirectory, seedDirectory string) error {
	goose.SetLogger(gooseLogger{})
	goose.SetBaseFS(dir)
	goose.SetTableName(migrationTable)
	_ = goose.SetDialect(dialect)

	entries, _ := dir.ReadDir(migrationDirectory)
	maxMigrationVersion := 0
	for _, entry := range entries {
		matches := migrationFile.FindStringSubmatch(entry.Name())
		if len(matches) > 0 {
			version, _ := strconv.Atoi(matches[1])
			if version > maxMigrationVersion {
				maxMigrationVersion = version
			}
		}
	}

	seeds := make(map[int]string)
	minSeedVersion := -1
	if localhost && seedDirectory != "" {
		entries, _ := dir.ReadDir(seedDirectory)
		for _, entry := range entries {
			if entry.IsDir() {
				matches := seedFile.FindStringSubmatch(entry.Name())
				if len(matches) > 0 {
					if version, err := strconv.Atoi(matches[1]); err == nil {
						seeds[version] = fmt.Sprintf("%s/%s",
							strings.TrimSuffix(seedDirectory, "/"),
							entry.Name(),
						)

						if minSeedVersion == -1 || version < minSeedVersion {
							minSeedVersion = version
						}
					}
				}
			}
		}
	}

	max := int(math.Max(float64(maxMigrationVersion), float64(len(seeds))))
	version, _ := goose.GetDBVersion(db)
	var err error
	if localhost {
		for i := 0; i < max; i++ {
			if err = goose.UpTo(db, migrationDirectory, int64(i+1)); err != nil && err != goose.ErrNoNextVersion {
				break
			}

			seedVersion := i + minSeedVersion
			if seed, present := seeds[seedVersion]; present && seedVersion > int(version) {
				if err = goose.Up(db, seed, goose.WithNoVersioning(), goose.WithAllowMissing()); err != nil && err != goose.ErrNoNextVersion {
					break
				}
			}
		}
	}

	if err == nil {
		err = goose.Up(db, migrationDirectory)
	}

	if err != nil && err != goose.ErrNoNextVersion {
		_ = goose.Down(db, migrationDirectory)
		for _, seed := range seeds {
			_ = goose.Down(db, seed)
		}
		return trail.Stacktrace(err)
	}

	return nil
}

// gooseLogger Custom goose logger implementation
type gooseLogger struct{}

func (g gooseLogger) Fatal(v ...interface{}) {
	trail.Fatal(fmt.Sprint(v...))
}

func (g gooseLogger) Fatalf(format string, v ...interface{}) {
	trail.Fatalf(format, v...)
}

func (g gooseLogger) Print(v ...interface{}) {
	trail.Info(fmt.Sprint(v...))
}

func (g gooseLogger) Println(v ...interface{}) {
	trail.Info(fmt.Sprint(v...))
}

func (g gooseLogger) Printf(format string, v ...interface{}) {
	trail.Infof(format, v...)
}

// placeholder placeholder prefix for replacing ?s
type placeholder string

func (ph placeholder) ReplacePlaceholders(sql string) (string, error) {
	if ph == "" || ph == "?" {
		return sql, nil
	}

	buf := &bytes.Buffer{}
	i := 0
	for {
		p := strings.Index(sql, "?")
		if p == -1 {
			break
		}

		if len(sql[p:]) > 1 && sql[p:p+2] == "??" {
			buf.WriteString(sql[:p])
			buf.WriteString("?")
			sql = sql[p+2:]
		} else {
			i++
			buf.WriteString(sql[:p])
			_, _ = fmt.Fprintf(buf, "%s%d", ph, i)
			sql = sql[p+1:]
		}
	}

	buf.WriteString(sql)
	return buf.String(), nil
}

type db interface {
	Ping(ctx context.Context) error
	Txn(ctx context.Context, opts *sql.TxOptions) (uow, error)
	SQL() *sql.DB
	URL() *url.URL
	placeholder() placeholder
}

func isLocalhost(host string) bool {
	hostPort := strings.Split(host, ":")
	host = hostPort[0]
	return host == "localhost" || host == "host.docker.internal" || host == "db"
}
