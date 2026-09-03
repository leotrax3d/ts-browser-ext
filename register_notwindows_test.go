//go:build !windows

package main

import "testing"

// useScratchRegistry does nothing off Windows, where there is no registry to
// redirect: browsers find the manifest by scanning the target directory, which
// the tests redirect through the environment instead.
func useScratchRegistry(t *testing.T) { t.Helper() }

// registeredPath reports that registry checks don't apply on this platform.
func registeredPath(t *testing.T, browserByte string) (path string, applies bool) {
	t.Helper()
	return "", false
}
