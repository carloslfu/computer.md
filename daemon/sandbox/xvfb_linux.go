// SPDX-License-Identifier: Apache-2.0

//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// xvfb_linux.go is the Phase 5 (Pattern B) core: a sandbox that needs a
// display gets its OWN Xvfb :N spawned by the daemon as a sibling
// process, and binds ONLY its own /tmp/.X11-unix/X<N> socket. This
// closes the last Pattern-A residual — a compromised worker on the
// shared :1 could `import` the agent's Chrome. With an owned display
// the sandbox cannot even see :1's socket (mount-namespace: only X<N>
// is bound in), so cross-sandbox X snooping is structurally impossible.
//
// The live daemon xdotool/screenshot retarget + the dashboard
// compositor (D6) ride on top of this and are staged separately (they
// touch the agent's core vision path — same brick-risk discipline as
// the Caddyfile generator). This file is the isolation mechanism.

type xvfbProc struct {
	display int
	socket  string
	cmd     *exec.Cmd
}

var dispAlloc = struct {
	mu   sync.Mutex
	used map[int]bool
}{used: map[int]bool{}}

// allocDisplay picks a free display number in [10,99] (well clear of
// the shared :1 the agent-shell uses under Pattern A).
func allocDisplay() (int, error) {
	dispAlloc.mu.Lock()
	defer dispAlloc.mu.Unlock()
	for n := 10; n <= 99; n++ {
		if dispAlloc.used[n] {
			continue
		}
		if _, err := os.Stat(fmt.Sprintf("/tmp/.X11-unix/X%d", n)); err == nil {
			continue // in use by something else
		}
		dispAlloc.used[n] = true
		return n, nil
	}
	return 0, fmt.Errorf("no free X display in 10..99")
}

func freeDisplay(n int) {
	dispAlloc.mu.Lock()
	delete(dispAlloc.used, n)
	dispAlloc.mu.Unlock()
}

// startXvfb spawns a private Xvfb :N and waits for its socket to
// appear. Caller must call (*xvfbProc).stop().
func startXvfb() (*xvfbProc, error) {
	n, err := allocDisplay()
	if err != nil {
		return nil, err
	}
	sock := fmt.Sprintf("/tmp/.X11-unix/X%d", n)
	cmd := exec.Command("Xvfb", fmt.Sprintf(":%d", n),
		"-screen", "0", "1280x800x24", "-nolisten", "tcp")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		freeDisplay(n)
		return nil, fmt.Errorf("Xvfb :%d: %w", n, err)
	}
	// Wait up to 5s for the socket.
	for i := 0; i < 50; i++ {
		if _, statErr := os.Stat(sock); statErr == nil {
			return &xvfbProc{display: n, socket: sock, cmd: cmd}, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	cmd.Process.Kill()
	freeDisplay(n)
	return nil, fmt.Errorf("Xvfb :%d socket never appeared", n)
}

func (x *xvfbProc) stop() {
	if x == nil {
		return
	}
	if x.cmd != nil && x.cmd.Process != nil {
		syscall.Kill(-x.cmd.Process.Pid, syscall.SIGKILL)
		x.cmd.Wait()
	}
	os.Remove(x.socket)
	freeDisplay(x.display)
}
