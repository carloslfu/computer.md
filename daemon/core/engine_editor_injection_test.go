// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carloslfu/computer.md/daemon/computer"
	managerclient "github.com/carloslfu/computer.md/daemon/manager"
)

// editorEngine builds a testEngine wired to a real (privilege-dropping) shell
// rooted at a temp dir, suitable for driving executeTextEditorTool.
func editorEngine(t *testing.T) *Engine {
	t.Helper()
	db := testDB(t)
	e := testEngine(t, db, &mockManager{fn: func(context.Context, string, []managerclient.Message) (*managerclient.Response, error) {
		return &managerclient.Response{}, nil
	}})
	e.shell = computer.NewShell(t.TempDir(), "")
	return e
}

// TestEditorTool_NoShellInjection is the regression test for the editor-tool
// RCE (EDITOR-RCE): path / old_str / new_str / file_text used to be spliced
// into a `python3 -c "..."` shell string via Go %q (Go-quoting, NOT
// shell-quoting), so $(...) / backticks executed. Every field now travels as
// JSON on stdin to a constant helper, so a command substitution anywhere is
// inert. Each sub-test plants a sentinel-file command and asserts it never ran.
func TestEditorTool_NoShellInjection(t *testing.T) {
	e := editorEngine(t)
	ctx := context.Background()
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "PWNED")
	payload := "$(touch " + sentinel + ")`touch " + sentinel + "`"

	assertNotPwned := func(t *testing.T) {
		t.Helper()
		if _, err := os.Stat(sentinel); err == nil {
			t.Fatalf("INJECTION EXECUTED: sentinel %s was created", sentinel)
		}
	}

	t.Run("malicious path on create", func(t *testing.T) {
		target := filepath.Join(dir, "create_"+payload+".txt")
		_, err := e.executeTextEditorTool(ctx, managerclient.ToolCall{
			Name: "str_replace_based_edit_tool",
			Input: map[string]string{
				"command":   "create",
				"path":      target,
				"file_text": "hello",
			},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		assertNotPwned(t)
		// The file was created with the LITERAL (untemplated) name.
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("expected literal-named file to exist: %v", err)
		}
	})

	t.Run("malicious file_text is written verbatim", func(t *testing.T) {
		target := filepath.Join(dir, "content.txt")
		body := "line1\n" + payload + "\n\"quotes\" 'and' $HOME\n"
		_, err := e.executeTextEditorTool(ctx, managerclient.ToolCall{
			Name: "str_replace_based_edit_tool",
			Input: map[string]string{
				"command":   "create",
				"path":      target,
				"file_text": body,
			},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		assertNotPwned(t)
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if string(got) != body {
			t.Fatalf("file content mangled.\nwant: %q\ngot:  %q", body, string(got))
		}
	})

	t.Run("malicious old_str on str_replace matches literally", func(t *testing.T) {
		target := filepath.Join(dir, "replace.txt")
		original := "before " + payload + " after"
		if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		_, err := e.executeTextEditorTool(ctx, managerclient.ToolCall{
			Name: "str_replace_based_edit_tool",
			Input: map[string]string{
				"command": "str_replace",
				"path":    target,
				"old_str": payload,
				"new_str": "SAFE",
			},
		})
		if err != nil {
			t.Fatalf("str_replace: %v", err)
		}
		assertNotPwned(t)
		got, _ := os.ReadFile(target)
		if string(got) != "before SAFE after" {
			t.Fatalf("unexpected replace result: %q", string(got))
		}
	})
}

// TestEditorTool_BasicOps confirms the reimplemented helper still does the
// ordinary view/create/insert work (behavioral parity with the old shell impl).
func TestEditorTool_BasicOps(t *testing.T) {
	e := editorEngine(t)
	ctx := context.Background()
	dir := t.TempDir()
	target := filepath.Join(dir, "f.txt")

	if _, err := e.executeTextEditorTool(ctx, managerclient.ToolCall{
		Input: map[string]string{"command": "create", "path": target, "file_text": "a\nb\nc\n"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	view, err := e.executeTextEditorTool(ctx, managerclient.ToolCall{
		Input: map[string]string{"command": "view", "path": target},
	})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	// cat -n style: each line numbered.
	if !strings.Contains(view.Content, "1\ta") || !strings.Contains(view.Content, "3\tc") {
		t.Fatalf("view output not line-numbered as expected:\n%s", view.Content)
	}

	if _, err := e.executeTextEditorTool(ctx, managerclient.ToolCall{
		Input: map[string]string{"command": "insert", "path": target, "insert_line": "1", "new_str": "INS"},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "a\nINS\nb\nc\n" {
		t.Fatalf("insert produced %q", string(got))
	}
}
