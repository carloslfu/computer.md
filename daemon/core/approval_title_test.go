// SPDX-License-Identifier: Apache-2.0

package core

import "testing"

// TestHumanTitle_DangerousCommands locks in the plain-English titles shown on
// the approval card for the dangerous_command_confirm rule. The halt/poweroff
// distinction matters: those commands power the machine OFF, so labeling them
// "Reboot the machine" (the old behavior) would mislead the operator into
// approving a shutdown while believing the box would come back up.
func TestHumanTitle_DangerousCommands(t *testing.T) {
	cases := []struct {
		command string
		want    string
	}{
		{"shutdown -h now", "Shut down the machine"},
		{"halt", "Shut down the machine"},
		{"poweroff", "Shut down the machine"},
		{"sudo poweroff", "Shut down the machine"},
		{"reboot", "Reboot the machine"},
		{"reboot now", "Reboot the machine"},
		{"ufw disable", "Disable the firewall"},
		{"chmod 777 /etc/passwd", "Change permissions on a system path"},
		{"chown root /etc/shadow", "Change ownership on a system path"},
		{"systemctl stop sshd", "Change a critical system service"},
		{"dd if=/dev/zero of=/dev/sda", "Run a potentially destructive command"},
	}
	for _, tc := range cases {
		got := humanTitle("bash", tc.command, "dangerous_command_confirm")
		if got != tc.want {
			t.Errorf("humanTitle(%q) = %q, want %q", tc.command, got, tc.want)
		}
	}
}
