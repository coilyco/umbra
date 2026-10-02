// Package tokenmint resolves a value that has to be MINTED rather than read.
// Every valuesource provider reads a secret that already exists; an OAuth
// client_credentials upstream has none until one is fetched (umbra#310).
package tokenmint

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/coilyco/umbra/pkg/valuesource"
)

// AuthStyle names how the client authenticates to the token endpoint. Basic is
// the default because RFC 6749 requires a server to support it.
const (
	AuthStyleBasic = "basic"
	AuthStylePost  = "post"
)

// Client is one named machine-to-machine OAuth client. The secret is a value
// SOURCE rather than a string, so policy never carries the credential itself.
type Client struct {
	Name         string
	TokenURL     string
	ClientID     string
	ClientSecret valuesource.Source
	Scopes       []string
	AuthStyle    string
}

// Registry mints and caches access tokens for the clients it was built with.
// It is the thing a consumer registers as the "oauth2" value provider.
type Registry struct {
	clients map[string]Client
	// secrets is the consumer's BASE registry, not the merged one: a client
	// secret is never itself an oauth2 value, so no cycle needs guarding.
	secrets map[string]valuesource.Provider
	client  *http.Client

	mu     sync.Mutex
	minted map[string]*mintedSource
}

// mintedSource is one client's live token source plus the secret it was built
// with, so a rotation is noticed rather than served from a stale source.
type mintedSource struct {
	secret string
	source oauth2.TokenSource
}

// New builds a Registry over the consumer's base value providers. A client with
// no name, token URL, or client id is refused here rather than at first call.
func New(clients []Client, secrets map[string]valuesource.Provider, httpClient *http.Client) (*Registry, error) {
	byName := make(map[string]Client, len(clients))
	for _, c := range clients {
		if err := validateClient(c); err != nil {
			return nil, err
		}
		if _, dup := byName[c.Name]; dup {
			return nil, fmt.Errorf("tokenmint: duplicate client %q", c.Name)
		}
		byName[c.Name] = c
	}
	return &Registry{
		clients: byName,
		secrets: secrets,
		client:  httpClient,
		minted:  map[string]*mintedSource{},
	}, nil
}

func validateClient(c Client) error {
	switch {
	case c.Name == "":
		return fmt.Errorf("tokenmint: client needs a name")
	case c.TokenURL == "":
		return fmt.Errorf("tokenmint: client %q needs a token URL", c.Name)
	case c.ClientID == "":
		return fmt.Errorf("tokenmint: client %q needs a client id", c.Name)
	case c.ClientSecret.Provider == "":
		return fmt.Errorf("tokenmint: client %q needs a client-secret value source", c.Name)
	case c.AuthStyle != "" && c.AuthStyle != AuthStyleBasic && c.AuthStyle != AuthStylePost:
		return fmt.Errorf("tokenmint: client %q auth style %q is not %s or %s", c.Name, c.AuthStyle, AuthStyleBasic, AuthStylePost)
	}
	return nil
}

// Provider is the valuesource.Provider to register as "oauth2". Its address is
// a CLIENT NAME, so a guardfile carries no endpoint and no credential.
func (r *Registry) Provider() valuesource.Provider {
	return func(ctx context.Context, name string) (string, error) {
		return r.token(ctx, name)
	}
}

// Names lists the registered clients, for a consumer's describe surface. It
// reports names only, never an endpoint or a credential.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.clients))
	for name := range r.clients {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// token returns a currently-valid access token for the named client, minting
// one only when the cached source has none left to give.
func (r *Registry) token(ctx context.Context, name string) (string, error) {
	client, ok := r.clients[name]
	if !ok {
		return "", fmt.Errorf("tokenmint: no oauth2 client named %q (registered: %s)", name, strings.Join(r.Names(), ", "))
	}
	// Resolved on every call, so a rotated secret takes effect without a
	// restart. The resolve is a map read or a file read, not a network call.
	secret, err := valuesource.Resolve(ctx, r.secrets, client.ClientSecret.Provider, client.ClientSecret.Address)
	if err != nil {
		return "", fmt.Errorf("tokenmint: client %q secret: %w", name, err)
	}
	source := r.sourceFor(ctx, client, secret)
	token, err := source.Token()
	if err != nil {
		// Names the endpoint, never the credential: an operator needs to know
		// where the mint failed and the secret must not reach a log.
		return "", fmt.Errorf("tokenmint: client %q could not mint a token at %s: %w", name, client.TokenURL, redactTokenError(err))
	}
	if token.AccessToken == "" {
		return "", fmt.Errorf("tokenmint: client %q got an empty access token from %s", name, client.TokenURL)
	}
	return token.AccessToken, nil
}

// sourceFor returns the cached token source, rebuilding it on a rotated
// secret. The lock is held across the build so first calls do not stampede.
func (r *Registry) sourceFor(ctx context.Context, client Client, secret string) oauth2.TokenSource {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cached, ok := r.minted[client.Name]; ok && cached.secret == secret {
		return cached.source
	}
	cfg := clientcredentials.Config{
		ClientID:     client.ClientID,
		ClientSecret: secret,
		TokenURL:     client.TokenURL,
		Scopes:       client.Scopes,
		AuthStyle:    authStyle(client.AuthStyle),
	}
	// TokenSource wraps ReuseTokenSource, so caching, expiry skew, and
	// serialized renewal are the library's rather than a mutex here.
	source := cfg.TokenSource(r.mintContext(ctx))
	r.minted[client.Name] = &mintedSource{secret: secret, source: source}
	return source
}

// mintContext carries the consumer's HTTP client into the mint, so a token
// endpoint is reached through the same bounded transport as everything else.
func (r *Registry) mintContext(ctx context.Context) context.Context {
	if r.client == nil {
		return ctx
	}
	return context.WithValue(ctx, oauth2.HTTPClient, r.client)
}

func authStyle(style string) oauth2.AuthStyle {
	if style == AuthStylePost {
		return oauth2.AuthStyleInParams
	}
	return oauth2.AuthStyleInHeader
}

// maxTokenErrorBytes bounds what an upstream's failure body can add to an
// error, so a server returning a page cannot flood a log through this path.
const maxTokenErrorBytes = 512

// redactTokenError bounds the upstream's own message. An OAuth error body is a
// short `{"error":"invalid_client"}`, and anything longer is not a reason.
func redactTokenError(err error) error {
	msg := err.Error()
	if len(msg) <= maxTokenErrorBytes {
		return err
	}
	return fmt.Errorf("%s... (%d bytes truncated)", msg[:maxTokenErrorBytes], len(msg)-maxTokenErrorBytes)
}
