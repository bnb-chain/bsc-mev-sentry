package service

import "errors"

const sentryErrorCode = -38006

// sentryError is an API error that encompasses an invalid bid with JSON error
// code and a binary data blob.
type sentryError struct {
	error
	code int
}

// ErrorCode returns the JSON error code for an invalid bid.
// See: https://github.com/ethereum/wiki/wiki/JSON-RPC-Error-Codes-Improvement-Proposal
func (e *sentryError) ErrorCode() int {
	return e.code
}

func newSentryError(message string) *sentryError {
	return &sentryError{
		error: errors.New(message),
		code:  sentryErrorCode,
	}
}
