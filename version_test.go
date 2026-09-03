package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestProcRunningReportsVersion checks the first message carries the protocol
// version. The extension compares it against its own, and treats a missing
// field as a backend too old to have one, so it has to be sent even when the
// backend is otherwise failing.
func TestProcRunningReportsVersion(t *testing.T) {
	for _, tt := range []struct {
		name string
		msg  *procRunningResult
	}{
		{
			name: "running",
			msg: &procRunningResult{
				Port:            1234,
				ProtocolVersion: protocolVersion,
				Version:         backendVersion(),
			},
		},
		{
			name: "failed to start",
			msg: &procRunningResult{
				Error:           "listening on localhost: no",
				ProtocolVersion: protocolVersion,
				Version:         backendVersion(),
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(&reply{ProcRunning: tt.msg})
			if err != nil {
				t.Fatal(err)
			}
			// The field must be present and non-zero: the extension reads a
			// missing or zero value as "older than the version check".
			if !strings.Contains(string(b), `"protocolVersion":`) {
				t.Errorf("protocolVersion missing from %s", b)
			}
			var got reply
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			if got.ProcRunning.ProtocolVersion != protocolVersion {
				t.Errorf("protocolVersion = %d, want %d",
					got.ProcRunning.ProtocolVersion, protocolVersion)
			}
		})
	}
}

// TestProtocolVersionMatchesExtension keeps the two halves in step: the
// constant here and the one in each fork's background.js are the same number
// by definition, and nothing else checks that they were bumped together.
func TestProtocolVersionMatchesExtension(t *testing.T) {
	for _, path := range []string{"background.js", "firefox/background.js"} {
		got, err := protocolVersionInJS(path)
		if err != nil {
			t.Errorf("%v: %v", path, err)
			continue
		}
		if got != protocolVersion {
			t.Errorf("%v declares PROTOCOL_VERSION = %d, backend speaks %d; "+
				"bump both or neither", path, got, protocolVersion)
		}
	}
}

func TestBackendVersion(t *testing.T) {
	if got := backendVersion(); got == "" {
		t.Error("backendVersion is empty; it goes into bug reports")
	}
}

// protocolVersionInJS reads the PROTOCOL_VERSION constant out of an extension
// background script.
func protocolVersionInJS(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	m := protocolVersionRE.FindSubmatch(b)
	if m == nil {
		return 0, fmt.Errorf("no PROTOCOL_VERSION declaration found")
	}
	return strconv.Atoi(string(m[1]))
}

var protocolVersionRE = regexp.MustCompile(`const PROTOCOL_VERSION = (\d+);`)
