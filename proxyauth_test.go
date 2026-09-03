package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// startTestProxy brings up the real proxy listener and returns its address.
func startTestProxy(t *testing.T) (addr string, h *host) {
	t.Helper()
	h = newTestHost(t, strings.NewReader(""), io.Discard)
	ln, err := h.getProxyListener()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String(), h
}

// TestHTTPProxyRequiresCredential is the security case: an unauthenticated
// caller must get nowhere. Any local process can reach this port, and on a
// multi-user machine so can any other logged-in user.
func TestHTTPProxyRequiresCredential(t *testing.T) {
	addr, h := startTestProxy(t)

	tests := []struct {
		name   string
		header string
	}{
		{name: "no header", header: ""},
		{name: "empty basic", header: "Basic "},
		{name: "not basic", header: "Bearer " + h.cred.Password},
		{name: "garbage", header: "Basic !!!not-base64!!!"},
		{name: "wrong password", header: basicHeader(h.cred.Username, "hunter2")},
		{name: "wrong username", header: basicHeader("someone-else", h.cred.Password)},
		{name: "password as username", header: basicHeader(h.cred.Password, h.cred.Password)},
		{name: "no colon", header: "Basic " + base64.StdEncoding.EncodeToString([]byte("nocolon"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Ask for the web client, which is served locally and so would
			// answer without a tailnet. Nothing may come back but a refusal.
			req, err := http.NewRequest("GET", "http://100.100.100.100/", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.header != "" {
				req.Header.Set("Proxy-Authorization", tt.header)
			}
			resp := roundTripVia(t, addr, req)
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusProxyAuthRequired {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
				t.Errorf("status = %d, want %d; body: %s",
					resp.StatusCode, http.StatusProxyAuthRequired, body)
			}
			if got := resp.Header.Get("Proxy-Authenticate"); !strings.HasPrefix(got, "Basic ") {
				t.Errorf("Proxy-Authenticate = %q, want a Basic challenge", got)
			}
		})
	}
}

// TestHTTPProxyAcceptsCredential checks the credential actually opens the
// door, so the test above is not passing because everything is refused.
func TestHTTPProxyAcceptsCredential(t *testing.T) {
	addr, h := startTestProxy(t)

	req, err := http.NewRequest("GET", "http://100.100.100.100/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Proxy-Authorization", basicHeader(h.cred.Username, h.cred.Password))
	resp := roundTripVia(t, addr, req)
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusProxyAuthRequired {
		t.Fatalf("the right credential was refused")
	}
	// Nothing has sent init, so the web client does not exist yet. What
	// matters here is that the request got past the proxy's own gate and was
	// answered rather than dropped.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Logf("status %d (any answer but 407 shows the credential was accepted)", resp.StatusCode)
	}
}

// TestSocksRequiresCredential checks the SOCKS5 half announces password
// authentication rather than letting a client in with none. Chrome uses the
// HTTP side and Firefox the SOCKS side, so both have to be closed.
func TestSocksRequiresCredential(t *testing.T) {
	addr, _ := startTestProxy(t)

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// SOCKS5 greeting offering "no authentication required" only.
	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatal(err)
	}
	var resp [2]byte
	if _, err := io.ReadFull(c, resp[:]); err != nil {
		t.Fatalf("reading the method selection: %v", err)
	}
	if resp[0] != 0x05 {
		t.Fatalf("version = %#x, want 5", resp[0])
	}
	// 0xFF is "no acceptable methods". Anything else, and in particular 0x00
	// for "none required", would mean an unauthenticated client got in.
	if resp[1] != 0xFF {
		t.Errorf("method = %#x, want 0xff (no acceptable methods)", resp[1])
	}
}

// TestRedactSecrets checks the password does not reach the log file, which
// exists to be attached to bug reports.
func TestRedactSecrets(t *testing.T) {
	msgb, err := json.Marshal(&reply{ProcRunning: &procRunningResult{
		Port:          1234,
		ProxyUsername: "ts-browser-ext",
		ProxyPassword: "s3cret-do-not-log",
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := string(redactSecrets(msgb))
	if strings.Contains(got, "s3cret-do-not-log") {
		t.Errorf("password survived redaction: %s", got)
	}
	// The username is not a secret and is useful when reading a log.
	if !strings.Contains(got, "ts-browser-ext") {
		t.Errorf("username should be kept: %s", got)
	}
	if !strings.Contains(got, `"port":1234`) {
		t.Errorf("redaction damaged the rest of the message: %s", got)
	}
}

func TestCredentialIsUnpredictable(t *testing.T) {
	a, err := newProxyCredential()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newProxyCredential()
	if err != nil {
		t.Fatal(err)
	}
	if a.Password == b.Password {
		t.Error("two runs produced the same password")
	}
	if len(a.Password) < 32 {
		t.Errorf("password is %d characters, too short to be worth having", len(a.Password))
	}
}

func basicHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// roundTripVia sends req to the proxy at addr as a proxy would receive it.
func roundTripVia(t *testing.T, addr string, req *http.Request) *http.Response {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	// A proxy request carries the absolute URL on the request line.
	if err := req.WriteProxy(c); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), req)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	return resp
}
