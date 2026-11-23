// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

// Package discovery writes a normalised discovery reference table: the
// entities a transformer saw in its source, and what it did with each.
//
// Only the transformer understands its provider's format. Whatever it withholds
// never reaches a silo, so nothing downstream can notice it unless the
// transformer says so. A Reporter lets it say so in one shape for every
// provider: one document per entity key in <db>/<collection>, written through
// the raw writer only when the entry changed, and read back by anyone with
// reftable.New[Record] (Schema, DecodeText).
//
// The package decides nothing about publishing; the caller reports the
// decision it already made.
package discovery

import (
	"context"
	"time"
)

// Schema is stamped on every document as meta.schema.
const Schema = "discovery.v1"

// KeyField is the meta field holding the entity key (X-OpenDataHub-key).
const KeyField = "key"

type Status string

const (
	// StatusPublished: the entity reached the silo.
	StatusPublished Status = "published"
	// StatusWithheld: the transformer saw it and chose not to publish it.
	StatusWithheld Status = "withheld"
	// StatusGone: absent from GoneAfter consecutive complete snapshots.
	StatusGone Status = "gone"
)

// Entry is what a transformer reports for one entity in one payload.
type Entry struct {
	Status Status
	// Reason is a short machine-readable code, e.g. "no-record",
	// "parent-incomplete". Empty for a plain publish.
	Reason string
	Name   string
	// Parent is the key of the parent entity, if any.
	Parent string
	Lat    *float64
	Lon    *float64
	// Data carries the transformer's normalised view of anything else worth
	// showing (capacity, provider-side names, ...). It must be JSON-encodable.
	Data map[string]any
	// Seed is a reference record, in the schema of the table the transformer
	// reads its records from, that overlaid reproduces what the transformer
	// publishes for this entity without one. Adopting it changes nothing
	// downstream, which is what makes gating a published entity safe.
	Seed map[string]any
}

// Record is the stored document body.
type Record struct {
	Status    Status         `json:"status"`
	Reason    string         `json:"reason,omitempty"`
	Name      string         `json:"name,omitempty"`
	Parent    string         `json:"parent,omitempty"`
	Lat       *float64       `json:"lat,omitempty"`
	Lon       *float64       `json:"lon,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	Seed      map[string]any `json:"seed,omitempty"`
	FirstSeen time.Time      `json:"firstSeen"`
	// Changed is when the entry's content last changed.
	Changed time.Time `json:"changed"`
}

// Observer is what a transformer holds, so tests can inject a Recorder.
type Observer interface {
	// Observe records the entity for the next Flush. Safe for concurrent use.
	Observe(key string, e Entry)
	// Flush writes every entry whose content changed since it was last
	// written. A failed write stays pending for the next Flush.
	Flush(ctx context.Context) error
	// Complete declares that the entries observed since the previous Complete
	// are the source's full set. Only full-snapshot sources call it.
	Complete()
}

type Config struct {
	// Name identifies the reporter in logs and metrics. Defaults to Provider.
	Name string
	// Provider is "<db>/<collection>" of the discovery table.
	Provider string
	// RawWriterURL is the raw writer the documents are posted to.
	RawWriterURL string
	// GoneAfter is how many consecutive Complete calls a key may be missing
	// from before it is marked StatusGone. 0 disables it.
	GoneAfter int
	// OnNew, when set, runs once for a key never seen before.
	OnNew func(ctx context.Context, key string, r Record)
}
