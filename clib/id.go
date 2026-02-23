// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package clib

import (
	"fmt"

	"github.com/google/uuid"
)

// IDNamespace is a fixed UUID v5 namespace for generating deterministic entity IDs.
var IDNamespace = uuid.MustParse("d5697669-e0d5-4521-995a-c5c83f90117a")

// GenerateDeterministicUUID returns a UUID v5 (SHA-1) from the fixed namespace
// and the given input string. The same input always produces the same UUID.
func GenerateDeterministicUUID(input string) uuid.UUID {
	return uuid.NewSHA1(IDNamespace, []byte(input))
}

// GenerateID builds an entity ID in the format "{prefix}:{uuid5(input)}".
// The prefix is typically a URN like "urn:announcements:provincebz".
// The input should be a unique string known by the caller.
func GenerateID(prefix string, input string) string {
	return fmt.Sprintf("%s:%s", prefix, GenerateDeterministicUUID(input).String())
}
