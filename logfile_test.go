package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingFileRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backend.log")

	// Room for one 10-byte line at a time, keeping two rolled-over files.
	r, err := newRotatingFile(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	for _, line := range []string{"aaaaaaaaa\n", "bbbbbbbbb\n", "ccccccccc\n"} {
		if _, err := r.Write([]byte(line)); err != nil {
			t.Fatalf("writing %q: %v", line, err)
		}
	}

	// The newest line is live, the two before it rolled over, oldest first out.
	for _, want := range []struct {
		name    string
		content string
	}{
		{name: "backend.log", content: "ccccccccc\n"},
		{name: "backend.log.1", content: "bbbbbbbbb\n"},
		{name: "backend.log.2", content: "aaaaaaaaa\n"},
	} {
		got, err := os.ReadFile(filepath.Join(dir, want.name))
		if err != nil {
			t.Errorf("reading %v: %v", want.name, err)
			continue
		}
		if string(got) != want.content {
			t.Errorf("%v = %q, want %q", want.name, got, want.content)
		}
	}
}

// TestRotatingFileDiscardsOldest checks the retention limit holds, so the logs
// can't grow without bound on a backend a browser keeps running for days.
func TestRotatingFileDiscardsOldest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backend.log")

	r, err := newRotatingFile(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	for i := range 10 {
		if _, err := r.Write([]byte(strings.Repeat(string(rune('a'+i)), 9) + "\n")); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The live file plus keep=2 rolled-over ones, and nothing else.
	if len(entries) != 3 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("kept %d files (%v), want 3", len(entries), names)
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Errorf("backend.log.3 should have been discarded (stat error: %v)", err)
	}

	// The live file holds the most recent line.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "jjjjjjjjj\n"; string(got) != want {
		t.Errorf("live file = %q, want %q", got, want)
	}
}

// TestRotatingFileAppends checks a restart adds to the existing log rather
// than truncating it: the interesting lines are often from just before the
// backend died.
func TestRotatingFileAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backend.log")

	r, err := newRotatingFile(path, 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("first run\n")); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r2, err := newRotatingFile(path, 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if _, err := r2.Write([]byte("second run\n")); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "first run\nsecond run\n"; string(got) != want {
		t.Errorf("log = %q, want %q", got, want)
	}
}

func TestRotatingFileRejectsBadSize(t *testing.T) {
	if _, err := newRotatingFile(filepath.Join(t.TempDir(), "x.log"), 0, 1); err == nil {
		t.Error("newRotatingFile accepted a zero maxSize")
	}
}

// TestLogDirFor checks the directory is created and usable, since the backend
// has nowhere else to report a failure to open it.
func TestLogDirFor(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))

	got, err := logDirFor()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(got)
	if err != nil {
		t.Fatalf("log directory was not created: %v", err)
	}
	if !fi.IsDir() {
		t.Errorf("%v is not a directory", got)
	}
	if filepath.Base(got) != "logs" {
		t.Errorf("log directory %q should end in %q", got, "logs")
	}
}
