//go:build !dev

package main

import (
	"context"
	"errors"

	"github.com/projanvil/woca/native-client/internal/bridge"
	"github.com/projanvil/woca/native-client/internal/transport"
)

func developmentServerURL() string { return "" }

func configureCredentials(_ context.Context, _ *bridge.Service, developmentURL string) (transport.CredentialProvider, error) {
	if developmentURL != "" {
		return nil, errors.New("development server override is unavailable in production builds")
	}
	return productionCredentials{}, nil
}
