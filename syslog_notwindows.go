//go:build !windows && !js

package main

import (
	"io"
	"log/syslog"
)

// dialSyslog connects to a TCP syslog listener at addr, for debug logging.
func dialSyslog(addr string) (io.Writer, error) {
	return syslog.Dial("tcp", addr, syslog.LOG_INFO, "browser")
}
