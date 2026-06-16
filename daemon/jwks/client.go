// SPDX-License-Identifier: Apache-2.0

// Package jwks provides a cached JWKS client for verifying RS256 JWTs.
//
// The daemon previously held a single PEM-on-disk public key. Rotating
// that key required shipping a new file to every machine. With JWKS,
// the daemon fetches keys by kid from the platform and caches them.
// The on-disk PEM still exists as the bootstrap entry of the cache so
// verification works offline from the first boot.
package jwks

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type jwk struct {
	Kty string `json:"kty"`
	N   string `json:"n"`
	E   string `json:"e"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

// Client holds a cache of kid → public key and refreshes it from the
// platform's JWKS endpoint when a kid is unknown or the cache is stale.
type Client struct {
	url     string
	httpc   *http.Client
	logger  *log.Logger
	ttl     time.Duration
	minWait time.Duration

	mu      sync.RWMutex
	cache   map[string]*rsa.PublicKey
	fetched time.Time
	lastTry time.Time

	// bootstrap holds the genuinely-immutable kids pre-loaded at
	// construction (the on-disk PEM). These are the ONLY entries
	// re-added across a successful refresh — every other kid is rebuilt
	// from the freshly-fetched JWKS so a kid the platform removed
	// (e.g. a compromised key) is actually dropped, not re-added
	// forever. Never mutated after NewClient, so reads need no lock.
	bootstrap map[string]*rsa.PublicKey

	// Observability counters (Workstream K). Bumped atomically; read by
	// /metrics. Kept inside the client so callers don't need to wrap
	// every call site with an instrumentation shim.
	mRefreshOK   atomic.Int64
	mRefreshFail atomic.Int64
	mUnknownKid  atomic.Int64
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the default HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(client *Client) { client.httpc = c }
}

// WithTTL sets how long the cache is considered fresh before a refresh
// is attempted on the next lookup. Default 24h.
func WithTTL(d time.Duration) Option {
	return func(c *Client) { c.ttl = d }
}

// WithMinWait sets the minimum interval between refresh attempts.
// Applies to both stale-TTL and unknown-kid paths so a bogus kid
// can't trigger unbounded refreshes. Default 1 minute.
func WithMinWait(d time.Duration) Option {
	return func(c *Client) { c.minWait = d }
}

// WithLogger overrides the default logger.
func WithLogger(l *log.Logger) Option {
	return func(c *Client) { c.logger = l }
}

// NewClient creates a Client. If bootstrapKey is non-nil, it is
// pre-loaded into the cache under bootstrapKid so verification works
// before the first successful platform refresh.
func NewClient(url string, bootstrapKid string, bootstrapKey *rsa.PublicKey, opts ...Option) *Client {
	c := &Client{
		url:       url,
		httpc:     &http.Client{Timeout: 10 * time.Second},
		logger:    log.Default(),
		ttl:       24 * time.Hour,
		minWait:   time.Minute,
		cache:     make(map[string]*rsa.PublicKey),
		bootstrap: make(map[string]*rsa.PublicKey),
	}
	if bootstrapKey != nil {
		c.cache[bootstrapKid] = bootstrapKey
		c.bootstrap[bootstrapKid] = bootstrapKey
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// KeyFor returns the public key for a kid, refreshing the cache from
// the platform if necessary. Refresh failures keep the existing cache;
// the cache is never invalidated on a network error.
func (c *Client) KeyFor(kid string) (*rsa.PublicKey, error) {
	c.mu.RLock()
	key, found := c.cache[kid]
	fresh := !c.fetched.IsZero() && time.Since(c.fetched) < c.ttl
	c.mu.RUnlock()

	if found && fresh {
		return key, nil
	}

	// Cache miss or stale: try a single refresh, rate-limited across
	// all refresh paths so a bogus kid can't bypass the limiter by
	// alternating with stale-TTL hits.
	c.tryRefresh()

	c.mu.RLock()
	defer c.mu.RUnlock()
	if key, ok := c.cache[kid]; ok {
		return key, nil
	}
	c.mUnknownKid.Add(1)
	return nil, fmt.Errorf("jwks: unknown kid %q", kid)
}

// tryRefresh records the attempt and calls refresh if the minWait
// rate limiter allows it. Holds the mutex only for the bookkeeping
// part; the actual HTTP call is unlocked.
func (c *Client) tryRefresh() {
	c.mu.Lock()
	if time.Since(c.lastTry) <= c.minWait {
		c.mu.Unlock()
		return
	}
	c.lastTry = time.Now()
	c.mu.Unlock()

	c.refresh()
}

// refresh fetches the JWKS document and rebuilds the live cache from
// it. The freshly-fetched key set is the authority: a kid the platform
// removed (e.g. a compromised key being revoked fleet-wide) is dropped,
// not carried over. Only the genuinely-immutable bootstrap kids (the
// on-disk PEM) are unioned back in so a daemon can still verify legacy
// tokens while the platform retires that one kid. On any fetch failure
// the existing cache is left untouched (see the early returns below),
// so a transient network error never bricks verification.
func (c *Client) refresh() {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, c.url, nil)
	if err != nil {
		c.logger.Printf("jwks: build request: %v", err)
		c.mRefreshFail.Add(1)
		return
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		c.logger.Printf("jwks: fetch %s: %v", c.url, err)
		c.mRefreshFail.Add(1)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.logger.Printf("jwks: fetch %s: status %d", c.url, resp.StatusCode)
		c.mRefreshFail.Add(1)
		return
	}
	var set jwkSet
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		c.logger.Printf("jwks: decode: %v", err)
		c.mRefreshFail.Add(1)
		return
	}

	newCache := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		if k.Kid == "" {
			c.logger.Printf("jwks: skipping key without kid")
			continue
		}
		pub, err := jwkToRSA(k)
		if err != nil {
			c.logger.Printf("jwks: convert kid=%s: %v", k.Kid, err)
			continue
		}
		newCache[k.Kid] = pub
	}

	// A 200 with an empty or zero-valid-key JWKS is NOT an authoritative
	// refresh. Committing it would drop every rotated (non-bootstrap) kid
	// fleet-wide and silently revoke valid tokens. Treat it like a fetch
	// failure: leave the existing cache untouched and bail. (Bootstrap
	// kids are unioned in below, so the count must be taken BEFORE that.)
	if len(newCache) == 0 {
		c.logger.Printf("jwks: fetch %s: empty/zero-valid-key JWKS, keeping previous keys", c.url)
		c.mRefreshFail.Add(1)
		return
	}

	// Union ONLY the immutable bootstrap kids back in — never the rest
	// of the previous live cache. This is what makes revocation real:
	// a kid the freshly-fetched JWKS no longer lists is absent from
	// newCache and stays absent, so a compromised key removed by the
	// platform stops verifying after this refresh. Bootstrap is set
	// once at construction and never mutated, so reading it without the
	// lock is safe.
	for kid, key := range c.bootstrap {
		if _, ok := newCache[kid]; !ok {
			newCache[kid] = key
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache = newCache
	c.fetched = time.Now()
	c.mRefreshOK.Add(1)
	c.logger.Printf("jwks: refreshed %d keys from %s", len(set.Keys), c.url)
}

// Stats returns a snapshot of the in-process counters for /metrics.
type Stats struct {
	RefreshOK   int64
	RefreshFail int64
	UnknownKid  int64
	FetchedAt   time.Time
}

// Stats returns counters + last-fetched time atomically so /metrics
// gets a consistent view.
func (c *Client) Stats() Stats {
	c.mu.RLock()
	fetched := c.fetched
	c.mu.RUnlock()
	return Stats{
		RefreshOK:   c.mRefreshOK.Load(),
		RefreshFail: c.mRefreshFail.Load(),
		UnknownKid:  c.mUnknownKid.Load(),
		FetchedAt:   fetched,
	}
}

// CacheAge returns how long ago the cache was last successfully
// refreshed. Returns 0 if the cache has never been refreshed (only
// bootstrap keys present).
func (c *Client) CacheAge() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.fetched.IsZero() {
		return 0
	}
	return time.Since(c.fetched)
}

// ClearCache wipes every cached key and resets the fetched timestamp.
// Used by the management channel's "refresh JWKS now" handler so a
// fresh fetch happens on the next KeyFor call (bypassing the rate
// limiter via ForceRefresh).
func (c *Client) ClearCache() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache = make(map[string]*rsa.PublicKey)
	c.fetched = time.Time{}
}

// ForceRefresh ignores the rate limiter and triggers an immediate
// refresh. Used by management-channel commands. Returns nil on
// successful refresh, error otherwise.
func (c *Client) ForceRefresh() error {
	c.mu.Lock()
	c.lastTry = time.Now()
	c.mu.Unlock()

	before := c.CacheAge()
	c.refresh()
	after := c.CacheAge()
	if after == 0 || (before != 0 && after >= before) {
		return fmt.Errorf("jwks: refresh did not update cache")
	}
	return nil
}

func jwkToRSA(k jwk) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}
	if len(eBytes) == 0 {
		return nil, fmt.Errorf("empty exponent")
	}
	n := new(big.Int).SetBytes(nBytes)
	var eInt int
	for _, b := range eBytes {
		eInt = eInt<<8 | int(b)
	}
	if eInt == 0 {
		return nil, fmt.Errorf("zero exponent")
	}
	return &rsa.PublicKey{N: n, E: eInt}, nil
}
