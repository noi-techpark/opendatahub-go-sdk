// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package clibmock

import (
	"context"
	"encoding/json"
	"fmt"
)

// toRawJSON marshals a value to json.RawMessage for stable snapshot comparison.
// This ensures payloads captured at runtime (Go structs with time.Time, embedded structs, etc.)
// are stored in the same JSON representation as snapshots loaded from files.
func toRawJSON(v interface{}) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("clibmock: failed to marshal payload: %v", err))
	}
	return data
}

// PostCall captures a single Post() invocation.
type PostCall struct {
	ApiPath     string            `json:"apiPath"`
	QueryParams map[string]string `json:"queryParams"`
	Payload     json.RawMessage   `json:"payload"`
}

// PutCall captures a single Put() invocation.
type PutCall struct {
	ApiPath string          `json:"apiPath"`
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload"`
}

// PutMultipleCall captures a single PutMultiple() invocation.
type PutMultipleCall struct {
	ApiPath string          `json:"apiPath"`
	Payload json.RawMessage `json:"payload"`
}

// GetCall captures a single Get() invocation.
type GetCall struct {
	ApiPath     string            `json:"apiPath"`
	QueryParams map[string]string `json:"queryParams"`
}

// MockCalls holds all captured API calls for snapshot comparison.
type MockCalls struct {
	Posts        []PostCall        `json:"posts"`
	Puts         []PutCall         `json:"puts"`
	PutMultiples []PutMultipleCall `json:"putMultiples"`
	Gets         []GetCall         `json:"gets"`
}

// ContentMock implements clib.ContentAPI and records all calls.
type ContentMock struct {
	posts        []PostCall
	puts         []PutCall
	putMultiples []PutMultipleCall
	gets         []GetCall

	// GetResponses maps apiPath to a function that populates responseStruct.
	// Used when tests need Get() to return data (e.g. for LoadExisting).
	GetResponses map[string]func(queryParams map[string]string, responseStruct interface{}) error
}

// NewContentMock creates a new ContentMock ready to record API calls.
func NewContentMock() *ContentMock {
	return &ContentMock{
		posts:        []PostCall{},
		puts:         []PutCall{},
		putMultiples: []PutMultipleCall{},
		gets:         []GetCall{},
		GetResponses: make(map[string]func(queryParams map[string]string, responseStruct interface{}) error),
	}
}

// Get records the call and optionally populates responseStruct via GetResponses.
func (m *ContentMock) Get(_ context.Context, apiPath string, queryParams map[string]string, responseStruct interface{}) error {
	m.gets = append(m.gets, GetCall{
		ApiPath:     apiPath,
		QueryParams: queryParams,
	})

	if handler, ok := m.GetResponses[apiPath]; ok {
		return handler(queryParams, responseStruct)
	}

	return nil
}

// Post records the call.
func (m *ContentMock) Post(_ context.Context, apiPath string, queryParams map[string]string, payload interface{}) error {
	m.posts = append(m.posts, PostCall{
		ApiPath:     apiPath,
		QueryParams: queryParams,
		Payload:     toRawJSON(payload),
	})
	return nil
}

// Put records the call.
func (m *ContentMock) Put(_ context.Context, apiPath string, id string, payload interface{}) error {
	m.puts = append(m.puts, PutCall{
		ApiPath: apiPath,
		ID:      id,
		Payload: toRawJSON(payload),
	})
	return nil
}

// PutMultiple records the call.
func (m *ContentMock) PutMultiple(_ context.Context, apiPath string, payload interface{}) error {
	m.putMultiples = append(m.putMultiples, PutMultipleCall{
		ApiPath: apiPath,
		Payload: toRawJSON(payload),
	})
	return nil
}

// Calls returns all captured API calls as a MockCalls snapshot.
func (m *ContentMock) Calls() MockCalls {
	return MockCalls{
		Posts:        m.posts,
		Puts:         m.puts,
		PutMultiples: m.putMultiples,
		Gets:         m.gets,
	}
}
