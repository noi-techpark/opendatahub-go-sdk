// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package reftable

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Lookup is the read side of a reference table.
//
// Consumers should depend on this rather than on *Table: it is the whole of
// what enrichment needs, and it lets a test supply a fixture instead of an
// ingestion stack. *Table[T] and *Static[T] both satisfy it.
type Lookup[T any] interface {
	Get(key string) (T, bool)
	// All returns a copy of the table. Consumers that have to enumerate the
	// whole thing — building an index, deriving a topology — need this, and a
	// copy keeps them from holding the table's lock while they work.
	All() map[string]T
	Len() int
}

// Static is a fixed reference table. It never fetches and never changes, which
// is what makes it useful for tests, for fixtures, and for the occasional
// consumer whose reference data genuinely is a constant.
//
// It deliberately is not a *Table: a Table with no bridge would be a half-built
// object that panics the moment anything refreshes it.
type Static[T any] struct {
	data map[string]T
}

// NewStatic returns a Lookup over a copy of data, so later edits to the caller's
// map cannot change what a test is asserting against.
func NewStatic[T any](data map[string]T) *Static[T] {
	cp := make(map[string]T, len(data))
	for k, v := range data {
		cp[k] = v
	}
	return &Static[T]{data: cp}
}

// StaticFromJSON builds a Static from a `{"key": {…}, …}` document, which is
// the convenient shape for a fixture file checked in next to a test.
func StaticFromJSON[T any](raw []byte) (*Static[T], error) {
	var data map[string]T
	// Decoded exactly as a live record is (see parseRecord): numbers keep their
	// literal instead of becoming float64. A fixture whose values differ in
	// type from the real thing is worse than no fixture — it is the mechanism
	// by which a test passes while production is broken.
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&data); err != nil {
		return nil, fmt.Errorf("reference fixture is not a json object of records: %w", err)
	}
	return NewStatic(data), nil
}

func (s *Static[T]) Get(key string) (T, bool) {
	v, ok := s.data[key]
	return v, ok
}

// All returns a copy, so a caller cannot reach back into the fixture.
func (s *Static[T]) All() map[string]T {
	out := make(map[string]T, len(s.data))
	for k, v := range s.data {
		out[k] = v
	}
	return out
}

func (s *Static[T]) Len() int { return len(s.data) }

// Set adds or replaces a record, so a test can model an operator edit without
// standing anything up.
func (s *Static[T]) Set(key string, v T) { s.data[key] = v }

// Delete removes a record, modelling a tombstone.
func (s *Static[T]) Delete(key string) { delete(s.data, key) }

// compile-time check that the live table satisfies the read contract
var _ Lookup[struct{}] = (*Table[struct{}])(nil)
var _ Lookup[struct{}] = (*Static[struct{}])(nil)
