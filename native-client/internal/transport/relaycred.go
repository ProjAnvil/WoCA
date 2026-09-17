package transport

import (
	"context"
	"net/url"
)

// StaticBearer is the relay profile's CredentialProvider (design
// 2026-09-17-personal-relay §3.3): the relay's login response carries the
// opaque session token in its body, and the native client presents it as
// a Bearer credential on every request — direct SSE included, where the
// asset proxy's cookie channel does not reach. The transport itself
// (FlushInterval: -1, SSE-safe) needs no changes.
type StaticBearer struct {
	Token string
}

// Credentials returns the static Bearer token; method and target are
// irrelevant to a relay session.
func (s StaticBearer) Credentials(context.Context, string, *url.URL) (Credentials, error) {
	return Credentials{Scheme: "Bearer", Session: s.Token}, nil
}
