package photopicker

import (
	"context"
	"time"
)

// TokenRecord is the set of fields the library persists per connected user.
// Implementations MUST encrypt RefreshToken and AccessToken at rest.
type TokenRecord struct {
	UserID       string
	RefreshToken string
	AccessToken  string
	ExpiresAt    time.Time
	Scopes       []string
}

// TokenStatus is a minimal, frontend-friendly view of a user's token state.
type TokenStatus struct {
	Connected bool
	Scopes    []string
}

// TokenStore persists per-user Google OAuth tokens.
//
// Save must preserve the existing refresh token when rec.RefreshToken is
// empty (Google only returns one on first consent unless prompt=consent).
// Delete returns the user's refresh token so callers can revoke it upstream,
// and returns ErrNoTokens if the user isn't connected.
type TokenStore interface {
	Save(ctx context.Context, rec TokenRecord) error
	Load(ctx context.Context, userID string) (TokenRecord, error)
	UpdateAccess(ctx context.Context, userID, accessToken string, expiresAt time.Time) error
	Status(ctx context.Context, userID string) (TokenStatus, error)
	Delete(ctx context.Context, userID string) (refreshToken string, err error)
}

// ImportStatus is the lifecycle of an import job.
type ImportStatus string

const (
	ImportStatusPending  ImportStatus = "pending"
	ImportStatusRunning  ImportStatus = "running"
	ImportStatusComplete ImportStatus = "complete"
	ImportStatusFailed   ImportStatus = "failed"
)

// ImportJob is the public shape of an import, suitable for JSON encoding in
// HTTP responses. The `-` tag on UserID/SessionID keeps them out of public
// payloads.
type ImportJob struct {
	ID             string       `json:"id"`
	UserID         string       `json:"-"`
	SessionID      string       `json:"-"`
	Status         ImportStatus `json:"status"`
	TotalItems     int          `json:"total"`
	CompletedItems int          `json:"completed"`
	FailedItems    int          `json:"failed"`
	ImageURLs      []string     `json:"imageUrls"`
	Error          string       `json:"error,omitempty"`
}

// ImportStore persists import jobs and their lifecycle.
//
// ClaimNextPending must atomically mark the oldest pending job as running and
// return it; (nil, nil) means no work is available. Get returns a terminal
// job once and is expected to delete it afterwards — terminal jobs only need
// to survive long enough for one final poll.
type ImportStore interface {
	CreateJob(ctx context.Context, userID, sessionID string) (jobID string, err error)
	ClaimNextPending(ctx context.Context) (*ImportJob, error)
	SetTotal(ctx context.Context, jobID string, total int) error
	RecordItemSuccess(ctx context.Context, jobID, savedID string) error
	RecordItemFailure(ctx context.Context, jobID string) error
	MarkComplete(ctx context.Context, jobID string) error
	MarkFailed(ctx context.Context, jobID, errMsg string) error
	Get(ctx context.Context, userID, jobID string) (*ImportJob, error)
}
