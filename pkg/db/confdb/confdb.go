package confdb

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	multierror "github.com/hashicorp/go-multierror"

	"github.com/keegancsmith/sqlf"
	"github.com/sourcegraph/jsonx"
	"github.com/sourcegraph/sourcegraph/pkg/db/dbconn"
)

// Config contains the contents of a critical/site config along with associated metadata.
type Config struct {
	ID        int32     // the unique ID of this config
	Type      string    // either "critical" or "site"
	Contents  string    // the raw JSON content (with comments and trailing commas allowed)
	CreatedAt time.Time // the date when this config was created
	UpdatedAt time.Time // the date when this config was updated
}

// SiteConfig contains the contents of a site config along with associated metadata.
type SiteConfig Config

// CriticalConfig contains the contents of a critical config along with associated metadata.
type CriticalConfig Config

// SiteCreateIfUpToDate saves the given site config "contents" to the database iff the
// supplied "lastID" is equal to the one that was most recently saved to the database.
//
// The site config that was most recently saved to the database is returned.
// An error is returned if "contents" is invalid JSON.
//
// 🚨 SECURITY: This method does NOT verify the user is an admin. The caller is
// responsible for ensuring this or that the response never makes it to a user.
func SiteCreateIfUpToDate(ctx context.Context, lastID *int32, contents string) (latest *SiteConfig, err error) {
	tx, done, err := newTransaction(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	newLastID, err := addDefault(ctx, tx, typeSite, defaultSiteConfig)
	if err != nil {
		return nil, err
	}
	if newLastID != nil {
		lastID = newLastID
	}

	criticalSite, err := createIfUpToDate(ctx, tx, typeSite, lastID, contents)
	return (*SiteConfig)(criticalSite), err
}

// CriticalCreateIfUpToDate saves the given critical config "contents" to the
// database iff the supplied "lastID" is equal to the one that was most
// recently saved to the database (i.e. SiteGetlatest's ID field).
//
// The critical config that was most recently saved to the database is returned.
// An error is returned if "contents" is invalid JSON.
//
// 🚨 SECURITY: This method does NOT verify the user is an admin. The caller is
// responsible for ensuring this or that the response never makes it to a user.
func CriticalCreateIfUpToDate(ctx context.Context, lastID *int32, contents string) (latest *CriticalConfig, err error) {
	tx, done, err := newTransaction(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	newLastID, err := addDefault(ctx, tx, typeCritical, defaultCriticalConfig)
	if err != nil {
		return nil, err
	}
	if newLastID != nil {
		lastID = newLastID
	}

	criticalSite, err := createIfUpToDate(ctx, tx, typeCritical, lastID, contents)
	return (*CriticalConfig)(criticalSite), err
}

// SiteGetLatest returns the site config that was most recently saved to the database.
// This returns nil, nil if there is not yet a site config in the database.
//
// 🚨 SECURITY: This method does NOT verify the user is an admin. The caller is
// responsible for ensuring this or that the response never makes it to a user.
func SiteGetLatest(ctx context.Context) (latest *SiteConfig, err error) {
	tx, done, err := newTransaction(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	_, err = addDefault(ctx, tx, typeSite, defaultSiteConfig)
	if err != nil {
		return nil, err
	}

	site, err := getLatest(ctx, tx, typeSite)
	return (*SiteConfig)(site), err
}

// CriticalGetLatest returns critical site config that was most recently saved to the database.
// This returns nil, nil if there is not yet a critical config in the database.
//
// 🚨 SECURITY: This method does NOT verify the user is an admin. The caller is
// responsible for ensuring this or that the response never makes it to a user.
func CriticalGetLatest(ctx context.Context) (latest *CriticalConfig, err error) {
	tx, done, err := newTransaction(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	_, err = addDefault(ctx, tx, typeCritical, defaultCriticalConfig)
	if err != nil {
		return nil, err
	}

	critical, err := getLatest(ctx, tx, typeCritical)
	return (*CriticalConfig)(critical), err
}

func newTransaction(ctx context.Context) (tx queryable, done func(), err error) {
	rtx, err := dbconn.Global.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}

	return rtx, func() {
		if err != nil {
			rollErr := rtx.Rollback()
			if rollErr != nil {
				err = multierror.Append(err, rollErr)
			}
			return
		}
		err = rtx.Commit()
	}, nil
}

func addDefault(ctx context.Context, tx queryable, configType configType, contents string) (newLastID *int32, err error) {
	latest, err := getLatest(ctx, tx, configType)
	if err != nil {
		return nil, err
	}
	if latest != nil {
		// We have an existing config!
		return nil, nil
	}

	// Create the default.
	latest, err = createIfUpToDate(ctx, tx, configType, nil, contents)
	if err != nil {
		return nil, err
	}
	return &latest.ID, nil
}

func createIfUpToDate(ctx context.Context, tx queryable, configType configType, lastID *int32, contents string) (latest *Config, err error) {
	// Validate JSON syntax before saving.
	if _, errs := jsonx.Parse(contents, jsonx.ParseOptions{Comments: true, TrailingCommas: true}); len(errs) > 0 {
		return nil, fmt.Errorf("invalid settings JSON: %v", errs)
	}

	new := Config{
		Contents: contents,
	}

	latest, err = getLatest(ctx, tx, configType)
	if err != nil {
		return nil, err
	}

	creatorIsUpToDate := latest != nil && lastID != nil && latest.ID == *lastID
	if latest == nil || creatorIsUpToDate {
		err := tx.QueryRowContext(
			ctx,
			"INSERT INTO critical_and_site_config(type, contents) VALUES($1, $2) RETURNING id, created_at, updated_at",
			configType, new.Contents,
		).Scan(&new.ID, &new.CreatedAt, &new.UpdatedAt)
		if err != nil {
			return nil, err
		}
		latest = &new
	}
	return latest, nil
}

func getLatest(ctx context.Context, tx queryable, configType configType) (*Config, error) {
	q := sqlf.Sprintf("SELECT s.id, s.type, s.contents, s.created_at, s.updated_at FROM critical_and_site_config s WHERE type=%s ORDER BY id DESC LIMIT 1", configType)
	rows, err := tx.QueryContext(ctx, q.Query(sqlf.PostgresBindVar), q.Args()...)
	if err != nil {
		return nil, err
	}
	versions, err := parseQueryRows(ctx, rows)
	if err != nil {
		return nil, err
	}
	if len(versions) != 1 {
		// No config has been written yet.
		return nil, nil
	}
	return versions[0], nil
}

func parseQueryRows(ctx context.Context, rows *sql.Rows) ([]*Config, error) {
	versions := []*Config{}
	defer rows.Close()
	for rows.Next() {
		f := Config{}
		err := rows.Scan(&f.ID, &f.Type, &f.Contents, &f.CreatedAt, &f.UpdatedAt)
		if err != nil {
			return nil, err
		}
		versions = append(versions, &f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return versions, nil
}

// queryable allows us to reuse the same logic for certain operations both
// inside and outside an explicit transaction.
type queryable interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

type configType string

const (
	typeCritical configType = "critical"
	typeSite     configType = "site"
)

// SetDefaultConfigs should be invoked once early on in the program
// startup, before calls to e.g. conf.Get are made. It will panic if called
// more than once.
func SetDefaultConfigs(critical, site string) {
	if setDefaultConfigsCalled {
		panic("confdb.SetDefaultConfigs may not be called twice")
	}
	setDefaultConfigsCalled = true
	defaultCriticalConfig = critical
	defaultSiteConfig = site
}

var (
	setDefaultConfigsCalled bool
	defaultCriticalConfig   string
	defaultSiteConfig       string
)
