package photopicker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// DefaultMaxMetadataBytes / DefaultMaxMetadataKeys bound the optional
// StartImport metadata so the import row can't be abused as a blob store via
// the public (untrusted) handler path. Used when HandlersConfig leaves the
// corresponding field <= 0. Server-derived metadata passed straight to
// Client.StartImport is trusted and not bound by these.
const (
	DefaultMaxMetadataBytes = 4 << 10
	DefaultMaxMetadataKeys  = 32
)

// UserIDResolver extracts the authenticated user ID from a request. Consumers
// typically close over their own auth-middleware context helper:
//
//	ResolveUserID: func(r *http.Request) (string, error) {
//	    uid := myauth.UserID(r.Context())
//	    if uid == "" { return "", errors.New("unauthenticated") }
//	    return uid, nil
//	}
//
// The Callback endpoint does not call the resolver: Google redirects the
// user's browser there directly, without a Bearer token.
type UserIDResolver func(*http.Request) (string, error)

// HandlersConfig is the constructor input for NewHandlers.
//
// MaxMetadataBytes / MaxMetadataKeys cap client-supplied StartImport metadata
// (the untrusted body path only). Each defaults to its Default* constant when
// left <= 0.
type HandlersConfig struct {
	Client           *Client
	ResolveUserID    UserIDResolver
	Callback         CallbackPage
	MaxMetadataBytes int
	MaxMetadataKeys  int
}

// Handlers mounts the library's HTTP surface. Each method returns a plain
// http.HandlerFunc that consumers wire into their existing router with
// whatever middleware (auth, CORS, method-matching) they like.
type Handlers struct {
	client           *Client
	resolve          UserIDResolver
	callback         CallbackPage
	maxMetadataBytes int
	maxMetadataKeys  int
}

// NewHandlers builds a *Handlers. Client and ResolveUserID are required.
func NewHandlers(cfg HandlersConfig) (*Handlers, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("%w: Client is required", ErrInvalidConfig)
	}
	if cfg.ResolveUserID == nil {
		return nil, fmt.Errorf("%w: ResolveUserID is required", ErrInvalidConfig)
	}
	h := &Handlers{
		client:           cfg.Client,
		resolve:          cfg.ResolveUserID,
		callback:         cfg.Callback,
		maxMetadataBytes: cfg.MaxMetadataBytes,
		maxMetadataKeys:  cfg.MaxMetadataKeys,
	}
	if h.maxMetadataBytes <= 0 {
		h.maxMetadataBytes = DefaultMaxMetadataBytes
	}
	if h.maxMetadataKeys <= 0 {
		h.maxMetadataKeys = DefaultMaxMetadataKeys
	}
	return h, nil
}

// Connect returns an OAuth consent URL as {"consentUrl": "..."}.
func (h *Handlers) Connect() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, err := h.resolve(r)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		u, _, err := h.client.ConsentURL(r.Context(), userID)
		if err != nil {
			http.Error(w, "failed to create state", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"consentUrl": u})
	}
}

// Callback handles Google's redirect and renders the postMessage page.
func (h *Handlers) Callback() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errParam := q.Get("error"); errParam != "" {
			renderCallback(w, h.callback, false, "Google consent was cancelled or failed: "+errParam)
			return
		}
		code, state := q.Get("code"), q.Get("state")
		if code == "" || state == "" {
			renderCallback(w, h.callback, false, "Missing code or state in callback.")
			return
		}
		if _, err := h.client.CompleteConsent(r.Context(), state, code); err != nil {
			if errors.Is(err, ErrInvalidState) {
				renderCallback(w, h.callback, false, "Invalid or expired state.")
				return
			}
			h.client.logger.Warn("photopicker: complete consent", "err", err)
			renderCallback(w, h.callback, false, "Token exchange failed.")
			return
		}
		renderCallback(w, h.callback, true, "")
	}
}

// Status returns {"connected": bool, "scopes": []string}.
func (h *Handlers) Status() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, err := h.resolve(r)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		st, err := h.client.Status(r.Context(), userID)
		if err != nil {
			http.Error(w, "failed to look up status", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"connected": st.Connected, "scopes": st.Scopes})
	}
}

