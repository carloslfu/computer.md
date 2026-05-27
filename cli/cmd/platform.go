// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/carloslfu/computer.md/cli/schema"
)

// Platform-API helpers. The CLI talks to the VibeCraft platform
// (www.vibecraft.so) for three things, all authenticated by the
// account key (`vc_account_*`) from `vibecraft auth login`:
//
//   - listing the account's machines       (GET  /api/v1/machines)
//   - brokering a per-machine daemon key    (POST /api/v1/machines/<id>/cli-key)
//   - reading notifications                 (GET  /api/v1/notifications)
//
// Everything else — tasks, files, vault, memory, streaming — goes
// straight to the machine's daemon with the brokered daemon key.

// platformBase returns the platform base URL. VIBECRAFT_PLATFORM_URL
// overrides for local dev / tests.
func platformBase() string {
	if v := os.Getenv("VIBECRAFT_PLATFORM_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://www.vibecraft.so"
}

// loadAccountKey returns the account key from config, or an
// auth_required error if there isn't one. VIBECRAFT_ACCOUNT_KEY
// overrides (headless / CI).
func loadAccountKey() (string, error) {
	if v := os.Getenv("VIBECRAFT_ACCOUNT_KEY"); v != "" {
		return strings.TrimSpace(v), nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return "", schema.Newf(schema.CodeInternal, "%s", err.Error())
	}
	if cfg == nil || cfg.AccountKey == "" {
		return "", schema.Newf(schema.CodeAuthRequired,
			"not authenticated to your VibeCraft account").
			WithHint("run 'vibecraft auth login'")
	}
	return cfg.AccountKey, nil
}

// platformDo issues an authenticated request to the platform and
// returns (body, status, error). A network failure is a
// machine_unreachable schema error; HTTP-level errors come back via
// the status int for the caller to map.
func platformDo(method, url, key string, body []byte) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return nil, 0, schema.Newf(schema.CodeInternal, "%s", err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", "vibecraft-cli/"+cliVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, schema.Newf(schema.CodeMachineUnreachable,
			"reaching the VibeCraft platform: %s", redactKey(err.Error()))
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return respBody, resp.StatusCode, nil
}

// platformError maps a non-2xx platform response to a schema error.
//
// A platform route may return an explicit machine-readable `code` in
// its JSON body — when present, the CLI trusts it over HTTP-status
// guessing (status codes like 404/409 are overloaded: "machine not
// found" and "no such user" are both 404 but want different codes).
// Status-based mapping is only the fallback for routes that don't set
// a code.
func platformError(status int, body []byte) error {
	var er struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(body, &er)
	msg := er.Error
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	if er.Code != "" {
		e := schema.Newf(er.Code, "%s", msg)
		if er.Code == schema.CodeAuthInvalid {
			e = e.WithHint("run 'vibecraft auth login' to re-authenticate")
		}
		if er.Code == schema.CodeLimitReached {
			e = e.WithHint("upgrade the plan from the dashboard, then retry")
		}
		return e
	}
	switch {
	case status == http.StatusBadRequest:
		return schema.Newf(schema.CodeValidationError, "%s", msg)
	case status == http.StatusUnauthorized:
		return schema.Newf(schema.CodeAuthInvalid, "%s", msg).
			WithHint("run 'vibecraft auth login' to re-authenticate")
	case status == http.StatusForbidden:
		return schema.Newf(schema.CodeAuthForbidden, "%s", msg)
	case status == http.StatusNotFound:
		return schema.Newf(schema.CodeMachineNotFound, "%s", msg)
	case status == http.StatusConflict:
		return schema.Newf(schema.CodeValidationError, "%s", msg)
	case status == http.StatusTooManyRequests:
		return schema.Newf(schema.CodeRateLimited, "%s", msg)
	case status >= 500:
		return schema.Newf(schema.CodeServerError, "%s", msg)
	}
	return schema.Newf(schema.CodeInternal, "%s", msg)
}

// PlatformMachine is one entry from GET /api/v1/machines.
type PlatformMachine struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Host   string `json:"host"`
	Status string `json:"status"`
	Access string `json:"access"` // owner | control
}

// listAccountMachines fetches every machine the account controls.
func listAccountMachines(accountKey string) ([]PlatformMachine, error) {
	body, status, err := platformDo("GET", platformBase()+"/api/v1/machines", accountKey, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, platformError(status, body)
	}
	var resp struct {
		Machines []PlatformMachine `json:"machines"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, schema.Newf(schema.CodeInternal, "decoding machines: %s", err.Error())
	}
	return resp.Machines, nil
}

// brokerDaemonKey exchanges the account key for a per-machine daemon
// key (`vc_machine_*`). The platform validates the account key, checks
// the user controls the machine, and mints the key at the daemon.
func brokerDaemonKey(accountKey, machineID string) (machineURL, apiKey string, err error) {
	url := platformBase() + "/api/v1/machines/" + machineID + "/cli-key"
	body, status, err := platformDo("POST", url, accountKey, []byte("{}"))
	if err != nil {
		return "", "", err
	}
	if status != http.StatusOK {
		return "", "", platformError(status, body)
	}
	var resp struct {
		MachineID  string `json:"machine_id"`
		MachineURL string `json:"machine_url"`
		APIKey     string `json:"api_key"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", "", schema.Newf(schema.CodeInternal, "decoding brokered key: %s", err.Error())
	}
	if resp.APIKey == "" || resp.MachineURL == "" {
		return "", "", schema.Newf(schema.CodeServerError, "platform returned an incomplete key")
	}
	return resp.MachineURL, resp.APIKey, nil
}

// platformGetJSON does an authenticated GET and returns the parsed body.
func platformGetJSON(accountKey, path string) (any, error) {
	body, status, err := platformDo("GET", platformBase()+path, accountKey, nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, platformError(status, body)
	}
	var v any
	if uerr := json.Unmarshal(body, &v); uerr != nil {
		return nil, schema.Newf(schema.CodeInternal, "decoding response: %s", uerr.Error())
	}
	return v, nil
}

// platformMutate does an authenticated POST/DELETE with an optional JSON
// body and returns the parsed response. Error mapping (including the
// limit_reached / validation_error distinction for overloaded 409s) is
// handled by platformError via the route's explicit `code` field.
func platformMutate(accountKey, method, path string, body []byte) (any, error) {
	respBody, status, err := platformDo(method, platformBase()+path, accountKey, body)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, platformError(status, respBody)
	}
	var v any
	if uerr := json.Unmarshal(respBody, &v); uerr != nil {
		return nil, schema.Newf(schema.CodeInternal, "decoding response: %s", uerr.Error())
	}
	return v, nil
}

// redactKey scrubs anything key-shaped from a string before it goes
// into an error message (mirror of the client package's redaction).
func redactKey(s string) string {
	for _, prefix := range []string{"vc_account_", "vc_machine_", "vk_"} {
		for {
			idx := strings.Index(s, prefix)
			if idx < 0 {
				break
			}
			end := idx + len(prefix)
			for end < len(s) {
				c := s[end]
				alnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
				if !alnum && c != '_' && c != '-' {
					break
				}
				end++
			}
			s = s[:idx] + "[redacted]" + s[end:]
		}
	}
	return s
}
