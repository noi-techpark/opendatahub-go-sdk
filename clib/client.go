// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package clib

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/noi-techpark/opendatahub-go-sdk/tel"
	xhttp "github.com/noi-techpark/opendatahub-go-sdk/tel/http"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// Config holds the necessary configuration for the ContentClient.
type Config struct {
	BaseURL      string
	TokenURL     string
	ClientID     string
	ClientSecret string
	DisableOAuth bool
	RetryMax     int
	Timeout      time.Duration
}

// ContentClient encapsulates the configuration and shared resources for making
// requests to the Content API. It is safe for concurrent use across goroutines.
type ContentClient struct {
	BaseURL string
	client  *retryablehttp.Client
}

// NewContentClient creates a new, configured ContentClient.
func NewContentClient(cfg Config) (*ContentClient, error) {
	baseURL, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}

	retryClient := retryablehttp.NewClient()
	if cfg.RetryMax == 0 {
		cfg.RetryMax = 3
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	retryClient.Logger = nil
	retryClient.HTTPClient.Timeout = cfg.Timeout

	baseTransport := &xhttp.TracingRoundTripper{}
	retryClient.HTTPClient.Transport = baseTransport

	if !cfg.DisableOAuth {
		if cfg.TokenURL == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
			return nil, fmt.Errorf("oauth is enabled but TokenURL, ClientID, or ClientSecret is missing")
		}

		config := &clientcredentials.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			TokenURL:     cfg.TokenURL,
		}

		ts := config.TokenSource(context.Background())
		_, err := ts.Token()
		if err != nil {
			return nil, fmt.Errorf("failed to get initial oauth token: %w", err)
		}

		oauthTransport := &oauth2.Transport{
			Source: ts,
			Base:   baseTransport,
		}

		retryClient.HTTPClient.Transport = oauthTransport
	}

	if baseURL.Path != "" && baseURL.Path[len(baseURL.Path)-1] != '/' {
		baseURL.Path += "/"
	}

	return &ContentClient{
		BaseURL: baseURL.String(),
		client:  retryClient,
	}, nil
}

// contentApiSpan creates and starts a new OpenTelemetry span for an API call.
func contentApiSpan(ctx context.Context, url, method string) (context.Context, trace.Span) {
	ctx, clientSpan := tel.TraceStart(
		ctx,
		fmt.Sprintf("Content Api: [%s] %s", method, url),
		trace.WithSpanKind(trace.SpanKindClient),
	)

	clientSpan.SetAttributes(
		attribute.String("db.name", "content-api"),
		attribute.String("peer.host", "content-api"),
	)
	return ctx, clientSpan
}

// doRequest is a private helper to execute the HTTP request using the configured client.
func (c *ContentClient) doRequest(ctx context.Context, method string, reqURL string, body interface{}) (*http.Response, error) {
	ctx, clientSpan := contentApiSpan(ctx, reqURL, method)
	defer clientSpan.End()

	var reqBody io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			clientSpan.RecordError(err)
			clientSpan.SetStatus(codes.Error, "marshalling payload failed")
			return nil, fmt.Errorf("could not marshal payload: %w", err)
		}
		reqBody = bytes.NewBuffer(jsonData)
	}

	req, err := retryablehttp.NewRequestWithContext(ctx, method, reqURL, reqBody)
	if err != nil {
		clientSpan.RecordError(err)
		clientSpan.SetStatus(codes.Error, "request creation failed")
		return nil, fmt.Errorf("could not create http request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		clientSpan.RecordError(err)
		clientSpan.SetStatus(codes.Error, "http request failed")
		return nil, fmt.Errorf("error during http request: %w", err)
	}

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		clientSpan.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
		clientSpan.SetStatus(codes.Error, "unauthorized")
		return nil, ErrUnauthorized
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			clientSpan.RecordError(readErr)
			clientSpan.SetStatus(codes.Error, "failed to read error response body")
			return nil, fmt.Errorf("API call failed (Status: %d) but failed to read response body: %w", resp.StatusCode, readErr)
		}

		bodyString := string(bodyBytes)
		var specialErr error
		switch bodyString {
		case "Data exists already":
			specialErr = ErrAlreadyExists
		case "No Data":
			specialErr = ErrNoData
		case "Data to update Not Found":
			specialErr = ErrNoDataToUpdate
		}
		if specialErr != nil {
			clientSpan.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
			clientSpan.RecordError(specialErr)
			clientSpan.SetStatus(codes.Error, fmt.Sprintf("HTTP %d error", resp.StatusCode))
			return nil, specialErr
		}

		var errorBody ErrorBody
		unmarshalErr := json.Unmarshal(bodyBytes, &errorBody)

		apiError := &APIError{
			StatusCode: resp.StatusCode,
			URL:        reqURL,
			Body:       errorBody,
			RawBody:    bodyBytes,
		}

		clientSpan.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
		clientSpan.RecordError(apiError)
		clientSpan.SetStatus(codes.Error, fmt.Sprintf("HTTP %d error", resp.StatusCode))

		if unmarshalErr != nil {
			return nil, fmt.Errorf("API call failed (Status: %d). Failed to unmarshal error body: %w. Original error: %s",
				resp.StatusCode, unmarshalErr, apiError.Error())
		}

		return nil, apiError
	}

	return resp, nil
}

