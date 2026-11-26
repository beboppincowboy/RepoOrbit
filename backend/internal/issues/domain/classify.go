package domain

import "fmt"

// ErrorKind is what a GitHub call failure means for a write, mapped by the caller from its transport errors.
type ErrorKind uint8

const (
	OtherError   ErrorKind = iota // no reliable answer, such as a transport failure
	NotSent                       // the request was never sent
	Unauthorized                  // GitHub answered 401
	NotFound                      // GitHub answered 404
	HTTPError                     // GitHub answered any other status code
)

// Classify returns the write state ("failed" or "unknown") and the reason stored for a failed GitHub call. status is
// the HTTP status GitHub answered; it is ignored for NotSent and OtherError.
func Classify(kind ErrorKind, status int) (string, string) {
	switch {
	case kind == NotSent:
		return "failed", "the request could not be prepared; nothing was sent to GitHub"
	case kind == Unauthorized || kind == NotFound || kind == HTTPError && status >= 400 && status < 500:
		return "failed", fmt.Sprintf("GitHub rejected the request (status %d)", status)
	case kind == HTTPError:
		return "unknown", fmt.Sprintf("GitHub answered status %d; the change may or may not have been applied", status)
	}
	return "unknown", "no reliable answer from GitHub; the change may or may not have been applied"
}
