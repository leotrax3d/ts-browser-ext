//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

// useScratchRegistry points the host registrations at a throwaway subtree for
// the duration of the test, so a run never disturbs a real browser's
// registration on the machine, and removes it again afterwards.
func useScratchRegistry(t *testing.T) {
	t.Helper()
	prefix := fmt.Sprintf(`Software\ts-browser-ext-test\%d-%d`, os.Getpid(), time.Now().UnixNano())

	old := registryPrefix
	registryPrefix = prefix
	t.Cleanup(func() {
		registryPrefix = old
		if err := deleteKeyRecursive(registry.CURRENT_USER, prefix); err != nil {
			t.Errorf("cleaning up HKCU\\%s: %v", prefix, err)
		}
	})
}

// registeredPath returns the manifest path the browser would find in the
// registry, and whether the platform registers hosts that way at all.
func registeredPath(t *testing.T, browserByte string) (path string, applies bool) {
	t.Helper()
	keyPath, err := registryKeyPath(browserByte)
	if err != nil {
		t.Fatal(err)
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", true
		}
		t.Fatalf("opening HKCU\\%s: %v", keyPath, err)
	}
	defer k.Close()

	v, _, err := k.GetStringValue("")
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", true
		}
		t.Fatalf("reading HKCU\\%s: %v", keyPath, err)
	}
	return v, true
}

// deleteKeyRecursive removes path and everything under it. registry.DeleteKey
// only removes leaf keys, and registerHost creates intermediates.
func deleteKeyRecursive(root registry.Key, path string) error {
	k, err := registry.OpenKey(root, path, registry.READ)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	names, err := k.ReadSubKeyNames(-1)
	k.Close()
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := deleteKeyRecursive(root, path+`\`+name); err != nil {
			return err
		}
	}
	return registry.DeleteKey(root, path)
}

// TestRegisterHost covers the registry side on its own: registering writes the
// manifest path, registering again overwrites rather than failing, and
// unregistering removes the key and tolerates it being gone already.
func TestRegisterHost(t *testing.T) {
	for _, browserByte := range []string{"C", "F"} {
		t.Run(browserByte, func(t *testing.T) {
			useScratchRegistry(t)

			if got, _ := registeredPath(t, browserByte); got != "" {
				t.Fatalf("scratch registry was not empty: %q", got)
			}

			const first = `C:\Users\someone\AppData\Local\Tailscale\BrowserExt\host.json`
			if err := registerHost(browserByte, first); err != nil {
				t.Fatal(err)
			}
			if got, _ := registeredPath(t, browserByte); got != first {
				t.Errorf("registered path = %q, want %q", got, first)
			}

			const second = `C:\Users\someone\AppData\Local\Tailscale\BrowserExt\other.json`
			if err := registerHost(browserByte, second); err != nil {
				t.Fatalf("re-registering: %v", err)
			}
			if got, _ := registeredPath(t, browserByte); got != second {
				t.Errorf("after re-registering, path = %q, want %q", got, second)
			}

			if err := unregisterHost(browserByte); err != nil {
				t.Fatal(err)
			}
			if got, _ := registeredPath(t, browserByte); got != "" {
				t.Errorf("key still present after unregistering: %q", got)
			}

			// Removing a registration that isn't there is not an error;
			// --uninstall runs for both browsers whichever was installed.
			if err := unregisterHost(browserByte); err != nil {
				t.Errorf("unregistering twice: %v", err)
			}
		})
	}
}
