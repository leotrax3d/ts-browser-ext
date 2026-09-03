//go:build windows

package main

import (
	"fmt"
	"log"

	"golang.org/x/sys/windows/registry"
)

// registryKeyPath returns the HKCU key a browser reads to discover native
// messaging hosts. Unlike macOS and Linux, Windows browsers don't scan a
// directory: the manifest may live anywhere and the registry points at it.
func registryKeyPath(browserByte string) (string, error) {
	switch browserByte {
	case "C":
		return `Software\Google\Chrome\NativeMessagingHosts\` + chromeHostName, nil
	case "F":
		return `Software\Mozilla\NativeMessagingHosts\` + firefoxHostName, nil
	}
	return "", fmt.Errorf("unknown browser prefix byte %q", browserByte)
}

// registerHost points the browser at the manifest at jsonPath.
func registerHost(browserByte, jsonPath string) error {
	path, err := registryKeyPath(browserByte)
	if err != nil {
		return err
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.WRITE)
	if err != nil {
		return fmt.Errorf("creating registry key HKCU\\%s: %w", path, err)
	}
	defer k.Close()

	// The default (unnamed) value holds the manifest path.
	if err := k.SetStringValue("", jsonPath); err != nil {
		return fmt.Errorf("writing registry key HKCU\\%s: %w", path, err)
	}
	log.Printf("registered HKCU\\%s -> %v", path, jsonPath)
	return nil
}

// unregisterHost removes the registry key written by [registerHost].
// A key that isn't there is not an error.
func unregisterHost(browserByte string) error {
	path, err := registryKeyPath(browserByte)
	if err != nil {
		return err
	}
	if err := registry.DeleteKey(registry.CURRENT_USER, path); err != nil {
		if err == registry.ErrNotExist {
			return nil
		}
		return fmt.Errorf("deleting registry key HKCU\\%s: %w", path, err)
	}
	log.Printf("removed HKCU\\%s", path)
	return nil
}
