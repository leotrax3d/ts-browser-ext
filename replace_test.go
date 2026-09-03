package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReplaceBinary covers the ordinary case, where nothing holds the old
// file: the content is replaced and no scratch files are left behind.
func TestReplaceBinary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, targetBinName())

	if err := replaceBinary(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := replaceBinary(path, []byte("second")); err != nil {
		t.Fatalf("replacing an existing binary: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Errorf("binary = %q, want %q", got, "second")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("left %d files behind (%v), want just the binary", len(entries), names)
	}
}

// TestReplaceBinaryKeepsMode checks the result is executable: the browser
// launches this path directly.
func TestReplaceBinaryKeepsMode(t *testing.T) {
	if isWindows() {
		t.Skip("no executable bit on Windows")
	}
	path := filepath.Join(t.TempDir(), targetBinName())
	if err := replaceBinary(path, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := replaceBinary(path, []byte("y")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0100 == 0 {
		t.Errorf("mode %v, want the owner execute bit set", fi.Mode().Perm())
	}
}

// TestReplaceBinaryClearsStaleOld checks a leftover .old from an earlier
// upgrade doesn't accumulate.
func TestReplaceBinaryClearsStaleOld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, targetBinName())

	if err := replaceBinary(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".old", []byte("stale"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := replaceBinary(path, []byte("second")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".old"); !os.IsNotExist(err) {
		t.Errorf("stale .old survived the upgrade (stat error: %v)", err)
	}
}
