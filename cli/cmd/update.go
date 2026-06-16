// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

var (
	flagUpdateBaseURL string
	flagUpdateCheck   bool
)

var updateHTTPClient = func(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

var (
	updateExecutablePath = os.Executable
	updateEvalSymlinks   = filepath.EvalSymlinks
	updateChmod          = os.Chmod
	updateRename         = os.Rename
	updateWriteFile      = os.WriteFile
)

// defaultUpdateBaseURL is the manifest base used both by the `update`
// command (the --base-url default) and the background auto-updater.
const defaultUpdateBaseURL = "https://www.vibecraft.so/install"

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Self-update from the platform manifest",
	Long: `Fetch the latest CLI release from https://www.vibecraft.so/install/manifest.json,
verify its SHA-256 and Ed25519 release signature, and atomically replace
the running binary.

The CLI also self-updates in the background about once a day; this
command forces the check immediately. Set VIBECRAFT_NO_AUTO_UPDATE=1 to
disable the background updater.

Flags:
  --check          report the latest version without downloading.
  --base-url URL   override the manifest base (default: https://www.vibecraft.so/install).`,
	RunE: runUpdate,
}

func init() {
	updateCmd.Flags().StringVar(&flagUpdateBaseURL, "base-url", defaultUpdateBaseURL, "Override the manifest base URL")
	updateCmd.Flags().BoolVar(&flagUpdateCheck, "check", false, "Check for an update without downloading")
	rootCmd.AddCommand(updateCmd)
}

type cliManifest struct {
	Version  string `json:"version"`
	Binaries map[string]struct {
		URL    string `json:"url"`
		SHA256 string `json:"sha256"`
	} `json:"binaries"`
}

func runUpdate(cmd *cobra.Command, args []string) error {
	data, err := selfUpdate(flagUpdateBaseURL, flagUpdateCheck)
	if err != nil {
		return err
	}
	return output.Emit(data)
}

// selfUpdate is the shared core of the `update` command and the
// background auto-updater (autoupdate.go). It fetches the release
// manifest and, unless check is set, downloads + verifies + atomically
// swaps the running binary when a newer version is advertised.
//
// On Unix the swap is an atomic rename: the running process keeps its
// open inode and the next invocation sees the new binary. On Windows a
// rename over a running .exe is impossible, so the new binary is staged
// as <bin>.new with a sentinel marker and the swap completes at the
// start of the next launch (see completeWindowsUpdate in main_windows.go).
func selfUpdate(baseURL string, check bool) (schema.UpdateData, error) {
	target := runtime.GOOS + "-" + runtime.GOARCH
	manifestURL := strings.TrimRight(baseURL, "/") + "/manifest.json"

	manifest, err := fetchManifest(manifestURL)
	if err != nil {
		return schema.UpdateData{}, err
	}

	entry, ok := manifest.Binaries[target]
	if !ok {
		return schema.UpdateData{}, schema.Newf(schema.CodeServerError,
			"manifest has no entry for %s", target).
			WithHint("the release may still be building — try again in a minute")
	}

	if cmp, ok := compareReleaseVersions(manifest.Version, cliVersion); ok {
		switch {
		case cmp < 0:
			return schema.UpdateData{}, schema.Newf(schema.CodeValidationError,
				"manifest version %s is older than current CLI %s", manifest.Version, cliVersion).
				WithHint("refusing to roll back a signed release automatically")
		case cmp == 0:
			return schema.UpdateData{
				Status:         "up_to_date",
				CurrentVersion: cliVersion,
			}, nil
		}
	} else if manifest.Version == cliVersion {
		return schema.UpdateData{
			Status:         "up_to_date",
			CurrentVersion: cliVersion,
		}, nil
	}

	if check {
		return schema.UpdateData{
			Status:         "updated", // future tense for --check: what WOULD happen
			CurrentVersion: cliVersion,
			LatestVersion:  manifest.Version,
		}, nil
	}

	currentBin, err := updateExecutablePath()
	if err != nil {
		return schema.UpdateData{}, schema.Newf(schema.CodeInternal, "finding own path: %s", err.Error())
	}
	currentBin, err = updateEvalSymlinks(currentBin)
	if err != nil {
		return schema.UpdateData{}, schema.Newf(schema.CodeInternal, "resolving own path: %s", err.Error())
	}

	tmpBin := currentBin + ".new"
	if err := downloadAndVerify(entry.URL, entry.SHA256, tmpBin); err != nil {
		_ = os.Remove(tmpBin)
		return schema.UpdateData{}, err
	}

	// Provenance gate. downloadAndVerify checks SHA-256, but the manifest,
	// the binary, and the hash are all same-origin — that catches
	// corruption, not a compromised origin. The Ed25519 signature, pinned
	// to the release key, is what proves provenance. Fail-closed: no valid
	// signature, no swap.
	if err := verifyReleaseSignature(tmpBin, entry.URL); err != nil {
		_ = os.Remove(tmpBin)
		return schema.UpdateData{}, err
	}

	if err := updateChmod(tmpBin, 0755); err != nil {
		_ = os.Remove(tmpBin)
		return schema.UpdateData{}, schema.Newf(schema.CodeInternal, "chmod new binary: %s", err.Error())
	}

	if runtime.GOOS == "windows" {
		sentinel := currentBin + ".pending-update"
		if err := updateWriteFile(sentinel, []byte(manifest.Version), 0644); err != nil {
			_ = os.Remove(tmpBin)
			return schema.UpdateData{}, schema.Newf(schema.CodeInternal, "writing sentinel: %s", err.Error())
		}
		return schema.UpdateData{
			Status:         "updated",
			CurrentVersion: cliVersion,
			LatestVersion:  manifest.Version,
			Path:           currentBin + " (swap on next launch)",
		}, nil
	}

	if err := updateRename(tmpBin, currentBin); err != nil {
		_ = os.Remove(tmpBin)
		return schema.UpdateData{}, schema.Newf(schema.CodeInternal, "swapping binary: %s", err.Error())
	}

	return schema.UpdateData{
		Status:         "updated",
		CurrentVersion: cliVersion,
		LatestVersion:  manifest.Version,
		Path:           currentBin,
	}, nil
}

