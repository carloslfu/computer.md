// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

var (
	flagScreenshotOpen   bool
	flagScreenshotOutput string
)

var screenshotCmd = &cobra.Command{
	Use:   "screenshot",
	Short: "Capture a screenshot of the machine's desktop",
	Long: `Download a PNG screenshot of your VibeCraft computer's current screen.

The PNG is written to disk. The JSON envelope describes the write (path,
bytes, ts).

  vibecraft screenshot                       # writes vibecraft-<ts>.png in CWD
  vibecraft screenshot -o ./shot.png         # explicit path
  vibecraft screenshot --open                # open the file after writing`,
	RunE: runScreenshot,
}

func init() {
	screenshotCmd.Flags().BoolVar(&flagScreenshotOpen, "open", false, "Open the screenshot after downloading")
	screenshotCmd.Flags().StringVarP(&flagScreenshotOutput, "out", "o", "", "Output file path (default: vibecraft-<timestamp>.png)")
	rootCmd.AddCommand(screenshotCmd)
}

func runScreenshot(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}

	data, err := c.GetScreenshot()
	if err != nil {
		return mapDaemonError(err, "capturing screenshot")
	}

	outPath := flagScreenshotOutput
	if outPath == "" {
		ts := time.Now().Format("20060102-150405")
		outPath = "vibecraft-" + ts + ".png"
	}

	absOut, err := filepath.Abs(outPath)
	if err != nil {
		return schema.Newf(schema.CodeInternal, "resolving output path: %s", err.Error())
	}
	outPath = absOut

	dir := filepath.Dir(outPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return schema.Newf(schema.CodeInternal, "creating output directory: %s", err.Error())
	}

	if err := os.WriteFile(outPath, data, 0644); err != nil {
		return schema.Newf(schema.CodeInternal, "writing screenshot: %s", err.Error())
	}

	if flagScreenshotOpen {
		_ = openFile(outPath)
	}

	return output.Emit(schema.ScreenshotData{
		Path:  outPath,
		Bytes: len(data),
		Ts:    time.Now().UTC().Format(time.RFC3339),
	})
}

// openFile opens a file with the system default application.
func openFile(path string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "linux":
		cmd = exec.Command("xdg-open", path)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
	default:
		return nil
	}

	return cmd.Start()
}
