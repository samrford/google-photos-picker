package photopicker

import "errors"

// Sentinel errors returned by the library. Wrap them with fmt.Errorf("...: %w",
// err) in your own code and inspect with errors.Is.
var (
	ErrNoTokens       = errors.New("photopicker: no tokens for user")
	ErrJobNotFound    = errors.New("photopicker: import job not found")
	ErrInvalidConfig  = errors.New("photopicker: invalid config")
	ErrInvalidState   = errors.New("photopicker: invalid or expired OAuth state")
	ErrNotConnected   = errors.New("photopicker: user has not connected Google")
	ErrDownloadTooBig = errors.New("photopicker: photo exceeds download cap")
)
