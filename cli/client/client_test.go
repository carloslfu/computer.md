// SPDX-License-Identifier: Apache-2.0

package client

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer is a minimal handler we point the client at for unit tests.
// Each test installs its own handlers via mux.
func fakeServer(t *testing.T) (*httptest.Server, *http.ServeMux) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close() })
	return srv, mux
}

func TestRedactSecrets(t *testing.T) {
	cases := []struct {
		in  string
		out string
	}{
		{"vc_machine_abcXYZ123-456 leaked", "[redacted] leaked"},
		{"prefix vc_machine_abc and middle vc_machine_xyz end", "prefix [redacted] and middle [redacted] end"},
		{"no secrets here", "no secrets here"},
		{"", ""},
	}
	for _, tc := range cases {
		got := redactSecrets(tc.in)
		if got != tc.out {
			t.Errorf("redactSecrets(%q) = %q, want %q", tc.in, got, tc.out)
		}
	}
}

func TestGetStatus_OK(t *testing.T) {
	srv, mux := fakeServer(t)
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer vc_machine_") {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"healthy","machine_id":"vc-test","uptime":42}`)
	})

	c := New(srv.URL, "vc_machine_test_abc")
	status, err := c.GetStatus()
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if status.Status != "healthy" {
		t.Errorf("status: got %q, want healthy", status.Status)
	}
	if status.MachineID != "vc-test" {
		t.Errorf("machine_id: got %q, want vc-test", status.MachineID)
	}
}

func TestSubmitTask_OK(t *testing.T) {
	srv, mux := fakeServer(t)
	mux.HandleFunc("/api/task", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"task-1","status":"queued","conversation_id":"conv-1"}`)
	})

	c := New(srv.URL, "vc_machine_test_abc")
	resp, err := c.SubmitTask("hello")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if resp.ID != "task-1" {
		t.Errorf("ID: got %q, want task-1", resp.ID)
	}
}

func TestRespondToTask_OK(t *testing.T) {
	srv, mux := fakeServer(t)
	mux.HandleFunc("/api/task/task-1/respond", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"input received"}`)
	})

	c := New(srv.URL, "vc_machine_test_abc")
	if err := c.RespondToTask("task-1", "yes"); err != nil {
		t.Fatalf("RespondToTask: %v", err)
	}
}

func TestDo_Retries5xxOnGET(t *testing.T) {
	var hits int32
	srv, mux := fakeServer(t)
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 3 {
			http.Error(w, `{"error":"upstream"}`, http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"healthy","machine_id":"vc-test","uptime":1}`)
	})

	c := New(srv.URL, "vc_machine_test_abc")
	c.HTTPClient.Timeout = 2 * time.Second
	if _, err := c.GetStatus(); err != nil {
		t.Fatalf("GetStatus should have retried past 5xx: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("hits: got %d, want 3", got)
	}
}

func TestDo_DoesNotRetry5xxOnPOST(t *testing.T) {
	var hits int32
	srv, mux := fakeServer(t)
	mux.HandleFunc("/api/task", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, `{"error":"oops"}`, http.StatusInternalServerError)
	})

	c := New(srv.URL, "vc_machine_test_abc")
	_, err := c.SubmitTask("hello")
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 500 {
		t.Fatalf("expected APIError 500, got %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("POST should not retry 5xx; hits=%d", got)
	}
}

// TestDo_DoesNotRetryPOSTAfterPostSendTimeout pins the at-most-once
// contract for non-idempotent methods: once the request body has been
// sent (the daemon received and may have processed it), a transport
// failure while awaiting/reading the response must NOT trigger a retry,
// or the side effect is applied twice. We simulate the common case of a
// slow daemon: the handler fully reads the request body (so the request
// landed) then stalls past the client's short timeout, surfacing as a
// post-send "sending request" error inside doOnce.
func TestDo_DoesNotRetryPOSTAfterPostSendTimeout(t *testing.T) {
	var hits int32
	srv, mux := fakeServer(t)
	mux.HandleFunc("/api/task/task-1/respond", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		// Drain the body so the request provably landed server-side.
		_, _ = io.Copy(io.Discard, r.Body)
		// Stall past the client timeout: the request is done, only the
		// response is missing — exactly the post-send failure window.
		time.Sleep(750 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"input received"}`)
	})

	c := New(srv.URL, "vc_machine_test_abc")
	c.HTTPClient.Timeout = 200 * time.Millisecond

	err := c.RespondToTask("task-1", "yes")
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	// The body must have been delivered exactly once. A retry here would
	// double-apply the user's reply to a waiting_for_input task.
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("post-send timeout on POST must not retry; handler hit %d times, want 1", got)
	}
}

