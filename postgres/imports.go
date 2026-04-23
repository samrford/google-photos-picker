package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/google/uuid"

	photopicker "github.com/samrford/google-photos-picker"
)

// PgImportStore is a Postgres-backed photopicker.ImportStore. Claims use
// FOR UPDATE SKIP LOCKED so multiple worker replicas can share a queue.
type PgImportStore struct {
	db *sql.DB
}

// NewImportStore builds a PgImportStore.
func NewImportStore(db *sql.DB) *PgImportStore {
	return &PgImportStore{db: db}
}

// CreateJob registers a new pending import job and returns its ID.
func (s *PgImportStore) CreateJob(ctx context.Context, userID, sessionID string) (string, error) {
	id := uuid.New().String()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO photopicker_imports (id, user_id, session_id, status, image_urls)
		VALUES ($1, $2, $3, 'pending', '[]')
	`, id, userID, sessionID)
	if err != nil {
		return "", err
	}
	return id, nil
}

// ClaimNextPending atomically marks the oldest pending job as running and
// returns it. (nil, nil) means the queue is empty.
func (s *PgImportStore) ClaimNextPending(ctx context.Context) (*photopicker.ImportJob, error) {
	row := s.db.QueryRowContext(ctx, `
		UPDATE photopicker_imports
		SET status = 'running', updated_at = NOW()
		WHERE id = (
			SELECT id FROM photopicker_imports
			WHERE status = 'pending'
			ORDER BY created_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, user_id, session_id, status, total_items, completed_items, failed_items
	`)
	var j photopicker.ImportJob
	err := row.Scan(&j.ID, &j.UserID, &j.SessionID, &j.Status, &j.TotalItems, &j.CompletedItems, &j.FailedItems)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.ImageURLs = []string{}
	return &j, nil
}

// SetTotal records how many items will be imported for a job.
func (s *PgImportStore) SetTotal(ctx context.Context, jobID string, total int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE photopicker_imports SET total_items = $2, updated_at = NOW()
		WHERE id = $1
	`, jobID, total)
	return err
}

// RecordItemSuccess appends a saved ID and bumps the success counter.
func (s *PgImportStore) RecordItemSuccess(ctx context.Context, jobID, savedID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE photopicker_imports
		SET completed_items = completed_items + 1,
		    image_urls      = image_urls || to_jsonb($2::text),
		    updated_at      = NOW()
		WHERE id = $1
	`, jobID, savedID)
	return err
}

// RecordItemFailure bumps the failure counter.
func (s *PgImportStore) RecordItemFailure(ctx context.Context, jobID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE photopicker_imports
		SET failed_items = failed_items + 1, updated_at = NOW()
		WHERE id = $1
	`, jobID)
	return err
}

// MarkComplete moves a job into the "complete" terminal state.
func (s *PgImportStore) MarkComplete(ctx context.Context, jobID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE photopicker_imports SET status = 'complete', updated_at = NOW()
		WHERE id = $1
	`, jobID)
	return err
}

// MarkFailed moves a job into the "failed" terminal state with an error msg.
func (s *PgImportStore) MarkFailed(ctx context.Context, jobID, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE photopicker_imports SET status = 'failed', error = $2, updated_at = NOW()
		WHERE id = $1
	`, jobID, errMsg)
	return err
}

// Get returns a job scoped to its owning user. Terminal jobs are deleted after
// being read — they only need to survive long enough for one final poll.
func (s *PgImportStore) Get(ctx context.Context, userID, jobID string) (*photopicker.ImportJob, error) {
	var (
		j            photopicker.ImportJob
		imageURLsRaw []byte
		errStr       sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, session_id, status, total_items, completed_items, failed_items, image_urls, error
		FROM photopicker_imports WHERE id = $1 AND user_id = $2
	`, jobID, userID).Scan(&j.ID, &j.UserID, &j.SessionID, &j.Status,
		&j.TotalItems, &j.CompletedItems, &j.FailedItems, &imageURLsRaw, &errStr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, photopicker.ErrJobNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(imageURLsRaw) > 0 {
		_ = json.Unmarshal(imageURLsRaw, &j.ImageURLs)
	}
	if j.ImageURLs == nil {
		j.ImageURLs = []string{}
	}
	if errStr.Valid {
		j.Error = errStr.String
	}
	if j.Status == photopicker.ImportStatusComplete || j.Status == photopicker.ImportStatusFailed {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM photopicker_imports WHERE id = $1 AND user_id = $2`, jobID, userID)
	}
	return &j, nil
}
