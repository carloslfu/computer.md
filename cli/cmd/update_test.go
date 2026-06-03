// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCompareReleaseVersions(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want int
		ok   bool
	}{
		{name: "newer", a: "v1.2.4", b: "v1.2.3", want: 1, ok: true},
		{name: "older", a: "v1.2.2", b: "v1.2.3", want: -1, ok: true},
		{name: "equal", a: "1.2.3", b: "v1.2.3", want: 0, ok: true},
		{name: "prerelease same core", a: "v1.2.3-rc.1", b: "v1.2.3", want: 0, ok: true},
		{name: "unknown dev", a: "v1.2.3", b: "dev", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := compareReleaseVersions(tt.a, tt.b)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("compareReleaseVersions(%q,%q) = (%d,%v), want (%d,%v)", tt.a, tt.b, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestSelfUpdateDownloadsVerifiesSignatureAndSwapsBinary(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))

	newBinary := []byte("new vibecraft cli binary")
	sum := sha256.Sum256(newBinary)
	target := runtime.GOOS + "-" + runtime.GOARCH
	assetName := "vibecraft-" + target
	if runtime.GOOS == "windows" {
		assetName += ".exe"
	}

	var manifestHits, binaryHits, sigHits int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			manifestHits++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(cliManifest{
				Version: "v9.9.9",
				Binaries: map[string]struct {
					URL    string `json:"url"`
					SHA256 string `json:"sha256"`
				}{
					target: {
						URL:    fmt.Sprintf("https://%s/%s", r.Host, assetName),
						SHA256: hex.EncodeToString(sum[:]),
					},
				},
			})
		case "/" + assetName:
			binaryHits++
			_, _ = w.Write(newBinary)
		case "/" + assetName + ".ed25519.sig":
			sigHits++
			_, _ = w.Write(ed25519.Sign(priv, newBinary))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	home := t.TempDir()
	currentBin := filepath.Join(home, "vibecraft")
	if err := os.WriteFile(currentBin, []byte("old binary"), 0755); err != nil {
		t.Fatal(err)
	}

	oldClient := updateHTTPClient
	oldExecutable := updateExecutablePath
	oldEvalSymlinks := updateEvalSymlinks
	oldCLIVersion := cliVersion
	oldPub := releasePublicKeyPEMForVerification
	t.Cleanup(func() {
		updateHTTPClient = oldClient
		updateExecutablePath = oldExecutable
		updateEvalSymlinks = oldEvalSymlinks
		cliVersion = oldCLIVersion
		releasePublicKeyPEMForVerification = oldPub
	})
	updateHTTPClient = func(timeout time.Duration) *http.Client {
		c := srv.Client()
		c.Timeout = timeout
		return c
	}
	updateExecutablePath = func() (string, error) { return currentBin, nil }
	updateEvalSymlinks = func(path string) (string, error) { return path, nil }
	cliVersion = "v0.1.0"
	releasePublicKeyPEMForVerification = pubPEM

	data, err := selfUpdate(srv.URL, false)
	if err != nil {
		t.Fatalf("selfUpdate: %v", err)
	}
	if data.Status != "updated" || data.LatestVersion != "v9.9.9" {
		t.Fatalf("unexpected update data: %+v", data)
	}
	got, err := os.ReadFile(currentBin)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newBinary) {
		t.Fatalf("binary not swapped: got %q", got)
	}
	if manifestHits != 1 || binaryHits != 1 || sigHits != 1 {
		t.Fatalf("hits manifest=%d binary=%d sig=%d, want 1 each", manifestHits, binaryHits, sigHits)
	}
}
