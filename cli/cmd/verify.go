// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/carloslfu/computer.md/cli/schema"
)

// releasePublicKeyPEM is the pinned Ed25519 public key the release
// pipeline signs every binary with (the "Sign CLI (Ed25519 release key)"
// step in .github/workflows/release.yml). selfUpdate refuses to swap in a
// binary whose detached .ed25519.sig does not verify against this key —
// so a compromised download origin cannot push a forged or unsigned
// binary, which the same-origin SHA-256 check alone cannot prevent.
//
// This is computer.md's OWN release-signing identity, generated for and
// owned by this repository. It is deliberately INDEPENDENT of any other
// project's signing key — this repo signs and verifies entirely on its
// own key, with no shared trust root. See SIGNING.md.
//
// To rotate: ship a CLI release that pins the new key here AND updates
// release.yml's two verify blocks, THEN switch the RELEASE_SIGNING_KEY
// repo secret — clients keep trusting the old key until they have
// updated to a binary that pins the new one. Keep the private key backed
// up (a password manager): if it is lost, already-installed clients
// cannot be migrated to a new key via self-update (they hard-fail on a
// signature they cannot verify) and must reinstall. Full procedure in
// SIGNING.md.
const releasePublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEA+Lcb8IpwuZjZHh6FddfgliKbupMfUSXv4PKCSjBn5mw=
-----END PUBLIC KEY-----`

// releaseSignatureURL returns the URL of a binary's detached Ed25519
// signature. release.yml uploads it next to every binary as
// vibecraft-<target>.ed25519.sig — the .exe suffix (Windows) is dropped
// so the sidecar name matches, mirroring the .sha256 convention.
func releaseSignatureURL(binaryURL string) string {
	return strings.TrimSuffix(binaryURL, ".exe") + ".ed25519.sig"
}

// verifyReleaseSignature downloads the detached Ed25519 signature for a
// freshly-downloaded binary and verifies the binary bytes against the
// pinned release key. A missing or unverifiable signature is a hard
// failure: an updater that fell back to "no signature available" could
// be silently downgraded by anyone able to strip the .sig from a
// compromised origin.
func verifyReleaseSignature(binaryPath, binaryURL string) error {
	pub, err := parseReleasePublicKey()
	if err != nil {
		return err
	}
	sig, err := fetchReleaseSignature(releaseSignatureURL(binaryURL))
	if err != nil {
		return err
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return schema.Newf(schema.CodeInternal, "reading downloaded binary: %s", err.Error())
	}
	if !ed25519.Verify(pub, binary, sig) {
		return schema.Newf(schema.CodeValidationError,
			"release signature verification failed").
			WithHint("the downloaded binary is not signed by the VibeCraft release key — refusing to install it")
	}
	return nil
}

// parseReleasePublicKey decodes the pinned Ed25519 public key.
func parseReleasePublicKey() (ed25519.PublicKey, error) {
	block, _ := pem.Decode([]byte(releasePublicKeyPEM))
	if block == nil {
		return nil, schema.Newf(schema.CodeInternal, "pinned release key is not valid PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, schema.Newf(schema.CodeInternal, "parsing pinned release key: %s", err.Error())
	}
	pub, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, schema.Newf(schema.CodeInternal, "pinned release key is not an Ed25519 key")
	}
	return pub, nil
}

// fetchReleaseSignature downloads a detached signature — a 64-byte file.
func fetchReleaseSignature(sigURL string) ([]byte, error) {
	if !strings.HasPrefix(sigURL, "https://") {
		return nil, schema.Newf(schema.CodeValidationError,
			"refusing non-HTTPS signature URL: %s", sigURL)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", sigURL, nil)
	if err != nil {
		return nil, schema.Newf(schema.CodeInternal, "creating request: %s", err.Error())
	}
	req.Header.Set("User-Agent", "vibecraft-cli/"+cliVersion)
	resp, err := client.Do(req)
	if err != nil {
		return nil, schema.Newf(schema.CodeMachineUnreachable, "fetching release signature: %s", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, schema.Newf(schema.CodeValidationError,
			"release signature unavailable (HTTP %d) — refusing to install an unsigned binary",
			resp.StatusCode)
	}
	sig, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return nil, schema.Newf(schema.CodeInternal, "reading release signature: %s", err.Error())
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, schema.Newf(schema.CodeValidationError,
			"release signature is %d bytes, expected %d", len(sig), ed25519.SignatureSize)
	}
	return sig, nil
}
