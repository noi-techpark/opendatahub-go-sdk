// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package clib

import (
	"context"
	"fmt"

	"github.com/mitchellh/hashstructure/v2"
)

// CacheEntry holds an entity and its content hash for change detection.
type CacheEntry[T any] struct {
	Entity T
	Hash   uint64
}

// Cache is a generic entity cache keyed by ID, with hash-based change detection.
type Cache[T any] struct {
	entries map[string]CacheEntry[T]
}

// NewCache creates an empty Cache.
func NewCache[T any]() *Cache[T] {
	return &Cache[T]{
		entries: make(map[string]CacheEntry[T]),
	}
}

// Get looks up an entity by ID. Returns the cache entry and whether it exists.
func (c *Cache[T]) Get(id string) (CacheEntry[T], bool) {
	entry, ok := c.entries[id]
	return entry, ok
}

// Set stores or updates an entity in the cache.
func (c *Cache[T]) Set(id string, entity T, hash uint64) {
	c.entries[id] = CacheEntry[T]{
		Entity: entity,
		Hash:   hash,
	}
}

// Delete removes an entity from the cache.
func (c *Cache[T]) Delete(id string) {
	delete(c.entries, id)
}

// Entries returns the underlying map for iteration (e.g., detecting ended entities).
func (c *Cache[T]) Entries() map[string]CacheEntry[T] {
	return c.entries
}

// HasChanged hashes a new entity and compares it with the existing cache entry.
// Returns the computed hash and true if the entity is new or has changed.
func (c *Cache[T]) HasChanged(id string, entity T) (hash uint64, changed bool, err error) {
	hash, err = HashEntity(entity)
	if err != nil {
		return 0, false, err
	}

	existing, exists := c.entries[id]
	if !exists {
		return hash, true, nil
	}

	return hash, hash != existing.Hash, nil
}

// HashEntity computes a hash of the given entity using hashstructure.FormatV2.
// Use struct tags (hash:"ignore", hash:"set") on fields to control inclusion.
func HashEntity[T any](entity T) (uint64, error) {
	return hashstructure.Hash(entity, hashstructure.FormatV2, nil)
}

// DefaultSortBy is the sort applied to paginated loads when the consumer does
// not choose one. The Content API's default page order is not stable across
// pagenumber requests, so paginating without an explicit sort makes pages
// overlap: some entities come back more than once and others are never
// returned at all. Any total order over the result set avoids that, and "Id"
// is present on every Content API entity.
const DefaultSortBy = "Id"

// LoadConfig configures paginated loading of existing entities from the Content API.
type LoadConfig[T any] struct {
	// EntityType is the Content API entity type path (e.g., "Announcement", "Trip").
	EntityType string

	// PageSize for pagination. Defaults to 200 if zero.
	PageSize int

	// QueryParams are the query parameters passed to the GET request.
	// Fully consumer-controlled (e.g., {"active": "true", "source": "A22"}).
	QueryParams map[string]string

	// SortBy pins the order the pages are walked in, as the Content API
	// "rawsort" parameter. It must be a total order over the result set or the
	// load silently returns fewer distinct entities than it claims.
	// Defaults to DefaultSortBy; a "rawsort" already present in QueryParams is
	// honoured when this is empty.
	SortBy string

	// IDFunc extracts the unique ID from an entity for use as the cache map key.
	// Required.
	IDFunc func(T) string
}

// paginatedResponse is the expected response format from the Content API.
type paginatedResponse[T any] struct {
	Items        []T `json:"Items"`
	TotalResults int `json:"TotalResults"`
	TotalPages   int `json:"TotalPages"`
	CurrentPage  int `json:"CurrentPage"`
}

// LoadExisting fetches all entities matching QueryParams via paginated GET requests,
// hashes each one, and returns a populated Cache.
//
// The walk is pinned to a deterministic order (see LoadConfig.SortBy) and the
// number of entities actually cached is checked against the TotalResults the
// API reports. A partially loaded cache is not a degraded cache: callers use it
// to decide which entities have disappeared upstream, so a missing entity is
// indistinguishable from one that was never there. That silently corrupts data,
// hence an incomplete load is an error rather than a shorter result.
func LoadExisting[T any](ctx context.Context, client ContentAPI, cfg LoadConfig[T]) (*Cache[T], error) {
	cache := NewCache[T]()

	pageSize := cfg.PageSize
	if pageSize == 0 {
		pageSize = 200
	}

	currentPage := 1
	totalPages := 1   // will be updated from response
	totalResults := 0 // will be updated from response
	rows := 0

	for currentPage <= totalPages {
		params := make(map[string]string)
		for k, v := range cfg.QueryParams {
			params[k] = v
		}
		params["pageSize"] = fmt.Sprintf("%d", pageSize)
		params["pagenumber"] = fmt.Sprintf("%d", currentPage)

		switch {
		case cfg.SortBy != "":
			params["rawsort"] = cfg.SortBy
		case params["rawsort"] == "":
			params["rawsort"] = DefaultSortBy
		}

		var res paginatedResponse[T]
		err := client.Get(ctx, cfg.EntityType, params, &res)
		if err != nil {
			return nil, fmt.Errorf("failed to load %s page %d: %w", cfg.EntityType, currentPage, err)
		}

		for _, item := range res.Items {
			id := cfg.IDFunc(item)
			hash, err := HashEntity(item)
			if err != nil {
				return nil, fmt.Errorf("failed to hash entity %s: %w", id, err)
			}
			cache.Set(id, item, hash)
		}

		rows += len(res.Items)
		totalResults = res.TotalResults
		totalPages = res.TotalPages
		currentPage++
	}

	if cached := len(cache.Entries()); cached != totalResults {
		return nil, fmt.Errorf(
			"incomplete load of %s: API reports %d entities, cached %d (%d rows over %d pages) - "+
				"the page order is not a total order, set LoadConfig.SortBy to a field that is",
			cfg.EntityType, totalResults, cached, rows, totalPages)
	}

	return cache, nil
}
