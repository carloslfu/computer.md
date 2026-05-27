// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

// Conversations — the threads tasks belong to. `task messages <id>`
// reads one transcript by task id; these verbs work at the
// conversation level: list every conversation, or fetch one whole
// (messages + tasks) by conversation id.

var conversationsCmd = &cobra.Command{
	Use:   "conversations",
	Short: "List and read conversations",
	Long:  `List the machine's conversations, or fetch one whole conversation by id.`,
}

var conversationsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List conversations",
	RunE:  runConversationsList,
}

var conversationsGetCmd = &cobra.Command{
	Use:   "get <conversation-id>",
	Short: "Fetch a conversation (messages + tasks)",
	Args:  cobra.ExactArgs(1),
	RunE:  runConversationsGet,
}

func init() {
	rootCmd.AddCommand(conversationsCmd)
	conversationsCmd.AddCommand(conversationsListCmd)
	conversationsCmd.AddCommand(conversationsGetCmd)
}

func runConversationsList(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	data, err := c.ListConversations()
	if err != nil {
		return mapDaemonError(err, "listing conversations")
	}
	return output.Emit(map[string]any{"conversations": data})
}

func runConversationsGet(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	conv, err := c.GetConversation(args[0])
	if err != nil {
		return mapDaemonError(err, "fetching conversation")
	}
	msgs := make([]schema.MessageData, 0, len(conv.Messages))
	for _, m := range conv.Messages {
		msgs = append(msgs, schema.MessageData{
			ID:        m.ID,
			Role:      m.Role,
			Content:   m.Content,
			TaskID:    m.TaskID,
			CreatedAt: m.CreatedAt,
		})
	}
	return output.Emit(map[string]any{
		"id":           args[0],
		"conversation": conv.Conversation,
		"messages":     msgs,
		"tasks":        conv.Tasks,
	})
}
