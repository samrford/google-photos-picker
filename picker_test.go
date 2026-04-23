package photopicker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func withFakeGoogle(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	prev := photosPickerAPIBase
	photosPickerAPIBase = srv.URL
	t.Cleanup(func() { photosPickerAPIBase = prev })
	return srv
}

func staticAuth(tok string) authorizer {
	return func(context.Context, string) (string, error) { return tok, nil }
}

func TestGoogleRequest_AttachesBearer(t *testing.T) {
	var gotAuth string
	srv := withFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{}`))
	})

	resp, err := googleRequest(context.Background(), http.DefaultClient, staticAuth("tok123"), "u", "GET", srv.URL+"/x", nil)
	if err != nil {
		t.Fatalf("googleRequest: %v", err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer tok123" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
}

func TestGoogleRequest_AuthorizerErrorPropagates(t *testing.T) {
	bad := func(context.Context, string) (string, error) { return "", errors.New("boom") }
	_, err := googleRequest(context.Background(), http.DefaultClient, bad, "u", "GET", "http://example.invalid", nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want auth error, got %v", err)
	}
}

func TestGoogleRequest_Non2xxReturnsError(t *testing.T) {
	srv := withFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusTeapot)
	})
	_, err := googleRequest(context.Background(), http.DefaultClient, staticAuth("t"), "u", "GET", srv.URL+"/x", nil)
	if err == nil {
		t.Fatal("expected error on 418")
	}
	if !strings.Contains(err.Error(), "418") {
		t.Fatalf("error should mention status: %v", err)
	}
}

func TestGoogleRequest_BodyAndContentType(t *testing.T) {
	var gotCT, gotBody string
	srv := withFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(`{}`))
	})

	resp, err := googleRequest(context.Background(), http.DefaultClient, staticAuth("t"), "u", "POST", srv.URL+"/x", []byte(`{"k":1}`))
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	resp.Body.Close()
	if gotCT != "application/json" {
		t.Fatalf("content-type: %q", gotCT)
	}
	if gotBody != `{"k":1}` {
		t.Fatalf("body: %q", gotBody)
	}
}

func TestListSessionMediaItems_Paginates(t *testing.T) {
	calls := 0
	srv := withFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/mediaItems" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("sessionId") != "sess-1" {
			t.Errorf("sessionId = %q", q.Get("sessionId"))
		}
		switch q.Get("pageToken") {
		case "":
			w.Write([]byte(`{"mediaItems":[{"id":"a"},{"id":"b"}],"nextPageToken":"p2"}`))
		case "p2":
			w.Write([]byte(`{"mediaItems":[{"id":"c"}]}`))
		default:
			t.Errorf("unexpected pageToken %q", q.Get("pageToken"))
		}
	})
	_ = srv

	items, err := listSessionMediaItems(context.Background(), http.DefaultClient, staticAuth("t"), "u", "sess-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	if items[0].ID != "a" || items[2].ID != "c" {
		t.Fatalf("order wrong: %+v", items)
	}
	if calls != 2 {
		t.Fatalf("expected 2 page calls, got %d", calls)
	}
}

func TestDownloadMediaItem_OK(t *testing.T) {
	data := []byte("PNGBYTES")
	dl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/photo=d") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer t" {
			t.Errorf("missing bearer")
		}
		w.Write(data)
	}))
	defer dl.Close()

	item := mediaItem{ID: "m1", MediaFile: mediaFile{BaseURL: dl.URL + "/photo", MimeType: "image/png", Filename: "x.png"}}
	got, err := downloadMediaItem(context.Background(), http.DefaultClient, staticAuth("t"), "u", item, 1024)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if got.Size != int64(len(data)) {
		t.Fatalf("size = %d", got.Size)
	}
	if got.GoogleMediaID != "m1" || got.Filename != "x.png" || got.MimeType != "image/png" {
		t.Fatalf("metadata wrong: %+v", got)
	}
	buf, _ := io.ReadAll(got.Bytes)
	if string(buf) != string(data) {
		t.Fatalf("bytes mismatch: %q", buf)
	}
}

func TestDownloadMediaItem_RejectsOversized(t *testing.T) {
	dl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 200))
	}))
	defer dl.Close()

	item := mediaItem{MediaFile: mediaFile{BaseURL: dl.URL + "/p", MimeType: "image/jpeg"}}
	_, err := downloadMediaItem(context.Background(), http.DefaultClient, staticAuth("t"), "u", item, 100)
	if !errors.Is(err, ErrDownloadTooBig) {
		t.Fatalf("want ErrDownloadTooBig, got %v", err)
	}
}

func TestDeletePickerSession_HitsDELETE(t *testing.T) {
	var method, path string
	srv := withFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
	})
	_ = srv

	if err := deletePickerSession(context.Background(), http.DefaultClient, staticAuth("t"), "u", "sess-9"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if method != http.MethodDelete {
		t.Fatalf("method = %q", method)
	}
	if path != "/sessions/sess-9" {
		t.Fatalf("path = %q", path)
	}
}
