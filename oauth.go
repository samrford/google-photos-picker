package photopicker

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"

	"golang.org/x/oauth2"
	googleoauth "golang.org/x/oauth2/google"
)

// GooglePhotosScope is the minimal read-only Picker-API scope required by the
// library. Consumers who need extra scopes can set OAuth.Scopes themselves.
const GooglePhotosScope = "https://www.googleapis.com/auth/photospicker.mediaitems.readonly"

// NewOAuthConfig returns a ready-to-use *oauth2.Config wired to Google's
// endpoint and requesting GooglePhotosScope.
func NewOAuthConfig(clientID, clientSecret, redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Scopes:       []string{GooglePhotosScope},
		Endpoint:     googleoauth.Endpoint,
	}
}

// stateTTL is how long a generated OAuth state parameter is valid.
const stateTTL = 10 * time.Minute

// stateEntry pairs a state parameter with the user it was issued to.
type stateEntry struct {
	userID    string
	expiresAt time.Time
}

// stateStore is a short-lived, in-memory, single-use CSRF state ↔ userID map.
// It is safe for concurrent use. A background goroutine sweeps expired entries
// every 5 minutes. Call Close when finished to stop the sweeper.
type stateStore struct {
	mu      sync.Mutex
	entries map[string]stateEntry
	now     func() time.Time
	stop    chan struct{}
}

func newStateStore() *stateStore {
	s := &stateStore{
		entries: make(map[string]stateEntry),
		now:     time.Now,
		stop:    make(chan struct{}),
	}
	go s.gcLoop()
	return s
}

func (s *stateStore) create(userID string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	state := base64.RawURLEncoding.EncodeToString(buf)
	s.mu.Lock()
	s.entries[state] = stateEntry{userID: userID, expiresAt: s.now().Add(stateTTL)}
	s.mu.Unlock()
	return state, nil
}

func (s *stateStore) consume(state string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[state]
	if !ok {
		return "", false
	}
	delete(s.entries, state)
	if s.now().After(e.expiresAt) {
		return "", false
	}
	return e.userID, true
}

func (s *stateStore) gcLoop() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.sweep()
		}
	}
}

func (s *stateStore) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, e := range s.entries {
		if now.After(e.expiresAt) {
			delete(s.entries, k)
		}
	}
}

// Close stops the background sweeper. Idempotent.
func (s *stateStore) Close() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
}
