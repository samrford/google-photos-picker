package photopicker

import (
	"sync"
	"testing"
	"time"
)

func TestStateStore_CreateConsume(t *testing.T) {
	s := newStateStore()
	defer s.Close()

	state, err := s.create("user-42")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if state == "" {
		t.Fatal("empty state")
	}

	uid, ok := s.consume(state)
	if !ok || uid != "user-42" {
		t.Fatalf("consume: got (%q, %v)", uid, ok)
	}
}

func TestStateStore_ReplayFails(t *testing.T) {
	s := newStateStore()
	defer s.Close()

	state, _ := s.create("u")
	if _, ok := s.consume(state); !ok {
		t.Fatal("first consume failed")
	}
	if _, ok := s.consume(state); ok {
		t.Fatal("replay should fail")
	}
}

func TestStateStore_UnknownStateFails(t *testing.T) {
	s := newStateStore()
	defer s.Close()
	if _, ok := s.consume("nope"); ok {
		t.Fatal("unknown state should not consume")
	}
}

func TestStateStore_Expiry(t *testing.T) {
	s := newStateStore()
	defer s.Close()

	now := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return now }

	state, _ := s.create("u")
	now = now.Add(stateTTL + time.Second)

	if _, ok := s.consume(state); ok {
		t.Fatal("expired state should not consume")
	}
}

func TestStateStore_SweepRemovesExpired(t *testing.T) {
	s := newStateStore()
	defer s.Close()

	now := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return now }

	_, _ = s.create("u")
	now = now.Add(stateTTL + time.Second)
	s.sweep()

	s.mu.Lock()
	n := len(s.entries)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("sweep left %d entries", n)
	}
}

func TestStateStore_ConcurrentCreateConsume(t *testing.T) {
	s := newStateStore()
	defer s.Close()

	const N = 200
	states := make(chan string, N)

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := s.create("u")
			if err != nil {
				t.Errorf("create: %v", err)
				return
			}
			states <- st
		}()
	}
	wg.Wait()
	close(states)

	seen := 0
	for st := range states {
		if _, ok := s.consume(st); ok {
			seen++
		}
	}
	if seen != N {
		t.Fatalf("consumed %d of %d", seen, N)
	}
}

func TestNewOAuthConfig(t *testing.T) {
	cfg := NewOAuthConfig("id", "secret", "https://example.com/cb")
	if cfg.ClientID != "id" || cfg.ClientSecret != "secret" || cfg.RedirectURL != "https://example.com/cb" {
		t.Fatalf("fields wrong: %+v", cfg)
	}
	if len(cfg.Scopes) != 1 || cfg.Scopes[0] != GooglePhotosScope {
		t.Fatalf("scopes: %v", cfg.Scopes)
	}
	if cfg.Endpoint.AuthURL == "" {
		t.Fatal("endpoint not set")
	}
}
