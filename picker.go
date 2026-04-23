package photopicker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// photosPickerAPIBase is the Google Photos Picker API root. Exposed as a
// variable (not a const) so tests can point it at an httptest.Server.
var photosPickerAPIBase = "https://photospicker.googleapis.com/v1"

// authorizer returns a valid access token for the given user, refreshing the
// underlying tokens silently if needed.
type authorizer func(ctx context.Context, userID string) (string, error)

// pickerSession is Google's Picker API session resource, trimmed to the fields
// the library cares about.
type pickerSession struct {
	ID            string `json:"id"`
	PickerURI     string `json:"pickerUri"`
	MediaItemsSet bool   `json:"mediaItemsSet"`
}

// mediaItem is a single picked item returned by Google's API.
type mediaItem struct {
	ID        string    `json:"id"`
	MediaFile mediaFile `json:"mediaFile"`
}

type mediaFile struct {
	BaseURL  string `json:"baseUrl"`
	MimeType string `json:"mimeType"`
	Filename string `json:"filename"`
}

type listMediaItemsResponse struct {
	MediaItems    []mediaItem `json:"mediaItems"`
	NextPageToken string      `json:"nextPageToken"`
}

// googleRequest executes an authenticated request against a Google API and
// returns the (opened) response. Caller must Close the body. A non-2xx status
// is returned as an error along with a bounded body excerpt (max 512 bytes).
func googleRequest(ctx context.Context, hc *http.Client, auth authorizer, userID, method, reqURL string, body []byte) (*http.Response, error) {
	access, err := auth(ctx, userID)
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, reqURL, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("%s %s: %d: %s", method, reqURL, resp.StatusCode, excerpt)
	}
	return resp, nil
}

// googleJSON is the JSON-decoding variant of googleRequest. Pass out=nil to
// discard the response body (e.g. for DELETE).
func googleJSON(ctx context.Context, hc *http.Client, auth authorizer, userID, method, reqURL string, body []byte, out any) error {
	resp, err := googleRequest(ctx, hc, auth, userID, method, reqURL, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// listSessionMediaItems pages through every picked item in a session.
func listSessionMediaItems(ctx context.Context, hc *http.Client, auth authorizer, userID, sessionID string) ([]mediaItem, error) {
	var out []mediaItem
	pageToken := ""
	for {
		params := url.Values{"sessionId": {sessionID}, "pageSize": {"100"}}
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}
		var page listMediaItemsResponse
		if err := googleJSON(ctx, hc, auth, userID, http.MethodGet, photosPickerAPIBase+"/mediaItems?"+params.Encode(), nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.MediaItems...)
		if page.NextPageToken == "" {
			return out, nil
		}
		pageToken = page.NextPageToken
	}
}

// downloadMediaItem fetches the original bytes of a picked item, bounded by
// maxBytes. If the item exceeds maxBytes, ErrDownloadTooBig is returned. The
// returned DownloadedPhoto owns a *bytes.Reader — callers don't need to Close
// anything.
func downloadMediaItem(ctx context.Context, hc *http.Client, auth authorizer, userID string, item mediaItem, maxBytes int64) (DownloadedPhoto, error) {
	// =d appended to baseUrl requests the original bytes.
	resp, err := googleRequest(ctx, hc, auth, userID, http.MethodGet, item.MediaFile.BaseURL+"=d", nil)
	if err != nil {
		return DownloadedPhoto{}, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return DownloadedPhoto{}, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return DownloadedPhoto{}, ErrDownloadTooBig
	}

	return DownloadedPhoto{
		GoogleMediaID: item.ID,
		Filename:      item.MediaFile.Filename,
		MimeType:      item.MediaFile.MimeType,
		Size:          int64(len(body)),
		Bytes:         bytes.NewReader(body),
	}, nil
}

// deletePickerSession asks Google to discard a session. Best-effort.
func deletePickerSession(ctx context.Context, hc *http.Client, auth authorizer, userID, sessionID string) error {
	return googleJSON(ctx, hc, auth, userID, http.MethodDelete, photosPickerAPIBase+"/sessions/"+sessionID, nil, nil)
}