// Get performs a GET request to the Content API.
func (c *ContentClient) Get(ctx context.Context, apiPath string, queryParams map[string]string, responseStruct interface{}) error {
	resourceURL, err := url.Parse(c.BaseURL)
	if err != nil {
		return fmt.Errorf("internal error: could not parse base URL: %w", err)
	}

	resourceURL.Path = path.Join(resourceURL.Path, apiPath)

	if len(queryParams) > 0 {
		q := resourceURL.Query()
		for key, value := range queryParams {
			q.Add(key, value)
		}
		resourceURL.RawQuery = q.Encode()
	}

	resp, err := c.doRequest(ctx, http.MethodGet, resourceURL.String(), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	target := responseStruct
	if target == nil {
		var defaultMap map[string]interface{}
		target = &defaultMap
	}

	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		_, clientSpan := contentApiSpan(ctx, resourceURL.String(), http.MethodGet)
		clientSpan.RecordError(err)
		clientSpan.SetStatus(codes.Error, "decoding response failed")
		clientSpan.End()
		return fmt.Errorf("could not decode response: %w", err)
	}

	return nil
}

// Put performs a PUT request to update a content item by ID.
func (c *ContentClient) Put(ctx context.Context, apiPath string, id string, payload interface{}) error {
	resourceURL, err := url.Parse(c.BaseURL)
	if err != nil {
		return fmt.Errorf("internal error: could not parse base URL: %w", err)
	}
	resourceURL.Path = path.Join(resourceURL.Path, apiPath, id)

	resp, err := c.doRequest(ctx, http.MethodPut, resourceURL.String(), payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

// PutMultiple performs a PUT request to upsert a list of entries.
func (c *ContentClient) PutMultiple(ctx context.Context, apiPath string, payload interface{}) error {
	resourceURL, err := url.Parse(c.BaseURL)
	if err != nil {
		return fmt.Errorf("internal error: could not parse base URL: %w", err)
	}
	resourceURL.Path = path.Join(resourceURL.Path, apiPath)

	resp, err := c.doRequest(ctx, http.MethodPut, resourceURL.String(), payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// Post performs a POST request to create a content item.
func (c *ContentClient) Post(ctx context.Context, apiPath string, queryParams map[string]string, payload interface{}) error {
	resourceURL, err := url.Parse(c.BaseURL)
	if err != nil {
		return fmt.Errorf("internal error: could not parse base URL: %w", err)
	}
	resourceURL.Path = path.Join(resourceURL.Path, apiPath)

	if len(queryParams) > 0 {
		q := resourceURL.Query()
		for key, value := range queryParams {
			q.Add(key, value)
		}
		resourceURL.RawQuery = q.Encode()
	}

	resp, err := c.doRequest(ctx, http.MethodPost, resourceURL.String(), payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}
