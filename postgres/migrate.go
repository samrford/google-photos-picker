// Package postgres is a reference Postgres-backed implementation of
// photopicker's TokenStore and ImportStore interfaces. OAuth tokens are
// encrypted at rest with AES-256-GCM.
//
// Migration table collision: Migrate uses the bookkeeping table
// "photopicker_schema_migrations" so it won't collide with a consuming app's
// own Goose migrations on the same database. If you drive migrations yourself
// (Flyway, Alembic, hand-rolled), use MigrationsFS() or the SchemaUpSQL /
// SchemaDownSQL constants.
package postgres

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// MigrationsFS returns an fs.FS containing the library's Goose migration
// files, rooted at the "migrations" directory (e.g. "00001_schema.sql").
// Useful for consumers that drive Goose themselves.
func MigrationsFS() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		// embed.FS returning an error here would indicate a broken build, not
		// a runtime condition worth recovering from.
		panic(fmt.Errorf("photopicker/postgres: embed: %w", err))
	}
	return sub
}

// Migrate applies all pending library migrations to db using Goose, recording
// state in the dedicated table "photopicker_schema_migrations" so it cannot
// collide with a consuming app's own Goose migrations.
//
// Call Migrate before any other goose.Up invocations in your process, or
// serialise them. Goose uses process-global state internally, and concurrent
// calls from different packages can race.
func Migrate(db *sql.DB) error {
	goose.SetTableName("photopicker_schema_migrations")
	defer goose.SetTableName("goose_db_version") // restore library default
	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("photopicker/postgres: set dialect: %w", err)
	}
	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("photopicker/postgres: migrate: %w", err)
	}
	return nil
}
