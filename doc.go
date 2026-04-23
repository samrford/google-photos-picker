// Package photopicker provides a drop-in Google Photos Picker integration for
// Go services: OAuth, picker sessions, an import worker, and ready-to-mount
// HTTP handlers.
//
// Consumers supply a PhotoSink which decides where downloaded photo bytes go
// (S3, local disk, a CMS) and a TokenStore / ImportStore pair for persistence.
// A reference Postgres-backed implementation of the stores lives in the
// postgres subpackage.
package photopicker
