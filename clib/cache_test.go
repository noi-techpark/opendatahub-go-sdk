// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package clib

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

type testEntity struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
}

func entity(id string) testEntity { return testEntity{ID: id, Name: "name-" + id} }

// fakePagedClient serves a fixed set of pages and records the query params of
// every request, so tests can assert both what was loaded and how it was asked
// for. TotalResults is set independently of the pages to model an API that
// reports more entities than its pagination actually hands out.
type fakePagedClient struct {
	pages        [][]testEntity
	totalResults int
	calls        []map[string]string
}

func (f *fakePagedClient) Get(_ context.Context, _ string, params map[string]string, out interface{}) error {
	f.calls = append(f.calls, params)

	res, ok := out.(*paginatedResponse[testEntity])
	if !ok {
		return nil
	}

	page, _ := strconv.Atoi(params["pagenumber"])
	if page >= 1 && page <= len(f.pages) {
		res.Items = f.pages[page-1]
	}
	res.CurrentPage = page
	res.TotalPages = len(f.pages)
	res.TotalResults = f.totalResults
	return nil
}

func (f *fakePagedClient) Post(context.Context, string, map[string]string, interface{}) error {
	return nil
}
func (f *fakePagedClient) Put(context.Context, string, string, interface{}) error { return nil }
func (f *fakePagedClient) PutMultiple(context.Context, string, interface{}) error { return nil }

func loadTestConfig(cfg *LoadConfig[testEntity]) LoadConfig[testEntity] {
	cfg.EntityType = "Announcement"
	cfg.IDFunc = func(e testEntity) string { return e.ID }
	return *cfg
}

// Test_LoadExisting_CachesEveryEntity is the happy path: disjoint pages that
// add up to TotalResults produce a cache holding all of them.
func Test_LoadExisting_CachesEveryEntity(t *testing.T) {
	client := &fakePagedClient{
		pages:        [][]testEntity{{entity("a"), entity("b")}, {entity("c"), entity("d")}},
		totalResults: 4,
	}

	cache, err := LoadExisting(context.Background(), client, loadTestConfig(&LoadConfig[testEntity]{PageSize: 2}))
	assert.NilError(t, err)
	assert.Equal(t, 4, len(cache.Entries()))

	for _, id := range []string{"a", "b", "c", "d"} {
		_, ok := cache.Get(id)
		assert.Assert(t, ok, "entity %s missing from cache", id)
	}
}

// Test_LoadExisting_RejectsIncompleteLoad reproduces the failure this guard
// exists for: an unstable page order hands out "b" twice and never hands out
// "d", so the walk returns TotalResults rows but fewer distinct entities.
// Callers use the cache to decide what disappeared upstream, so returning a
// short cache would silently mark "d" as ended.
func Test_LoadExisting_RejectsIncompleteLoad(t *testing.T) {
	client := &fakePagedClient{
		pages:        [][]testEntity{{entity("a"), entity("b")}, {entity("b"), entity("c")}},
		totalResults: 4,
	}

	cache, err := LoadExisting(context.Background(), client, loadTestConfig(&LoadConfig[testEntity]{PageSize: 2}))
	assert.Assert(t, cache == nil, "no cache may be returned when the load is short")
	assert.ErrorContains(t, err, "incomplete load of Announcement")
	assert.ErrorContains(t, err, "API reports 4 entities, cached 3")
}

// Test_LoadExisting_AppliesDefaultSort pins the actual fix: every page request
// carries a deterministic sort, otherwise the Content API is free to return a
// different order per page.
func Test_LoadExisting_AppliesDefaultSort(t *testing.T) {
	client := &fakePagedClient{
		pages:        [][]testEntity{{entity("a")}, {entity("b")}},
		totalResults: 2,
	}

	_, err := LoadExisting(context.Background(), client, loadTestConfig(&LoadConfig[testEntity]{PageSize: 1}))
	assert.NilError(t, err)
	assert.Equal(t, 2, len(client.calls))

	for i, call := range client.calls {
		assert.Equal(t, DefaultSortBy, call["rawsort"], "page %d was requested without the default sort", i+1)
	}
}

// Test_LoadExisting_SortByWins lets a consumer pick the sort field, for entity
// types where "Id" is not the cheapest index to page over.
func Test_LoadExisting_SortByWins(t *testing.T) {
	client := &fakePagedClient{pages: [][]testEntity{{entity("a")}}, totalResults: 1}

	cfg := loadTestConfig(&LoadConfig[testEntity]{
		SortBy:      "LastChange",
		QueryParams: map[string]string{"rawsort": "Shortname"},
	})
	_, err := LoadExisting(context.Background(), client, cfg)
	assert.NilError(t, err)
	assert.Equal(t, "LastChange", client.calls[0]["rawsort"])
}

// Test_LoadExisting_KeepsRawsortFromQueryParams keeps working for callers that
// already pass rawsort themselves and never set SortBy.
func Test_LoadExisting_KeepsRawsortFromQueryParams(t *testing.T) {
	client := &fakePagedClient{pages: [][]testEntity{{entity("a")}}, totalResults: 1}

	cfg := loadTestConfig(&LoadConfig[testEntity]{
		QueryParams: map[string]string{"rawsort": "Shortname"},
	})
	_, err := LoadExisting(context.Background(), client, cfg)
	assert.NilError(t, err)
	assert.Equal(t, "Shortname", client.calls[0]["rawsort"])
}

// Test_LoadExisting_DoesNotMutateQueryParams guards against the config map
// being polluted with pagination state, which would leak between loads when a
// caller reuses the same map for several entity types.
func Test_LoadExisting_DoesNotMutateQueryParams(t *testing.T) {
	client := &fakePagedClient{pages: [][]testEntity{{entity("a")}}, totalResults: 1}

	params := map[string]string{"source": "a22"}
	cfg := loadTestConfig(&LoadConfig[testEntity]{QueryParams: params})
	_, err := LoadExisting(context.Background(), client, cfg)
	assert.NilError(t, err)

	assert.Equal(t, 1, len(params))
	assert.Equal(t, "a22", params["source"])
}

// Test_LoadExisting_ErrorNamesTheFix keeps the remedy in the error text: the
// gap is invisible in logs otherwise, and the caller cannot act on a bare count
// mismatch.
func Test_LoadExisting_ErrorNamesTheFix(t *testing.T) {
	client := &fakePagedClient{
		pages:        [][]testEntity{{entity("a"), entity("a")}},
		totalResults: 2,
	}

	_, err := LoadExisting(context.Background(), client, loadTestConfig(&LoadConfig[testEntity]{PageSize: 2}))
	assert.Assert(t, err != nil)
	assert.Assert(t, strings.Contains(err.Error(), "LoadConfig.SortBy"), "error should name the knob that fixes it: %v", err)
}
