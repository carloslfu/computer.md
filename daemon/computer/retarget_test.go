// SPDX-License-Identifier: Apache-2.0

package computer

import (
	"slices"
	"testing"
)

// Phase 5 retarget: the default Controller/ScreenshotService target the
// shared Xvfb :1 with the same XAUTHORITY as before (zero regression to
// the live agent vision path); the *ForDisplay constructors target a
// sandbox's private Xvfb :N with no auth file.

func TestControllerXEnv_DefaultIsSharedDisplayUnchanged(t *testing.T) {
	got := NewController().xenv()
	want := []string{"DISPLAY=:1", "XAUTHORITY=/home/vibecraft/.Xauthority"}
	if !slices.Equal(got, want) {
		t.Fatalf("default controller xenv = %v, want %v (Pattern A must be unchanged)", got, want)
	}
}

func TestControllerXEnv_ForDisplayTargetsPrivateXvfbNoAuth(t *testing.T) {
	got := NewControllerForDisplay(37).xenv()
	want := []string{"DISPLAY=:37"}
	if !slices.Equal(got, want) {
		t.Fatalf("xenv(:37) = %v, want %v (private Xvfb, no XAUTHORITY)", got, want)
	}
}

func TestScreenshotXEnv_DefaultVsForDisplay(t *testing.T) {
	if got, want := NewScreenshotService("/tmp").xenv(),
		[]string{"DISPLAY=:1", "XAUTHORITY=/home/vibecraft/.Xauthority"}; !slices.Equal(got, want) {
		t.Fatalf("default screenshot xenv = %v, want %v", got, want)
	}
	if got, want := NewScreenshotServiceForDisplay("/tmp", 12).xenv(),
		[]string{"DISPLAY=:12"}; !slices.Equal(got, want) {
		t.Fatalf("screenshot xenv(:12) = %v, want %v", got, want)
	}
}
