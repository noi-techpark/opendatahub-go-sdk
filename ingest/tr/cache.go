// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package tr

import (
	"fmt"

	"github.com/mitchellh/hashstructure/v2"
)

// Cache remembers what a transformer last pushed, so unchanged entities can be
// left out of the next write.
//
// Stations are the motivating case: a transformer re-synchronises its whole
// station list on every message, but the list rarely changes. Hashing what was
// pushed turns that into a no-op for everything that stayed the same.
//
// Use struct tags to control what participates: `hash:"ignore"` on fields that
// change without meaning anything, such as a sync timestamp, otherwise every
// entity looks different every round.
//
// The cache is in-memory only. A restarted transformer re-pushes everything
// once, which is the correct behaviour — it cannot know what the target holds,
// and a redundant write is harmless where a skipped one is not.
//
// Not safe for concurrent use; transformers apply one message at a time.
type Cache[T any] struct {
	hashes map[string]uint64
}

func NewCache[T any]() *Cache[T] {
	return &Cache[T]{hashes: map[string]uint64{}}
}

// Changed reports whether an entity differs from what was last committed.
//
// It deliberately does not record anything. Committing before the write
// succeeds would remember a state that never reached the target, and because
// the next round would then see no difference, that change would be lost for
// good. Call Commit once the push has actually succeeded.
func (c *Cache[T]) Changed(id string, v T) (bool, error) {
	h, err := Hash(v)
	if err != nil {
		return false, err
	}
	prev, seen := c.hashes[id]
	return !seen || prev != h, nil
}

// Commit records an entity as successfully pushed.
func (c *Cache[T]) Commit(id string, v T) error {
	h, err := Hash(v)
	if err != nil {
		return err
	}
	c.hashes[id] = h
	return nil
}

// CommitAll records a batch, for the common case of one push carrying many
// entities.
func (c *Cache[T]) CommitAll(items map[string]T) error {
	for id, v := range items {
		if err := c.Commit(id, v); err != nil {
			return err
		}
	}
	return nil
}

// Changes returns the subset of items that differ from what was committed.
// Pass the result to the writer, then hand the same map to CommitAll once the
// write succeeded.
func (c *Cache[T]) Changes(items map[string]T) (map[string]T, error) {
	out := map[string]T{}
	for id, v := range items {
		changed, err := c.Changed(id, v)
		if err != nil {
			return nil, err
		}
		if changed {
			out[id] = v
		}
	}
	return out, nil
}

// Forget drops an entity, so the next Changed reports it as new. Use it when an
// entity disappears upstream, or to force a re-push after a failure elsewhere.
func (c *Cache[T]) Forget(id string) { delete(c.hashes, id) }

// Reset empties the cache, forcing a full re-push on the next round.
func (c *Cache[T]) Reset() { clear(c.hashes) }

func (c *Cache[T]) Len() int { return len(c.hashes) }

// Hash computes the content hash used for change detection. It honours the
// `hash:"ignore"` struct tag.
func Hash[T any](v T) (uint64, error) {
	h, err := hashstructure.Hash(v, hashstructure.FormatV2, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to hash entity: %w", err)
	}
	return h, nil
}
