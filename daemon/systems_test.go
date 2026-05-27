// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestStripSystemFromCrontab covers the patterns the uninstall API
// will see in the wild: single entry, entry with a preceding comment,
// multiple entries for the same system, unrelated entries that must
// survive, and the "name appears as a substring of something else"
// false-positive risk (system "log" must NOT eat /var/log/... jobs).
func TestStripSystemFromCrontab(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		system    string
		wantOut   string
		wantCount int
	}{
		{
			name:      "no match returns input unchanged",
			in:        "*/5 * * * * /usr/bin/uptime\n",
			system:    "foo",
			wantOut:   "*/5 * * * * /usr/bin/uptime\n",
			wantCount: 0,
		},
		{
			name: "single matching entry with preceding comment",
			in: "# Check example.com every 10 min\n" +
				"*/10 * * * * /home/vibecraft/systems/status-watcher/run.sh\n",
			system:    "status-watcher",
			wantOut:   "",
			wantCount: 1,
		},
		{
			name: "matching entry surrounded by unrelated entries",
			in: "*/5 * * * * /usr/bin/uptime\n" +
				"*/10 * * * * /home/vibecraft/systems/foo/run.sh\n" +
				"0 * * * * /usr/bin/df -h\n",
			system: "foo",
			wantOut: "*/5 * * * * /usr/bin/uptime\n" +
				"0 * * * * /usr/bin/df -h\n",
			wantCount: 1,
		},
		{
			name: "multiple lines for same system removed together",
			in: "*/5 * * * * /home/vibecraft/systems/foo/poll.sh\n" +
				"0 9 * * * /home/vibecraft/systems/foo/summary.sh\n" +
				"# comment for bar\n" +
				"*/30 * * * * /home/vibecraft/systems/bar/run.sh\n",
			system: "foo",
			wantOut: "# comment for bar\n" +
				"*/30 * * * * /home/vibecraft/systems/bar/run.sh\n",
			wantCount: 2,
		},
		{
			name: "name as substring does not match other paths",
			in: "0 * * * * /var/log/cleanup.sh\n" +
				"*/10 * * * * /home/vibecraft/systems/log/run.sh\n" +
				"5 4 * * * /home/vibecraft/systems/blog/run.sh\n",
			system: "log",
			wantOut: "0 * * * * /var/log/cleanup.sh\n" +
				"5 4 * * * /home/vibecraft/systems/blog/run.sh\n",
			wantCount: 1,
		},
		{
			name: "preceding contiguous comment block is removed",
			in: "# weekly report\n" +
				"# generated every monday at 9am\n" +
				"0 9 * * 1 /home/vibecraft/systems/weekly-report/run.sh\n" +
				"# unrelated comment\n" +
				"\n" +
				"*/5 * * * * /usr/bin/uptime\n",
			system: "weekly-report",
			wantOut: "# unrelated comment\n" +
				"\n" +
				"*/5 * * * * /usr/bin/uptime\n",
			wantCount: 1,
		},
		{
			name: "blank line stops comment drag",
			in: "# top-of-file header\n" +
				"\n" +
				"# inline note\n" +
				"*/10 * * * * /home/vibecraft/systems/foo/run.sh\n",
			system: "foo",
			wantOut: "# top-of-file header\n" +
				"\n",
			wantCount: 1,
		},
		{
			name: "commented-out cron line for system is NOT removed",
			in: "# */10 * * * * /home/vibecraft/systems/foo/run.sh # disabled\n" +
				"0 * * * * /usr/bin/df -h\n",
			system: "foo",
			wantOut: "# */10 * * * * /home/vibecraft/systems/foo/run.sh # disabled\n" +
				"0 * * * * /usr/bin/df -h\n",
			wantCount: 0,
		},
		{
			name:      "CRLF line endings tolerated",
			in:        "*/10 * * * * /home/vibecraft/systems/foo/run.sh\r\n0 * * * * /usr/bin/uptime\r\n",
			system:    "foo",
			wantOut:   "0 * * * * /usr/bin/uptime\r\n",
			wantCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, count := stripSystemFromCrontab([]byte(tt.in), tt.system)
			if count != tt.wantCount {
				t.Errorf("count = %d, want %d", count, tt.wantCount)
			}
			if string(got) != tt.wantOut {
				t.Errorf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s",
					string(got), tt.wantOut)
			}
		})
	}
}

func TestScheduledSystemNames(t *testing.T) {
	root := t.TempDir()
	mustMkdir := func(name string) string {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		return dir
	}
	mustWrite := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	mustWrite(filepath.Join(mustMkdir("daily-report"), "crontab"),
		"# run every day\n0 9 * * * /home/vibecraft/systems/daily-report/run.sh\n")
	mustWrite(filepath.Join(mustMkdir("empty-schedule"), "crontab"), "\n# disabled\n")
	mustMkdir("no-crontab")
	mustWrite(filepath.Join(mustMkdir("bad.name"), "crontab"),
		"* * * * * /home/vibecraft/systems/bad.name/run.sh\n")
	mustWrite(filepath.Join(mustMkdir("status-watcher"), "crontab"),
		"*/10 * * * * /home/vibecraft/systems/status-watcher/check.sh\n")

	got, err := scheduledSystemNames(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"daily-report", "status-watcher"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scheduledSystemNames() = %#v, want %#v", got, want)
	}
}

func TestScheduledSystemNamesMissingRoot(t *testing.T) {
	got, err := scheduledSystemNames(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("scheduledSystemNames(missing) = %#v, want empty", got)
	}
}

// TestSystemNameValidation is a quick sanity check on the regex used
// to bound the path-segment manager provides — names that escape
// systemsRoot via traversal or shell metacharacters must be rejected.
func TestSystemNameValidation(t *testing.T) {
	valid := []string{
		"foo", "foo-bar", "foo_bar", "foo-bar-baz", "f1", "a", "abc123",
		"status-watcher", "weekly-report",
		strings.Repeat("a", 64), // boundary: exactly 64 chars
	}
	invalid := []string{
		"", ".", "..", "../etc", "/etc/shadow", "foo/bar",
		"Foo", "FOO", // uppercase
		"-foo", "_foo", // leading non-alnum
		"foo.bar",               // dot
		"foo;rm",                // shell metachar
		"foo\x00",               // NUL
		strings.Repeat("a", 65), // boundary: too long
	}
	for _, v := range valid {
		if !systemNamePattern.MatchString(v) {
			t.Errorf("expected %q to be valid", v)
		}
	}
	for _, v := range invalid {
		if systemNamePattern.MatchString(v) {
			t.Errorf("expected %q to be invalid", v)
		}
	}
}
