// Package oauth implements an OAuth 2.0 device-authorization-grant client
// (the flow Claude Code uses for browser-based login). Endpoints and client ID
// are configured by the caller.
package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config configures a device flow.
type Config struct {
	ClientID       string
	DeviceEndpoint string
	TokenEndpoint  string
	Scopes         []string
	// Client is optional; defaults to http.DefaultClient.
	Client *http.Client
	// Interval overrides the poll interval returned by the server.
	Interval time.Duration
}

// DeviceAuthResponse is the device-authorization response.
type DeviceAuthResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}

// Token is an access-token response.
type Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
}

// BeginDeviceFlow requests device codes from the device endpoint.
func BeginDeviceFlow(ctx context.Context, cfg Config) (*DeviceAuthResponse, error) {
	form := url.Values{}
	form.Set("client_id", cfg.ClientID)
	if len(cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.DeviceEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := cfg.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth: device endpoint returned %d: %s", resp.StatusCode, body)
	}
	var out DeviceAuthResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("oauth: decode device response: %w", err)
	}
	return &out, nil
}

// PollDeviceToken polls the token endpoint until the user authorizes or the
// flow expires. onStatus is called between polls (e.g. to update the user).
func PollDeviceToken(ctx context.Context, cfg Config, dev *DeviceAuthResponse, onStatus func(string)) (*Token, error) {
	interval := cfg.Interval
	if interval <= 0 {
		interval = time.Duration(dev.Interval) * time.Second
		if interval <= 0 {
			interval = 5 * time.Second
		}
	}
	client := cfg.Client
	if client == nil {
		client = http.DefaultClient
	}

	deadline := time.Now().Add(time.Duration(dev.ExpiresIn) * time.Second)
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("oauth: device flow expired")
		}
		token, pending, err := pollOnce(ctx, client, cfg, dev.DeviceCode)
		if err != nil {
			return nil, err
		}
		if !pending {
			return token, nil
		}
		if onStatus != nil {
			onStatus("Waiting for authorization…")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func pollOnce(ctx context.Context, client *http.Client, cfg Config, deviceCode string) (*Token, bool, error) {
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	form.Set("device_code", deviceCode)
	form.Set("client_id", cfg.ClientID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var tok Token
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &tok); err != nil {
			return nil, false, fmt.Errorf("oauth: decode token: %w", err)
		}
		return &tok, false, nil
	}

	// 400/403 with authorization_pending or slow_down means keep polling.
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Error == "authorization_pending" || e.Error == "slow_down" {
		return nil, true, nil
	}
	return nil, false, fmt.Errorf("oauth: token endpoint returned %d: %s", resp.StatusCode, body)
}
