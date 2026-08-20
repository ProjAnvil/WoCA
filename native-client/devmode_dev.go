//go:build dev

package main

import (
	"context"
	"net/url"
	"os"
	"strings"

	"github.com/projanvil/woca/native-client/internal/bridge"
	"github.com/projanvil/woca/native-client/internal/transport"
)

func developmentServerURL() string { return os.Getenv("WOCA_NATIVE_DEV_BFF_URL") }

type developmentCredentials struct {
	baseURL string
}

func (d developmentCredentials) Credentials(_ context.Context, _ string, target *url.URL) (transport.Credentials, error) {
	if target == nil || !strings.EqualFold(target.Scheme+"://"+target.Host, d.baseURL) {
		return transport.Credentials{}, authorizationRequiredError{Code: "authorization_required"}
	}
	return transport.Credentials{}, nil
}

func configureCredentials(ctx context.Context, service *bridge.Service, developmentURL string, production func() transport.CredentialProvider) (transport.CredentialProvider, error) {
	if developmentURL == "" {
		return production(), nil
	}
	baseURL, err := service.ApplyDevelopmentOverride(ctx, developmentURL)
	if err != nil {
		return nil, err
	}
	return developmentCredentials{baseURL: baseURL}, nil
}
