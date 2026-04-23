package photopicker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// ─── fake stores ───────────────────────────────────────────────────────────

type fakeTokenStore struct {
	mu      sync.Mutex
	records map[string]TokenRecord
	loadErr error
}

func newFakeTokenStore() *fakeTokenStore {
	return &fakeTokenStore{records: make(map[string]TokenRecord)}
}

func (f *fakeTokenStore) Save(_ context.Context, rec TokenRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	existing := f.records[rec.UserID]
	if rec.RefreshToken == "" {
		rec.RefreshToken = existing.RefreshToken
	}
	f.records[rec.UserID] = rec
	return nil
}
func (f *fakeTokenStore) Load(_ context.Context, userID string) (TokenRecord, error) {
	if f.loadErr != nil {
		return TokenRecord{}, f.loadErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.records[userID]
	if !ok {
		return TokenRecord{}, ErrNoTokens
	}
	return r, nil
}
func (f *fakeTokenStore) UpdateAccess(_ context.Context, userID, access string, expires time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.records[userID]
	r.AccessToken = access
	r.ExpiresAt = expires
	f.records[userID] = r
	return nil
}
func (f *fakeTokenStore) Status(_ context.Context, userID string) (TokenStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.records[userID]
	if !ok {
		return TokenStatus{Connected: false}, nil
	}
	return TokenStatus{Connected: true, Scopes: r.Scopes}, nil
}
func (f *fakeTokenStore) Delete(_ context.Context, userID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.records[userID]
	if !ok {
		return "", ErrNoTokens
	}
	delete(f.records, userID)
	return r.RefreshToken, nil
}

type fakeImportStore struct {
	mu   sync.Mutex
	jobs map[string]*ImportJob
	next int
}

func newFakeImportStore() *fakeImportStore {
	return &fakeImportStore{jobs: make(map[string]*ImportJob)}
}

func (f *fakeImportStore) CreateJob(_ context.Context, userID, sessionID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("job-%d", f.next)
	f.jobs[id] = &ImportJob{ID: id, UserID: userID, SessionID: sessionID, Status: ImportStatusPending, ImageURLs: []string{}}
	return id, nil
}
func (f *fakeImportStore) ClaimNextPending(_ context.Context) (*ImportJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, j := range f.jobs {
		if j.Status == ImportStatusPending {
			j.Status = ImportStatusRunning
			cp := *j
			return &cp, nil
		}
	}
	return nil, nil
}
func (f *fakeImportStore) SetTotal(_ context.Context, id string, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j, ok := f.jobs[id]; ok {
		j.TotalItems = n
	}
	return nil
}
func (f *fakeImportStore) RecordItemSuccess(_ context.Context, id, saved string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[id]
	j.CompletedItems++
	j.ImageURLs = append(j.ImageURLs, saved)
	return nil
}
func (f *fakeImportStore) RecordItemFailure(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[id].FailedItems++
	return nil
}
func (f *fakeImportStore) MarkComplete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[id].Status = ImportStatusComplete
	return nil
}
func (f *fakeImportStore) MarkFailed(_ context.Context, id, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[id].Status = ImportStatusFailed
	f.jobs[id].Error = msg
	return nil
}
func (f *fakeImportStore) Get(_ context.Context, userID, id string) (*ImportJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok || j.UserID != userID {
		return nil, ErrJobNotFound
	}
	cp := *j
	if j.Status == ImportStatusComplete || j.Status == ImportStatusFailed {
		delete(f.jobs, id)
	}
	return &cp, nil
}

type fakeSink struct {
	mu    sync.Mutex
	saved []DownloadedPhoto
}

func (s *fakeSink) SavePhoto(_ context.Context, _, _ string, p DownloadedPhoto) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, p)
	return "saved://" + p.GoogleMediaID, nil
}

// ─── helpers ───────────────────────────────────────────────────────────────