func fetchManifest(manifestURL string) (*cliManifest, error) {
	client := updateHTTPClient(15 * time.Second)
	req, err := http.NewRequest("GET", manifestURL, nil)
	if err != nil {
		return nil, schema.Newf(schema.CodeInternal, "creating request: %s", err.Error())
	}
	req.Header.Set("User-Agent", "vibecraft-cli/"+cliVersion)
	resp, err := client.Do(req)
	if err != nil {
		return nil, schema.Newf(schema.CodeMachineUnreachable, "fetching manifest: %s", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, schema.Newf(schema.CodeServerError,
			"manifest GET returned %d: %s", resp.StatusCode, string(body))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, schema.Newf(schema.CodeInternal, "reading manifest: %s", err.Error())
	}
	var m cliManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, schema.Newf(schema.CodeServerError, "parsing manifest: %s", err.Error())
	}
	return &m, nil
}

func downloadAndVerify(downloadURL, expectedSHA, destPath string) error {
	if !strings.HasPrefix(downloadURL, "https://") {
		return schema.Newf(schema.CodeValidationError,
			"refusing non-HTTPS download URL: %s", downloadURL)
	}

	client := updateHTTPClient(5 * time.Minute)
	req, err := http.NewRequest("GET", downloadURL, nil)
	if err != nil {
		return schema.Newf(schema.CodeInternal, "creating request: %s", err.Error())
	}
	req.Header.Set("User-Agent", "vibecraft-cli/"+cliVersion)
	resp, err := client.Do(req)
	if err != nil {
		return schema.Newf(schema.CodeMachineUnreachable, "downloading: %s", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return schema.Newf(schema.CodeServerError, "download returned %d", resp.StatusCode)
	}

	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return schema.Newf(schema.CodeInternal, "opening destination: %s", err.Error())
	}
	defer out.Close()

	hasher := sha256.New()
	tee := io.TeeReader(io.LimitReader(resp.Body, 256<<20), hasher)
	if _, err := io.Copy(out, tee); err != nil {
		return schema.Newf(schema.CodeInternal, "writing download: %s", err.Error())
	}
	if err := out.Close(); err != nil {
		return schema.Newf(schema.CodeInternal, "closing download: %s", err.Error())
	}

	actual := hex.EncodeToString(hasher.Sum(nil))
	// hex.EncodeToString emits lowercase; a manifest may publish the digest
	// in uppercase. Compare case-insensitively so a correct-but-uppercase
	// hash isn't rejected as a mismatch (fail-closed stays intact — only
	// the hex case is normalized, the bytes still have to match).
	if !strings.EqualFold(actual, strings.TrimSpace(expectedSHA)) {
		return schema.Newf(schema.CodeValidationError,
			"SHA-256 mismatch (expected %s, got %s)", expectedSHA, actual).
			WithHint("refusing to install a binary that doesn't match the manifest")
	}
	return nil
}

var releaseVersionPattern = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)(?:[-+].*)?$`)

func compareReleaseVersions(a, b string) (int, bool) {
	av, okA := parseReleaseVersion(a)
	bv, okB := parseReleaseVersion(b)
	if !okA || !okB {
		return 0, false
	}
	for i := 0; i < len(av); i++ {
		switch {
		case av[i] < bv[i]:
			return -1, true
		case av[i] > bv[i]:
			return 1, true
		}
	}
	return 0, true
}

func parseReleaseVersion(v string) ([3]int, bool) {
	var out [3]int
	m := releaseVersionPattern.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return out, false
	}
	for i := 0; i < 3; i++ {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
