package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseInstallArg(t *testing.T) {
	const chromeID = "abcdefghijklmnopqrstuvwxyzabcdef" // 32 chars

	tests := []struct {
		name        string
		arg         string
		wantBrowser string
		wantExt     string
		wantErr     bool
	}{
		{name: "chrome", arg: "C" + chromeID, wantBrowser: "C", wantExt: chromeID},
		{name: "firefox", arg: "F", wantBrowser: "F"},
		{name: "empty", arg: "", wantErr: true},
		{name: "unknown browser", arg: "X" + chromeID, wantErr: true},
		{name: "lowercase browser byte", arg: "c" + chromeID, wantErr: true},
		{name: "chrome without ID", arg: "C", wantErr: true},
		{name: "chrome ID too short", arg: "C" + chromeID[:31], wantErr: true},
		{name: "chrome ID too long", arg: "C" + chromeID + "a", wantErr: true},
		{name: "chrome ID uppercase", arg: "C" + strings.ToUpper(chromeID), wantErr: true},
		{name: "chrome ID with separator", arg: "C" + chromeID[:31] + "/", wantErr: true},
		{name: "firefox with trailing ID", arg: "F" + chromeID, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			browser, ext, err := parseInstallArg(tt.arg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseInstallArg(%q) error = %v, wantErr %v", tt.arg, err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if browser != tt.wantBrowser || ext != tt.wantExt {
				t.Errorf("parseInstallArg(%q) = %q, %q; want %q, %q", tt.arg, browser, ext, tt.wantBrowser, tt.wantExt)
			}
		})
	}
}

func TestValidInitID(t *testing.T) {
	tests := []struct {
		id      string
		wantErr bool
	}{
		{id: "3f2504e0-4f89-11d3-9a0c-0305e82c3301"},
		{id: "0123456789abcdef"},
		{id: "-"},
		{id: "", wantErr: true},
		{id: strings.Repeat("a", 60)},
		{id: strings.Repeat("a", 61), wantErr: true},
		{id: "ABCDEF", wantErr: true}, // uppercase hex is not accepted
		{id: "g", wantErr: true},      // not hex
		{id: "../etc", wantErr: true}, // the ID becomes a path element
		{id: "a/b", wantErr: true},    //
		{id: "a\x00b", wantErr: true}, //
	}
	for _, tt := range tests {
		err := validInitID(tt.id)
		if (err != nil) != tt.wantErr {
			t.Errorf("validInitID(%q) = %v, wantErr %v", tt.id, err, tt.wantErr)
		}
	}
}

// TestHostManifestJSON checks that a Windows-style path, which contains
// backslashes, survives into the manifest as a valid JSON string.
func TestHostManifestJSON(t *testing.T) {
	const path = `C:\Users\someone\AppData\Local\Tailscale\BrowserExt\ts-browser-ext.exe`
	b, err := json.Marshal(&hostManifest{
		Name:           chromeHostName,
		Path:           path,
		Type:           "stdio",
		AllowedOrigins: []string{"chrome-extension://abc/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got hostManifest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("manifest is not valid JSON: %v (%s)", err, b)
	}
	if got.Path != path {
		t.Errorf("path round-tripped as %q, want %q", got.Path, path)
	}
	if len(got.AllowedExtensions) != 0 {
		t.Errorf("allowed_extensions should be omitted for Chrome; got %q", got.AllowedExtensions)
	}
}

func TestHostName(t *testing.T) {
	if got, err := hostName("C"); err != nil || got != chromeHostName {
		t.Errorf(`hostName("C") = %q, %v; want %q`, got, err, chromeHostName)
	}
	if got, err := hostName("F"); err != nil || got != firefoxHostName {
		t.Errorf(`hostName("F") = %q, %v; want %q`, got, err, firefoxHostName)
	}
	if _, err := hostName("X"); err == nil {
		t.Error(`hostName("X") succeeded, want error`)
	}
}

// TestGetTargetJSON checks the manifest is named after the host, which is how
// the browser finds it.
func TestGetTargetJSON(t *testing.T) {
	got, err := getTargetJSON("F", filepath.Join("some", "dir"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("some", "dir", firefoxHostName+".json"); got != want {
		t.Errorf("getTargetJSON = %q, want %q", got, want)
	}
}

// TestMessageRoundTrip runs a message through the native messaging framing and
// reads it back, checking the 4-byte little-endian length prefix agrees
// between the writer and the reader.
func TestMessageRoundTrip(t *testing.T) {
	want := &request{Cmd: CmdInit, InitID: "3f2504e0-4f89-11d3-9a0c-0305e82c3301"}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	w := newTestHost(t, strings.NewReader(""), &buf)
	if err := w.writeFramed(wantJSON); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.Len(), len(wantJSON)+4; got != want {
		t.Errorf("framed length %v, want %v", got, want)
	}

	r := newTestHost(t, bytes.NewReader(buf.Bytes()), io.Discard)
	got, err := r.readMessage()
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-tripped %+v, want %+v", got, want)
	}
}

// TestSendFraming checks send's output is readable by readMessage, so the two
// halves of the protocol agree.
func TestSendFraming(t *testing.T) {
	var buf bytes.Buffer
	h := newTestHost(t, strings.NewReader(""), &buf)
	if err := h.send(&reply{Status: &status{Running: true, Tailnet: "example.com"}}); err != nil {
		t.Fatal(err)
	}

	var lenBuf [4]byte
	if _, err := io.ReadFull(&buf, lenBuf[:]); err != nil {
		t.Fatal(err)
	}
	n := binary.LittleEndian.Uint32(lenBuf[:])
	if int(n) != buf.Len() {
		t.Fatalf("length prefix says %v bytes, %v remain", n, buf.Len())
	}
	var got reply
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if got.Status == nil || !got.Status.Running || got.Status.Tailnet != "example.com" {
		t.Errorf("got %+v, want a running status for example.com", got.Status)
	}
}

// TestReadMessageTooBig checks an oversized length prefix is rejected before
// allocating, rather than trusting the browser's framing.
func TestReadMessageTooBig(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0xff, 0xff, 0xff, 0xff})
	h := newTestHost(t, bytes.NewReader(buf.Bytes()), io.Discard)
	if _, err := h.readMessage(); err == nil {
		t.Fatal("readMessage accepted a 4GiB message")
	}
}

// TestSendTooBig checks we don't try to frame a message the browser would
// reject anyway.
func TestSendTooBig(t *testing.T) {
	h := newTestHost(t, strings.NewReader(""), io.Discard)
	if err := h.send(&reply{Status: &status{Tailnet: strings.Repeat("x", maxMsgSize+1)}}); err == nil {
		t.Fatal("send accepted an over-sized message")
	}
}

// newTestHost builds a host for tests, failing the test rather than returning
// the credential error every caller would only pass along.
func newTestHost(t *testing.T, r io.Reader, w io.Writer) *host {
	t.Helper()
	h, err := newHost(r, w)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
