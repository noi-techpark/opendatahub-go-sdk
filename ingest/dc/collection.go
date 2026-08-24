// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package dc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/noi-techpark/opendatahub-go-sdk/ingest/rdb"
	"github.com/noi-techpark/opendatahub-go-sdk/tel"
	"github.com/noi-techpark/opendatahub-go-sdk/tel/logger"
	"go.opentelemetry.io/otel/trace"
)

type Collection struct {
	spans        []*trace.Span
	rawWriterURL string
	provider     string
}

func NewCollection(ctx context.Context, rawWriterURL, provider string) (context.Context, *Collection) {
	c := &Collection{
		spans:        make([]*trace.Span, 0),
		rawWriterURL: rawWriterURL,
		provider:     provider,
	}

	// check if the provided context is already recording
	root_span_is_recording := trace.SpanFromContext(ctx).IsRecording()
	if !root_span_is_recording {
		ctx_, serverSpan, collectorSpan := initializeSpans(ctx)
		c.spans = append(c.spans, serverSpan, collectorSpan)
		ctx = ctx_
	}

	// logger uses root span if recording, otherwhise collectorSpan
	ctx = logger.WithTracedLogger(ctx)
	return ctx, c
}

func (c *Collection) Publish(ctx context.Context, raw_data *rdb.RawAny) error {
	var rawBytes []byte
	switch v := raw_data.Rawdata.(type) {
	case []byte:
		rawBytes = v
	case string:
		rawBytes = []byte(v)
	default:
		var err error
		rawBytes, err = json.Marshal(v)
		if err != nil {
			return fmt.Errorf("failed to marshal raw_data: %s", err.Error())
		}
	}
	contentType := raw_data.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	return sendRaw(c.rawWriterURL, c.provider, raw_data.Timestamp, rawBytes, contentType, raw_data.Meta)
}

func (c *Collection) End(ctx context.Context) {
	tel.OnSuccess(ctx)

	// end all spans
	for _, s := range c.spans {
		(*s).End()
	}
}

func initializeSpans(ctx context.Context) (context.Context, *trace.Span, *trace.Span) {
	// root server span to enable RED collection of the collector span
	ctx, serverSpan := tel.TraceStart(
		ctx,
		fmt.Sprintf("%s.trigger", tel.GetServiceName()),
		trace.WithSpanKind(trace.SpanKindServer),
	)

	// collect span creation
	ctx, producerSpan := tel.TraceStart(
		ctx,
		fmt.Sprintf("%s.collect", tel.GetServiceName()),
		trace.WithSpanKind(trace.SpanKindProducer),
	)

	return ctx, &serverSpan, &producerSpan
}

// MetaHeaderPrefix is the header namespace the raw writer harvests into the
// stored document's `meta` sub-document. It must match the writer's
// HEADER_PREFIX setting.
const MetaHeaderPrefix = "X-OpenDataHub-"

// validMetaKey accepts the header-name characters that survive the round trip
// intact. Anything else would either be rejected by net/http or silently
// mangled by header canonicalisation, and a key that changes shape in flight
// cannot be grouped on later.
func validMetaKey(k string) bool {
	if k == "" {
		return false
	}
	for _, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func sendRaw(baseURL, provider string, timestamp time.Time, data []byte, contentType string, meta map[string]string) error {
	parts := strings.SplitN(provider, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("PROVIDER must be in the form 'provider1/provider2', got: %s", provider)
	}
	p1 := url.PathEscape(parts[0])
	p2 := url.PathEscape(parts[1])
	path := fmt.Sprintf("%s/%s/%s/%s", baseURL, p1, p2, url.PathEscape(timestamp.UTC().Format(time.RFC3339)))
	req, err := http.NewRequest(http.MethodPost, path, bytes.NewBuffer(data))
	if err != nil {
		return fmt.Errorf("could not create raw writer request: %w", err)
	}
	req.Header.Set("User-Agent", tel.GetServiceName())
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// Meta fields are refused rather than sanitised: a key silently dropped here
	// resurfaces much later as a reference table that is quietly missing rows.
	for k, v := range meta {
		if !validMetaKey(k) {
			return fmt.Errorf("invalid meta key %q: only letters, digits, '-' and '_' are allowed", k)
		}
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("invalid meta value for %q: must not contain newlines", k)
		}
		req.Header.Set(MetaHeaderPrefix+k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("raw writer request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("raw writer returned non-2xx status %d", resp.StatusCode)
	}
	return nil
}
