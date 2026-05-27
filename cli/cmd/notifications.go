// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

var (
	flagNotifUnread bool
	flagNotifSince  string
	flagNotifLimit  int
	flagNotifAll    bool
)

var notificationsCmd = &cobra.Command{
	Use:   "notifications",
	Short: "Read notifications the manager pushed to the platform",
	Long: `Notifications live on VibeCraft's platform (not on the machine itself).
The manager pushes them when its systems finish, fail, or need attention.

These commands use the account key from 'vibecraft auth login' and span
every machine on your account. Narrow to one machine with --machine.`,
}

var notifListCmd = &cobra.Command{
	Use:   "list",
	Short: "List recent notifications",
	RunE:  runNotifList,
}

var notifAckCmd = &cobra.Command{
	Use:   "ack <id>",
	Short: "Mark one notification as read (or --all for every unread)",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runNotifAck,
}

func init() {
	notifListCmd.Flags().BoolVar(&flagNotifUnread, "unread", false, "Only unread notifications")
	notifListCmd.Flags().StringVar(&flagNotifSince, "since", "", "Only items created at-or-after this RFC3339 timestamp")
	notifListCmd.Flags().IntVar(&flagNotifLimit, "limit", 0, "Cap result count (default 50, max 200)")

	notifAckCmd.Flags().BoolVar(&flagNotifAll, "all", false, "Acknowledge every unread notification")

	rootCmd.AddCommand(notificationsCmd)
	notificationsCmd.AddCommand(notifListCmd)
	notificationsCmd.AddCommand(notifAckCmd)
}

func runNotifList(cmd *cobra.Command, args []string) error {
	key, err := loadAccountKey()
	if err != nil {
		return err
	}
	q := url.Values{}
	if flagNotifUnread {
		q.Set("unread", "1")
	}
	if flagNotifSince != "" {
		q.Set("since", flagNotifSince)
	}
	if flagNotifLimit > 0 {
		q.Set("limit", fmt.Sprintf("%d", flagNotifLimit))
	}
	if flagMachineID != "" && !isFanoutSelector(flagMachineID) {
		q.Set("machine", flagMachineID)
	}
	u := platformBase() + "/api/v1/notifications"
	if encoded := q.Encode(); encoded != "" {
		u += "?" + encoded
	}

	body, status, err := platformDo("GET", u, key, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return platformError(status, body)
	}
	var resp struct {
		Notifications []struct {
			ID             string `json:"id"`
			Kind           string `json:"kind"`
			Title          string `json:"title"`
			Body           string `json:"body"`
			Priority       string `json:"priority"`
			ConversationID string `json:"conversationId"`
			CreatedAt      string `json:"createdAt"`
			ReadAt         string `json:"readAt"`
		} `json:"notifications"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return schema.Newf(schema.CodeInternal, "decoding response: %s", err.Error())
	}
	out := schema.NotificationListData{
		Notifications: make([]schema.NotificationData, 0, len(resp.Notifications)),
	}
	for _, n := range resp.Notifications {
		out.Notifications = append(out.Notifications, schema.NotificationData{
			ID:             n.ID,
			Kind:           n.Kind,
			Title:          n.Title,
			Body:           n.Body,
			Priority:       n.Priority,
			ConversationID: n.ConversationID,
			CreatedAt:      n.CreatedAt,
			ReadAt:         n.ReadAt,
		})
	}
	return output.Emit(out)
}

func runNotifAck(cmd *cobra.Command, args []string) error {
	if !flagNotifAll && len(args) != 1 {
		return schema.Newf(schema.CodeValidationError,
			"pass a notification id or use --all")
	}
	if flagNotifAll && len(args) != 0 {
		return schema.Newf(schema.CodeValidationError,
			"--all takes no positional argument")
	}
	key, err := loadAccountKey()
	if err != nil {
		return err
	}
	var u string
	if flagNotifAll {
		u = platformBase() + "/api/v1/notifications/ack-all"
	} else {
		u = platformBase() + "/api/v1/notifications/" + url.PathEscape(args[0]) + "/ack"
	}
	body, status, err := platformDo("POST", u, key, []byte("{}"))
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return platformError(status, body)
	}
	var resp struct {
		Acked int `json:"acked"`
	}
	_ = json.Unmarshal(body, &resp)
	return output.Emit(schema.NotificationAckData{Acked: resp.Acked})
}
