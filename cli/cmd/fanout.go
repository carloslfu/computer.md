// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/carloslfu/computer.md/cli/client"
	"github.com/carloslfu/computer.md/cli/exit"
	"github.com/carloslfu/computer.md/cli/schema"
)

// Multi-machine fan-out (Track A12). When --machine resolves to more
// than one machine, the runner orchestrates concurrent unary calls
// against every selected machine and emits JSON Lines on stdout — one
// envelope per machine, each carrying a top-level "machine" field. The
// process exit code is the WORST per-machine code (4 > 3 > 2 > 1 > 0).
//
// Streaming verbs under fan-out are NOT implemented today; the plan
// describes the demux pattern (interleaved events with `machine` per
// event) but it overlaps significantly with single-machine streaming
// and gives marginal value over the agent running `task stream` in
// parallel processes. If demand surfaces, add the streaming form here.

var (
	flagFanoutMaxConc     int
	flagFanoutPerMachine  time.Duration
	flagFanoutOrdered     bool
)

// matchMachines returns the set of machines that match the --machine
// selector (a single ID, comma list, glob, or "all"). Empty selector
// returns the active machine only.
//
// When an account key is present it first refreshes cfg.Machines from
// the platform, so `--machine all` / globs see machines provisioned
// since the last login — not just the locally-cached set.
func matchMachines(cfg *Config, selector string) ([]string, error) {
	// Resolve the account key via loadAccountKey() — env var first, then
	// config — so `--machine all` / globs work under headless / CI auth
	// (VIBECRAFT_ACCOUNT_KEY, no config file), not just config-file auth.
	if accountKey, keyErr := loadAccountKey(); keyErr == nil && accountKey != "" {
		live, fetchErr := listAccountMachines(accountKey)
		if fetchErr != nil {
			// Env-var-only auth has no local cache to fall back to.
			if cfg == nil {
				return nil, fetchErr
			}
			// Config auth: fall through to whatever's cached locally.
		} else {
			rebuilt := map[string]MachineConfig{}
			for _, m := range live {
				var prev MachineConfig
				if cfg != nil {
					prev = cfg.Machines[m.ID]
				}
				rebuilt[m.ID] = MachineConfig{
					URL:    "https://" + m.Host,
					APIKey: prev.APIKey,
					Name:   m.Name,
				}
			}
			if cfg == nil {
				// Headless / env-var auth: no config file to cache into —
				// synthesize an in-memory fleet so the matching below works.
				cfg = &Config{Machines: rebuilt}
			} else {
				cfg.Machines = rebuilt
				_ = saveConfig(cfg)
			}
		}
	}
	if cfg == nil || len(cfg.Machines) == 0 {
		return nil, schema.Newf(schema.CodeAuthRequired, "no machines available").
			WithHint("run 'vibecraft auth login', or set VIBECRAFT_ACCOUNT_KEY")
	}
	ids := make([]string, 0, len(cfg.Machines))
	for id := range cfg.Machines {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if selector == "" {
		if cfg.ActiveMachine != "" {
			return []string{cfg.ActiveMachine}, nil
		}
		return ids[:1], nil
	}
	if selector == "all" {
		return ids, nil
	}
	// Comma list — accept exact ids only.
	if strings.Contains(selector, ",") {
		want := strings.Split(selector, ",")
		out := make([]string, 0, len(want))
		for _, w := range want {
			w = strings.TrimSpace(w)
			if _, ok := cfg.Machines[w]; !ok {
				return nil, schema.Newf(schema.CodeMachineNotFound, "machine %q not in config", w)
			}
			out = append(out, w)
		}
		return out, nil
	}
	// Glob pattern (filepath.Match semantics on the machine id).
	if strings.ContainsAny(selector, "*?[") {
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			ok, _ := filepath.Match(selector, id)
			if ok {
				out = append(out, id)
			}
		}
		if len(out) == 0 {
			return nil, schema.Newf(schema.CodeMachineNotFound, "no machines match %q", selector)
		}
		return out, nil
	}
	// Bare id.
	if _, ok := cfg.Machines[selector]; !ok {
		return nil, schema.Newf(schema.CodeMachineNotFound, "machine %q not in config", selector)
	}
	return []string{selector}, nil
}

// fanoutCallable is the unary work each machine runs. It returns
// (dataValue, perMachineExitCode, error). The data value is what would
// otherwise be passed to output.Emit; on fanout it gets wrapped in the
// {v,machine,ok,data|error} envelope.
type fanoutCallable func(machineID string, c *client.Client) (any, int, error)

