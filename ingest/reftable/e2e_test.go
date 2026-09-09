// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

// The check no fake can substitute for: a real raw-writer-2, a real MongoDB and
// a real raw-data-bridge, driven through the same client a transformer uses.
//
// Every earlier defect in this path lived in the gap between what the fake
// bridge modelled and what the real stack actually does — the payload encoding, the
// place the key is stored, the shape of the envelope. A fake cannot find those
// by construction, because it was written from the same misunderstanding.
//
//	cd infrastructure-v2
//	podman compose up -d mongodb mongodb-init rabbitmq minio minio-init writer-2 bridge
//	RDB_E2E_BRIDGE=http://localhost:2000 RDB_E2E_WRITER=http://localhost:9021 \
//	  go test ./reftable/ -run E2E -v

package reftable

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/noi-techpark/opendatahub-go-sdk/ingest/rdb"
)

type stack struct {
	bridge string
	writer string
	db     string
}

func e2eOrSkip(t *testing.T) stack {
	t.Helper()
	b, w := os.Getenv("RDB_E2E_BRIDGE"), os.Getenv("RDB_E2E_WRITER")
	if b == "" || w == "" {
		t.Skip("RDB_E2E_BRIDGE / RDB_E2E_WRITER not set")
	}
	// A database per run: these tests write to a real lake and must not read
	// each other's leftovers.
	return stack{
		bridge: strings.TrimRight(b, "/"),
		writer: strings.TrimRight(w, "/"),
		db:     fmt.Sprintf("e2e_%d", time.Now().UnixNano()),
	}
}

// publish posts a record exactly as a collector does: the key and control fields
// as X-OpenDataHub-* headers, the payload as the body.
func (s stack) publish(t *testing.T, coll, key, op, schema, contentType, body string, ts time.Time) {
	t.Helper()
	url := fmt.Sprintf("%s/%s/%s/%s", s.writer, s.db, coll, ts.UTC().Format(time.RFC3339))
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "reftable-e2e")
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-OpenDataHub-key", key)
	if op != "" {
		req.Header.Set("X-OpenDataHub-op", op)
	}
	if schema != "" {
		req.Header.Set("X-OpenDataHub-schema", schema)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("publish %s: %v", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("publish %s: status %d: %s", key, resp.StatusCode, b)
	}
}

func (s stack) table(t *testing.T, cfg Config) *Table[enrichment] {
	t.Helper()
	cfg.DB = s.db
	if cfg.Key == "" {
		cfg.Key = "key"
	}
	return New[enrichment](
		rdb.NewRDBridge(rdb.Env{RAW_DATA_BRIDGE_ENDPOINT: s.bridge}), cfg)
}

