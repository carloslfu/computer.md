// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func TestParseReleasePublicKey(t *testing.T) {
	pub, err := parseReleasePublicKey()
	if err != nil {
		t.Fatalf("the pinned release key must parse: %v", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Errorf("pinned key size = %d, want %d", len(pub), ed25519.PublicKeySize)
	}
}

func TestReleaseSignatureURL(t *testing.T) {
	cases := map[string]string{
		"https://www.vibecraft.so/install/vibecraft-darwin-arm64":      "https://www.vibecraft.so/install/vibecraft-darwin-arm64.ed25519.sig",
		"https://www.vibecraft.so/install/vibecraft-linux-amd64":       "https://www.vibecraft.so/install/vibecraft-linux-amd64.ed25519.sig",
		"https://www.vibecraft.so/install/vibecraft-windows-amd64.exe": "https://www.vibecraft.so/install/vibecraft-windows-amd64.ed25519.sig",
		"https://www.vibecraft.so/install/vibecraft-windows-arm64.exe": "https://www.vibecraft.so/install/vibecraft-windows-arm64.ed25519.sig",
	}
	for in, want := range cases {
		if got := releaseSignatureURL(in); got != want {
			t.Errorf("releaseSignatureURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEd25519VerifyContract pins the exact primitive selfUpdate relies on
// (matching the openssl pkeyutl -rawin signing in release.yml): a good
// signature verifies; a tampered binary, a tampered signature, and a
// wrong key all fail closed.
func TestEd25519VerifyContract(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	binary := []byte("pretend this is a vibecraft release binary")
	sig := ed25519.Sign(priv, binary)

	if !ed25519.Verify(pub, binary, sig) {
		t.Error("a valid signature must verify")
	}

	tamperedBinary := append([]byte(nil), binary...)
	tamperedBinary[0] ^= 0xff
	if ed25519.Verify(pub, tamperedBinary, sig) {
		t.Error("a tampered binary must not verify")
	}

	tamperedSig := append([]byte(nil), sig...)
	tamperedSig[0] ^= 0xff
	if ed25519.Verify(pub, binary, tamperedSig) {
		t.Error("a tampered signature must not verify")
	}

	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if ed25519.Verify(otherPub, binary, sig) {
		t.Error("a signature must not verify under a different key")
	}
}
