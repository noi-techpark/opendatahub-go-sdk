// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

// Wrapper of leodido/go-urn to handle urn in the OpendataHub domain
package urn

import (
	"strings"

	gourn "github.com/leodido/go-urn"
)

// URN wraps the urn.URN type
type URN struct {
	raw gourn.URN
}

// RawUrnFromProviderURI creates a URN from a provider URI
func RawUrnFromProviderURI(providerURI string) (*URN, bool) {
	rawString := "urn:raw:" + strings.Join(strings.Split(providerURI, "/"), ":")
	u, ok := gourn.Parse([]byte(rawString))
	if !ok {
		return nil, false
	}
	return &URN{raw: *u}, true
}

// Parse parses a string using go-urn. If the urn is invalid returns false
func Parse(rawString string) (*URN, bool) {
	u, ok := gourn.Parse([]byte(rawString))
	if !ok {
		return nil, false
	}
	return &URN{raw: *u}, true
}

// AddNSS adds a namespace-specific string (NSS) to the URN
func (u *URN) AddNSS(c string) error {
	u.raw.SS += ":" + c
	return nil
}

// GetNSSs returns only the NSS parts of the URN, removing "urn" and NID
func (u *URN) GetNSSs() []string {
	return strings.Split(u.raw.SS, ":")
}

// GetNSSWithoutID returns all NSS components except the last, which represents the document ID
func (u *URN) GetNSSWithoutID() []string {
	tok := u.GetNSSs()
	if len(tok) > 1 {
		return tok[:len(tok)-1]
	}
	return nil
}

// GetResourceID returns the last NSS compoenent, which represents the document ID
func (u *URN) GetResourceID() string {
	tok := u.GetNSSs()
	return tok[len(tok)-1]
}

func (u *URN) String() string {
	return u.raw.String()
}