// The whole reference-table contract, against the real stack.
func TestE2EReferenceTable(t *testing.T) {
	s := e2eOrSkip(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)

	const coll = "enrichment"
	// Five keys, one superseded, so compaction has something to collapse.
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("K%03d", i)
		s.publish(t, coll, key, "", "test.v1", "application/json",
			payload("first-"+key), base)
	}
	s.publish(t, coll, "K002", "", "test.v1", "application/json",
		payload("second-K002"), base.Add(time.Minute))

	// PageLimit 2 over 5 keys: the walk has to page, and the transformer's
	// bootstrap is the thing that has to survive it.
	tbl := s.table(t, Config{Name: "e2e", Collection: coll, Schema: "test.v1",
		Decode: DecodeText, PageLimit: 2})

	t.Run("bootstrap pages and compacts", func(t *testing.T) {
		if err := tbl.bootstrap(ctx); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		if tbl.Len() != 5 {
			t.Fatalf("keys = %d, want 5", tbl.Len())
		}
		if v, ok := tbl.Get("K002"); !ok || v.Name != "second-K002" {
			t.Errorf("K002 = %+v, want the newer revision", v)
		}
		if v, ok := tbl.Get("K000"); !ok || v.Name != "first-K000" {
			t.Errorf("K000 = %+v", v)
		}
	})

	t.Run("a reconcile with nothing new reports no change", func(t *testing.T) {
		changed, err := tbl.reconcile(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if changed != 0 {
			t.Errorf("changed = %d on an unchanged collection, want 0", changed)
		}
	})

	t.Run("an operator edit is picked up by a reconcile", func(t *testing.T) {
		s.publish(t, coll, "K004", "", "test.v1", "application/json",
			payload("edited"), base.Add(2*time.Minute))
		changed, err := tbl.reconcile(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if changed != 1 {
			t.Errorf("changed = %d, want 1", changed)
		}
		if v, _ := tbl.Get("K004"); v.Name != "edited" {
			t.Errorf("K004 = %q, want edited", v.Name)
		}
	})

	// The case an incremental catch-up on the publisher's clock would lose:
	// written now, dated before everything the table has already seen.
	t.Run("a record dated in the past still arrives", func(t *testing.T) {
		s.publish(t, coll, "K999", "", "test.v1", "application/json",
			payload("backdated"), base.AddDate(-3, 0, 0))
		if _, err := tbl.reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if v, ok := tbl.Get("K999"); !ok || v.Name != "backdated" {
			t.Error("a record dated three years ago never reached the table")
		}
	})

	t.Run("a tombstone removes the key", func(t *testing.T) {
		s.publish(t, coll, "K003", OpDelete, "test.v1", "application/json",
			"{}", base.Add(3*time.Minute))
		if _, err := tbl.reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if _, ok := tbl.Get("K003"); ok {
			t.Error("K003 is still in the table after a delete tombstone")
		}
	})

	t.Run("a record stamped with another schema is refused", func(t *testing.T) {
		s.publish(t, coll, "K001", "", "other.v9", "application/json",
			payload("wrong-schema"), base.Add(4*time.Minute))
		if _, err := tbl.reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if v, ok := tbl.Get("K001"); ok && v.Name == "wrong-schema" {
			t.Error("a record stamped other.v9 was applied to a table declaring test.v1")
		}
	})
}

// The other live encoding. raw-writer-2 stores a textual body verbatim and a
// non-textual one as binary, which the bridge returns base64-encoded — so which
// Decoder is correct is a fact about how the collector publishes, not a
// preference. parking-skidata runs both.
func TestE2EBinaryPayloadNeedsDecodeBase64(t *testing.T) {
	s := e2eOrSkip(t)
	ctx := context.Background()
	const coll = "binary_enrichment"

	s.publish(t, coll, "B1", "", "test.v1", "application/octet-stream",
		payload("binary-published"), time.Now().UTC())

	t.Run("DecodeBase64 reads it", func(t *testing.T) {
		tbl := s.table(t, Config{Name: "e2e-b64", Collection: coll, Decode: DecodeBase64})
		if err := tbl.bootstrap(ctx); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		if v, ok := tbl.Get("B1"); !ok || v.Name != "binary-published" {
			t.Errorf("B1 = %+v, want the decoded payload", v)
		}
	})

	// And the wrong Decoder loses the record rather than failing loudly, which
	// is the trap the two-encoding deployment sets.
	t.Run("DecodeText silently drops it", func(t *testing.T) {
		tbl := s.table(t, Config{Name: "e2e-text", Collection: coll, Decode: DecodeText})
		if err := tbl.bootstrap(ctx); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		if tbl.Len() != 0 {
			t.Skip("the wrong decoder happened to parse this payload")
		}
		t.Log("confirmed: the wrong Decoder yields an empty table and no error")
	})
}

// A collection that does not exist must stop a transformer starting, not hand it
// an empty table it would publish over live data.
func TestE2EBootstrapFailsOnATypo(t *testing.T) {
	s := e2eOrSkip(t)
	tbl := s.table(t, Config{Name: "e2e-typo", Collection: "no_such_collection"})
	err := tbl.bootstrap(context.Background())
	if err == nil {
		t.Fatal("bootstrap succeeded against a collection that does not exist")
	}
	if !errors.Is(err, rdb.ErrCollectionNotFound) {
		t.Errorf("err = %v, want it to wrap ErrCollectionNotFound", err)
	}
}

// The last document written to a collection, which is the other thing the bridge
// is asked for.
func TestE2ELastDocument(t *testing.T) {
	s := e2eOrSkip(t)
	ctx := context.Background()
	const coll = "log"
	bridge := rdb.NewRDBridge(rdb.Env{RAW_DATA_BRIDGE_ENDPOINT: s.bridge})

	if _, err := bridge.GetLastDocument(ctx, s.db, coll); !errors.Is(err, rdb.ErrCollectionNotFound) {
		t.Errorf("err = %v, want ErrCollectionNotFound before anything is written", err)
	}

	base := time.Now().UTC()
	s.publish(t, coll, "L1", "", "", "application/json", payload("older"), base)
	s.publish(t, coll, "L2", "", "", "application/json", payload("newest"), base.Add(time.Minute))

	doc, err := bridge.GetLastDocument(ctx, s.db, coll)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "newest") {
		t.Errorf("last document = %s, want the most recently written", doc)
	}
}

