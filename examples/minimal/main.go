// Command minimal is a runnable reference server that demonstrates the full
// photopicker library end-to-end: OAuth, picker sessions, import worker, and
// an in-memory sink that saves downloaded photos to a local directory.
//
// Configuration is via environment variables:
//
//	GOOGLE_CLIENT_ID        – Google OAuth client ID
//	GOOGLE_CLIENT_SECRET    – Google OAuth client secret
//	GOOGLE_REDIRECT_URL     – OAuth callback URL (e.g. http://localhost:8080/callback)
//	GOOGLE_ENCRYPTION_KEY   – 32-byte hex key for token encryption (openssl rand -hex 32)
//	POSTGRES_DSN            – Postgres connection string
//	LISTEN_ADDR             – address to listen on (default :8080)
//
// Run:
//
//	export GOOGLE_CLIENT_ID=... GOOGLE_CLIENT_SECRET=... GOOGLE_REDIRECT_URL=http://localhost:8080/callback
//	export GOOGLE_ENCRYPTION_KEY=$(openssl rand -hex 32)
//	export POSTGRES_DSN="postgres://user:pass@localhost:5432/photopicker?sslmode=disable"
//	go run ./examples/minimal
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"

	photopicker "github.com/samrford/google-photos-picker"
	ppg "github.com/samrford/google-photos-picker/postgres"
)

// localSink saves downloaded photos to a local directory and returns the
// relative file path as the savedID. This is the simplest possible PhotoSink
// — production consumers would typically upload to S3, GCS, or a CMS.
type localSink struct {
	dir string
}

func (s *localSink) SavePhoto(_ context.Context, userID, jobID string, p photopicker.DownloadedPhoto) (string, error) {
	ext := filepath.Ext(p.Filename)
	if ext == "" {
		ext = extFromMime(p.MimeType)
	}
	name := uuid.New().String() + ext

	// Create user-scoped subdirectory.
	userDir := filepath.Join(s.dir, userID)
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		return "", fmt.Errorf("create dir: %w", err)
	}

	dst := filepath.Join(userDir, name)
	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, p.Bytes); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}

	slog.Info("saved photo", "user", userID, "job", jobID, "file", dst, "size", p.Size)
	return dst, nil
}

func extFromMime(mime string) string {
	switch {
	case strings.Contains(mime, "jpeg"), strings.Contains(mime, "jpg"):
		return ".jpg"
	case strings.Contains(mime, "png"):
		return ".png"
	case strings.Contains(mime, "gif"):
		return ".gif"
	case strings.Contains(mime, "webp"):
		return ".webp"
	case strings.Contains(mime, "heic"):
		return ".heic"
	default:
		return ".jpg"
	}
}

func main() {
	clientID := requireEnv("GOOGLE_CLIENT_ID")
	clientSecret := requireEnv("GOOGLE_CLIENT_SECRET")
	redirectURL := requireEnv("GOOGLE_REDIRECT_URL")
	encKey := requireEnv("GOOGLE_ENCRYPTION_KEY")
	dsn := requireEnv("POSTGRES_DSN")
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	// ── Database ────────────────────────────────────────────────────────
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("ping db: %v", err)
	}

	if err := ppg.Migrate(db); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	slog.Info("database migrations applied")

	// ── Stores & sink ──────────────────────────────────────────────────
	tokenStore, err := ppg.NewTokenStore(db, encKey)
	if err != nil {
		log.Fatalf("token store: %v", err)
	}
	importStore := ppg.NewImportStore(db)

	photosDir := "./photos"
	if err := os.MkdirAll(photosDir, 0o755); err != nil {
		log.Fatalf("create photos dir: %v", err)
	}
	sink := &localSink{dir: photosDir}

	// ── Client ─────────────────────────────────────────────────────────
	client, err := photopicker.New(photopicker.Config{
		OAuth:       photopicker.NewOAuthConfig(clientID, clientSecret, redirectURL),
		TokenStore:  tokenStore,
		ImportStore: importStore,
		Sink:        sink,
	})
	if err != nil {
		log.Fatalf("photopicker client: %v", err)
	}
	defer client.Close()

	// ── Handlers ───────────────────────────────────────────────────────

	// For this minimal example the "user ID" is a query param ?user=<id>.
	// A real app would extract it from an auth token / session cookie.
	resolveUser := func(r *http.Request) (string, error) {
		uid := r.URL.Query().Get("user")
		if uid == "" {
			return "", fmt.Errorf("missing ?user= parameter")
		}
		return uid, nil
	}

	h, err := photopicker.NewHandlers(photopicker.HandlersConfig{
		Client:        client,
		ResolveUserID: resolveUser,
		Callback: photopicker.CallbackPage{
			TargetOrigin: "*",
		},
	})
	if err != nil {
		log.Fatalf("handlers: %v", err)
	}

	// ── Routes ─────────────────────────────────────────────────────────
	mux := http.NewServeMux()

	// OAuth
	mux.HandleFunc("/connect", h.Connect())
	mux.HandleFunc("/callback", h.Callback())
	mux.HandleFunc("/status", h.Status())
	mux.HandleFunc("/disconnect", h.Disconnect())

	// Picker sessions
	mux.HandleFunc("/sessions", h.CreateSession())
	mux.HandleFunc("/sessions/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/sessions/")
		sessionID := strings.TrimSuffix(path, "/import")
		extract := func(*http.Request) string { return sessionID }

		switch {
		case strings.HasSuffix(path, "/import") && r.Method == http.MethodPost:
			h.StartImport(extract)(w, r)
		case r.Method == http.MethodGet:
			h.PollSession(extract)(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Imports
	mux.HandleFunc("/imports/", func(w http.ResponseWriter, r *http.Request) {
		jobID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/imports/"), "/")
		h.GetImport(func(*http.Request) string { return jobID })(w, r)
	})

	// Health check
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	// ── Worker ─────────────────────────────────────────────────────────
	worker, err := photopicker.NewWorker(photopicker.WorkerConfig{Client: client})
	if err != nil {
		log.Fatalf("worker: %v", err)
	}
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	go worker.Run(workerCtx)
	slog.Info("import worker started")

	// ── Server ─────────────────────────────────────────────────────────
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		slog.Info("listening", "addr", addr)
		slog.Info("usage",
			"connect", fmt.Sprintf("GET  http://localhost%s/connect?user=demo", addr),
			"status", fmt.Sprintf("GET  http://localhost%s/status?user=demo", addr),
		)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	// ── Graceful shutdown ──────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down…")
	cancelWorker()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Fatalf("shutdown: %v", err)
	}
	slog.Info("bye")
}

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required environment variable %s is not set", key)
	}
	return v
}
