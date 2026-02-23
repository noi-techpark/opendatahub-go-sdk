// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package clibmock

import (
	"encoding/json"
	"testing"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/assert/cmp"
)

// CompareMockCalls compares two MockCalls structs for equality.
// Both sides are expected to use json.RawMessage for payloads, so comparison
// works by normalizing everything through JSON round-trip to ensure consistent types.
func CompareMockCalls(t *testing.T, expected, actual MockCalls) {
	t.Helper()

	expectedNorm := normalizeViaJSON(t, expected)
	actualNorm := normalizeViaJSON(t, actual)

	assert.Assert(t, cmp.DeepEqual(expectedNorm, actualNorm))
}

// normalizeViaJSON performs a JSON round-trip to ensure consistent types for comparison.
// This converts json.RawMessage fields into their parsed representations (maps, slices, etc.)
// so that DeepEqual can compare them structurally.
func normalizeViaJSON(t *testing.T, v interface{}) interface{} {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("clibmock: failed to marshal for normalization: %v", err)
	}
	var out interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("clibmock: failed to unmarshal for normalization: %v", err)
	}
	return out
}