// runFanout dispatches workFn against each of the resolved machines
// concurrently. Each machine gets its own client built from the config.
// One JSON Lines envelope per machine is emitted as soon as the work
// returns (finish-order). --ordered buffers and sorts by id.
//
// Returns the worst per-machine exit code via *exit.OutcomeError when
// non-zero, so the caller can `return runFanout(...)` and main() picks
// up the code.
func runFanout(machines []string, workFn fanoutCallable) error {
	maxConc := flagFanoutMaxConc
	if maxConc <= 0 {
		maxConc = 8
	}
	if maxConc > 32 {
		maxConc = 32
	}
	if len(machines) < maxConc {
		maxConc = len(machines)
	}

	type result struct {
		Machine string
		Data    any
		Err     *schema.Error
		Code    int
	}

	cfg, err := loadConfig()
	if err != nil {
		return schema.Newf(schema.CodeInternal, "%s", err.Error())
	}

	// Pre-resolve daemon creds for every machine SEQUENTIALLY before the
	// worker pool runs. Brokering mutates + saves cfg; doing it single-
	// threaded here avoids a concurrent-map / concurrent-save race. After
	// this, the worker pool only does read-only work against fixed creds.
	type creds struct {
		url string
		key string
		err *schema.Error
	}
	resolved := make(map[string]creds, len(machines))
	for _, id := range machines {
		u, k, cerr := daemonCredsFor(cfg, id)
		if cerr != nil {
			var se *schema.Error
			if errors.As(cerr, &se) {
				resolved[id] = creds{err: se}
			} else {
				resolved[id] = creds{err: schema.Newf(schema.CodeInternal, "%s", cerr.Error())}
			}
			continue
		}
		resolved[id] = creds{url: u, key: k}
	}

	results := make(chan result, len(machines))
	work := make(chan string, len(machines))
	for _, m := range machines {
		work <- m
	}
	close(work)

	var wg sync.WaitGroup
	wg.Add(maxConc)
	for i := 0; i < maxConc; i++ {
		go func() {
			defer wg.Done()
			for id := range work {
				cr := resolved[id]
				if cr.err != nil {
					results <- result{Machine: id, Err: cr.err, Code: exit.CLIError}
					continue
				}
				c := client.New(cr.url, cr.key)
				c.UserAgent = "vibecraft-cli/" + cliVersion
				// Per-machine timeout.
				if flagFanoutPerMachine > 0 {
					c.HTTPClient.Timeout = flagFanoutPerMachine
				}
				data, code, err := workFn(id, c)
				r := result{Machine: id, Data: data, Code: code}
				if err != nil {
					var se *schema.Error
					if errors.As(err, &se) {
						r.Err = se
					} else {
						r.Err = schema.Newf(schema.CodeInternal, "%s", err.Error())
					}
					if r.Code == 0 {
						r.Code = exit.CLIError
					}
				}
				results <- r
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	worstCode := exit.OK
	var collected []result
	for r := range results {
		if r.Code > worstCode {
			worstCode = r.Code
		}
		if flagFanoutOrdered {
			collected = append(collected, r)
			continue
		}
		emitFanoutLine(os.Stdout, r.Machine, r.Data, r.Err)
	}
	if flagFanoutOrdered {
		sort.Slice(collected, func(i, j int) bool { return collected[i].Machine < collected[j].Machine })
		for _, r := range collected {
			emitFanoutLine(os.Stdout, r.Machine, r.Data, r.Err)
		}
	}

	if worstCode != exit.OK {
		return &exit.OutcomeError{Code: worstCode}
	}
	return nil
}

// emitFanoutLine writes one JSON Lines envelope for a single machine's
// result.
func emitFanoutLine(w io.Writer, machine string, data any, err *schema.Error) {
	out := map[string]any{
		"v":       schema.Version,
		"machine": machine,
	}
	if err != nil {
		out["ok"] = false
		out["error"] = err
	} else {
		out["ok"] = true
		out["data"] = data
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(out)
}

// isFanoutSelector returns true when the --machine selector resolves to
// more than one machine (or is the literal "all" / a glob — handled in
// matchMachines).
func isFanoutSelector(selector string) bool {
	if selector == "all" {
		return true
	}
	if strings.ContainsAny(selector, ",*?[") {
		return true
	}
	return false
}

func init() {
	// Fan-out flags are registered on root so they apply to every verb
	// (the verb itself decides whether to honor --machine for fan-out).
	rootCmd.PersistentFlags().IntVar(&flagFanoutMaxConc, "max-concurrency", 8, "Cap concurrent fan-out workers (max 32)")
	rootCmd.PersistentFlags().DurationVar(&flagFanoutPerMachine, "per-machine-timeout", 0, "Per-machine timeout (0 = use client default)")
	rootCmd.PersistentFlags().BoolVar(&flagFanoutOrdered, "ordered", false, "Buffer and emit fan-out lines sorted by machine id")
}

// runFanoutForStatus is the fan-out variant of `vibecraft status`.
// Exposed so the status command can dispatch through it when the user
// passes `--machine all` or a list/glob.
func runFanoutForStatus(machines []string) error {
	return runFanout(machines, func(id string, c *client.Client) (any, int, error) {
		st, err := c.GetStatus()
		if err != nil {
			return nil, 0, mapDaemonError(err, "machine unreachable")
		}
		return schema.StatusData{
			MachineID:     st.MachineID,
			Status:        st.Status,
			UptimeSeconds: st.Uptime,
			CurrentTask:   st.CurrentTask,
		}, exit.OK, nil
	})
}

var _ = context.Canceled // import-pin
var _ = fmt.Sprintf      // import-pin
