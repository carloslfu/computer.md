// SPDX-License-Identifier: Apache-2.0

package computer

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

// distinctive so a coincidental substring match can't pass the argv check.
const testSecretVal = "S3CR3T-vibecraft-do-not-leak-9f1a"

func TestExecuteWithEnv_ExpandsFromEnvNotArgv(t *testing.T) {
	sh := NewShell(t.TempDir(), "")
	ctx := context.Background()
	env := []string{"TEST_SECRET=" + testSecretVal}

	t.Run("bash expands $NAME from injected env", func(t *testing.T) {
		out, err := sh.ExecuteWithEnv(ctx, `printf '%s' "$TEST_SECRET"`, env)
		if err != nil {
			t.Fatalf("ExecuteWithEnv: %v (out=%q)", err, out)
		}
		if strings.TrimSpace(out) != testSecretVal {
			t.Fatalf("expanded value = %q, want %q", out, testSecretVal)
		}
	})

	t.Run("command text is passed verbatim (no pre-substitution)", func(t *testing.T) {
		// Killer regression test for the env-not-argv switch: the old
		// vault.ResolveReferences was a dumb string-replace that substituted
		// the plaintext even inside single quotes. The new env path passes
		// the command verbatim, so bash's own quoting rules apply: a
		// single-quoted $TEST_SECRET must stay literal, NOT be replaced
		// with the secret value.
		out, err := sh.ExecuteWithEnv(ctx, `printf '%s' 'see:$TEST_SECRET'`, env)
		if err != nil {
			t.Fatalf("ExecuteWithEnv: %v (out=%q)", err, out)
		}
		if strings.Contains(out, testSecretVal) {
			t.Fatalf("plaintext leaked: command text was pre-substituted, got %q", out)
		}
		if strings.TrimSpace(out) != `see:$TEST_SECRET` {
			t.Fatalf("expected literal see:$TEST_SECRET, got %q", out)
		}
	})

	t.Run("secret is not substituted into unrelated command text", func(t *testing.T) {
		out, err := sh.ExecuteWithEnv(ctx, `echo no-reference-here`, env)
		if err != nil {
			t.Fatalf("ExecuteWithEnv: %v", err)
		}
		if strings.Contains(out, testSecretVal) {
			t.Fatalf("secret leaked into output of a command that never referenced it: %q", out)
		}
	})

	// Exact Done-when probe from the plan; /proc is Linux-only.
	t.Run("linux /proc cmdline shows literal not expanded", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skipf("skipping /proc cmdline check on %s", runtime.GOOS)
		}
		out, err := sh.ExecuteWithEnv(ctx,
			`echo $TEST_SECRET; cat /proc/$$/cmdline | tr "\0" "\n"`, env)
		if err != nil {
			t.Fatalf("ExecuteWithEnv: %v (out=%q)", err, out)
		}
		if !strings.Contains(out, testSecretVal) {
			t.Fatalf("stdout should contain expanded secret %q, got %q", testSecretVal, out)
		}
		if !strings.Contains(out, "$TEST_SECRET") {
			t.Fatalf("cmdline should contain literal $TEST_SECRET, got %q", out)
		}
	})
}

// Execute (no extra env) must behave exactly as before the refactor.
func TestExecute_NoExtraEnvRegression(t *testing.T) {
	sh := NewShell(t.TempDir(), "")
	out, err := sh.Execute(context.Background(), `echo hello-world`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.TrimSpace(out) != "hello-world" {
		t.Fatalf("Execute output = %q, want hello-world", out)
	}
}
