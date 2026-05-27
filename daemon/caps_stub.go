// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

// Capability dropping is Linux-only. On the macOS dev host this is a
// no-op so the daemon builds and runs unchanged.

func capRetained() []string { return nil }

func dropResidualCaps() error { return nil }
