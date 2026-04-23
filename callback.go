package photopicker

import (
	"html/template"
	"net/http"
)

// DefaultPostMessageType is the default `type` field sent via postMessage to
// the opener window from the OAuth callback page. Consumers can override via
// CallbackPage.PostMessageType to preserve an existing frontend contract.
const DefaultPostMessageType = "google-photos-picker:oauth"

// DefaultTargetOrigin is the default postMessage targetOrigin. It is wide-open
// for local-dev convenience; in production consumers should override with
// their own frontend origin.
const DefaultTargetOrigin = "*"

// CallbackPage controls the HTML served at the OAuth callback endpoint.
//
//   - PostMessageType: the `type` field of the window.postMessage payload.
//   - TargetOrigin: the targetOrigin passed to postMessage. Defaults to "*"
//     for convenience in local dev, but consumers SHOULD override with their
//     own frontend origin in production.
//   - HTMLTemplate: optional override of the built-in template. It is rendered
//     with callbackData.
type CallbackPage struct {
	PostMessageType string
	TargetOrigin    string
	HTMLTemplate    *template.Template
}

// callbackData is the template context. Exposed so custom HTMLTemplates can
// reference the same field names.
type callbackData struct {
	Status          string // "success" or "error"
	Message         string
	PostMessageType string
	TargetOrigin    string
}

// defaultCallbackTmpl renders a minimal page that posts a message to the
// opener window and self-closes. postMessage arguments are passed through
// html/template's default contextual escaping — the JS context escapes strings
// safely.
var defaultCallbackTmpl = template.Must(template.New("callback").Parse(`<!doctype html>
<html><body>
<p>You can close this window.</p>
<script>
(function() {
  var payload = { type: {{.PostMessageType}}, status: {{.Status}}, message: {{.Message}} };
  try {
    if (window.opener) {
      window.opener.postMessage(payload, {{.TargetOrigin}});
    }
  } catch (e) {}
  setTimeout(function(){ window.close(); }, 300);
})();
</script>
</body></html>`))

// renderCallback writes the callback page response. success chooses between
// "success" and "error" status; message is shown in the postMessage payload.
func renderCallback(w http.ResponseWriter, page CallbackPage, success bool, message string) {
	data := callbackData{
		Status:          "error",
		Message:         message,
		PostMessageType: page.PostMessageType,
		TargetOrigin:    page.TargetOrigin,
	}
	if success {
		data.Status = "success"
	}
	if data.PostMessageType == "" {
		data.PostMessageType = DefaultPostMessageType
	}
	if data.TargetOrigin == "" {
		data.TargetOrigin = DefaultTargetOrigin
	}

	tmpl := page.HTMLTemplate
	if tmpl == nil {
		tmpl = defaultCallbackTmpl
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.Execute(w, data)
}
