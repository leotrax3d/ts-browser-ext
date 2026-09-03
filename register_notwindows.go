//go:build !windows

package main

// registerHost is a no-op off Windows: Chrome and Firefox find the manifest by
// scanning the directory [getTargetDir] writes it to.
func registerHost(browserByte, jsonPath string) error { return nil }

// unregisterHost is a no-op off Windows, for the same reason.
func unregisterHost(browserByte string) error { return nil }
