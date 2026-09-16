package transport

import (
	"context"
	"net/url"
	"testing"
)

func TestStaticBearer(t *testing.T) {
	p := StaticBearer{Token: "sess-abc"}
	target, err := url.Parse("https://relay.example.com")
	if err != nil {
		t.Fatal(err)
	}
	creds, err := p.Credentials(context.Background(), "GET", target)
	if err != nil {
		t.Fatal(err)
	}
	// The relay login's body token rides as a Bearer credential — the
	// native client's direct SSE path needs an explicit Bearer because
	// cookies do not reach it (design §3.3). No DPoP proof for relays.
	if creds.Scheme != "Bearer" || creds.Session != "sess-abc" || creds.Proof != "" {
		t.Fatalf("credentials: %+v", creds)
	}
	// Method and target are irrelevant to a relay session.
	creds2, err := p.Credentials(context.Background(), "POST", target)
	if err != nil || creds2 != creds {
		t.Fatalf("method-dependent credentials: %+v err=%v", creds2, err)
	}
}
