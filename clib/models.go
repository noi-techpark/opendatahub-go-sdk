// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package clib

import "time"

// StringPtr returns a pointer to the given string.
func StringPtr(s string) *string {
	return &s
}

// LicenseInfo corresponds to the C# LicenseInfo class.
type LicenseInfo struct {
	License       *string `json:"License,omitempty"`
	LicenseHolder *string `json:"LicenseHolder,omitempty"`
	Author        *string `json:"Author,omitempty"`
	ClosedData    bool    `json:"ClosedData"`
}

// UpdateInfo placeholder for C# UpdateInfo.
type UpdateInfo struct{}

// Metadata corresponds to the C# Metadata class.
type Metadata struct {
	ID         string      `json:"Id"`
	Type       string      `json:"Type"`
	LastUpdate *time.Time  `json:"LastUpdate,omitempty"`
	Source     string      `json:"Source"`
	Reduced    bool        `json:"Reduced"`
	UpdateInfo *UpdateInfo `json:"UpdateInfo,omitempty"`
}

// DetailGeneric corresponds to the C# DetailGeneric class.
type DetailGeneric struct {
	BaseText *string `json:"BaseText,omitempty"`
	Title    *string `json:"Title,omitempty"`
	Language *string `json:"Language,omitempty"`
}

// RelatedContent corresponds to the C# RelatedContent class.
type RelatedContent struct {
	ID   *string `json:"Id,omitempty"`
	Type *string `json:"Type,omitempty"`
	Self *string `json:"Self,omitempty"`
}

// GpsInfo corresponds to the C# GpsInfo class.
type GpsInfo struct {
	Gpstype               *string  `json:"Gpstype,omitempty"`
	Latitude              *float64 `json:"Latitude,omitempty"`
	Longitude             *float64 `json:"Longitude,omitempty"`
	Altitude              *float64 `json:"Altitude,omitempty"`
	AltitudeUnitofMeasure *string  `json:"AltitudeUnitofMeasure,omitempty"`
	Geometry              *string  `json:"Geometry,omitempty"`
	Default               bool     `json:"Default"`
}

// Tag corresponds to the C# TagLinked class used for Content API tag operations.
type Tag struct {
	ID                   *string                      `json:"Id,omitempty"`
	TagName              map[string]string             `json:"TagName"`
	ValidForEntity       []string                      `json:"ValidForEntity"`
	FirstImport          *time.Time                    `json:"FirstImport,omitempty" hash:"ignore"`
	LastChange           *time.Time                    `json:"LastChange,omitempty" hash:"ignore"`
	LicenseInfo          *LicenseInfo                  `json:"LicenseInfo,omitempty" hash:"ignore"`
	Mapping              map[string]map[string]string  `json:"Mapping"`
	PublishDataWithTagOn map[string]bool               `json:"PublishDataWithTagOn,omitempty"`
	PublishedOn          []string                      `json:"PublishedOn,omitempty"`
	Meta                 *Metadata                     `json:"_Meta,omitempty" hash:"ignore"`
	Types                []string                      `json:"Types"`
	Source               string                        `json:"Source"`
	Active               bool                          `json:"Active"`
	Description          map[string]string             `json:"Description"`
}
