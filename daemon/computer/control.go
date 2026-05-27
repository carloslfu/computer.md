// SPDX-License-Identifier: Apache-2.0

package computer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// xdotoolTimeout is the maximum time any xdotool command can run.
// xdotool commands should complete in milliseconds; anything longer
// means X11 is frozen or unresponsive.
const xdotoolTimeout = 10 * time.Second

// Controller provides mouse and keyboard control via xdotool.
//
// display/xauth select the X server xdotool drives. Default is the
// shared Xvfb :1 (Pattern A) — the agent-shell and the dashboard live
// here, UNCHANGED. NewControllerForDisplay binds to a sandbox's private
// Xvfb :N (Pattern B) so the daemon can drive a fully-isolated workload
// on its own display without that workload being able to see — or be
// seen on — :1 (Phase 5). A Pattern-B Xvfb is spawned with no auth file,
// so xauth is empty for :N.
type Controller struct {
	display string
	xauth   string
}

// NewController creates a Controller for the shared display (:1).
// Behavior is byte-identical to before Phase 5's retarget.
func NewController() *Controller {
	return &Controller{display: ":1", xauth: "/home/vibecraft/.Xauthority"}
}

// NewControllerForDisplay binds a Controller to a sandbox's private
// Xvfb :n (Pattern B). Used only to drive an owned-display workload.
func NewControllerForDisplay(n int) *Controller {
	return &Controller{display: fmt.Sprintf(":%d", n)}
}

// xenv is the X environment for the exec'd xdotool: DISPLAY always,
// XAUTHORITY only when this Controller targets an auth-protected server
// (the shared :1). A private :N Xvfb has no auth file.
func (c *Controller) xenv() []string {
	e := []string{"DISPLAY=" + c.display}
	if c.xauth != "" {
		e = append(e, "XAUTHORITY="+c.xauth)
	}
	return e
}

// MouseMove moves the cursor to the given screen coordinates.
// Note: --sync is intentionally omitted to prevent indefinite hangs
// when Xvfb is overloaded. The follow-up screenshot after each action
// provides visual confirmation of the screen state.
func (c *Controller) MouseMove(x, y int) error {
	return c.runXdotool("mousemove", strconv.Itoa(x), strconv.Itoa(y))
}

// LeftClick performs a left mouse click at the current cursor position.
func (c *Controller) LeftClick() error {
	return c.runXdotool("click", "1")
}

// RightClick performs a right mouse click at the current cursor position.
func (c *Controller) RightClick() error {
	return c.runXdotool("click", "3")
}

// MiddleClick performs a middle mouse click at the current cursor position.
func (c *Controller) MiddleClick() error {
	return c.runXdotool("click", "2")
}

// DoubleClick performs a double left click at the current cursor position.
func (c *Controller) DoubleClick() error {
	return c.runXdotool("click", "--repeat", "2", "--delay", "100", "1")
}

// LeftClickDrag drags from the current position to (x, y) while holding the left button.
func (c *Controller) LeftClickDrag(x, y int) error {
	if err := c.runXdotool("mousedown", "1"); err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	if err := c.runXdotool("mousemove", strconv.Itoa(x), strconv.Itoa(y)); err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	return c.runXdotool("mouseup", "1")
}

// TypeText types text using xdotool. For long strings, it uses xdotool type
// with a small delay between keystrokes to avoid dropped characters.
func (c *Controller) TypeText(text string) error {
	if len(text) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), xdotoolTimeout)
	defer cancel()

	// Feed the text via stdin (`--file -`) instead of as a positional arg.
	// Passing it in argv would expose vault-resolved values typed into
	// forms to `ps`/`/proc/<pid>/cmdline` (Phase 0a follow-up — the argv
	// leak class the bash path already closed). xdotool types stdin
	// verbatim, so no trailing newline is added.
	cmd := exec.CommandContext(ctx, "xdotool", "type", "--clearmodifiers", "--delay", "12", "--file", "-")
	cmd.Env = append(os.Environ(), c.xenv()...)
	cmd.Stdin = strings.NewReader(text)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("xdotool type failed: %w (output: %s)", err, string(output))
	}
	return nil
}

