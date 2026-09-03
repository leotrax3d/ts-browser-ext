package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// redirectInstallDirs points getTargetDir at a throwaway directory. It sets the
// variables every supported platform reads: HOME on Linux and macOS,
// LOCALAPPDATA on Windows, and USERPROFILE for the Windows fallback path.
func redirectInstallDirs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
}

// fakeBackend writes a stand-in for the backend binary, so the test doesn't
// copy the (large) test binary around just to check the plumbing.
func fakeBackend(t *testing.T) (path string, content []byte) {
	t.Helper()
	content = []byte("not really a backend\n")
	path = filepath.Join(t.TempDir(), "ts-browser-ext-source")
	if err := os.WriteFile(path, content, 0755); err != nil {
		t.Fatal(err)
	}
	return path, content
}

const testChromeID = "abcdefghijklmnopqrstuvwxyzabcdef"

// TestInstallUninstall runs a full registration round trip against redirected
// directories and, on Windows, a scratch registry subtree. This is the only
// test that exercises the platform-specific install paths end to end.
func TestInstallUninstall(t *testing.T) {
	tests := []struct {
		name        string
		installArg  string
		browserByte string
		wantHost    string
	}{
		{name: "chrome", installArg: "C" + testChromeID, browserByte: "C", wantHost: chromeHostName},
		{name: "firefox", installArg: "F", browserByte: "F", wantHost: firefoxHostName},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			redirectInstallDirs(t)
			useScratchRegistry(t)
			exe, content := fakeBackend(t)

			if err := installFrom(exe, tt.installArg); err != nil {
				t.Fatalf("installFrom: %v", err)
			}

			targetDir, err := getTargetDir(tt.browserByte)
			if err != nil {
				t.Fatal(err)
			}
			targetBin := filepath.Join(targetDir, targetBinName())
			targetJSON, err := getTargetJSON(tt.browserByte, targetDir)
			if err != nil {
				t.Fatal(err)
			}

			// The binary must be copied, not linked to, since the source is
			// often a temporary build directory that won't outlive the install.
			gotBin, err := os.ReadFile(targetBin)
			if err != nil {
				t.Fatalf("reading installed binary: %v", err)
			}
			if string(gotBin) != string(content) {
				t.Errorf("installed binary differs from the source")
			}
			if runtime.GOOS != "windows" {
				fi, err := os.Stat(targetBin)
				if err != nil {
					t.Fatal(err)
				}
				if fi.Mode().Perm()&0100 == 0 {
					t.Errorf("installed binary is not executable: mode %v", fi.Mode().Perm())
				}
			}

			gotJSON, err := os.ReadFile(targetJSON)
			if err != nil {
				t.Fatalf("reading manifest: %v", err)
			}
			var m hostManifest
			if err := json.Unmarshal(gotJSON, &m); err != nil {
				t.Fatalf("manifest is not valid JSON: %v (%s)", err, gotJSON)
			}
			if m.Name != tt.wantHost {
				t.Errorf("manifest name = %q, want %q", m.Name, tt.wantHost)
			}
			if m.Type != "stdio" {
				t.Errorf("manifest type = %q, want %q", m.Type, "stdio")
			}
			// The browser launches exactly this path, so it has to be the
			// installed copy rather than wherever the install was run from.
			if m.Path != targetBin {
				t.Errorf("manifest path = %q, want %q", m.Path, targetBin)
			}

			switch tt.browserByte {
			case "C":
				want := "chrome-extension://" + testChromeID + "/"
				if len(m.AllowedOrigins) != 1 || m.AllowedOrigins[0] != want {
					t.Errorf("allowed_origins = %q, want [%q]", m.AllowedOrigins, want)
				}
				if len(m.AllowedExtensions) != 0 {
					t.Errorf("allowed_extensions should be absent for Chrome, got %q", m.AllowedExtensions)
				}
			case "F":
				if len(m.AllowedExtensions) != 1 || m.AllowedExtensions[0] != firefoxExtensionID {
					t.Errorf("allowed_extensions = %q, want [%q]", m.AllowedExtensions, firefoxExtensionID)
				}
				if len(m.AllowedOrigins) != 0 {
					t.Errorf("allowed_origins should be absent for Firefox, got %q", m.AllowedOrigins)
				}
			}

			if got, applies := registeredPath(t, tt.browserByte); applies && got != targetJSON {
				t.Errorf("registry points at %q, want %q", got, targetJSON)
			}

			// Installing over an existing installation is the upgrade path and
			// must not fail.
			if err := installFrom(exe, tt.installArg); err != nil {
				t.Fatalf("installing twice: %v", err)
			}

			if err := uninstall(); err != nil {
				t.Fatalf("uninstall: %v", err)
			}
			if _, err := os.Stat(targetBin); !os.IsNotExist(err) {
				t.Errorf("binary still present after uninstall (stat error: %v)", err)
			}
			if _, err := os.Stat(targetJSON); !os.IsNotExist(err) {
				t.Errorf("manifest still present after uninstall (stat error: %v)", err)
			}
			if got, applies := registeredPath(t, tt.browserByte); applies && got != "" {
				t.Errorf("registry entry still present after uninstall: %q", got)
			}

			// --uninstall covers both browsers whether or not each was
			// installed, so a second run must stay quiet.
			if err := uninstall(); err != nil {
				t.Errorf("uninstalling twice: %v", err)
			}
		})
	}
}

// TestInstallRejectsBadArg checks nothing is written when the argument is
// rejected, so a typo can't leave a half-registered host behind.
func TestInstallRejectsBadArg(t *testing.T) {
	dir := redirectInstallDirs(t)
	useScratchRegistry(t)
	exe, _ := fakeBackend(t)

	if err := installFrom(exe, "C"+"tooshort"); err == nil {
		t.Fatal("installFrom accepted an invalid Chrome extension ID")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a rejected install left %d entries behind: %v", len(entries), entries)
	}
}
