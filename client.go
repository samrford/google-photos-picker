package photopicker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// DefaultDownloadCap is the per-photo byte cap applied when Config.DownloadCap
// is zero.
const DefaultDownloadCap = 50 << 20

// DefaultMaxDecodedBytes is the per-photo decoded-pixel-buffer ceiling used
// when Config.MaxDecodedBytes is zero. Only relevant for sinks that decode
// images in memory.
const DefaultMaxDecodedBytes = 200 << 20

// revokeURL is the Google token-revocation endpoint. Exposed as a var so tests
// can point it at an httptest.Server.
var revokeURL = "https://oauth2.googleapis.com/revoke"

// Config is the constructor input for Client.
//
// OAuth, TokenStore, ImportStore, and Sink are required.
//
// Optional:
//   - DownloadCap: per-photo byte ceiling (default DefaultDownloadCap).
//   - MaxDecodedBytes: per-photo decoded-pixel-buffer ceiling, computed as
//     width × height × 4. This is only relevant for sinks that decode images in memory.
//     Photos beyond this return ErrPhotoTooLarge before reaching SavePhoto.
//     Default DefaultMaxDecodedBytes; set explicitly to a negative value to disable
//     the check.
//   - HTTPClient: overrides http.DefaultClient for outgoing Google API calls.
//   - Logger: structured logger; defaults to slog.Default().
//   - Clock: overridable clock; defaults to time.Now.
//
// WARNING: *oauth2.Config is held by reference. Do not mutate it after passing
// it to New.
type Config struct {
	OAuth       *oauth2.Config
	TokenStore  TokenStore
	ImportStore ImportStore
	Sink        PhotoSink

	DownloadCap     int64
	MaxDecodedBytes int64
	HTTPClient      *http.Client
	Logger          *slog.Logger
	Clock           func() time.Time
}

// Client is the low-level programmatic API. Methods are safe for concurrent
// use unless noted otherwise.
type Client struct {
	oauth           *oauth2.Config
	tokens          TokenStore
	imports         ImportStore
	sink            PhotoSink
	downloadCap     int64
	maxDecodedBytes int64
	httpClient      *http.Client
	logger          *slog.Logger
	now             func() time.Time

	state *stateStore
}

