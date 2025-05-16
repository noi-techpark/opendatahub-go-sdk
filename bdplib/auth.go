// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package bdplib

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Token struct {
	AccessToken      string `json:"access_token"`
	ExpiresIn        int64  `json:"expires_in"`
	NotBeforePolicy  int64  `json:"not-before-policy"`
	RefreshExpiresIn int64  `json:"refresh_expires_in"`
	TokenType        string `json:"token_type"`
	RefreshToken     string `json:"refresh_token"`
	Scope            string
}

type Auth struct {
	TokenUrl     string
	ClientId     string
	ClientSecret string
	token        Token
	tokenExpiry  int64
}

func AuthFromEnv() *Auth {
	a := Auth{}
	a.TokenUrl = os.Getenv("ODH_TOKEN_URL")
	a.ClientId = os.Getenv("ODH_CLIENT_ID")
	a.ClientSecret = os.Getenv("ODH_CLIENT_SECRET")
	return &a
}

func (a *Auth) getToken() string {
	ts := time.Now().Unix()

	if len(a.token.AccessToken) == 0 || ts > a.tokenExpiry {
		// if no token is available or refreshToken is expired, get new token
		a.newToken()
	}

	return a.token.AccessToken
}

func (a *Auth) newToken() {
	slog.Info("Getting new token...")
	params := url.Values{}
	params.Add("client_id", a.ClientId)
	params.Add("client_secret", a.ClientSecret)
	params.Add("grant_type", "client_credentials")

	a.authRequest(params)

	slog.Info("Getting new token done.")
}

func (a *Auth) authRequest(params url.Values) {
	body := strings.NewReader(params.Encode())

	req, err := http.NewRequest("POST", a.TokenUrl, body)
	if err != nil {
		slog.Error("error", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("error", "err", err)
		return
	}
	defer resp.Body.Close()

	slog.Info("Auth response code is: " + strconv.Itoa(resp.StatusCode))
	if resp.StatusCode == http.StatusOK {
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			slog.Error("error", "err", err)
			return
		}

		err = json.Unmarshal(bodyBytes, &a.token)
		if err != nil {
			slog.Error("error", "err", err)
			return
		}
	}

	// calculate token expiry timestamp with 600 seconds margin
	a.tokenExpiry = time.Now().Unix() + a.token.ExpiresIn - 600

	slog.Debug("auth token expires in " + strconv.FormatInt(a.tokenExpiry, 10))
}
