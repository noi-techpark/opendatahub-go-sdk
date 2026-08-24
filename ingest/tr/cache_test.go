// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package tr

import "testing"

type station struct {
	ID       string
	Name     string
	Capacity int
	SyncedAt string `hash:"ignore"`
}

func TestCacheChangedDoesNotCommit(t *testing.T) {
	c := NewCache[station]()
	s := station{ID: "a", Name: "P01", Capacity: 100}

	// The whole point of splitting Changed from Commit: asking twice must keep
	// reporting a change, or a failed push would silently swallow it.
	for i := 0; i < 3; i++ {
		changed, err := c.Changed("a", s)
		if err != nil {
			t.Fatalf("Changed: %v", err)
		}
		if !changed {
			t.Fatalf("call %d: Changed = false before any Commit", i+1)
		}
	}

	if err := c.Commit("a", s); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	changed, err := c.Changed("a", s)
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if changed {
		t.Error("Changed = true after Commit of an identical value")
	}
}

func TestCacheDetectsRealChange(t *testing.T) {
	c := NewCache[station]()
	s := station{ID: "a", Name: "P01", Capacity: 100}
	if err := c.Commit("a", s); err != nil {
		t.Fatal(err)
	}

	s.Capacity = 120
	changed, err := c.Changed("a", s)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("Changed = false after Capacity changed")
	}
}

func TestCacheIgnoresTaggedFields(t *testing.T) {
	c := NewCache[station]()
	s := station{ID: "a", Name: "P01", Capacity: 100, SyncedAt: "2026-08-11T09:00:00Z"}
	if err := c.Commit("a", s); err != nil {
		t.Fatal(err)
	}

	s.SyncedAt = "2026-08-11T09:05:00Z"
	changed, err := c.Changed("a", s)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error(`Changed = true when only a hash:"ignore" field moved`)
	}
}

func TestCacheChangesAndCommitAll(t *testing.T) {
	c := NewCache[station]()
	items := map[string]station{
		"a": {ID: "a", Name: "P01", Capacity: 100},
		"b": {ID: "b", Name: "P02", Capacity: 200},
	}

	changes, err := c.Changes(items)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("first round: %d changes, want 2", len(changes))
	}
	if err := c.CommitAll(changes); err != nil {
		t.Fatal(err)
	}

	changes, err = c.Changes(items)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("second round: %d changes, want 0", len(changes))
	}

	items["b"] = station{ID: "b", Name: "P02 renamed", Capacity: 200}
	changes, err = c.Changes(items)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("after edit: %d changes, want 1", len(changes))
	}
	if _, ok := changes["b"]; !ok {
		t.Error("expected b in the change set")
	}
}

func TestCacheForgetAndReset(t *testing.T) {
	c := NewCache[station]()
	s := station{ID: "a", Name: "P01"}
	if err := c.Commit("a", s); err != nil {
		t.Fatal(err)
	}

	c.Forget("a")
	changed, err := c.Changed("a", s)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("Changed = false after Forget")
	}

	if err := c.Commit("a", s); err != nil {
		t.Fatal(err)
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
	c.Reset()
	if c.Len() != 0 {
		t.Errorf("Len = %d after Reset, want 0", c.Len())
	}
}