// New validates cfg and constructs a Client.
func New(cfg Config) (*Client, error) {
	if cfg.OAuth == nil {
		return nil, fmt.Errorf("%w: OAuth is required", ErrInvalidConfig)
	}
	if cfg.TokenStore == nil {
		return nil, fmt.Errorf("%w: TokenStore is required", ErrInvalidConfig)
	}
	if cfg.ImportStore == nil {
		return nil, fmt.Errorf("%w: ImportStore is required", ErrInvalidConfig)
	}
	if cfg.Sink == nil {
		return nil, fmt.Errorf("%w: Sink is required", ErrInvalidConfig)
	}

	c := &Client{
		oauth:           cfg.OAuth,
		tokens:          cfg.TokenStore,
		imports:         cfg.ImportStore,
		sink:            cfg.Sink,
		downloadCap:     cfg.DownloadCap,
		maxDecodedBytes: cfg.MaxDecodedBytes,
		httpClient:      cfg.HTTPClient,
		logger:          cfg.Logger,
		now:             cfg.Clock,
		state:           newStateStore(),
	}
	if c.downloadCap == 0 {
		c.downloadCap = DefaultDownloadCap
	}
	if c.maxDecodedBytes == 0 {
		c.maxDecodedBytes = DefaultMaxDecodedBytes
	}
	if c.httpClient == nil {
		c.httpClient = http.DefaultClient
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// Close releases background resources (the state-store sweeper). Idempotent.
func (c *Client) Close() {
	c.state.Close()
}

// PickerSession is the result of CreatePickerSession.
type PickerSession struct {
	SessionID string
	PickerURI string
}

// PickerSessionStatus is the result of PollPickerSession.
type PickerSessionStatus struct {
	Ready bool
}

// ─── OAuth ──────────────────────────────────────────────────────────────────

// ConsentURL issues a new short-lived state parameter bound to userID and
// returns the consent URL the frontend should open in a popup.
func (c *Client) ConsentURL(_ context.Context, userID string) (consentURL, state string, err error) {
	state, err = c.state.create(userID)
	if err != nil {
		return "", "", err
	}
	consentURL = c.oauth.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce,
	)
	return consentURL, state, nil
}

// CompleteConsent exchanges the authorization code for tokens and persists
// them under the userID originally bound to state. Returns ErrInvalidState if
// the state is unknown or expired.
func (c *Client) CompleteConsent(ctx context.Context, state, code string) (string, error) {
	userID, ok := c.state.consume(state)
	if !ok {
		return "", ErrInvalidState
	}
	tok, err := c.oauth.Exchange(ctx, code)
	if err != nil {
		return "", fmt.Errorf("oauth exchange: %w", err)
	}
	rec := TokenRecord{
		UserID:       userID,
		RefreshToken: tok.RefreshToken,
		AccessToken:  tok.AccessToken,
		ExpiresAt:    tok.Expiry,
		Scopes:       grantedScopes(tok, c.oauth.Scopes),
	}
	if err := c.tokens.Save(ctx, rec); err != nil {
		return "", fmt.Errorf("save tokens: %w", err)
	}
	return userID, nil
}

// Status reports whether the user has connected Google Photos.
func (c *Client) Status(ctx context.Context, userID string) (TokenStatus, error) {
	return c.tokens.Status(ctx, userID)
}

// Disconnect deletes the user's tokens and best-effort revokes the refresh
// token at Google. Revocation runs on a detached context with a 10s timeout
// so shutdown and tests are bounded.
func (c *Client) Disconnect(ctx context.Context, userID string) error {
	refresh, err := c.tokens.Delete(ctx, userID)
	if errors.Is(err, ErrNoTokens) {
		return nil
	}
	if err != nil {
		return err
	}
	if refresh != "" {
		go c.revokeAtGoogle(refresh)
	}
	return nil
}

func (c *Client) revokeAtGoogle(token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body := strings.NewReader(url.Values{"token": {token}}.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, revokeURL, body)
	if err != nil {
		c.logger.Warn("photopicker: build revoke request", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Warn("photopicker: revoke request failed", "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		c.logger.Warn("photopicker: revoke returned non-2xx", "status", resp.StatusCode)
	}
}

// ─── Access-token refresh loop ─────────────────────────────────────────────

// accessToken returns a valid non-expired access token for userID, refreshing
// via the oauth2 TokenSource if needed and persisting the fresh access token
// back to the TokenStore.
func (c *Client) accessToken(ctx context.Context, userID string) (string, error) {
	rec, err := c.tokens.Load(ctx, userID)
	if errors.Is(err, ErrNoTokens) {
		return "", ErrNotConnected
	}
	if err != nil {
		return "", err
	}
	if rec.AccessToken != "" && !rec.ExpiresAt.IsZero() && rec.ExpiresAt.Sub(c.now()) > 60*time.Second {
		return rec.AccessToken, nil
	}
	src := c.oauth.TokenSource(ctx, &oauth2.Token{RefreshToken: rec.RefreshToken})
	fresh, err := src.Token()
	if err != nil {
		return "", fmt.Errorf("refresh token: %w", err)
	}
	if err := c.tokens.UpdateAccess(ctx, userID, fresh.AccessToken, fresh.Expiry); err != nil {
		return "", fmt.Errorf("persist refreshed token: %w", err)
	}
	return fresh.AccessToken, nil
}

func (c *Client) authorizer() authorizer {
	return func(ctx context.Context, userID string) (string, error) {
		return c.accessToken(ctx, userID)
	}
}

// ─── Picker sessions ───────────────────────────────────────────────────────

// CreatePickerSession creates a new picking session with Google.
func (c *Client) CreatePickerSession(ctx context.Context, userID string) (PickerSession, error) {
	var sess pickerSession
	err := googleJSON(ctx, c.httpClient, c.authorizer(), userID, http.MethodPost, photosPickerAPIBase+"/sessions", []byte(`{}`), &sess)
	if err != nil {
		return PickerSession{}, mapNotConnected(err)
	}
	return PickerSession{SessionID: sess.ID, PickerURI: sess.PickerURI}, nil
}

// PollPickerSession reports whether the user has finished picking.
func (c *Client) PollPickerSession(ctx context.Context, userID, sessionID string) (PickerSessionStatus, error) {
	var sess pickerSession
	err := googleJSON(ctx, c.httpClient, c.authorizer(), userID, http.MethodGet, photosPickerAPIBase+"/sessions/"+sessionID, nil, &sess)
	if err != nil {
		return PickerSessionStatus{}, mapNotConnected(err)
	}
	return PickerSessionStatus{Ready: sess.MediaItemsSet}, nil
}

// ─── Imports ───────────────────────────────────────────────────────────────

// StartImport creates a pending import job for a session. The background
// Worker (see NewWorker) picks it up and drives it to completion.
func (c *Client) StartImport(ctx context.Context, userID, sessionID string) (string, error) {
	return c.imports.CreateJob(ctx, userID, sessionID)
}

// GetImport returns a job scoped to its owning user. Implementations of
// ImportStore are expected to delete the job on terminal read — the frontend
// only needs it to survive long enough for one final poll.
func (c *Client) GetImport(ctx context.Context, userID, jobID string) (*ImportJob, error) {
	return c.imports.Get(ctx, userID, jobID)
}

// grantedScopes returns the scopes actually granted by Google (from the token
// response's "scope" field) falling back to the requested scopes when absent.
func grantedScopes(tok *oauth2.Token, requested []string) []string {
	if s, ok := tok.Extra("scope").(string); ok && s != "" {
		return strings.Fields(s)
	}
	return append([]string(nil), requested...)
}

// mapNotConnected surfaces ErrNotConnected when the underlying cause is a
// missing-tokens condition.
func mapNotConnected(err error) error {
	if errors.Is(err, ErrNoTokens) || errors.Is(err, ErrNotConnected) {
		return ErrNotConnected
	}
	return err
}
