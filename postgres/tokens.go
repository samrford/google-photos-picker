package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"

	photopicker "github.com/samrford/google-photos-picker"
	"github.com/samrford/google-photos-picker/internal/cryptobox"
)

// PgTokenStore is a Postgres-backed photopicker.TokenStore that encrypts
// refresh and access tokens at rest with AES-256-GCM.
type PgTokenStore struct {
	db  *sql.DB
	box *cryptobox.Box
}

// NewTokenStore builds a PgTokenStore. encryptionKeyHex must be a 32-byte key
// encoded as hex (64 hex chars); generate one with `openssl rand -hex 32`.
func NewTokenStore(db *sql.DB, encryptionKeyHex string) (*PgTokenStore, error) {
	box, err := cryptobox.NewFromHex(encryptionKeyHex)
	if err != nil {
		return nil, err
	}
	return &PgTokenStore{db: db, box: box}, nil
}

// Save upserts a user's tokens. If rec.RefreshToken is empty the existing
// refresh token is preserved (Google only returns a refresh token on first
// consent unless prompt=consent).
func (s *PgTokenStore) Save(ctx context.Context, rec photopicker.TokenRecord) error {
	accessCT, err := s.box.Seal(rec.AccessToken)
	if err != nil {
		return err
	}
	var expiry any
	if !rec.ExpiresAt.IsZero() {
		expiry = rec.ExpiresAt
	}

	if rec.RefreshToken == "" {
		_, err = s.db.ExecContext(ctx, `
			UPDATE photopicker_oauth_tokens
			SET access_token = $2, expires_at = $3, scopes = $4, updated_at = NOW()
			WHERE user_id = $1
		`, rec.UserID, accessCT, expiry, pq.Array(rec.Scopes))
		return err
	}

	refreshCT, err := s.box.Seal(rec.RefreshToken)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO photopicker_oauth_tokens (user_id, refresh_token, access_token, expires_at, scopes)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id) DO UPDATE SET
			refresh_token = EXCLUDED.refresh_token,
			access_token  = EXCLUDED.access_token,
			expires_at    = EXCLUDED.expires_at,
			scopes        = EXCLUDED.scopes,
			updated_at    = NOW()
	`, rec.UserID, refreshCT, accessCT, expiry, pq.Array(rec.Scopes))
	return err
}

// Load returns the user's TokenRecord with plaintext tokens restored, or
// photopicker.ErrNoTokens if the user isn't connected.
func (s *PgTokenStore) Load(ctx context.Context, userID string) (photopicker.TokenRecord, error) {
	var (
		refreshCT []byte
		accessCT  []byte
		expires   sql.NullTime
		scopes    []string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT refresh_token, access_token, expires_at, scopes
		FROM photopicker_oauth_tokens WHERE user_id = $1
	`, userID).Scan(&refreshCT, &accessCT, &expires, pq.Array(&scopes))
	if errors.Is(err, sql.ErrNoRows) {
		return photopicker.TokenRecord{}, photopicker.ErrNoTokens
	}
	if err != nil {
		return photopicker.TokenRecord{}, err
	}
	access, err := s.box.Open(accessCT)
	if err != nil {
		return photopicker.TokenRecord{}, fmt.Errorf("decrypt access token: %w", err)
	}
	refresh, err := s.box.Open(refreshCT)
	if err != nil {
		return photopicker.TokenRecord{}, fmt.Errorf("decrypt refresh token: %w", err)
	}
	rec := photopicker.TokenRecord{
		UserID:       userID,
		RefreshToken: refresh,
		AccessToken:  access,
		Scopes:       scopes,
	}
	if expires.Valid {
		rec.ExpiresAt = expires.Time
	}
	return rec, nil
}

// UpdateAccess persists just the access_token/expires_at fields after a silent
// refresh, leaving the refresh_token untouched.
func (s *PgTokenStore) UpdateAccess(ctx context.Context, userID, accessToken string, expiresAt time.Time) error {
	accessCT, err := s.box.Seal(accessToken)
	if err != nil {
		return err
	}
	var expiry any
	if !expiresAt.IsZero() {
		expiry = expiresAt
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE photopicker_oauth_tokens
		SET access_token = $2, expires_at = $3, updated_at = NOW()
		WHERE user_id = $1
	`, userID, accessCT, expiry)
	return err
}

// Status reports whether the user has tokens and which scopes they granted.
func (s *PgTokenStore) Status(ctx context.Context, userID string) (photopicker.TokenStatus, error) {
	var scopes []string
	err := s.db.QueryRowContext(ctx, `
		SELECT scopes FROM photopicker_oauth_tokens WHERE user_id = $1
	`, userID).Scan(pq.Array(&scopes))
	if errors.Is(err, sql.ErrNoRows) {
		return photopicker.TokenStatus{Connected: false}, nil
	}
	if err != nil {
		return photopicker.TokenStatus{}, err
	}
	return photopicker.TokenStatus{Connected: true, Scopes: scopes}, nil
}

// Delete removes a user's tokens and returns the plaintext refresh token so
// callers can revoke it upstream. Returns photopicker.ErrNoTokens if there
// was nothing to delete.
func (s *PgTokenStore) Delete(ctx context.Context, userID string) (string, error) {
	var refreshCT []byte
	err := s.db.QueryRowContext(ctx, `
		DELETE FROM photopicker_oauth_tokens WHERE user_id = $1 RETURNING refresh_token
	`, userID).Scan(&refreshCT)
	if errors.Is(err, sql.ErrNoRows) {
		return "", photopicker.ErrNoTokens
	}
	if err != nil {
		return "", err
	}
	return s.box.Open(refreshCT)
}
