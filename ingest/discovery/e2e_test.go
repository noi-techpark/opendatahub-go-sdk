// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

// The check no fake can substitute for: a Reporter writing through a real
// raw-writer-2 into a real MongoDB, and reftable reading it back through a real
// raw-data-bridge with the decoder a consumer uses.
//
//	cd infrastructure-v2
//	podman compose up -d mongodb mongodb-init rabbitmq minio minio-init writer-2 bridge
//	RDB_E2E_BRIDGE=http://localhost:2000 RDB_E2E_WRITER=http://localhost:9021 \
//	  go test ./discovery/ -run E2E -v

package discovery

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/noi-techpark/opendatahub-go-sdk/ingest/rdb"
	"github.com/noi-techpark/opendatahub-go-sdk/ingest/reftable"
)

func TestE2EReporterWritesWhatReftableReads(t *testing.T) {
	bridgeURL, writerURL := os.Getenv("RDB_E2E_BRIDGE"), os.Getenv("RDB_E2E_WRITER")
	if bridgeURL == "" || writerURL == "" {
		t.Skip("RDB_E2E_BRIDGE / RDB_E2E_WRITER not set")
	}
	ctx := context.Background()
	// A database per run, so leftovers of other runs are never read.
	db := fmt.Sprintf("e2e_%d", time.Now().UnixNano())
	provider := db + "/parking-discovery"
	bridge := rdb.NewRDBridge(rdb.Env{RAW_DATA_BRIDGE_ENDPOINT: strings.TrimRight(bridgeURL, "/")})

	read := func() map[string]Record {
		t.Helper()
		tbl := reftable.New[Record](bridge, reftable.Config{
			DB: db, Collection: "parking-discovery", Key: KeyField,
			Schema: Schema, Decode: reftable.DecodeText,
		})
		if err := reftable.NewSet(tbl).Start(ctx); err != nil {
			t.Fatalf("read back: %v", err)
		}
		return tbl.All()
	}
	newReporter := func(onNew func(string)) *Reporter {
		r := New(bridge, Config{
			Provider: provider, RawWriterURL: strings.TrimRight(writerURL, "/"), GoneAfter: 1,
			OnNew: func(_ context.Context, key string, _ Record) { onNew(key) },
		})
		if err := r.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		return r
	}
	snapshot := func(r *Reporter) map[string]Record {
		r.mu.Lock()
		defer r.mu.Unlock()
		out := map[string]Record{}
		for k, it := range r.items {
			out[k] = it.rec
		}
		return out
	}

	var created []string
	r := newReporter(func(k string) { created = append(created, k) })

	lat, lon := 46.4983, 11.3548
	r.Observe("P1", Entry{
		Status: StatusPublished, Reason: "no-record", Name: "Laurin", Lat: &lat, Lon: &lon,
		Data: map[string]any{"capacity": 120, "names": []string{"a", "b"}},
		Seed: map[string]any{"name": "Laurin", "gps.lat": lat},
	})
	r.Observe("P2", Entry{Status: StatusWithheld, Reason: "parent-incomplete", Name: "Garage", Parent: "P1"})
	if err := r.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !reflect.DeepEqual(created, []string{"P1", "P2"}) && !reflect.DeepEqual(created, []string{"P2", "P1"}) {
		t.Errorf("OnNew = %v", created)
	}

	t.Run("the table reads back what the reporter holds", func(t *testing.T) {
		if got, want := read(), snapshot(r); !reflect.DeepEqual(got, want) {
			t.Errorf("read back\n%#v\nwant\n%#v", got, want)
		}
	})

	t.Run("a restart seeds from the table and rewrites nothing", func(t *testing.T) {
		before := read()
		var again []string
		r2 := newReporter(func(k string) { again = append(again, k) })
		r2.Observe("P2", Entry{Status: StatusWithheld, Reason: "parent-incomplete", Name: "Garage", Parent: "P1"})
		r2.Observe("P1", Entry{
			Status: StatusPublished, Reason: "no-record", Name: "Laurin", Lat: &lat, Lon: &lon,
			Data: map[string]any{"capacity": 120, "names": []string{"a", "b"}},
			Seed: map[string]any{"name": "Laurin", "gps.lat": lat},
		})
		if err := r2.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		if len(again) != 0 {
			t.Errorf("OnNew fired for known keys: %v", again)
		}
		if got := read(); !reflect.DeepEqual(got, before) {
			t.Errorf("an unchanged restart changed the table:\n%+v\nwas\n%+v", got, before)
		}
	})

	t.Run("a change and a gone key supersede the stored document", func(t *testing.T) {
		first := read()["P2"].FirstSeen
		r3 := newReporter(func(string) {})
		r3.Observe("P2", Entry{Status: StatusPublished, Name: "Garage", Parent: "P1"})
		r3.Complete() // P1 is not in the snapshot
		if err := r3.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		got := read()
		if got["P2"].Status != StatusPublished || !got["P2"].FirstSeen.Equal(first) {
			t.Errorf("P2 = %+v, want published with FirstSeen %v", got["P2"], first)
		}
		if got["P1"].Status != StatusGone || got["P1"].Name != "Laurin" {
			t.Errorf("P1 = %+v, want gone with its fields kept", got["P1"])
		}
	})
}
