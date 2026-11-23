// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package discovery

import (
	"context"
	"sync"
)

// Recorder is an in-memory Observer for tests: it keeps the last Entry per key.
type Recorder struct {
	mu        sync.Mutex
	entries   map[string]Entry
	Flushes   int
	Completes int
}

func NewRecorder() *Recorder { return &Recorder{entries: map[string]Entry{}} }

func (r *Recorder) Observe(key string, e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[key] = e
}

func (r *Recorder) Flush(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Flushes++
	return nil
}

func (r *Recorder) Complete() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Completes++
}

// Get returns the last entry observed for key.
func (r *Recorder) Get(key string) (Entry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[key]
	return e, ok
}

// All returns a copy of every observed entry.
func (r *Recorder) All() map[string]Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Entry, len(r.entries))
	for k, v := range r.entries {
		out[k] = v
	}
	return out
}
