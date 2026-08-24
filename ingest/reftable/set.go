// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package reftable

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/noi-techpark/opendatahub-go-sdk/tel/logger"
)

// Loader is one reference table, viewed without its element type.
//
// Table[A] and Table[B] are different types and cannot share a slice, so a Set
// holds them through this interface while callers keep the concrete, typed
// *Table[T] for lookups. The methods are unexported on purpose: only tables
// created by this package can satisfy it.
type Loader interface {
	Name() string
	bootstrap(context.Context) error
	subscribe(context.Context) error
	run(context.Context)
	close() error
}

// Set starts and stops a group of reference tables together.
//
// Bootstrap runs in parallel and is all-or-nothing. A transformer that starts
// with an empty table would write provider data over every enriched entity on
// its first message, so a failed bootstrap has to stop the process rather than
// degrade it.
type Set struct {
	tables []Loader

	mu      sync.Mutex
	started bool
	wg      sync.WaitGroup
	cancel  context.CancelFunc
}

func NewSet(tables ...Loader) *Set {
	return &Set{tables: tables}
}

// Start bootstraps every table in parallel, then leaves each one refreshing in
// the background until ctx is cancelled. It returns once all tables are ready,
// so a caller may read from them immediately after it returns nil.
func (s *Set) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("reference table set already started")
	}

	log := logger.Get(ctx)

	errs := make([]error, len(s.tables))
	var boot sync.WaitGroup
	for i, t := range s.tables {
		boot.Add(1)
		go func(i int, t Loader) {
			defer boot.Done()
			errs[i] = t.bootstrap(ctx)
		}(i, t)
	}
	boot.Wait()

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("reference tables not ready: %w", err)
	}

	// Subscriptions come up only after every table holds a complete view, so a
	// notification can never be applied to a half-built table.
	for _, t := range s.tables {
		if err := t.subscribe(ctx); err != nil {
			return err
		}
	}

	// The refreshers run under a context Close can cancel itself, so shutting
	// down does not depend on the caller cancelling first. Relying on that
	// deadlocks under the natural `defer cancel(); defer set.Close()` ordering,
	// because deferred calls run last-in-first-out.
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	for _, t := range s.tables {
		s.wg.Add(1)
		go func(t Loader) {
			defer s.wg.Done()
			t.run(runCtx)
		}(t)
	}

	s.started = true
	names := make([]string, 0, len(s.tables))
	for _, t := range s.tables {
		names = append(names, t.Name())
	}
	log.Info("reference tables ready", "tables", names)
	return nil
}

// Close stops the background refreshers and releases each table. It is safe to
// call without cancelling the context passed to Start.
func (s *Set) Close() error {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
	errs := make([]error, 0, len(s.tables))
	for _, t := range s.tables {
		errs = append(errs, t.close())
	}
	return errors.Join(errs...)
}