// The lifecycle a transformer actually runs: Set.Start bootstraps fail-closed,
// then the background reconcile corrects the table without anyone asking.
func TestE2ESetStartAndBackgroundReconcile(t *testing.T) {
	s := e2eOrSkip(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const coll = "lifecycle"

	base := time.Now().UTC()
	s.publish(t, coll, "S1", "", "test.v1", "application/json", payload("initial"), base)

	changes := make(chan int, 8)
	tbl := s.table(t, Config{
		Name: "e2e-lifecycle", Collection: coll, Schema: "test.v1",
		Decode: DecodeText, Sweep: time.Second,
		OnChange: func(ctx context.Context, applied int) { changes <- applied },
	})

	set := NewSet(tbl)
	if err := set.Start(ctx); err != nil {
		t.Fatalf("Set.Start: %v", err)
	}
	defer set.Close()

	if v, ok := tbl.Get("S1"); !ok || v.Name != "initial" {
		t.Fatalf("S1 = %+v after Start", v)
	}
	select {
	case n := <-changes:
		t.Errorf("OnChange fired with %d on an unchanged table", n)
	case <-time.After(2 * time.Second):
	}

	// An operator edit, with nobody calling reconcile.
	s.publish(t, coll, "S2", "", "test.v1", "application/json", payload("added"), base.Add(time.Minute))

	select {
	case n := <-changes:
		if n != 1 {
			t.Errorf("OnChange applied = %d, want 1", n)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the background reconcile never picked up the new record")
	}
	if v, ok := tbl.Get("S2"); !ok || v.Name != "added" {
		t.Errorf("S2 = %+v, want added", v)
	}
}

// The queue path, end to end: raw-writer-2 publishes to `ready`, the router
// derives a routing key of {db}.{collection} and republishes to `routed`, and the
// table's subscription turns that into a fetch of the one document by URN.
//
// Sweep is set far beyond the test's patience on purpose. Nothing but the
// notification can deliver this change, so a pass means the queue path works
// rather than that the reconcile covered for it.
func TestE2EQueueNotificationAppliesAChange(t *testing.T) {
	s := e2eOrSkip(t)
	mq := os.Getenv("RDB_E2E_MQ")
	if mq == "" {
		t.Skip("RDB_E2E_MQ not set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const coll = "notified"

	base := time.Now().UTC()
	s.publish(t, coll, "N1", "", "test.v1", "application/json", payload("initial"), base)

	changes := make(chan int, 8)
	tbl := s.table(t, Config{
		Name: "e2e-mq", Collection: coll, Schema: "test.v1", Decode: DecodeText,
		Sweep:     time.Hour,
		MQ_URI:    mq,
		MQ_CLIENT: "reftable-e2e",
		// The router's routing key is the URN's namespace without the document
		// id, joined by '.', which is exactly {db}.{collection}.
		MQ_EXCHANGE: "routed",
		MQ_KEY:      s.db + "." + coll,
		MQ_QUEUE:    "reftable-e2e-" + s.db,
		OnChange:    func(ctx context.Context, applied int) { changes <- applied },
	})

	set := NewSet(tbl)
	if err := set.Start(ctx); err != nil {
		t.Fatalf("Set.Start: %v", err)
	}
	defer set.Close()

	if v, ok := tbl.Get("N1"); !ok || v.Name != "initial" {
		t.Fatalf("N1 = %+v after Start", v)
	}

	// Published after the subscription is bound, so the notification cannot be
	// lost before anything is listening.
	s.publish(t, coll, "N2", "", "test.v1", "application/json", payload("by-notification"), base.Add(time.Minute))

	select {
	case n := <-changes:
		if n != 1 {
			t.Errorf("OnChange applied = %d, want 1", n)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no notification arrived; the queue path did not deliver the change")
	}
	if v, ok := tbl.Get("N2"); !ok || v.Name != "by-notification" {
		t.Errorf("N2 = %+v, want the notified record", v)
	}

	// An edit to an existing key travels the same way.
	s.publish(t, coll, "N1", "", "test.v1", "application/json", payload("edited"), base.Add(2*time.Minute))
	select {
	case <-changes:
	case <-time.After(30 * time.Second):
		t.Fatal("no notification for the edit")
	}
	if v, _ := tbl.Get("N1"); v.Name != "edited" {
		t.Errorf("N1 = %q, want edited", v.Name)
	}
}
