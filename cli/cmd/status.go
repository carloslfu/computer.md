// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/client"
	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show machine health",
	Long: `Display the current status and health of your VibeCraft computer.

Default output is the JSON success envelope:

  {"v":1,"ok":true,"data":{"machine_id":"vc-abc","status":"healthy",
                           "uptime_seconds":3600,"current_task":"abc-123"}}

Pass --text for a human-readable rendering.

Exit codes: 0 on success, 1 on auth/network/server error.`,
	RunE: runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, args []string) error {
	// Fan-out when --machine resolves to many.
	if isFanoutSelector(flagMachineID) {
		cfg, err := loadConfig()
		if err != nil {
			return schema.Newf(schema.CodeInternal, "%s", err.Error())
		}
		machines, err := matchMachines(cfg, flagMachineID)
		if err != nil {
			return err
		}
		return runFanoutForStatus(machines)
	}

	c, err := newClient()
	if err != nil {
		return err
	}

	status, err := c.GetStatus()
	if err != nil {
		return mapDaemonError(err, "machine unreachable")
	}

	return output.Emit(schema.StatusData{
		MachineID:     status.MachineID,
		Status:        status.Status,
		UptimeSeconds: status.Uptime,
		CurrentTask:   status.CurrentTask,
	})
}

// mapDaemonError converts a transport-level client error into a structured
// *schema.Error so the JSON envelope on stderr has a stable `code` field
// for agents to branch on. The fallback hint nudges the user toward auth
// when the most likely cause is a missing or expired key.
func mapDaemonError(err error, defaultMessage string) error {
	if err == nil {
		return nil
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 400, 409, 422:
			if isTaskNotFoundContext(defaultMessage, apiErr.Message, apiErr.StatusCode) {
				return schema.Newf(schema.CodeTaskNotFound, "%s", apiErr.Error())
			}
			// The daemon rejected the request as malformed / not
			// applicable (bad body, stale state, etc.).
			// That's a caller error: validation_error, not internal_error.
			return schema.Newf(schema.CodeValidationError, "%s", apiErr.Error())
		case 401:
			return schema.Newf(schema.CodeAuthInvalid,
				"daemon rejected credentials").WithHint("run 'vibecraft auth login' to refresh")
		case 403:
			return schema.Newf(schema.CodeAuthForbidden, "%s", apiErr.Error())
		case 404:
			if isPathNotFoundContext(defaultMessage) {
				return schema.Newf(schema.CodePathNotFound, "%s", apiErr.Error())
			}
			if isTaskNotFoundContext(defaultMessage, apiErr.Message, apiErr.StatusCode) {
				return schema.Newf(schema.CodeTaskNotFound, "%s", apiErr.Error())
			}
			if defaultMessage != "machine unreachable" && defaultMessage != "version probe" {
				return schema.Newf(schema.CodeValidationError, "%s", apiErr.Error())
			}
			return schema.Newf(schema.CodeMachineNotFound, "%s", apiErr.Error())
		case 429:
			return schema.Newf(schema.CodeRateLimited, "%s", apiErr.Error())
		}
		if apiErr.StatusCode >= 500 {
			return schema.Newf(schema.CodeServerError, "%s", apiErr.Error())
		}
		return schema.Newf(schema.CodeInternal, "%s", apiErr.Error())
	}
	return schema.Newf(schema.CodeMachineUnreachable, "%s: %s", defaultMessage, err.Error())
}

func isPathNotFoundContext(defaultMessage string) bool {
	msg := strings.ToLower(defaultMessage)
	for _, needle := range []string{"file", "download", "computer.md"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func isTaskNotFoundContext(defaultMessage, apiMessage string, statusCode int) bool {
	ctx := strings.ToLower(defaultMessage)
	if statusCode == http.StatusNotFound && strings.Contains(ctx, "task") {
		return true
	}
	msg := strings.ToLower(defaultMessage + " " + apiMessage)
	return strings.Contains(msg, "task") && strings.Contains(msg, "not found")
}