func newTestClient(t *testing.T) (*Client, *fakeTokenStore, *fakeImportStore, *fakeSink) {
	t.Helper()
	ts := newFakeTokenStore()
	is := newFakeImportStore()
	sk := &fakeSink{}
	c, err := New(Config{
		OAuth:       &oauth2.Config{ClientID: "id", ClientSecret: "s", RedirectURL: "http://x", Scopes: []string{GooglePhotosScope}, Endpoint: oauth2.Endpoint{AuthURL: "http://a", TokenURL: "http://t"}},
		TokenStore:  ts,
		ImportStore: is,
		Sink:        sk,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(c.Close)
	return c, ts, is, sk
}

// ─── config validation ────────────────────────────────────────────────────

func TestNew_MissingFields(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no oauth", Config{TokenStore: newFakeTokenStore(), ImportStore: newFakeImportStore(), Sink: &fakeSink{}}},
		{"no tokens", Config{OAuth: &oauth2.Config{}, ImportStore: newFakeImportStore(), Sink: &fakeSink{}}},
		{"no imports", Config{OAuth: &oauth2.Config{}, TokenStore: newFakeTokenStore(), Sink: &fakeSink{}}},
		{"no sink", Config{OAuth: &oauth2.Config{}, TokenStore: newFakeTokenStore(), ImportStore: newFakeImportStore()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("got %v, want ErrInvalidConfig", err)
			}
		})
	}
}

// ─── OAuth flow ────────────────────────────────────────────────────────────

func TestConsentURL_EmbedsState(t *testing.T) {
	c, _, _, _ := newTestClient(t)
	u, state, err := c.ConsentURL(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("ConsentURL: %v", err)
	}
	if state == "" || !strings.Contains(u, "state="+state) {
		t.Fatalf("state=%q not in url %q", state, u)
	}
	if !strings.Contains(u, "access_type=offline") {
		t.Fatalf("missing offline access: %s", u)
	}
}

func TestCompleteConsent_BadStateRejected(t *testing.T) {
	c, _, _, _ := newTestClient(t)
	_, err := c.CompleteConsent(context.Background(), "bogus", "code")
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("want ErrInvalidState, got %v", err)
	}
}

