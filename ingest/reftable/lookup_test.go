// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package reftable

import (
	"fmt"
	"testing"
	"time"
)

func TestStaticLookup(t *testing.T) {
	src := map[string]enrichment{"A": {Name: "one"}}
	s := NewStatic(src)

	if v, ok := s.Get("A"); !ok || v.Name != "one" {
		t.Errorf("Get(A) = %+v, %v", v, ok)
	}
	if _, ok := s.Get("missing"); ok {
		t.Error("Get(missing) reported a hit")
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}

	// The fixture must be insulated from later edits to the caller's map,
	// or one test can change what another asserts.
	src["A"] = enrichment{Name: "mutated"}
	src["B"] = enrichment{Name: "added"}
	if v, _ := s.Get("A"); v.Name != "one" {
		t.Errorf("Get(A) = %q after mutating the source map, want one", v.Name)
	}
	if _, ok := s.Get("B"); ok {
		t.Error("a key added to the source map appeared in the fixture")
	}
}

func TestStaticSetAndDelete(t *testing.T) {
	s := NewStatic(map[string]enrichment{"A": {Name: "one"}})

	s.Set("A", enrichment{Name: "edited"})
	if v, _ := s.Get("A"); v.Name != "edited" {
		t.Errorf("Get(A) = %q after Set, want edited", v.Name)
	}

	s.Delete("A")
	if _, ok := s.Get("A"); ok {
		t.Error("key still present after Delete")
	}
}

func TestStaticFromJSON(t *testing.T) {
	s, err := StaticFromJSON[enrichment](
		[]byte(`{"0404467_0":{"name":"Dante","municipality":"Bressanone"}}`))
	if err != nil {
		t.Fatalf("StaticFromJSON: %v", err)
	}
	v, ok := s.Get("0404467_0")
	if !ok {
		t.Fatal("key missing")
	}
	if v.Name != "Dante" || v.Municipality != "Bressanone" {
		t.Errorf("decoded %+v", v)
	}

	if _, err := StaticFromJSON[enrichment]([]byte(`["not","an","object"]`)); err == nil {
		t.Error("StaticFromJSON accepted a json array")
	}
}

// A fixture must be indistinguishable from a record that came off the wire.
//
// If the two decode differently, a unit test is asserting against a shape
// production never sees — which is exactly how a fake bridge once let two real
// defects through.
func TestStaticFixtureDecodesLikeARealRecord(t *testing.T) {
	const payload = `{"big":9007199254740993,"lat":46.71602,"capacity":245}`

	fixture, err := StaticFromJSON[map[string]any]([]byte(`{"A":` + payload + `}`))
	if err != nil {
		t.Fatalf("StaticFromJSON: %v", err)
	}
	fromFixture, _ := fixture.Get("A")

	live, err := parseRecord[map[string]any](
		storedDoc("A", "", "", payload, time.Now().UTC()), "key", DecodeBase64)
	if err != nil {
		t.Fatalf("parseRecord: %v", err)
	}

	for _, key := range []string{"big", "lat", "capacity"} {
		f, l := fromFixture[key], live.Data[key]
		if fmt.Sprintf("%T %v", f, f) != fmt.Sprintf("%T %v", l, l) {
			t.Errorf("%s: fixture has %T %v, a live record has %T %v", key, f, f, l, l)
		}
	}
}
