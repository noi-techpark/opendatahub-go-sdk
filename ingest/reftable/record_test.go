// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package reftable

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type enrichment struct {
	Name         string `json:"name"`
	Municipality string `json:"municipality"`
}

// storedDoc builds a document exactly as the pipeline stores it: identity and
// control fields under `meta` (set by the publisher as X-OpenDataHub-* headers
// and flattened there by the raw writer), the writer's own fields at the root,
// and the payload in rawdata.
func storedDoc(key, op, schema, payload string, ts time.Time) json.RawMessage {
	meta := map[string]any{"key": key}
	if op != "" {
		meta["op"] = op
	}
	if schema != "" {
		meta["schema"] = schema
	}
	d := map[string]any{
		"meta":         meta,
		"provider":     "enrichment/parking",
		"timestamp":    ts.Format(time.RFC3339Nano),
		"content_type": "application/json",
		"provenance":   "test",
	}
	if payload != "" {
		d["rawdata"] = base64.StdEncoding.EncodeToString([]byte(payload))
	}
	b, _ := json.Marshal(d)
	return b
}

func TestParseRecordReadsMetaFieldsAndBase64Payload(t *testing.T) {
	ts := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	doc := storedDoc("A", "", "parking.v1", `{"name":"Laurin","municipality":"Bolzano"}`, ts)

	r, err := parseRecord[enrichment](doc, "key", DecodeBase64)
	if err != nil {
		t.Fatalf("parseRecord: %v", err)
	}
	if r.Key != "A" {
		t.Errorf("Key = %q, want A — the key lives under meta", r.Key)
	}
	if r.Schema != "parking.v1" {
		t.Errorf("Schema = %q", r.Schema)
	}
	if !r.Timestamp.Equal(ts) {
		t.Errorf("Timestamp = %v, want %v — timestamp stays at the root", r.Timestamp, ts)
	}
	if r.Data.Name != "Laurin" || r.Data.Municipality != "Bolzano" {
		t.Errorf("Data = %+v, want the decoded payload", r.Data)
	}
}

// A key at the document root is the pre-meta shape. It must not be honoured,
// because the bridge cannot group on it — accepting it here would let a
// publisher build a table that bootstraps empty in production.
func TestParseRecordIgnoresRootLevelKey(t *testing.T) {
	doc := json.RawMessage(`{"key":"A","timestamp":"2026-08-11T09:00:00Z","rawdata":"e30="}`)
	if _, err := parseRecord[enrichment](doc, "key", DecodeBase64); err == nil {
		t.Fatal("a root-level key was accepted; the bridge cannot group on it")
	}
}

func TestParseRecordTombstoneNeedsNoPayload(t *testing.T) {
	doc := storedDoc("A", OpDelete, "", "", time.Now().UTC())
	r, err := parseRecord[enrichment](doc, "key", DecodeBase64)
	if err != nil {
		t.Fatalf("a tombstone must parse without a payload: %v", err)
	}
	if r.Op != OpDelete {
		t.Errorf("Op = %q, want %q", r.Op, OpDelete)
	}
}

