package photopicker

import (
	"context"
	"io"
)

// DownloadedPhoto is the payload handed to a PhotoSink for each picked media
// item. Bytes is pre-buffered (a *bytes.Reader) and safe to read once; Size is
// the exact byte length. Filename and MimeType come straight from Google and
// may be empty.
//
// JobMetadata is the opaque caller context supplied at StartImport (e.g. which
// item/entity these photos belong to). It is the same map for every photo in a
// job and is nil when none was supplied. The library never inspects or trusts
// it — validating it is the consumer's responsibility.
type DownloadedPhoto struct {
	GoogleMediaID string
	Filename      string
	MimeType      string
	Size          int64
	Bytes         io.Reader
	JobMetadata   map[string]string
}

// PhotoSink is the consumer-supplied extension point. SavePhoto is called once
// per successfully-downloaded photo; the returned savedID is free-form (URL,
// UUID, storage key) and is surfaced back to the caller in ImportJob.SavedIDs.
//
// The library enforces the configured download cap before invoking SavePhoto,
// so implementations don't need to bound-check Size themselves.
type PhotoSink interface {
	SavePhoto(ctx context.Context, userID, jobID string, p DownloadedPhoto) (savedID string, err error)
}

// SinkFunc adapts a plain function to PhotoSink.
type SinkFunc func(ctx context.Context, userID, jobID string, p DownloadedPhoto) (string, error)

// SavePhoto implements PhotoSink.
func (f SinkFunc) SavePhoto(ctx context.Context, userID, jobID string, p DownloadedPhoto) (string, error) {
	return f(ctx, userID, jobID, p)
}
