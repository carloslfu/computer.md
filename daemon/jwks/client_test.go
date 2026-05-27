// SPDX-License-Identifier: Apache-2.0

package jwks

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mustGenKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	return k
}

func pubJWK(t *testing.T, k *rsa.PublicKey, kid string) jwk {
	t.Helper()
	nBytes := k.N.Bytes()
	// big-endian exponent
	e := k.E
	var eBytes []byte
	for e > 0 {
		eBytes = append([]byte{byte(e & 0xff)}, eBytes...)
		e >>= 8
	}
	if len(eBytes) == 0 {
		eBytes = []byte{0x01}
	}
	return jwk{
		Kty: "RSA",
		Kid: kid,
		Alg: "RS256",
		Use: "sig",
		N:   base64.RawURLEncoding.EncodeToString(nBytes),
		E:   base64.RawURLEncoding.EncodeToString(eBytes),
	}
}

// jwksServer is a controllable in-memory JWKS endpoint.
type jwksServer struct {
	mu       sync.Mutex
	keys     []jwk
	hits     atomic.Int32
	failNext atomic.Bool
	server   *httptest.Server
}

func newJwksServer(keys ...jwk) *jwksServer {
	s := &jwksServer{keys: keys}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func (s *jwksServer) handle(w http.ResponseWriter, r *http.Request) {
	s.hits.Add(1)
	if s.failNext.Swap(false) {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	out := jwkSet{Keys: append([]jwk(nil), s.keys...)}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *jwksServer) setKeys(keys ...jwk) {
	s.mu.Lock()
	s.keys = keys
	s.mu.Unlock()
}

func (s *jwksServer) close() { s.server.Close() }

func silentLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(io.Discard, "", 0)
}

func TestKeyFor_BootstrapKey(t *testing.T) {
	keyA := mustGenKey(t)
	srv := newJwksServer()
	defer srv.close()

	c := NewClient(srv.server.URL, "vibecraft-1", &keyA.PublicKey,
		WithMinWait(time.Hour), // suppress refresh
		WithLogger(silentLogger(t)),
	)

	got, err := c.KeyFor("vibecraft-1")
	if err != nil {
		t.Fatalf("KeyFor: %v", err)
	}
	if got.N.Cmp(keyA.PublicKey.N) != 0 {
		t.Fatalf("returned wrong key")
	}
	// One refresh attempt fires on the first miss-or-stale path (cache fetched is zero).
	if srv.hits.Load() > 1 {
		t.Fatalf("unexpected server hits: %d", srv.hits.Load())
	}
}

func TestKeyFor_RotationDrill(t *testing.T) {
	keyA := mustGenKey(t)
	keyB := mustGenKey(t)
	srv := newJwksServer(pubJWK(t, &keyA.PublicKey, "test-1"))
	defer srv.close()

	c := NewClient(srv.server.URL, "vibecraft-1", nil,
		WithTTL(time.Hour),
		WithMinWait(time.Millisecond), // allow rapid refresh in tests
		WithLogger(silentLogger(t)),
	)

	// First lookup: refreshes, finds test-1.
	if _, err := c.KeyFor("test-1"); err != nil {
		t.Fatalf("initial lookup: %v", err)
	}

	// Rotate JWKS: add test-2.
	srv.setKeys(pubJWK(t, &keyA.PublicKey, "test-1"), pubJWK(t, &keyB.PublicKey, "test-2"))

	// Wait past the rate limiter, then look up the new kid.
	time.Sleep(5 * time.Millisecond)

	got, err := c.KeyFor("test-2")
	if err != nil {
		t.Fatalf("post-rotation lookup: %v", err)
	}
	if got.N.Cmp(keyB.PublicKey.N) != 0 {
		t.Fatalf("post-rotation key mismatch")
	}
}

func TestKeyFor_PlatformUnreachable(t *testing.T) {
	keyA := mustGenKey(t)
	// Point at an unreachable address.
	c := NewClient("http://127.0.0.1:1/jwks", "vibecraft-1", &keyA.PublicKey,
		WithTTL(time.Hour),
		WithMinWait(time.Millisecond),
		WithHTTPClient(&http.Client{Timeout: 50 * time.Millisecond}),
		WithLogger(silentLogger(t)),
	)

	got, err := c.KeyFor("vibecraft-1")
	if err != nil {
		t.Fatalf("KeyFor with unreachable platform: %v", err)
	}
	if got.N.Cmp(keyA.PublicKey.N) != 0 {
		t.Fatalf("bootstrap key not returned")
	}
}

func TestKeyFor_UnknownKidRateLimited(t *testing.T) {
	srv := newJwksServer()
	defer srv.close()

	c := NewClient(srv.server.URL, "vibecraft-1", nil,
		WithTTL(time.Hour),
		WithMinWait(time.Hour), // hard rate limit
		WithLogger(silentLogger(t)),
	)

	// First lookup of bogus-kid: triggers one refresh, then fails.
	if _, err := c.KeyFor("bogus"); err == nil {
		t.Fatalf("expected error for bogus kid")
	}
	first := srv.hits.Load()

	// Spam more lookups: should NOT trigger more refreshes.
	for i := 0; i < 20; i++ {
		_, _ = c.KeyFor("bogus")
	}
	if got := srv.hits.Load(); got != first {
		t.Fatalf("rate limiter leak: hits=%d before=%d", got, first)
	}
}

func TestKeyFor_RefreshFailureKeepsCache(t *testing.T) {
	keyA := mustGenKey(t)
	srv := newJwksServer(pubJWK(t, &keyA.PublicKey, "test-1"))
	defer srv.close()

	c := NewClient(srv.server.URL, "vibecraft-1", nil,
		WithTTL(time.Hour),
		WithMinWait(time.Millisecond),
		WithLogger(silentLogger(t)),
	)

	if _, err := c.KeyFor("test-1"); err != nil {
		t.Fatalf("initial lookup: %v", err)
	}

	// Make the next fetch fail.
	srv.failNext.Store(true)

	// Force a refresh by looking up an unknown kid.
	time.Sleep(5 * time.Millisecond)
	_, _ = c.KeyFor("bogus")

	// test-1 must still be in the cache.
	got, err := c.KeyFor("test-1")
	if err != nil {
		t.Fatalf("cache should survive refresh failure: %v", err)
	}
	if got.N.Cmp(keyA.PublicKey.N) != 0 {
		t.Fatalf("wrong key after failed refresh")
	}
}

func TestKeyFor_BootstrapSurvivesRefresh(t *testing.T) {
	keyBootstrap := mustGenKey(t)
	keyA := mustGenKey(t)

	// Upstream JWKS does NOT contain the bootstrap kid.
	srv := newJwksServer(pubJWK(t, &keyA.PublicKey, "test-1"))
	defer srv.close()

	c := NewClient(srv.server.URL, "vibecraft-1", &keyBootstrap.PublicKey,
		WithTTL(time.Hour),
		WithMinWait(time.Millisecond),
		WithLogger(silentLogger(t)),
	)

	// Force a refresh.
	if _, err := c.KeyFor("test-1"); err != nil {
		t.Fatalf("upstream lookup: %v", err)
	}

	// Bootstrap key must still verify (we preserve un-mirrored bootstrap entries).
	got, err := c.KeyFor("vibecraft-1")
	if err != nil {
		t.Fatalf("bootstrap kid lost after refresh: %v", err)
	}
	if got.N.Cmp(keyBootstrap.PublicKey.N) != 0 {
		t.Fatalf("bootstrap key replaced unexpectedly")
	}
}

func TestForceRefresh(t *testing.T) {
	keyA := mustGenKey(t)
	srv := newJwksServer(pubJWK(t, &keyA.PublicKey, "test-1"))
	defer srv.close()

	c := NewClient(srv.server.URL, "vibecraft-1", nil,
		WithTTL(time.Hour),
		WithMinWait(time.Hour), // hard rate limit on normal path
		WithLogger(silentLogger(t)),
	)

	if err := c.ForceRefresh(); err != nil {
		t.Fatalf("force refresh: %v", err)
	}
	if _, err := c.KeyFor("test-1"); err != nil {
		t.Fatalf("after force refresh: %v", err)
	}
}
