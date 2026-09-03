//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// openLikeRunningExe opens path with the sharing mode Windows uses for a
// loaded executable image: others may read it, and may rename or delete it,
// but not write to it. That combination is what makes a running backend
// impossible to overwrite and still possible to move aside.
//
// This models the sharing mode rather than launching a real process, so it
// pins the behaviour replaceBinary relies on without needing a runnable
// binary in the fixture.
func openLikeRunningExe(t *testing.T, path string) {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("opening %v: %v", path, err)
	}
	t.Cleanup(func() { windows.CloseHandle(h) })
}

// TestReplaceBinaryWhileRunning is the regression test for the upgrade path:
// re-running --install while the browser still has the backend started.
func TestReplaceBinaryWhileRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), targetBinName())
	if err := replaceBinary(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	openLikeRunningExe(t, path)

	// First establish that the straightforward write this replaced really
	// does fail here. Without this the test could pass for the wrong reason,
	// on a Windows that had stopped caring.
	if err := os.WriteFile(path, []byte("second"), 0755); err == nil {
		t.Fatal("writing over a held binary succeeded; this no longer reproduces the blocker")
	}

	if err := replaceBinary(path, []byte("second")); err != nil {
		t.Fatalf("replacing a held binary: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Errorf("binary = %q, want %q", got, "second")
	}
}
