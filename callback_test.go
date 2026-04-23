package photopicker

import (
	"html/template"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRenderCallback_Success(t *testing.T) {
	w := httptest.NewRecorder()
	renderCallback(w, CallbackPage{}, true, "")
	body := w.Body.String()

	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type: %q", ct)
	}
	if !strings.Contains(body, `"success"`) {
		t.Fatalf("expected success status in body, got:\n%s", body)
	}
	if !strings.Contains(body, `"google-photos-picker:oauth"`) {
		t.Fatalf("expected default postMessage type, got:\n%s", body)
	}
	if !strings.Contains(body, `"*"`) {
		t.Fatalf("expected default target origin, got:\n%s", body)
	}
	if !strings.Contains(body, "window.opener.postMessage") {
		t.Fatal("missing postMessage script")
	}
}

func TestRenderCallback_FailureIncludesMessage(t *testing.T) {
	w := httptest.NewRecorder()
	renderCallback(w, CallbackPage{}, false, "token exchange failed")
	body := w.Body.String()

	if !strings.Contains(body, `"error"`) {
		t.Fatalf("expected error status, got:\n%s", body)
	}
	if !strings.Contains(body, "token exchange failed") {
		t.Fatalf("expected message in body, got:\n%s", body)
	}
}

func TestRenderCallback_OverridesApplied(t *testing.T) {
	w := httptest.NewRecorder()
	renderCallback(w, CallbackPage{
		PostMessageType: "beyond:google-oauth",
		TargetOrigin:    "https://app.example.com",
	}, true, "")
	body := w.Body.String()

	if !strings.Contains(body, `"beyond:google-oauth"`) {
		t.Fatalf("missing custom postMessage type:\n%s", body)
	}
	if !strings.Contains(body, `"https://app.example.com"`) {
		t.Fatalf("missing custom target origin:\n%s", body)
	}
	if strings.Contains(body, `postMessage(payload, "*")`) {
		t.Fatal("should not contain default target origin when overridden")
	}
}

func TestRenderCallback_EscapesMaliciousInput(t *testing.T) {
	w := httptest.NewRecorder()
	renderCallback(w, CallbackPage{}, false, `"); alert(1); //`)
	body := w.Body.String()
	// Raw injection would appear as ); alert(1); // unquoted — reject that.
	if strings.Contains(body, `alert(1)`) && !strings.Contains(body, `\u003`) && !strings.Contains(body, `\"`) {
		t.Fatalf("injection not escaped:\n%s", body)
	}
}

func TestRenderCallback_CustomTemplate(t *testing.T) {
	custom := template.Must(template.New("c").Parse(`STATUS={{.Status}};MSG={{.Message}}`))
	w := httptest.NewRecorder()
	renderCallback(w, CallbackPage{HTMLTemplate: custom}, true, "hi")
	if got := w.Body.String(); got != "STATUS=success;MSG=hi" {
		t.Fatalf("custom template not used: %q", got)
	}
}