// KeyPress sends a key combination (e.g., "Return", "ctrl+c", "alt+F4").
func (c *Controller) KeyPress(key string) error {
	key = normalizeKeyName(key)
	return c.runXdotool("key", "--clearmodifiers", key)
}

// Scroll scrolls the mouse wheel at position (x, y) in the given direction.
func (c *Controller) Scroll(x, y int, direction string, amount int) error {
	if x != 0 || y != 0 {
		if err := c.MouseMove(x, y); err != nil {
			return err
		}
	}

	var button string
	switch direction {
	case "up":
		button = "4"
	case "down":
		button = "5"
	case "left":
		button = "6"
	case "right":
		button = "7"
	default:
		return fmt.Errorf("unknown scroll direction: %s", direction)
	}

	return c.runXdotool("click", "--repeat", strconv.Itoa(amount), "--delay", "50", button)
}

// GetCursorPosition returns the current cursor x, y coordinates.
func (c *Controller) GetCursorPosition() (int, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), xdotoolTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "xdotool", "getmouselocation", "--shell")
	cmd.Env = append(os.Environ(), c.xenv()...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, 0, fmt.Errorf("getting cursor position: %w", err)
	}

	var x, y int
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "X=") {
			x, _ = strconv.Atoi(strings.TrimPrefix(line, "X="))
		} else if strings.HasPrefix(line, "Y=") {
			y, _ = strconv.Atoi(strings.TrimPrefix(line, "Y="))
		}
	}

	return x, y, nil
}

// GetScreenSize returns the screen width and height.
func (c *Controller) GetScreenSize() (int, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), xdotoolTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "xdotool", "getdisplaygeometry")
	cmd.Env = append(os.Environ(), c.xenv()...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, 0, fmt.Errorf("getting screen size: %w", err)
	}

	parts := strings.Fields(strings.TrimSpace(string(output)))
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("unexpected xdotool output: %s", string(output))
	}

	w, _ := strconv.Atoi(parts[0])
	h, _ := strconv.Atoi(parts[1])
	return w, h, nil
}

// GetActiveWindow returns the window ID of the currently focused window.
func (c *Controller) GetActiveWindow() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), xdotoolTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "xdotool", "getactivewindow")
	cmd.Env = append(os.Environ(), c.xenv()...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("getting active window: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// GetActiveWindowName returns the title of the currently focused window.
func (c *Controller) GetActiveWindowName() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), xdotoolTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "xdotool", "getactivewindow", "getwindowname")
	cmd.Env = append(os.Environ(), c.xenv()...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("getting active window name: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// runXdotool executes an xdotool command with a hard timeout.
// All xdotool commands must complete within xdotoolTimeout or be killed.
func (c *Controller) runXdotool(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), xdotoolTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "xdotool", args...)
	cmd.Env = append(os.Environ(), c.xenv()...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("xdotool %s timed out after %s (X11 may be unresponsive)", args[0], xdotoolTimeout)
		}
		return fmt.Errorf("xdotool %s failed: %w (output: %s)", args[0], err, string(output))
	}
	return nil
}

// normalizeKeyName converts common key name variants to xdotool format.
func normalizeKeyName(key string) string {
	replacements := map[string]string{
		"enter":     "Return",
		"return":    "Return",
		"tab":       "Tab",
		"escape":    "Escape",
		"esc":       "Escape",
		"backspace": "BackSpace",
		"delete":    "Delete",
		"space":     "space",
		"up":        "Up",
		"down":      "Down",
		"left":      "Left",
		"right":     "Right",
		"home":      "Home",
		"end":       "End",
		"pageup":    "Page_Up",
		"page_up":   "Page_Up",
		"pagedown":  "Page_Down",
		"page_down": "Page_Down",
	}

	lower := strings.ToLower(key)
	if mapped, ok := replacements[lower]; ok {
		return mapped
	}

	return key
}
