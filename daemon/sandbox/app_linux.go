// SPDX-License-Identifier: Apache-2.0

//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"
)

// app_linux.go is the Phase 4 customer-app runtime: a persistent
// sandbox running the app's start command, plus a host→sandbox TCP
// forwarder so the UNCHANGED Caddy `reverse_proxy localhost:<port>`
// reaches the sandboxed app (see app.go for why the Caddyfile is left
// byte-identical — brick-risk avoidance).

type appInstance struct {
	sb     *Sandbox
	fwd    net.Listener
	cancel context.CancelFunc
}

var appReg = struct {
	mu sync.Mutex
	m  map[string]*appInstance
}{m: map[string]*appInstance{}}

// StartApp deploys (or restarts) a customer app: builds its sandbox
// from the manifest, launches the start command inside (restarted per
// policy), and opens the host forwarder 127.0.0.1:<port> →
// <sandbox-veth-ip>:<port>. Idempotent — a running app is stopped+
// replaced.
func StartApp(appsRoot, name string, resolveVault func([]string) map[string]string, handler http.Handler) error {
	m, err := LoadAppManifest(appsRoot, name)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("app %q has no manifest.json", name)
	}
	StopApp(name) // clean replace

	appDir := filepath.Join(appsRoot, name)
	var env map[string]string
	if resolveVault != nil {
		env = resolveVault(m.VaultRefs)
	}
	sb, err := Create(m.ToSandboxManifest(name, appDir, env), m.Mode(), handler)
	if err != nil {
		return fmt.Errorf("app %q sandbox: %w", name, err)
	}

	// Host forwarder: 127.0.0.1:port -> sandbox veth ip:port. Keeps the
	// generated Caddyfile (reverse_proxy localhost:port) untouched.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", m.Port))
	if err != nil {
		sb.Destroy()
		return fmt.Errorf("app %q forwarder bind :%d: %w", name, m.Port, err)
	}
	target := fmt.Sprintf("%s:%d", sb.Net.SandboxIP, m.Port)
	go acceptLoop(ln, target)

	ctx, cancel := context.WithCancel(context.Background())
	appReg.mu.Lock()
	appReg.m[name] = &appInstance{sb: sb, fwd: ln, cancel: cancel}
	appReg.mu.Unlock()

	// Run the app inside the sandbox; restart per policy.
	go func() {
		for {
			out, runErr := sb.Run(ctx, m.StartCmd, nil)
			if ctx.Err() != nil {
				return // stopped
			}
			log.Printf("app %q exited (err=%v); tail: %.200s", name, runErr, out)
			if !m.restartAlways() {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
	return nil
}

// StopApp tears down a running app: forwarder, run loop, sandbox.
func StopApp(name string) {
	appReg.mu.Lock()
	ai := appReg.m[name]
	delete(appReg.m, name)
	appReg.mu.Unlock()
	if ai == nil {
		return
	}
	ai.cancel()
	if ai.fwd != nil {
		ai.fwd.Close()
	}
	ai.sb.Destroy()
}

func acceptLoop(ln net.Listener, target string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return // listener closed
		}
		go proxyConn(c, target)
	}
}

func proxyConn(client net.Conn, target string) {
	defer client.Close()
	up, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, client); done <- struct{}{} }()
	go func() { io.Copy(client, up); done <- struct{}{} }()
	<-done
}
