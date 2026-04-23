# google-photos-picker

Drop-in Google Photos Picker integration for Go HTTP services. Handles OAuth,
picker sessions, photo download, and an import worker. You supply a `PhotoSink`
that decides where the bytes land (S3, MinIO, local disk, your CMS).

```
import photopicker "github.com/samrford/google-photos-picker"
```

Status: **pre-release (v0.x).** APIs may shift before v1.0.

---

## Quick start

```go
// 1. Build the client
client, err := photopicker.New(photopicker.Config{
    OAuth:       photopicker.NewOAuthConfig(clientID, clientSecret, redirectURL),
    TokenStore:  tokenStore,   // photopicker.TokenStore — see postgres subpackage
    ImportStore: importStore,  // photopicker.ImportStore
    Sink:        mySink,       // photopicker.PhotoSink — you implement this
})

// 2. Mount handlers (URLs, JSON shapes, and postMessage type are yours to keep)
h, _ := photopicker.NewHandlers(photopicker.HandlersConfig{
    Client:        client,
    ResolveUserID: func(r *http.Request) (string, error) {
        uid := auth.UserID(r.Context())
        if uid == "" { return "", errors.New("unauthenticated") }
        return uid, nil
    },
    Callback: photopicker.CallbackPage{
        PostMessageType: "myapp:google-oauth",   // matches your frontend
        TargetOrigin:    "https://myapp.example", // see Security below
    },
})

mux.HandleFunc("/auth/google/connect",  authed(h.Connect()))
mux.HandleFunc("/auth/google/callback", h.Callback())          // no auth — browser redirect
mux.HandleFunc("/auth/google/status",   authed(h.Status()))
mux.HandleFunc("/auth/google",          authed(h.Disconnect())) // DELETE

mux.HandleFunc("/photos/sessions", authed(h.CreateSession()))
mux.HandleFunc("/photos/sessions/", authed(func(w http.ResponseWriter, r *http.Request) {
    path      := strings.TrimPrefix(r.URL.Path, "/photos/sessions/")
    sessionID := strings.TrimSuffix(path, "/import")
    extract   := func(*http.Request) string { return sessionID }
    switch {
    case strings.HasSuffix(path, "/import") && r.Method == http.MethodPost:
        h.StartImport(extract)(w, r)
    case r.Method == http.MethodGet:
        h.PollSession(extract)(w, r)
    default:
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
    }
}))
mux.HandleFunc("/photos/imports/", authed(func(w http.ResponseWriter, r *http.Request) {
    jobID := strings.TrimPrefix(r.URL.Path, "/photos/imports/")
    h.GetImport(func(*http.Request) string { return jobID })(w, r)
}))

// 3. Start the import worker
worker, _ := photopicker.NewWorker(photopicker.WorkerConfig{Client: client})
go worker.Run(ctx)
```

**Implementing `PhotoSink`** — one method, called once per successfully downloaded photo:

```go
type mySink struct{ s3 *s3.Client }

func (sk *mySink) SavePhoto(ctx context.Context, userID, jobID string, p photopicker.DownloadedPhoto) (string, error) {
    key := uuid.New().String() + filepath.Ext(p.Filename)
    _, err := sk.s3.PutObject(ctx, &s3.PutObjectInput{
        Bucket:        aws.String("my-bucket"),
        Key:           aws.String(key),
        Body:          p.Bytes,
        ContentLength: aws.Int64(p.Size),
        ContentType:   aws.String(p.MimeType),
    })
    if err != nil { return "", err }
    return "https://cdn.example/" + key, nil
}
```

`savedID` (the return value) is free-form — it's surfaced back to callers in `ImportJob.ImageURLs`.

See [`examples/minimal/main.go`](examples/minimal/main.go) for a complete runnable server with a local-disk sink.

---

## Endpoint reference

| Method | Path (your choice) | Handler | Auth required |
|--------|-------------------|---------|---------------|
| GET / POST | `/connect` | `h.Connect()` | yes |
| GET | `/callback` | `h.Callback()` | **no** — browser redirect from Google |
| GET | `/status` | `h.Status()` | yes |
| DELETE | `/disconnect` | `h.Disconnect()` | yes |
| POST | `/sessions` | `h.CreateSession()` | yes |
| GET | `/sessions/{id}` | `h.PollSession(extract)` | yes |
| POST | `/sessions/{id}/import` | `h.StartImport(extract)` | yes |
| GET | `/imports/{id}` | `h.GetImport(extract)` | yes |

All authenticated handlers return **401** if `ResolveUserID` errors, **428** with
`{"error":"google_not_connected"}` if the user hasn't linked Google, and **502** on
upstream Google failures.

---

## Config reference

### `photopicker.Config`

| Field | Type | Default | Notes |
|-------|------|---------|-------|
| `OAuth` | `*oauth2.Config` | required | Use `NewOAuthConfig` or build your own |
| `TokenStore` | `TokenStore` | required | See `postgres` subpackage |
| `ImportStore` | `ImportStore` | required | See `postgres` subpackage |
| `Sink` | `PhotoSink` | required | You implement this |
| `DownloadCap` | `int64` | 50 MiB | Per-photo byte ceiling; increase for video |
| `HTTPClient` | `*http.Client` | `http.DefaultClient` | Override for testing |
| `Logger` | `*slog.Logger` | `slog.Default()` | |
| `Clock` | `func() time.Time` | `time.Now` | Override for testing |

### `photopicker.WorkerConfig`

| Field | Type | Default |
|-------|------|---------|
| `Client` | `*Client` | required |
| `PollInterval` | `time.Duration` | 2s |
| `JobTimeout` | `time.Duration` | 35m |
| `Logger` | `*slog.Logger` | client's logger |

---

## Postgres subpackage

```go
import ppg "github.com/samrford/google-photos-picker/postgres"

if err := ppg.Migrate(db); err != nil { ... }       // run once at startup

tokenStore, err := ppg.NewTokenStore(db, encKeyHex) // AES-256-GCM at rest
importStore      := ppg.NewImportStore(db)           // FOR UPDATE SKIP LOCKED
```

> **Migration table collision.** `ppg.Migrate` records state in
> `photopicker_schema_migrations` — a dedicated Goose bookkeeping table — so it
> won't collide with a consuming app's own Goose migrations on the same database.
> Call `ppg.Migrate` **before** any other `goose.Up` calls in your process, or
> serialise them, because Goose uses process-global state internally.
>
> If you drive migrations yourself (Flyway, Alembic, raw SQL), use
> `ppg.MigrationsFS()` or the `ppg.SchemaUpSQL` / `ppg.SchemaDownSQL` constants
> instead.

OAuth tokens are encrypted at rest; the import store uses `FOR UPDATE SKIP LOCKED`
so multiple worker replicas can share a queue safely.

---

## Security

- **Encryption key** — generate with `openssl rand -hex 32`. Treat it like a
  password; rotate it by re-encrypting tokens or by disconnecting and
  re-authorising all users.
- **`TargetOrigin`** — set `CallbackPage.TargetOrigin` to your frontend origin
  (e.g. `"https://myapp.example"`). Leaving it as `"*"` leaks the OAuth result
  to any frame on the page; only acceptable in local development.
- **`*oauth2.Config` aliasing** — do not mutate the `Config.OAuth` value after
  passing it to `photopicker.New`.
- **Download cap** — defaults to 50 MiB per photo. Raise `Config.DownloadCap`
  if you expect large videos; lower it for tighter memory budgets.

---

## License

MIT © Sam Ford
