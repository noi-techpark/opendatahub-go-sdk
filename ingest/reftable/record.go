// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package reftable

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// FieldMeta is the sub-document the raw writer harvests publisher-set headers
// into. Identity and control fields live there because it is the only part of a
// stored document a publisher controls that is not the payload — and therefore
// the only part the database can group on.
const FieldMeta = "meta"

// Control fields, read from inside `meta`. The key field is named by Config.Key
// because the bridge groups on it; these are fixed.
const (
	MetaOp     = "op"
	MetaSchema = "schema"
)

// Fields read from the document root, written by the raw writer itself.
const (
	FieldTimestamp = "timestamp"
	FieldRawdata   = "rawdata"
)

// Operations a publisher can express. An empty op means OpUpsert, so a
// publisher only has to think about this when deleting.
const (
	OpUpsert = "upsert"
	OpDelete = "delete"
)

// Decoder turns the stored `rawdata` value into the payload bytes.
//
// The representation depends on how the document was published, and that is a
// deployment fact rather than something to guess at. Declare which one you
// publish.
type Decoder func(json.RawMessage) ([]byte, error)

// DecodeText reads `rawdata` as a JSON string whose contents are the payload.
//
// This is what raw-writer-2 stores for every textual content type
// (application/json included): the body is kept verbatim as a string rather
// than parsed, so it has to be unwrapped once before it can be decoded. It is
// the default, because it is what the HTTP write path produces.
func DecodeText(raw json.RawMessage) ([]byte, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("rawdata is not a string: %w", err)
	}
	return []byte(s), nil
}

// DecodeBase64 reads `rawdata` as a base64 string, which is what the legacy
// rest-push collector produced.
func DecodeBase64(raw json.RawMessage) ([]byte, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("rawdata is not a string, so it cannot be base64: %w", err)
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("rawdata is not valid base64: %w", err)
	}
	return b, nil
}

// DecodeJSON reads `rawdata` as an already-decoded JSON value. This is what the
// deprecated queue-based writer produced, which parsed JSON bodies on the way
// in.
func DecodeJSON(raw json.RawMessage) ([]byte, error) { return raw, nil }

// record is one reference document, as the pipeline actually stores it.
//
// There is no separate envelope inside the payload: the document *is* the
// envelope. Identity, op and schema are set by the publisher as
// X-OpenDataHub-* headers and land together under `meta`; `rawdata` carries
// only the domain payload. That keeps the key in exactly one place, which
// matters because `meta` is the only place the bridge can group on.
type record[T any] struct {
	Key       string
	Op        string
	Schema    string
	Timestamp time.Time
	Data      T
}

// parseRecord pulls a record out of a stored document.
func parseRecord[T any](doc json.RawMessage, keyField string, decode Decoder) (record[T], error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(doc, &fields); err != nil {
		return record[T]{}, fmt.Errorf("document is not a json object: %w", err)
	}

	var r record[T]

	rawMeta, ok := fields[FieldMeta]
	if !ok {
		return r, fmt.Errorf("document has no %q sub-document, so it carries no identity", FieldMeta)
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(rawMeta, &meta); err != nil {
		return r, fmt.Errorf("%q is not a json object: %w", FieldMeta, err)
	}

	rawKey, ok := meta[keyField]
	if !ok {
		return r, fmt.Errorf("document has no %s.%s field", FieldMeta, keyField)
	}
	if err := json.Unmarshal(rawKey, &r.Key); err != nil {
		// A non-scalar key cannot be grouped or paged on, so it is a publisher
		// error rather than something to coerce.
		return r, fmt.Errorf("field %s.%s is not a string: %w", FieldMeta, keyField, err)
	}
	if r.Key == "" {
		return r, fmt.Errorf("field %s.%s is empty", FieldMeta, keyField)
	}

	// Optional control fields; absence is normal.
	_ = json.Unmarshal(meta[MetaOp], &r.Op)
	_ = json.Unmarshal(meta[MetaSchema], &r.Schema)
	_ = json.Unmarshal(fields[FieldTimestamp], &r.Timestamp)

	// A tombstone carries no payload worth decoding.
	if r.Op == OpDelete {
		return r, nil
	}

	rawPayload, ok := fields[FieldRawdata]
	if !ok {
		return r, fmt.Errorf("document %q has no %q field", r.Key, FieldRawdata)
	}
	payload, err := decode(rawPayload)
	if err != nil {
		return r, fmt.Errorf("document %q: %w", r.Key, err)
	}
	// UseNumber rather than a plain Unmarshal: the default decoder turns every
	// JSON number into a float64, whose 53-bit mantissa silently rounds a large
	// integer landing in an untyped map. Keeping the literal means a record's
	// value survives the trip through the table exactly as it was published —
	// an integer stays an integer, a decimal keeps its digits — which is what
	// lets a consumer merge it into an output without changing it. Decoding
	// into a typed T is unaffected, since the field's own type governs there.
	d := json.NewDecoder(bytes.NewReader(payload))
	d.UseNumber()
	if err := d.Decode(&r.Data); err != nil {
		return r, fmt.Errorf("document %q payload does not fit the target type: %w", r.Key, err)
	}
	return r, nil
}