func TestCompleteConsent_SavesTokens(t *testing.T) {
	// Fake Google token endpoint.
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"A","refresh_token":"R","token_type":"Bearer","expires_in":3600}`)
	}))
	defer tokSrv.Close()

	ts := newFakeTokenStore()
	is := newFakeImportStore()
	c, err := New(Config{
		OAuth: &oauth2.Config{
			ClientID: "id", ClientSecret: "s", RedirectURL: "http://x",
			Scopes:   []string{GooglePhotosScope},
			Endpoint: oauth2.Endpoint{AuthURL: "http://a", TokenURL: tokSrv.URL},
		},
		TokenStore: ts, ImportStore: is, Sink: &fakeSink{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	_, state, _ := c.ConsentURL(context.Background(), "user-42")
	uid, err := c.CompleteConsent(context.Background(), state, "authcode")
	if err != nil {
		t.Fatalf("CompleteConsent: %v", err)
	}
	if uid != "user-42" {
		t.Fatalf("uid = %q", uid)
	}
	rec, _ := ts.Load(context.Background(), "user-42")
	if rec.AccessToken != "A" || rec.RefreshToken != "R" {
		t.Fatalf("token not saved: %+v", rec)
	}
}

// ─── status / disconnect ───────────────────────────────────────────────────

func TestStatus_Disconnected(t *testing.T) {
	c, _, _, _ := newTestClient(t)
	st, err := c.Status(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Connected {
		t.Fatal("should be disconnected")
	}
}

func TestDisconnect_NoopWhenMissing(t *testing.T) {
	c, _, _, _ := newTestClient(t)
	if err := c.Disconnect(context.Background(), "nobody"); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
}

func TestDisconnect_DeletesAndRevokes(t *testing.T) {
	revoked := make(chan string, 1)
	revSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		revoked <- r.FormValue("token")
	}))
	defer revSrv.Close()

	prev := revokeURL
	revokeURL = revSrv.URL
	defer func() { revokeURL = prev }()

	c, ts, _, _ := newTestClient(t)
	ts.records["u"] = TokenRecord{UserID: "u", RefreshToken: "rt", Scopes: []string{GooglePhotosScope}}
	if err := c.Disconnect(context.Background(), "u"); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if _, ok := ts.records["u"]; ok {
		t.Fatal("tokens still present after Disconnect")
	}
	select {
	case tok := <-revoked:
		if tok != "rt" {
			t.Fatalf("revoked token = %q, want %q", tok, "rt")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revoke request never received")
	}
}

// ─── access-token refresh ──────────────────────────────────────────────────

func TestAccessToken_ReturnsNonExpired(t *testing.T) {
	c, ts, _, _ := newTestClient(t)
	now := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return now }
	ts.records["u"] = TokenRecord{UserID: "u", AccessToken: "still-good", ExpiresAt: now.Add(2 * time.Minute), RefreshToken: "r"}

	tok, err := c.accessToken(context.Background(), "u")
	if err != nil {
		t.Fatalf("accessToken: %v", err)
	}
	if tok != "still-good" {
		t.Fatalf("got %q", tok)
	}
}

func TestAccessToken_RefreshesExpired(t *testing.T) {
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"NEW","token_type":"Bearer","expires_in":3600}`)
	}))
	defer tokSrv.Close()

	ts := newFakeTokenStore()
	c, err := New(Config{
		OAuth: &oauth2.Config{
			ClientID: "id", ClientSecret: "s",
			Endpoint: oauth2.Endpoint{AuthURL: "http://a", TokenURL: tokSrv.URL},
		},
		TokenStore: ts, ImportStore: newFakeImportStore(), Sink: &fakeSink{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	now := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return now }
	ts.records["u"] = TokenRecord{UserID: "u", AccessToken: "old", RefreshToken: "rt", ExpiresAt: now.Add(-time.Hour)}

	tok, err := c.accessToken(context.Background(), "u")
	if err != nil {
		t.Fatalf("accessToken: %v", err)
	}
	if tok != "NEW" {
		t.Fatalf("got %q", tok)
	}
	if ts.records["u"].AccessToken != "NEW" {
		t.Fatal("fresh access not persisted")
	}
}

func TestAccessToken_NoTokensMapsToNotConnected(t *testing.T) {
	c, _, _, _ := newTestClient(t)
	_, err := c.accessToken(context.Background(), "ghost")
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("got %v, want ErrNotConnected", err)
	}
}

// ─── session and imports plumbed through ───────────────────────────────────

func TestCreatePickerSession_HappyPath(t *testing.T) {
	srv := withFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"id":"s1","pickerUri":"https://g/pick"}`)
	})
	_ = srv

	c, ts, _, _ := newTestClient(t)
	ts.records["u"] = TokenRecord{UserID: "u", AccessToken: "a", ExpiresAt: time.Now().Add(time.Hour)}

	s, err := c.CreatePickerSession(context.Background(), "u")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if s.SessionID != "s1" || s.PickerURI != "https://g/pick" {
		t.Fatalf("got %+v", s)
	}
}

func TestCreatePickerSession_NotConnected(t *testing.T) {
	c, _, _, _ := newTestClient(t)
	_, err := c.CreatePickerSession(context.Background(), "ghost")
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("want ErrNotConnected, got %v", err)
	}
}

func TestStartGetImport(t *testing.T) {
	c, _, is, _ := newTestClient(t)
	id, err := c.StartImport(context.Background(), "u", "sess")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	job, err := c.GetImport(context.Background(), "u", id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job.ID != id {
		t.Fatalf("id mismatch")
	}
	if _, err := c.GetImport(context.Background(), "other", id); !errors.Is(err, ErrJobNotFound) {
		t.Fatal("expected ErrJobNotFound for wrong user")
	}
	_ = is
}