// TestDo_RetriesPOSTOnPreSendNetworkError confirms the safe half of the
// contract still holds: a pre-send connection failure (request never
// written) is retryable even for non-idempotent methods, because no side
// effect could have been applied yet.
func TestDo_RetriesPOSTOnPreSendNetworkError(t *testing.T) {
	var hits int32
	srv, mux := fakeServer(t)
	mux.HandleFunc("/api/task/task-1/respond", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"input received"}`)
	})

	c := New(srv.URL, "vc_machine_test_abc")
	c.HTTPClient.Timeout = 2 * time.Second
	// First attempt fails pre-send (connection refused on a dead port),
	// then we repoint at the live server to prove the retry happened.
	c.BaseURL = "http://127.0.0.1:1"
	var attempts int32
	c.HTTPClient.Transport = preSendThenLive(srv.URL, &attempts)

	if err := c.RespondToTask("task-1", "yes"); err != nil {
		t.Fatalf("pre-send network error on POST should retry and then succeed: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got < 2 {
		t.Errorf("expected a retry after pre-send failure; transport attempts=%d", got)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("server should have been hit exactly once after retry; hits=%d", got)
	}
}

// preSendThenLive returns a RoundTripper whose first call fails before the
// request reaches any server (simulating connection-refused / DNS), and
// whose subsequent calls are routed to liveURL. It never reads the request
// body on the failing call, mirroring a real pre-flight transport error.
func preSendThenLive(liveURL string, attempts *int32) http.RoundTripper {
	live := http.DefaultTransport.(*http.Transport).Clone()
	parsed, _ := neturl.Parse(liveURL)
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		n := atomic.AddInt32(attempts, 1)
		if n == 1 {
			return nil, &neturl.Error{Op: "Post", URL: req.URL.String(), Err: errConnRefused}
		}
		req.URL.Scheme = parsed.Scheme
		req.URL.Host = parsed.Host
		return live.RoundTrip(req)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

var errConnRefused = errors.New("connect: connection refused")

func TestDo_Retries429WithRetryAfter(t *testing.T) {
	var hits int32
	srv, mux := fakeServer(t)
	mux.HandleFunc("/api/task", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, `{"error":"slow down"}`, http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"task-2","status":"queued"}`)
	})

	c := New(srv.URL, "vc_machine_test_abc")
	c.HTTPClient.Timeout = 5 * time.Second
	resp, err := c.SubmitTask("hello")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if resp.ID != "task-2" {
		t.Errorf("ID: got %q, want task-2", resp.ID)
	}
	if got := atomic.LoadInt32(&hits); got < 2 {
		t.Errorf("expected at least 2 attempts; got %d", got)
	}
}

func TestListTasks_AppliesFilters(t *testing.T) {
	srv, mux := fakeServer(t)
	mux.HandleFunc("/api/tasks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"tasks":[
			{"id":"a","status":"completed","created_at":"2026-05-21T10:00:00Z"},
			{"id":"b","status":"failed","created_at":"2026-05-21T11:00:00Z"},
			{"id":"c","status":"completed","created_at":"2026-05-20T10:00:00Z"}
		]}`)
	})

	c := New(srv.URL, "vc_machine_test_abc")
	got, err := c.ListTasksWith(ListTasksOpts{Status: "completed", Limit: 1})
	if err != nil {
		t.Fatalf("ListTasksWith: %v", err)
	}
	if len(got.Tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(got.Tasks))
	}
	if got.Tasks[0].ID != "a" {
		t.Errorf("first task ID = %q, want a", got.Tasks[0].ID)
	}
}
