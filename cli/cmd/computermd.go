// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

// COMPUTER.md — the machine config the customer and the manager share.
// `vibecraft computer-md` prints it; `--set` overwrites it from stdin.

var flagComputerMDSet bool

var computerMDCmd = &cobra.Command{
	Use:   "computer-md",
	Short: "Read or replace the machine's COMPUTER.md",
	Long: `COMPUTER.md is the shared config/notes file the customer and the
on-machine manager both read.

  vibecraft computer-md                       # print it
  cat new-computer.md | vibecraft computer-md --set   # replace it (stdin)`,
	RunE: runComputerMD,
}

func init() {
	computerMDCmd.Flags().BoolVar(&flagComputerMDSet, "set", false, "Replace COMPUTER.md with content read from stdin")
	rootCmd.AddCommand(computerMDCmd)
}

func runComputerMD(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}

	if flagComputerMDSet {
		raw, readErr := io.ReadAll(os.Stdin)
		if readErr != nil {
			return schema.Newf(schema.CodeInternal, "reading stdin: %s", readErr.Error())
		}
		if len(strings.TrimSpace(string(raw))) == 0 {
			return schema.Newf(schema.CodeValidationError,
				"--set reads the new content from stdin, but stdin was empty")
		}
		if err := c.SetComputerMD(string(raw)); err != nil {
			return mapDaemonError(err, "writing COMPUTER.md")
		}
		// Read it back so the envelope reflects what's now on disk.
		content, path, getErr := c.GetComputerMD()
		if getErr != nil {
			// The write succeeded; a read-back failure shouldn't fail the
			// command. Report what we wrote.
			return output.Emit(schema.ComputerMDData{Content: string(raw)})
		}
		return output.Emit(schema.ComputerMDData{Path: path, Content: content})
	}

	content, path, err := c.GetComputerMD()
	if err != nil {
		return mapDaemonError(err, "reading COMPUTER.md")
	}
	return output.Emit(schema.ComputerMDData{Path: path, Content: content})
}
