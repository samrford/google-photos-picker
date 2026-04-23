package photopicker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewWorker_Validation(t *testing.T) {
	if _, err := NewWorker(WorkerConfig{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", err)
	}
}

func TestWorker_DrainOnce_ProcessesPendingJobs(t *testing.T) {
	var downloads, deletes int
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/mediaItems", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"mediaItems":[
			{"id":"a","mediaFile":{"baseUrl":"%s/dl/a","mimeType":"image/jpeg","filename":"a.jpg"}},
			{"id":"b","mediaFile":{"baseUrl":"%s/dl/b","mimeType":"image/jpeg","filename":"b.jpg"}}
		]}`, srv.URL, srv.URL)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		downloads++
		w.Write([]byte("payload-" + strings.TrimPrefix(r.URL.Path, "/dl/")))
	})
	mux.HandleFunc("/sessions/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes++
		}
	})

	prev := photosPickerAPIBase
	photosPickerAPIBase = srv.URL
	defer func() { photosPickerAPIBase = prev }()

	c, ts, is, sk := newTestClient(t)
	ts.records["u"] = TokenRecord{UserID: "u", AccessToken: "a", ExpiresAt: time.Now().Add(time.Hour)}
	jobID, _ := is.CreateJob(context.Background(), "u", "sess-1")

	w, err := NewWorker(WorkerConfig{Client: c})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	w.DrainOnce(context.Background())

	is.mu.Lock()
	job := is.jobs[jobID]
	is.mu.Unlock()
	if job == nil {
		t.Fatal("job disappeared")
	}
	if job.Status != ImportStatusComplete {
		t.Fatalf("status = %q", job.Status)
	}
	if job.CompletedItems != 2 {
		t.Fatalf("completed = %d", job.CompletedItems)
	}
	if len(job.ImageURLs) != 2 {
		t.Fatalf("image urls = %v", job.ImageURLs)
	}
	if len(sk.saved) != 2 {
		t.Fatalf("sink saved %d", len(sk.saved))
	}
	if downloads != 2 {
		t.Fatalf("downloads = %d", downloads)
	}
	if deletes != 1 {
		t.Fatalf("session delete = %d", deletes)
	}
}

func TestWorker_ProcessJob_FailureMarksFailed(t *testing.T) {
	srv := withFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	})
	_ = srv

	c, ts, is, _ := newTestClient(t)
	ts.records["u"] = TokenRecord{UserID: "u", AccessToken: "a", ExpiresAt: time.Now().Add(time.Hour)}
	jobID, _ := is.CreateJob(context.Background(), "u", "sess-x")

	w, _ := NewWorker(WorkerConfig{Client: c})
	job := &ImportJob{ID: jobID, UserID: "u", SessionID: "sess-x"}
	if err := w.ProcessJob(context.Background(), job); err == nil {
		t.Fatal("expected error")
	}

	is.mu.Lock()
	got := is.jobs[jobID]
	is.mu.Unlock()
	if got.Status != ImportStatusFailed {
		t.Fatalf("status = %q", got.Status)
	}
	if got.Error == "" {
		t.Fatal("expected error message")
	}
}
