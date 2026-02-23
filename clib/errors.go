// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package clib

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrUnauthorized   = errors.New("unauthorized")
	ErrAlreadyExists  = errors.New("data exists already")
	ErrNoDataToUpdate = errors.New("data to update not found")
	ErrNoData         = errors.New("no data")
)

// ErrorBody is a general-purpose structure to capture common JSON error response formats.
type ErrorBody struct {
	ErrorID     int    `json:"error,omitempty"`
	ErrorReason string `json:"errorreason,omitempty"`

	Type   string `json:"type,omitempty"`
	Title  string `json:"title,omitempty"`
	Status int    `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`

	Errors map[string][]string `json:"errors,omitempty"`
}

// APIError wraps an HTTP error response, including the status code and the deserialized body.
type APIError struct {
	StatusCode int
	URL        string
	Body       ErrorBody
	RawBody    []byte
}

// Error implements the error interface.
func (e *APIError) Error() string {
	var b strings.Builder

	fmt.Fprintf(&b, "API request to %s failed with status %d", e.URL, e.StatusCode)

	if e.Body.Title != "" {
		fmt.Fprintf(&b, ": %s", e.Body.Title)
	} else if e.Body.ErrorReason != "" {
		fmt.Fprintf(&b, ": %s", e.Body.ErrorReason)
	}

	if e.Body.Detail != "" {
		fmt.Fprintf(&b, " - %s", e.Body.Detail)
	}

	if len(e.Body.Errors) > 0 {
		fmt.Fprintf(&b, "\nValidation errors:")
		for field, messages := range e.Body.Errors {
			fmt.Fprintf(&b, "\n  • %s: %s", field, strings.Join(messages, "; "))
		}
	}

	if b.Len() == 0 && len(e.RawBody) > 0 {
		fmt.Fprintf(&b, "\nRaw response: %s", string(e.RawBody))
	}

	return b.String()
}

// RawResponse returns the underlying raw response body, useful for inspection.
func (e *APIError) RawResponse() []byte {
	return e.RawBody
}
