package postgres

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	_ "github.com/lib/pq"

	photopicker "github.com/samrford/google-photos-picker"
)

// newTestDB connects to TEST_DATABASE_URL, resets the library's tables, and
// applies Migrate, so a broken migration fails here before any store assertion.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping %s: %v", dsn, err)
	}
	dropLibraryTables(t, db)
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate (00001 + destructive 00002): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// dropLibraryTables removes every table the library (and its Goose
// bookkeeping) creates, so a test can rebuild the schema from scratch by a
// different route.
func dropLibraryTables(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS photopicker_imports`,
		`DROP TABLE IF EXISTS photopicker_oauth_tokens`,
		`DROP TABLE IF EXISTS photopicker_schema_migrations`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("reset (%s): %v", stmt, err)
		}
	}
}

// introspectImportsColumns returns photopicker_imports' live column→data_type
// map straight from information_schema, or an empty map if the table is gone.
func introspectImportsColumns(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`
		SELECT column_name, data_type FROM information_schema.columns
		WHERE table_name = 'photopicker_imports'`)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	defer rows.Close()

	cols := map[string]string{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cols[name] = typ
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return cols
}

// TestMigration_SchemaShape pins the post-migration shape of
// photopicker_imports to exactly what the store code in imports.go reads and
// writes. newTestDB runs the full Migrate chain, so any future migration that
// drops, renames, retypes, or silently adds a column the store doesn't know
// about fails here with a precise message — not a cryptic scan error at
// runtime. (This also implicitly proves the destructive 00002 RENAME applied:
// image_urls is absent from the expected set, saved_ids + metadata are in it.)
func TestMigration_SchemaShape(t *testing.T) {
	db := newTestDB(t)

	// column name → information_schema.columns.data_type
	want := map[string]string{
		"id":              "character varying",
		"user_id":         "character varying",
		"session_id":      "text",
		"status":          "character varying",
		"total_items":     "integer",
		"completed_items": "integer",
		"failed_items":    "integer",
		"saved_ids":       "jsonb",
		"metadata":        "jsonb",
		"error":           "text",
		"created_at":      "timestamp with time zone",
		"updated_at":      "timestamp with time zone",
	}

	got := introspectImportsColumns(t, db)

	for name, wantType := range want {
		switch gotType, ok := got[name]; {
		case !ok:
			t.Errorf("column %q missing from migrated schema", name)
		case gotType != wantType:
			t.Errorf("column %q type = %q, want %q", name, gotType, wantType)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected column %q — store code in imports.go doesn't know about it; "+
				"update this test and the store together", name)
		}
	}
}

// TestSchemaUpSQL_MatchesMigrations guards the hand-maintained SchemaUpSQL /
// SchemaDownSQL constants (applied by non-Goose consumers) against the Goose
// migration chain. Both routes must produce an identical photopicker_imports
// shape, so a future migration that updates one path but forgets the other
// fails here instead of silently shipping a wrong "cumulative" constant.
func TestSchemaUpSQL_MatchesMigrations(t *testing.T) {
	db := newTestDB(t)
	migrated := introspectImportsColumns(t, db)

	// Rebuild the same database from the standalone constant and compare.
	dropLibraryTables(t, db)
	if _, err := db.Exec(SchemaUpSQL); err != nil {
		t.Fatalf("apply SchemaUpSQL: %v", err)
	}
	fromConst := introspectImportsColumns(t, db)

	if !reflect.DeepEqual(migrated, fromConst) {
		t.Errorf("SchemaUpSQL and migrations disagree on photopicker_imports:\n"+
			"  migrations: %v\n  SchemaUpSQL: %v", migrated, fromConst)
	}

	// SchemaDownSQL must actually tear the schema back down.
	if _, err := db.Exec(SchemaDownSQL); err != nil {
		t.Fatalf("apply SchemaDownSQL: %v", err)
	}
	if cols := introspectImportsColumns(t, db); len(cols) != 0 {
		t.Errorf("SchemaDownSQL left photopicker_imports behind: %v", cols)
	}
}

func TestPgImportStore_Lifecycle(t *testing.T) {
	db := newTestDB(t)
	store := NewImportStore(db)
	ctx := context.Background()
	meta := map[string]string{"item_id": "it-1", "project_id": "pr-1"}

	id, err := store.CreateJob(ctx, "u1", "sess-1", meta)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	job, err := store.ClaimNextPending(ctx)
	if err != nil {
		t.Fatalf("ClaimNextPending: %v", err)
	}
	if job == nil {
		t.Fatal("claim returned nil for a pending job")
	}
	if job.ID != id || job.UserID != "u1" || job.SessionID != "sess-1" {
		t.Fatalf("claimed job mismatch: %+v", job)
	}
	if job.Status != photopicker.ImportStatusRunning {
		t.Fatalf("claimed status = %q, want running", job.Status)
	}
	if job.Metadata["item_id"] != "it-1" || job.Metadata["project_id"] != "pr-1" {
		t.Fatalf("claimed metadata = %v", job.Metadata)
	}
	if len(job.SavedIDs) != 0 {
		t.Fatalf("fresh job SavedIDs = %v, want empty", job.SavedIDs)
	}

	if again, err := store.ClaimNextPending(ctx); err != nil || again != nil {
		t.Fatalf("second claim = (%+v, %v), want (nil, nil)", again, err)
	}

	if err := store.SetTotal(ctx, id, 3); err != nil {
		t.Fatalf("SetTotal: %v", err)
	}
	if err := store.RecordItemSuccess(ctx, id, "saved-a"); err != nil {
		t.Fatalf("RecordItemSuccess a: %v", err)
	}
	if err := store.RecordItemSuccess(ctx, id, "saved-b"); err != nil {
		t.Fatalf("RecordItemSuccess b: %v", err)
	}
	if err := store.RecordItemFailure(ctx, id); err != nil {
		t.Fatalf("RecordItemFailure: %v", err)
	}
	if err := store.MarkComplete(ctx, id); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}

	got, err := store.Get(ctx, "u1", id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != photopicker.ImportStatusComplete {
		t.Fatalf("status = %q, want complete", got.Status)
	}
	if got.TotalItems != 3 || got.CompletedItems != 2 || got.FailedItems != 1 {
		t.Fatalf("counts = total %d, completed %d, failed %d", got.TotalItems, got.CompletedItems, got.FailedItems)
	}
	if len(got.SavedIDs) != 2 || got.SavedIDs[0] != "saved-a" || got.SavedIDs[1] != "saved-b" {
		t.Fatalf("SavedIDs = %v, want [saved-a saved-b] in order", got.SavedIDs)
	}
	if got.Metadata["item_id"] != "it-1" || got.Metadata["project_id"] != "pr-1" {
		t.Fatalf("Get metadata = %v", got.Metadata)
	}

	// Terminal jobs are deleted on read — a second Get must miss.
	if _, err := store.Get(ctx, "u1", id); !errors.Is(err, photopicker.ErrJobNotFound) {
		t.Fatalf("second Get err = %v, want ErrJobNotFound", err)
	}
}

func TestPgImportStore_NilMetadataAndDefaults(t *testing.T) {
	db := newTestDB(t)
	store := NewImportStore(db)
	ctx := context.Background()

	id, err := store.CreateJob(ctx, "u2", "sess-2", nil)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	// Get on a non-terminal job returns it without deleting.
	got, err := store.Get(ctx, "u2", id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Metadata) != 0 {
		t.Fatalf("nil metadata stored as %v, want empty", got.Metadata)
	}
	if got.SavedIDs == nil || len(got.SavedIDs) != 0 {
		t.Fatalf("SavedIDs = %#v, want non-nil empty slice", got.SavedIDs)
	}

	if _, err := store.Get(ctx, "someone-else", id); !errors.Is(err, photopicker.ErrJobNotFound) {
		t.Fatalf("cross-user Get err = %v, want ErrJobNotFound", err)
	}
}

func TestPgImportStore_MarkFailed(t *testing.T) {
	db := newTestDB(t)
	store := NewImportStore(db)
	ctx := context.Background()

	id, err := store.CreateJob(ctx, "u3", "sess-3", nil)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := store.ClaimNextPending(ctx); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := store.MarkFailed(ctx, id, "boom"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	got, err := store.Get(ctx, "u3", id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != photopicker.ImportStatusFailed || got.Error != "boom" {
		t.Fatalf("got status=%q error=%q, want failed/boom", got.Status, got.Error)
	}
	if _, err := store.Get(ctx, "u3", id); !errors.Is(err, photopicker.ErrJobNotFound) {
		t.Fatalf("second Get err = %v, want ErrJobNotFound (terminal delete)", err)
	}
}

func TestPgImportStore_ClaimsOldestFirst(t *testing.T) {
	db := newTestDB(t)
	store := NewImportStore(db)
	ctx := context.Background()

	older, err := store.CreateJob(ctx, "u4", "s-older", nil)
	if err != nil {
		t.Fatalf("CreateJob older: %v", err)
	}
	newer, err := store.CreateJob(ctx, "u4", "s-newer", nil)
	if err != nil {
		t.Fatalf("CreateJob newer: %v", err)
	}

	// Pin created_at explicitly rather than sleeping between inserts: claim
	// order is then decided by the data, not by how fast the inserts ran.
	if _, err := db.ExecContext(ctx,
		`UPDATE photopicker_imports SET created_at = $2 WHERE id = $1`,
		older, time.Unix(1_000, 0)); err != nil {
		t.Fatalf("pin older created_at: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE photopicker_imports SET created_at = $2 WHERE id = $1`,
		newer, time.Unix(2_000, 0)); err != nil {
		t.Fatalf("pin newer created_at: %v", err)
	}

	j1, err := store.ClaimNextPending(ctx)
	if err != nil || j1 == nil {
		t.Fatalf("claim 1 = (%+v, %v)", j1, err)
	}
	j2, err := store.ClaimNextPending(ctx)
	if err != nil || j2 == nil {
		t.Fatalf("claim 2 = (%+v, %v)", j2, err)
	}
	if j1.ID != older || j2.ID != newer {
		t.Fatalf("claim order = [%s, %s], want [%s, %s]", j1.ID, j2.ID, older, newer)
	}
	if j3, err := store.ClaimNextPending(ctx); err != nil || j3 != nil {
		t.Fatalf("claim 3 = (%+v, %v), want (nil, nil)", j3, err)
	}
}
