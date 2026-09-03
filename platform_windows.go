//go:build windows

package main

// isWindows reports whether this build targets Windows, for the handful of
// tests whose expectations differ there.
func isWindows() bool { return true }