func TestParseRecordRejects(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	cases := []struct {
		name string
		doc  json.RawMessage
		want string
	}{
		{"no meta at all", json.RawMessage(`{"provider":"p","rawdata":"e30="}`), `has no "meta"`},
		{"meta not an object", json.RawMessage(`{"meta":"x","rawdata":"e30="}`), `is not a json object`},
		{"no key field", json.RawMessage(`{"meta":{"other":"z"},"rawdata":"e30="}`), `has no meta.key`},
		{"empty key", json.RawMessage(`{"meta":{"key":""},"rawdata":"e30="}`), `is empty`},
		{"non-scalar key", json.RawMessage(`{"meta":{"key":["A"]},"rawdata":"e30="}`), `is not a string`},
		{"no payload", json.RawMessage(`{"meta":{"key":"A"}}`), `has no "rawdata" field`},
		{"payload not base64", json.RawMessage(`{"meta":{"key":"A"},"rawdata":"!!!not base64!!!"}`), `not valid base64`},
		{"payload not json", json.RawMessage(`{"meta":{"key":"A"},"rawdata":"` + b64("plain text") + `"}`), `does not fit`},
		{"not an object", json.RawMessage(`"scalar"`), `not a json object`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseRecord[enrichment](c.doc, "key", DecodeBase64)
			if err == nil {
				t.Fatal("parseRecord succeeded, want an error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// An array-valued key cannot be grouped or paged on, so it must be refused
// rather than coerced.
func TestParseRecordRefusesArrayKey(t *testing.T) {
	_, err := parseRecord[enrichment](
		json.RawMessage(`{"meta":{"key":["A","B"]},"rawdata":"e30="}`), "key", DecodeBase64)
	if err == nil {
		t.Fatal("an array key was accepted")
	}
}

func TestDecoders(t *testing.T) {
	payload := `{"name":"x"}`

	// DecodeText is what raw-writer-2 produces: the body kept verbatim as a
	// JSON string rather than parsed.
	stored, _ := json.Marshal(payload)
	b, err := DecodeText(stored)
	if err != nil || string(b) != payload {
		t.Errorf("DecodeText = %q, %v", b, err)
	}
	if _, err := DecodeText(json.RawMessage(`{"name":"x"}`)); err == nil {
		t.Error("DecodeText accepted an object; it must say so rather than guess")
	}

	b, err = DecodeBase64(json.RawMessage(
		`"` + base64.StdEncoding.EncodeToString([]byte(payload)) + `"`))
	if err != nil || string(b) != payload {
		t.Errorf("DecodeBase64 = %q, %v", b, err)
	}
	if _, err := DecodeBase64(json.RawMessage(`{"name":"x"}`)); err == nil {
		t.Error("DecodeBase64 accepted an object; it must say so rather than guess")
	}

	b, err = DecodeJSON(json.RawMessage(payload))
	if err != nil || string(b) != payload {
		t.Errorf("DecodeJSON = %q, %v", b, err)
	}
}

// The default matters: it is what every table gets that does not think about
// encodings, and the HTTP write path is now the only write path.
func TestDefaultDecoderIsText(t *testing.T) {
	tbl := New[enrichment](nil, Config{DB: "d", Collection: "c", Key: "key"})
	stored, _ := json.Marshal(`{"name":"x"}`)
	b, err := tbl.cfg.Decode(stored)
	if err != nil || string(b) != `{"name":"x"}` {
		t.Errorf("default decoder = %q, %v; want DecodeText behaviour", b, err)
	}
}

// A record's numbers must survive decoding exactly as published.
//
// The default decoder turns every JSON number into a float64, whose 53-bit
// mantissa silently rounds anything past 2^53 — the kind of corruption that
// surfaces much later as an id that stops matching. A consumer merging a record
// into an output has to be able to trust that the table did not change it.
func TestRecordNumbersSurviveDecodingExactly(t *testing.T) {
	const big = "9007199254740993" // 2^53 + 1, not representable as a float64
	payload := `{"big":` + big + `,"decimal":46.71602,"small":245,"zero":0,"neg":-7}`

	r, err := parseRecord[map[string]any](
		storedDoc("A", "", "parking.v1", payload, time.Now().UTC()), "key", DecodeBase64)
	if err != nil {
		t.Fatalf("parseRecord: %v", err)
	}

	for key, want := range map[string]string{
		"big": big, "decimal": "46.71602", "small": "245", "zero": "0", "neg": "-7",
	} {
		got, ok := r.Data[key]
		if !ok {
			t.Errorf("%s missing from the decoded record", key)
			continue
		}
		if fmt.Sprint(got) != want {
			t.Errorf("%s = %v (%T), want the literal %s", key, got, got, want)
		}
	}

	// And it re-serializes as a number, not as a quoted string, or every
	// consumer downstream would see a different type than was published.
	out, err := json.Marshal(r.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"big":`+big) {
		t.Errorf("re-marshalled as %s, want big to stay a bare number", out)
	}
}

// Decoding into a typed record is governed by the field's own type, so nothing
// about the above leaks into consumers that declare a struct.
func TestTypedRecordsAreUnaffectedByNumberPreservation(t *testing.T) {
	type sized struct {
		Capacity int     `json:"capacity"`
		Lat      float64 `json:"lat"`
	}
	r, err := parseRecord[sized](
		storedDoc("A", "", "", `{"capacity":245,"lat":46.71602}`, time.Now().UTC()),
		"key", DecodeBase64)
	if err != nil {
		t.Fatalf("parseRecord: %v", err)
	}
	if r.Data.Capacity != 245 || r.Data.Lat != 46.71602 {
		t.Errorf("typed record = %+v", r.Data)
	}
}
