// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/client"
	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

var (
	flagEventsTail bool
)

var eventsCmd = &cobra.Command{
	Use:   "events",
	Short: "Stream events from this machine",
	Long: `Subscribe to the daemon's event stream and emit JSON Lines events on stdout.

  vibecraft events --tail              # follow forever (Ctrl-C to stop)

Each event is one JSON object per line: {v,event,ts,...}. See 'vibecraft docs'
for the event-type catalog.`,
	RunE: runEvents,
}

func init() {
	eventsCmd.Flags().BoolVar(&flagEventsTail, "tail", false, "Follow forever until Ctrl-C")
	rootCmd.AddCommand(eventsCmd)
}

func runEvents(cmd *cobra.Command, args []string) error {
	if !flagEventsTail {
		return schema.Newf(schema.CodeValidationError,
			"vibecraft events requires --tail").
			WithHint("there is no point-in-time form; pass --tail")
	}

	c, err := newClient()
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	onEvent := func(ev client.StreamEvent) error {
		fields := map[string]any{}
		if len(ev.Data) > 0 {
			var generic map[string]any
			if err := json.Unmarshal(ev.Data, &generic); err == nil {
				for k, v := range generic {
					if k == "v" || k == "event" || k == "ts" {
						continue
					}
					fields[k] = v
				}
			} else {
				fields["raw"] = ev.Raw
			}
		}
		eventType := ev.Type
		if eventType == "" {
			eventType = schema.EventMessage
		}
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		return output.EmitEvent(eventType, ts, fields)
	}

	streamErr := c.Stream(ctx, client.StreamOpts{}, onEvent)
	if streamErr != nil {
		if errors.Is(streamErr, context.Canceled) {
			return output.EmitEvent(schema.EventEnd, time.Now().UTC().Format(time.RFC3339Nano),
				map[string]any{"reason": "interrupted"})
		}
		return mapDaemonError(streamErr, "streaming")
	}
	return nil
}
