package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// chromeExtensionID is the ID Chrome derives from the "key" field in
// manifest.json. It is fixed precisely so the native messaging host can be
// registered for it ahead of time.
const chromeExtensionID = "oejgagifdijhoenbjndjmdbgdddifeno"

// TestChromeLaunchesBackend is the only test that exercises the link nobody
// has ever run by hand: the browser reading our registration, launching the
// backend, and the backend coming up far enough to serve.
//
// It needs a real Chrome, so it is skipped unless TS_BROWSER_EXT_CHROME points
// at one. Unlike the other install tests it registers under the real
// per-user location, since that is where Chrome looks; it uninstalls again
// afterwards.
func TestChromeLaunchesBackend(t *testing.T) {
	chrome := os.Getenv("TS_BROWSER_EXT_CHROME")
	if chrome == "" {
		t.Skip("set TS_BROWSER_EXT_CHROME to a Chromium binary to run the handshake test")
	}
	if _, err := os.Stat(chrome); err != nil {
		t.Fatalf("TS_BROWSER_EXT_CHROME=%q: %v", chrome, err)
	}

	repoDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	// The backend has to be the real program, not this test binary. Build it
	// before redirecting HOME below: with HOME moved, the build populates a
	// fresh module cache inside the scratch directory, which is slow and
	// which the test framework then cannot delete, since module cache files
	// are read-only.
	backend := filepath.Join(t.TempDir(), targetBinName())
	build := exec.Command("go", "build", "-o", backend, ".")
	build.Dir = repoDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the backend: %v\n%s", err, out)
	}

	// Point the install and the log directory at scratch space. Chrome
	// inherits this environment, and so does the backend Chrome launches,
	// so all three agree on where things go.
	scratch := t.TempDir()
	t.Setenv("HOME", scratch)
	t.Setenv("USERPROFILE", scratch)
	t.Setenv("LOCALAPPDATA", scratch)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(scratch, "cache"))

	if err := installFrom(backend, "C"+chromeExtensionID); err != nil {
		t.Fatalf("installing the native messaging host: %v", err)
	}
	t.Cleanup(func() {
		if err := uninstall(); err != nil {
			t.Errorf("uninstalling: %v", err)
		}
	})

	logDir, err := logDirFor()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logDir, "backend.log")

	// Where the browser looks for the host manifest differs by platform, and
	// it decides what --user-data-dir has to be:
	//
	// On macOS and Linux the per-user native messaging directory is resolved
	// relative to the user data directory, so it has to be the one install
	// wrote into. (The documented ~/.config/google-chrome/NativeMessagingHosts
	// is just that rule applied to the default profile root.)
	//
	// On Windows the lookup goes through HKCU and is independent of the
	// profile, so any scratch directory will do.
	userDataDir := filepath.Join(scratch, "chrome-profile")
	if runtime.GOOS != "windows" {
		targetDir, err := getTargetDir("C")
		if err != nil {
			t.Fatal(err)
		}
		userDataDir = filepath.Dir(targetDir)
	}

	cmd := exec.Command(chrome,
		"--headless=new",
		"--no-sandbox",
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		"--user-data-dir="+userDataDir,
		"--load-extension="+repoDir,
		"--disable-extensions-except="+repoDir,
		// Branded Google Chrome rejects both of the switches above outright
		// ("--disable-extensions-except is not allowed in Google Chrome,
		// ignoring") and there is no flag that re-enables them, so this test
		// has to be pointed at a Chromium build. Real users load the
		// extension from the Extensions page, which is unaffected.
		// Puts the extension's console output, and the browser's own
		// complaints about native messaging, where this test can quote them.
		"--enable-logging=stderr",
		"about:blank",
	)
	// Without this a failure says only that nothing happened, which is the
	// least useful thing it could say.
	var browserOut lockedBuffer
	cmd.Stdout = &browserOut
	cmd.Stderr = &browserOut
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting Chrome: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	// Two things have to show up in the log, and the second is the point:
	// the first only says the backend started, the second says the extension
	// found it and talked to it, which is the whole handshake.
	want := []string{
		"Proxy listening on localhost:",
		`got command "init"`,
	}

	deadline := time.Now().Add(90 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(logPath); err == nil {
			last = string(b)
			if containsAll(last, want) {
				t.Logf("handshake completed; backend log begins:\n%s", firstLines(last, 4))
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}

	for _, w := range want {
		if !strings.Contains(last, w) {
			t.Errorf("backend log never contained %q", w)
		}
	}
	t.Fatalf("handshake did not complete within the deadline.\n"+
		"backend log %v:\n%s\n\nbrowser said:\n%s",
		logPath, firstLines(last, 20), interestingLines(browserOut.String(), 25))
}

// lockedBuffer collects the browser's output, which arrives on a goroutine
// the test also reads from when reporting a failure.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// interestingLines picks the browser output worth reading: what the extension
// logged and what the browser said about extensions or native messaging. The
// rest is startup noise about graphics and D-Bus.
func interestingLines(s string, max int) string {
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		switch {
		case strings.Contains(line, "CONSOLE"),
			strings.Contains(line, "native messaging"),
			strings.Contains(line, "Native"),
			strings.Contains(line, "extension"),
			strings.Contains(line, "Extension"):
			keep = append(keep, line)
		}
		if len(keep) >= max {
			keep = append(keep, "...")
			break
		}
	}
	if len(keep) == 0 {
		return "(nothing about extensions in the browser output; " +
			"it may not have started at all)"
	}
	return strings.Join(keep, "\n")
}

func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// firstLines keeps test output readable: the backend log runs to hundreds of
// lines once tsnet starts up.
func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = append(lines[:n], "...")
	}
	return strings.Join(lines, "\n")
}
