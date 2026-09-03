//go:build windows

package main

import (
	"errors"
	"io"
)

// dialSyslog reports that debug syslog logging is unavailable: the log/syslog
// package isn't implemented on Windows.
func dialSyslog(addr string) (io.Writer, error) {
	return nil, errors.New("--syslog is not supported on Windows")
}
