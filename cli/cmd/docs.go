// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	_ "embed"

	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

// llmsTxt is the agent-facing reference, embedded into the binary at
// build time from the repo-root llms.txt. The same file is served at
// https://www.vibecraft.so/llms.txt by the platform — single source,
// two consumers, drift impossible by construction.
//
//go:embed llms_embed.txt
var llmsTxt string

var docsCmd = &cobra.Command{
	Use:   "docs",
	Short: "Print the agent-facing reference (llms.txt)",
	Long: `Print the bundled llms.txt — the canonical agent-facing reference for
this CLI. The same content is served at https://www.vibecraft.so/llms.txt.

  vibecraft docs              # raw markdown to stdout
  vibecraft docs --json       # wrapped in {v,ok,data:{url,content,...}}`,
	RunE: runDocs,
}

func init() {
	rootCmd.AddCommand(docsCmd)
}

func runDocs(cmd *cobra.Command, args []string) error {
	return output.Emit(schema.DocsData{
		URL:     "https://www.vibecraft.so/llms.txt",
		Version: schema.Version,
		Content: llmsTxt,
	})
}
