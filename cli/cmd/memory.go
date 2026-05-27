// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/client"
	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

var (
	flagMemoryCategory string
	flagMemoryQuery    string
	flagMemoryFromFile string
)

var memoryCmd = &cobra.Command{
	Use:   "memory",
	Short: "Manage long-term memory the manager refers to across tasks",
	Long: `Memory is the manager's persistent key/value store for facts about the
customer, their conventions, and ongoing context. It is NOT for secrets —
use 'vault' for those.

Memory items have a category (default "general"), a key, and a value. The
manager reads memory when assembling task context; it picks what to surface
based on the task.`,
}

var memoryListCmd = &cobra.Command{
	Use:   "list",
	Short: "List memory items",
	Long: `List memory items. Filter by --category or --search (full-text against value).`,
	RunE: runMemoryList,
}

var memorySetCmd = &cobra.Command{
	Use:   "set <key> [value]",
	Short: "Store a memory item",
	Long: `Write or overwrite a memory item. Value comes from:

  positional arg                literal
  -                             from stdin (vibecraft memory set k -)
  --from-file <path>            from a file

Use --category to group items; defaults to "general".`,
	Args: cobra.MinimumNArgs(1),
	RunE: runMemorySet,
}

var memoryDeleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Remove a memory item by id",
	Args:  cobra.ExactArgs(1),
	RunE:  runMemoryDelete,
}

func init() {
	rootCmd.AddCommand(memoryCmd)

	memoryListCmd.Flags().StringVar(&flagMemoryCategory, "category", "", "Filter by category")
	memoryListCmd.Flags().StringVar(&flagMemoryQuery, "search", "", "Search for text in values")

	memorySetCmd.Flags().StringVar(&flagMemoryCategory, "category", "", "Category (default \"general\")")
	memorySetCmd.Flags().StringVar(&flagMemoryFromFile, "from-file", "", "Read value from this file")

	memoryCmd.AddCommand(memoryListCmd)
	memoryCmd.AddCommand(memorySetCmd)
	memoryCmd.AddCommand(memoryDeleteCmd)
}

func runMemoryList(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	items, err := c.ListMemory(client.ListMemoryOpts{
		Category: flagMemoryCategory,
		Query:    flagMemoryQuery,
	})
	if err != nil {
		return mapDaemonError(err, "listing memory")
	}
	out := schema.MemoryListData{Items: make([]schema.MemoryItemData, 0, len(items))}
	for _, it := range items {
		out.Items = append(out.Items, schema.MemoryItemData{
			ID:        it.ID,
			Category:  it.Category,
			Key:       it.Key,
			Value:     it.Value,
			Metadata:  it.Metadata,
			CreatedAt: it.CreatedAt,
			UpdatedAt: it.UpdatedAt,
		})
	}
	return output.Emit(out)
}

func runMemorySet(cmd *cobra.Command, args []string) error {
	key := args[0]
	rest := args[1:]

	value, err := resolveMemoryValue(rest)
	if err != nil {
		return err
	}
	if value == "" {
		return schema.Newf(schema.CodeValidationError, "memory value cannot be empty")
	}

	c, err := newClient()
	if err != nil {
		return err
	}
	item, err := c.SetMemory(flagMemoryCategory, key, value)
	if err != nil {
		return mapDaemonError(err, "setting memory")
	}
	return output.Emit(schema.MemoryItemData{
		ID:        item.ID,
		Category:  item.Category,
		Key:       item.Key,
		Value:     item.Value,
		Metadata:  item.Metadata,
		CreatedAt: item.CreatedAt,
		UpdatedAt: item.UpdatedAt,
	})
}

func runMemoryDelete(cmd *cobra.Command, args []string) error {
	id := args[0]
	c, err := newClient()
	if err != nil {
		return err
	}
	if err := c.DeleteMemory(id); err != nil {
		return mapDaemonError(err, "deleting memory")
	}
	return output.Emit(schema.MemoryDeleteData{ID: id, Status: "deleted"})
}

func resolveMemoryValue(rest []string) (string, error) {
	if flagMemoryFromFile != "" {
		if len(rest) > 0 {
			return "", schema.Newf(schema.CodeValidationError,
				"--from-file and a positional value are mutually exclusive")
		}
		b, err := os.ReadFile(flagMemoryFromFile)
		if err != nil {
			return "", schema.Newf(schema.CodePathNotFound,
				"reading value file: %s", err.Error())
		}
		return strings.TrimRight(string(b), "\n"), nil
	}
	if len(rest) == 1 && rest[0] == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", schema.Newf(schema.CodeInternal,
				"reading value from stdin: %s", err.Error())
		}
		return strings.TrimRight(string(b), "\n"), nil
	}
	return strings.Join(rest, " "), nil
}