// Disconnect deletes the user's tokens. Returns 204 on success.
func (h *Handlers) Disconnect() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, err := h.resolve(r)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err := h.client.Disconnect(r.Context(), userID); err != nil {
			http.Error(w, "failed to disconnect", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// CreateSession creates a new picker session and returns {"sessionId", "pickerUri"}.
func (h *Handlers) CreateSession() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, err := h.resolve(r)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		sess, err := h.client.CreatePickerSession(r.Context(), userID)
		if err != nil {
			writeGoogleError(w, h.client, err, "create picker session")
			return
		}
		writeJSON(w, map[string]string{"sessionId": sess.SessionID, "pickerUri": sess.PickerURI})
	}
}

// PollSession polls a session's state. extractSessionID pulls the session ID
// from the request (from the URL path, typically).
func (h *Handlers) PollSession(extractSessionID func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, err := h.resolve(r)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		sid := extractSessionID(r)
		if sid == "" {
			http.Error(w, "missing session id", http.StatusBadRequest)
			return
		}
		st, err := h.client.PollPickerSession(r.Context(), userID, sid)
		if err != nil {
			writeGoogleError(w, h.client, err, "poll session")
			return
		}
		writeJSON(w, map[string]string{"status": string(st.Phase)})
	}
}

// StartImport kicks off an async import job and returns {"importJobId": "..."}.
//
// An optional JSON body {"metadata": {"k":"v"}} attaches opaque caller context
// to the job (surfaced to the sink as DownloadedPhoto.JobMetadata). Apps that
// derive the destination server-side should ignore this path and call
// Client.StartImport directly with trusted metadata instead.
func (h *Handlers) StartImport(extractSessionID func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, err := h.resolve(r)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		sid := extractSessionID(r)
		if sid == "" {
			http.Error(w, "missing session id", http.StatusBadRequest)
			return
		}
		meta, err := h.decodeStartImportMetadata(r)
		if err != nil {
			http.Error(w, `{"error":"invalid metadata"}`, http.StatusBadRequest)
			return
		}
		jobID, err := h.client.StartImport(r.Context(), userID, sid, meta)
		if err != nil {
			http.Error(w, "failed to create import job", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"importJobId": jobID})
	}
}

// decodeStartImportMetadata reads the optional {"metadata":{...}} body. An
// absent or empty body yields (nil, nil) — metadata is opt-in. Oversized
// bodies or too many keys are rejected (per the configured caps) so the
// import row stays bounded.
func (h *Handlers) decodeStartImportMetadata(r *http.Request) (map[string]string, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(h.maxMetadataBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, nil
	}
	if len(body) > h.maxMetadataBytes {
		return nil, fmt.Errorf("metadata exceeds %d bytes", h.maxMetadataBytes)
	}
	var envelope struct {
		Metadata map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.Metadata) > h.maxMetadataKeys {
		return nil, fmt.Errorf("metadata exceeds %d keys", h.maxMetadataKeys)
	}
	return envelope.Metadata, nil
}

// GetImport reports job progress.
func (h *Handlers) GetImport(extractJobID func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, err := h.resolve(r)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		jid := extractJobID(r)
		if jid == "" {
			http.Error(w, "missing job id", http.StatusBadRequest)
			return
		}
		job, err := h.client.GetImport(r.Context(), userID, jid)
		if errors.Is(err, ErrJobNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "failed to look up job", http.StatusInternalServerError)
			return
		}
		writeJSON(w, job)
	}
}

// writeGoogleError maps library errors to HTTP status codes:
//   - ErrNotConnected → 428 Precondition Required with {"error":"google_not_connected"}
//   - anything else   → 502 Bad Gateway (upstream Google failure)
func writeGoogleError(w http.ResponseWriter, c *Client, err error, stage string) {
	switch {
	case errors.Is(err, ErrNotConnected):
		http.Error(w, `{"error":"google_not_connected"}`, http.StatusPreconditionRequired)
	default:
		c.logger.Warn("photopicker: "+stage, "err", err)
		http.Error(w, "failed to talk to Google", http.StatusBadGateway)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
