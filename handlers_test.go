package photopicker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func newTestHandlers(t *testing.T, uid string, resolveErr error) (*Handlers, *fakeTokenStore, *fakeImportStore) {
	t.Helper()
	c, ts, is, _ := newTestClient(t)
	h, err := NewHandlers(HandlersConfig{
		Client: c,
		ResolveUserID: func(*http.Request) (string, error) {
			if resolveErr != nil {
				return "", resolveErr
			}
			return uid, nil
		},
	})
	if err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}
	return h, ts, is
}

func TestNewHandlers_Validation(t *testing.T) {
	if _, err := NewHandlers(HandlersConfig{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", err)
	}
	c, _, _, _ := newTestClient(t)
	if _, err := NewHandlers(HandlersConfig{Client: c}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal("missing resolver should error")
	}
}

func TestConnect_ReturnsConsentURL(t *testing.T) {
	h, _, _ := newTestHandlers(t, "u", nil)
	w := httptest.NewRecorder()
	h.Connect()(w, httptest.NewRequest("GET", "/connect", nil))
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if !strings.Contains(body["consentUrl"], "state=") {
		t.Fatalf("no state in consentUrl: %s", body["consentUrl"])
	}
}

func TestConnect_ResolverErrorIs401(t *testing.T) {
	h, _, _ := newTestHandlers(t, "", errors.New("no auth"))
	w := httptest.NewRecorder()
	h.Connect()(w, httptest.NewRequest("GET", "/connect", nil))
	if w.Code != 401 {
		t.Fatalf("status %d", w.Code)
	}
}

func TestStatus_ConnectedReflectsStore(t *testing.T) {
	h, ts, _ := newTestHandlers(t, "u", nil)
	ts.records["u"] = TokenRecord{UserID: "u", RefreshToken: "r", Scopes: []string{"s1"}}

	w := httptest.NewRecorder()
	h.Status()(w, httptest.NewRequest("GET", "/status", nil))
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var body struct {
		Connected bool     `json:"connected"`
		Scopes    []string `json:"scopes"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if !body.Connected || len(body.Scopes) != 1 || body.Scopes[0] != "s1" {
		t.Fatalf("got %+v", body)
	}
}

func TestDisconnect_Returns204(t *testing.T) {
	h, ts, _ := newTestHandlers(t, "u", nil)
	ts.records["u"] = TokenRecord{UserID: "u"}

	w := httptest.NewRecorder()
	h.Disconnect()(w, httptest.NewRequest("DELETE", "/google", nil))
	if w.Code != 204 {
		t.Fatalf("status %d", w.Code)
	}
}

func TestCreateSession_NotConnected_Is428(t *testing.T) {
	h, _, _ := newTestHandlers(t, "ghost", nil)

	w := httptest.NewRecorder()
	h.CreateSession()(w, httptest.NewRequest("POST", "/sessions", nil))
	if w.Code != http.StatusPreconditionRequired {
		t.Fatalf("status %d, want 428", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"google_not_connected"`) {
		t.Fatalf("body = %q", w.Body.String())
	}
}

func TestCreateSession_HappyPath(t *testing.T) {
	srv := withFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"id":"sess-123","pickerUri":"https://g/pick"}`)
	})
	_ = srv

	h, ts, _ := newTestHandlers(t, "u", nil)
	ts.records["u"] = TokenRecord{UserID: "u", AccessToken: "a", ExpiresAt: time.Now().Add(time.Hour)}

	w := httptest.NewRecorder()
	h.CreateSession()(w, httptest.NewRequest("POST", "/sessions", nil))
	if w.Code != 200 {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["sessionId"] != "sess-123" || body["pickerUri"] != "https://g/pick" {
		t.Fatalf("body = %+v", body)
	}
}

func TestPollSession_ReadyAndPending(t *testing.T) {
	var ready bool
	srv := withFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		if ready {
			fmt.Fprintln(w, `{"id":"s","mediaItemsSet":true}`)
		} else {
			fmt.Fprintln(w, `{"id":"s"}`)
		}
	})
	_ = srv

	h, ts, _ := newTestHandlers(t, "u", nil)
	ts.records["u"] = TokenRecord{UserID: "u", AccessToken: "a", ExpiresAt: time.Now().Add(time.Hour)}
	extract := func(*http.Request) string { return "s" }

	w := httptest.NewRecorder()
	h.PollSession(extract)(w, httptest.NewRequest("GET", "/s", nil))
	if !strings.Contains(w.Body.String(), `"pending"`) {
		t.Fatalf("got %s", w.Body.String())
	}

	ready = true
	w = httptest.NewRecorder()
	h.PollSession(extract)(w, httptest.NewRequest("GET", "/s", nil))
	if !strings.Contains(w.Body.String(), `"ready"`) {
		t.Fatalf("got %s", w.Body.String())
	}
}

func TestStartGetImport_RoundTrip(t *testing.T) {
	h, _, _ := newTestHandlers(t, "u", nil)
	extractSID := func(*http.Request) string { return "sess-x" }

	w := httptest.NewRecorder()
	h.StartImport(extractSID)(w, httptest.NewRequest("POST", "/import", nil))
	if w.Code != 200 {
		t.Fatalf("start status %d", w.Code)
	}
	var sb map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &sb)
	jobID := sb["importJobId"]
	if jobID == "" {
		t.Fatal("no job id")
	}

	extractJID := func(*http.Request) string { return jobID }
	w = httptest.NewRecorder()
	h.GetImport(extractJID)(w, httptest.NewRequest("GET", "/get", nil))
	if w.Code != 200 {
		t.Fatalf("get status %d", w.Code)
	}

	// unknown job → 404
	w = httptest.NewRecorder()
	h.GetImport(func(*http.Request) string { return "nope" })(w, httptest.NewRequest("GET", "/get", nil))
	if w.Code != 404 {
		t.Fatalf("want 404 for unknown, got %d", w.Code)
	}
}

func TestCallback_SuccessAndErrors(t *testing.T) {
	// Fake Google token endpoint.
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"A","refresh_token":"R","token_type":"Bearer","expires_in":3600}`)
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

	h, err := NewHandlers(HandlersConfig{Client: c, ResolveUserID: func(*http.Request) (string, error) { return "u", nil }})
	if err != nil {
		t.Fatalf("handlers: %v", err)
	}

	// happy path: issue state via client, exchange via callback.
	_, state, _ := c.ConsentURL(context.Background(), "u")
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/cb?state="+state+"&code=abc", nil)
	h.Callback()(w, r)
	if !strings.Contains(w.Body.String(), `"success"`) {
		t.Fatalf("body: %s", w.Body.String())
	}

	// Google-reported error
	w = httptest.NewRecorder()
	h.Callback()(w, httptest.NewRequest("GET", "/cb?error=access_denied", nil))
	if !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("body: %s", w.Body.String())
	}

	// missing code/state
	w = httptest.NewRecorder()
	h.Callback()(w, httptest.NewRequest("GET", "/cb", nil))
	if !strings.Contains(w.Body.String(), "Missing code or state") {
		t.Fatalf("body: %s", w.Body.String())
	}
}
