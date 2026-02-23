// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package clib

import "context"

// ContentAPI defines the operations available on the Content API.
// ContentClient implements this interface. Use clibmock.ContentMock for testing.
type ContentAPI interface {
	Get(ctx context.Context, apiPath string, queryParams map[string]string, responseStruct interface{}) error
	Post(ctx context.Context, apiPath string, queryParams map[string]string, payload interface{}) error
	Put(ctx context.Context, apiPath string, id string, payload interface{}) error
	PutMultiple(ctx context.Context, apiPath string, payload interface{}) error
}
