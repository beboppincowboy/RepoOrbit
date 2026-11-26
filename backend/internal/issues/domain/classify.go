package domain

import "fmt"

type ErrorKind uint8

const (
	OtherError ErrorKind = iota
	NotSent
	HTTPError
)

func Classify(kind ErrorKind, status int) (string, string) {
	if kind == NotSent {
		return "failed", "the request could not be prepared; nothing was sent to GitHub"
	}
	if kind == HTTPError && status >= 400 && status < 500 {
		return "failed", fmt.Sprintf("GitHub rejected the request (status %d)", status)
	}
	if kind == HTTPError {
		return "unknown", fmt.Sprintf("GitHub answered status %d; the change may or may not have been applied", status)
	}
	return "unknown", "no reliable answer from GitHub; the change may or may not have been applied"
}
