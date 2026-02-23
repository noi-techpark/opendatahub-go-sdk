// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package clib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// TagDef represents a tag definition loaded from a JSON file.
// Types are specified directly in the JSON, making tag configuration declarative.
type TagDef struct {
	ID     string   `json:"id"`
	NameEn string   `json:"name-en"`
	NameIt string   `json:"name-it"`
	NameDe string   `json:"name-de"`
	Types  []string `json:"types"`
}

// TagDefs is a collection of tag definitions.
type TagDefs []TagDef

// FindById returns a pointer to the TagDef with the given ID, or nil if not found.
func (t TagDefs) FindById(id string) *TagDef {
	for _, tag := range t {
		if tag.ID == id {
			return &tag
		}
	}
	return nil
}

// ReadTagDefs reads tag definitions from a JSON file.
func ReadTagDefs(filename string) (TagDefs, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("failed opening json file: %w", err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("failed reading json file: %w", err)
	}

	var tags TagDefs
	if err := json.Unmarshal(data, &tags); err != nil {
		return nil, fmt.Errorf("failed unmarshaling tags: %w", err)
	}

	return tags, nil
}

// SyncTagsConfig configures how tags are synced to the Content API.
type SyncTagsConfig struct {
	// Source is the source identifier for the tags (e.g., "skyalps", "announcement").
	Source string

	// LicenseInfo is the license to set on each tag.
	// If nil, defaults to CC0 with ClosedData=false.
	LicenseInfo *LicenseInfo
}

// SyncTags syncs tag definitions to the Content API.
// It creates each tag via POST, ignoring ErrAlreadyExists for idempotency.
// Tag types are read directly from each TagDef.Types field.
func SyncTags(ctx context.Context, client ContentAPI, tags TagDefs, cfg SyncTagsConfig) error {
	license := cfg.LicenseInfo
	if license == nil {
		license = &LicenseInfo{
			ClosedData: false,
			License:    StringPtr("CC0"),
		}
	}

	for _, tag := range tags {
		err := client.Post(ctx, "Tag", map[string]string{"generateid": "false"}, &Tag{
			ID:     StringPtr(tag.ID),
			Source: cfg.Source,
			TagName: map[string]string{
				"it": tag.NameIt,
				"de": tag.NameDe,
				"en": tag.NameEn,
			},
			Types:       tag.Types,
			LicenseInfo: license,
		})
		if err != nil && !errors.Is(err, ErrAlreadyExists) {
			return err
		}
	}
	return nil
}
