package authn

import (
	"errors"
	"reflect"
	"testing"

	"github.com/projanvil/woca/internal/platform/tenancy"
)

func TestLocal(t *testing.T) {
	p := NewLocal()
	if p == nil {
		t.Fatal("NewLocal() returned nil")
	}
	var _ Provider = p

	tests := []struct {
		name       string
		credential string
	}{
		{name: "empty credential", credential: ""},
		{name: "bearer token", credential: "Bearer sk-abc123"},
		{name: "api key", credential: "sk-abc123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := p.Authenticate(t.Context(), tt.credential)
			if err != nil {
				t.Fatalf("Authenticate(%q) returned error: %v", tt.credential, err)
			}
			want := tenancy.Local()
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Authenticate(%q) = %+v, want %+v", tt.credential, got, want)
			}
		})
	}
}

func TestLocalConcurrent(t *testing.T) {
	p := NewLocal()
	const workers = 16
	done := make(chan struct{}, workers)
	for range workers {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 100 {
				got, err := p.Authenticate(t.Context(), "tok")
				if err != nil {
					t.Errorf("Authenticate returned error: %v", err)
					return
				}
				if !reflect.DeepEqual(got, tenancy.Local()) {
					t.Errorf("Authenticate = %+v, want %+v", got, tenancy.Local())
					return
				}
			}
		}()
	}
	for range workers {
		<-done
	}
}

func TestStatic(t *testing.T) {
	tc := tenancy.Context{
		TenantID: "tenant-1",
		OrgID:    "org-1",
		UserID:   "user-1",
		Scopes:   []string{"read", "write"},
	}
	p := NewStatic("secret-token", tc)
	if p == nil {
		t.Fatal("NewStatic() returned nil")
	}
	var _ Provider = p

	tests := []struct {
		name       string
		credential string
		wantErr    bool
	}{
		{name: "matching token", credential: "secret-token", wantErr: false},
		{name: "wrong token", credential: "wrong-token", wantErr: true},
		{name: "empty credential", credential: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := p.Authenticate(t.Context(), tt.credential)
			if tt.wantErr {
				if !errors.Is(err, ErrUnauthenticated) {
					t.Fatalf("Authenticate(%q) error = %v, want ErrUnauthenticated", tt.credential, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Authenticate(%q) returned error: %v", tt.credential, err)
			}
			if !reflect.DeepEqual(got, tc) {
				t.Errorf("Authenticate(%q) = %+v, want %+v", tt.credential, got, tc)
			}
		})
	}
}

func TestStaticConcurrent(t *testing.T) {
	tc := tenancy.Context{TenantID: "t", OrgID: "o", UserID: "u", Scopes: []string{"s"}}
	p := NewStatic("tok", tc)
	const workers = 16
	done := make(chan struct{}, workers)
	for i := range workers {
		go func() {
			defer func() { done <- struct{}{} }()
			cred := "tok"
			wantErr := false
			if i%2 == 0 {
				cred = "bad"
				wantErr = true
			}
			for range 100 {
				got, err := p.Authenticate(t.Context(), cred)
				if wantErr {
					if !errors.Is(err, ErrUnauthenticated) {
						t.Errorf("Authenticate(%q) error = %v, want ErrUnauthenticated", cred, err)
						return
					}
					continue
				}
				if err != nil {
					t.Errorf("Authenticate(%q) returned error: %v", cred, err)
					return
				}
				if !reflect.DeepEqual(got, tc) {
					t.Errorf("Authenticate(%q) = %+v, want %+v", cred, got, tc)
					return
				}
			}
		}()
	}
	for range workers {
		<-done
	}
}

func TestStaticContextIsolation(t *testing.T) {
	tc := tenancy.Context{
		TenantID: "tenant-1",
		Scopes:   []string{"read"},
	}
	p := NewStatic("tok", tc)

	got, err := p.Authenticate(t.Context(), "tok")
	if err != nil {
		t.Fatalf("Authenticate returned error: %v", err)
	}
	got.Scopes[0] = "mutated"
	again, err := p.Authenticate(t.Context(), "tok")
	if err != nil {
		t.Fatalf("Authenticate returned error: %v", err)
	}
	if again.Scopes[0] != "read" {
		t.Errorf("returned context shares backing array with stored context: Scopes[0] = %q, want %q", again.Scopes[0], "read")
	}
}

func TestTenancyContextRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		p       Provider
		cred    string
		wantErr bool
	}{
		{name: "local", p: NewLocal(), cred: ""},
		{
			name: "static",
			p: NewStatic("tok", tenancy.Context{
				TenantID: "tenant-1",
				OrgID:    "org-1",
				UserID:   "user-1",
				Scopes:   []string{"admin"},
			}),
			cred: "tok",
		},
		{
			name:    "static rejection",
			p:       NewStatic("tok", tenancy.Context{}),
			cred:    "nope",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc, err := tt.p.Authenticate(t.Context(), tt.cred)
			if tt.wantErr {
				if !errors.Is(err, ErrUnauthenticated) {
					t.Fatalf("Authenticate error = %v, want ErrUnauthenticated", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Authenticate returned error: %v", err)
			}

			ctx := tenancy.NewContext(t.Context(), tc)
			got, ok := tenancy.FromContext(ctx)
			if !ok {
				t.Fatal("FromContext returned ok=false after NewContext")
			}
			if !reflect.DeepEqual(got, tc) {
				t.Errorf("round trip = %+v, want %+v", got, tc)
			}
		})
	}
}
