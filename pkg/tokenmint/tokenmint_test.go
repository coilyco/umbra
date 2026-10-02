package tokenmint_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/coilyco/umbra/pkg/tokenmint"
	"github.com/coilyco/umbra/pkg/valuesource"
)

// tokenServer counts mints and records what the client sent, so "did not hit
// the endpoint again" is measured rather than asserted about.
type tokenServer struct {
	*httptest.Server
	mints     atomic.Int64
	expiresIn int
	mu        sync.Mutex
	lastAuth  string
	lastForm  map[string][]string
}

func newTokenServer(t *testing.T, expiresIn int) *tokenServer {
	t.Helper()
	ts := &tokenServer{expiresIn: expiresIn}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		ts.mu.Lock()
		ts.lastAuth = r.Header.Get("Authorization")
		ts.lastForm = r.Form
		ts.mu.Unlock()
		n := ts.mints.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"token-%d","token_type":"Bearer","expires_in":%d}`, n, ts.expiresIn)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (ts *tokenServer) form() map[string][]string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.lastForm
}

func (ts *tokenServer) auth() string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.lastAuth
}

// secretHolder is a mutable value source, so a rotation can be simulated the
// way SSM would deliver one.
type secretHolder struct {
	mu    sync.Mutex
	value string
}

func (s *secretHolder) providers() map[string]valuesource.Provider {
	return valuesource.Merge(map[string]valuesource.Provider{
		"vault": func(_ context.Context, _ string) (string, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.value, nil
		},
	})
}

func (s *secretHolder) set(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = v
}

func newRegistry(t *testing.T, ts *tokenServer, secrets *secretHolder, scopes ...string) *tokenmint.Registry {
	t.Helper()
	reg, err := tokenmint.New([]tokenmint.Client{{
		Name:         "someupstream",
		TokenURL:     ts.URL,
		ClientID:     "client-id",
		ClientSecret: valuesource.Source{Provider: "vault", Address: "/oauth/secret"},
		Scopes:       scopes,
	}}, secrets.providers(), ts.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return reg
}

// The ask: a value that is minted rather than read, resolving through the same
// registry every other credential uses.
func TestMintsATokenThroughTheValueRegistry(t *testing.T) {
	ts := newTokenServer(t, 3600)
	secrets := &secretHolder{value: "s3cret"}
	reg := newRegistry(t, ts, secrets, "read", "write")

	providers := valuesource.Merge(map[string]valuesource.Provider{"oauth2": reg.Provider()})
	got, err := valuesource.Resolve(context.Background(), providers, "oauth2", "someupstream")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "token-1" {
		t.Fatalf("token = %q", got)
	}
	if grant := ts.form()["grant_type"]; len(grant) != 1 || grant[0] != "client_credentials" {
		t.Errorf("grant_type = %v", grant)
	}
	if scope := ts.form()["scope"]; len(scope) != 1 || scope[0] != "read write" {
		t.Errorf("scope = %v, want the joined scopes", scope)
	}
	// Basic by default, because RFC 6749 requires a server to support it.
	if auth := ts.auth(); !strings.HasPrefix(auth, "Basic ") {
		t.Errorf("Authorization = %q, want Basic", auth)
	}
}

// A second call inside the token's lifetime must not hit the endpoint again,
// or every request pays for a mint.
func TestCachesWithinTheTokenLifetime(t *testing.T) {
	ts := newTokenServer(t, 3600)
	reg := newRegistry(t, ts, &secretHolder{value: "s3cret"})
	for range 5 {
		if _, err := reg.Provider()(context.Background(), "someupstream"); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	if n := ts.mints.Load(); n != 1 {
		t.Fatalf("token endpoint hit %d times, want 1", n)
	}
}

// An already-expired token is re-minted rather than served.
func TestRemintsAnExpiredToken(t *testing.T) {
	ts := newTokenServer(t, 1) // 1s, inside oauth2's own expiry delta, so always stale
	reg := newRegistry(t, ts, &secretHolder{value: "s3cret"})
	first, err := reg.Provider()(context.Background(), "someupstream")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	second, err := reg.Provider()(context.Background(), "someupstream")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if first == second {
		t.Fatalf("an expired token was served again: %q", first)
	}
	if n := ts.mints.Load(); n != 2 {
		t.Fatalf("token endpoint hit %d times, want 2", n)
	}
}

// A rotated client secret takes effect without a restart, which is the whole
// reason the secret resolves per call rather than at construction.
func TestPicksUpARotatedSecretWithoutARestart(t *testing.T) {
	ts := newTokenServer(t, 3600)
	secrets := &secretHolder{value: "old"}
	reg := newRegistry(t, ts, secrets)

	if _, err := reg.Provider()(context.Background(), "someupstream"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if sent := ts.form()["client_secret"]; len(sent) == 1 && sent[0] != "old" {
		t.Fatalf("first mint sent %v", sent)
	}
	secrets.set("new")
	if _, err := reg.Provider()(context.Background(), "someupstream"); err != nil {
		t.Fatalf("resolve after rotation: %v", err)
	}
	if n := ts.mints.Load(); n != 2 {
		t.Fatalf("token endpoint hit %d times, want 2 (the rotation must re-mint)", n)
	}
}

// Concurrent first calls must not stampede the token endpoint.
func TestConcurrentCallsDoNotStampede(t *testing.T) {
	ts := newTokenServer(t, 3600)
	reg := newRegistry(t, ts, &secretHolder{value: "s3cret"})

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := reg.Provider()(context.Background(), "someupstream"); err != nil {
				t.Errorf("resolve: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := ts.mints.Load(); n != 1 {
		t.Fatalf("token endpoint hit %d times under 20 concurrent calls, want 1", n)
	}
}

// A failure is loud and names the endpoint, and the secret never reaches it.
func TestAFailedMintNamesTheEndpointAndNotTheSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
	}))
	defer srv.Close()

	secrets := &secretHolder{value: "sup3r-s3cret-value"}
	reg, err := tokenmint.New([]tokenmint.Client{{
		Name: "someupstream", TokenURL: srv.URL, ClientID: "client-id",
		ClientSecret: valuesource.Source{Provider: "vault", Address: "/oauth/secret"},
	}}, secrets.providers(), srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = reg.Provider()(context.Background(), "someupstream")
	if err == nil {
		t.Fatalf("a failed mint returned success")
	}
	if !strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error = %q, want it to name the token endpoint", err)
	}
	if strings.Contains(err.Error(), "sup3r-s3cret-value") {
		t.Fatalf("the client secret reached the error: %q", err)
	}
}

// An unknown client name fails closed, naming what is registered.
func TestUnknownClientFailsClosed(t *testing.T) {
	ts := newTokenServer(t, 3600)
	reg := newRegistry(t, ts, &secretHolder{value: "s3cret"})
	_, err := reg.Provider()(context.Background(), "nosuchclient")
	if err == nil {
		t.Fatalf("an unknown client resolved")
	}
	if !strings.Contains(err.Error(), "someupstream") {
		t.Errorf("error = %q, want it to name the registered clients", err)
	}
	if n := ts.mints.Load(); n != 0 {
		t.Errorf("an unknown client reached the token endpoint")
	}
}

// A half-specified client is refused at construction rather than at first call.
func TestNewFailsClosedOnAHalfSpecifiedClient(t *testing.T) {
	secret := valuesource.Source{Provider: "vault", Address: "/x"}
	for name, client := range map[string]tokenmint.Client{
		"no name":        {TokenURL: "https://t", ClientID: "c", ClientSecret: secret},
		"no token url":   {Name: "a", ClientID: "c", ClientSecret: secret},
		"no client id":   {Name: "a", TokenURL: "https://t", ClientSecret: secret},
		"no secret":      {Name: "a", TokenURL: "https://t", ClientID: "c"},
		"bad auth style": {Name: "a", TokenURL: "https://t", ClientID: "c", ClientSecret: secret, AuthStyle: "magic"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := tokenmint.New([]tokenmint.Client{client}, nil, nil); err == nil {
				t.Fatalf("New accepted %+v", client)
			}
		})
	}
}

func TestNewRefusesDuplicateClients(t *testing.T) {
	c := tokenmint.Client{
		Name: "a", TokenURL: "https://t", ClientID: "c",
		ClientSecret: valuesource.Source{Provider: "vault", Address: "/x"},
	}
	if _, err := tokenmint.New([]tokenmint.Client{c, c}, nil, nil); err == nil {
		t.Fatalf("New accepted a duplicate client name")
	}
}

// The post auth style puts the credentials in the form rather than the header.
func TestPostAuthStyleSendsCredentialsInTheForm(t *testing.T) {
	ts := newTokenServer(t, 3600)
	secrets := &secretHolder{value: "s3cret"}
	reg, err := tokenmint.New([]tokenmint.Client{{
		Name: "someupstream", TokenURL: ts.URL, ClientID: "client-id",
		ClientSecret: valuesource.Source{Provider: "vault", Address: "/x"},
		AuthStyle:    tokenmint.AuthStylePost,
	}}, secrets.providers(), ts.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := reg.Provider()(context.Background(), "someupstream"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id := ts.form()["client_id"]; len(id) != 1 || id[0] != "client-id" {
		t.Errorf("client_id in form = %v", id)
	}
	if auth := ts.auth(); auth != "" {
		t.Errorf("Authorization = %q, want none for the post style", auth)
	}
}

// A secret the registry cannot read is an error, never a mint attempted
// without one.
func TestAnUnreadableSecretIsAnError(t *testing.T) {
	ts := newTokenServer(t, 3600)
	providers := valuesource.Merge(map[string]valuesource.Provider{
		"vault": func(context.Context, string) (string, error) { return "", fmt.Errorf("vault sealed") },
	})
	reg, err := tokenmint.New([]tokenmint.Client{{
		Name: "someupstream", TokenURL: ts.URL, ClientID: "client-id",
		ClientSecret: valuesource.Source{Provider: "vault", Address: "/x"},
	}}, providers, ts.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := reg.Provider()(context.Background(), "someupstream"); err == nil {
		t.Fatalf("an unreadable secret resolved")
	}
	if n := ts.mints.Load(); n != 0 {
		t.Errorf("a mint was attempted with no secret")
	}
}
