package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/csrf"
	"tailscale.com/client/tailscale"
	"tailscale.com/client/web"
	"tailscale.com/hostinfo"
	"tailscale.com/ipn"
	"tailscale.com/net/proxymux"
	"tailscale.com/net/socks5"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"
	"tailscale.com/types/netmap"
)

var (
	installFlag   = flag.String("install", "", "register the browser extension; string is 'C' (Chrome) or 'F' (Firefox) followed by extension ID")
	uninstallFlag = flag.Bool("uninstall", false, "unregister the browser extension")
	syslogFlag    = flag.String("syslog", os.Getenv("TS_BROWSER_EXT_SYSLOG"), "if non-empty, host:port of a TCP syslog listener to send logs to, for debugging; defaults to $TS_BROWSER_EXT_SYSLOG")
)

func main() {
	flag.Parse()
	if *installFlag != "" {
		if err := install(*installFlag); err != nil {
			log.Fatalf("installation error: %v", err)
		}
		return
	}
	if *uninstallFlag {
		if err := uninstall(); err != nil {
			log.Fatalf("uninstallation error: %v", err)
		}
		return
	}

	if flag.NArg() == 0 {
		fmt.Printf(`ts-browser-ext is the backend for the Tailscale browser extension,
running as a child process HTTP/SOCKS5 proxy under your browser.

It is normally started by the browser, not by hand. To register it once,
run the command the extension's popup prints, which looks like:

     $ ts-browser-ext --install=C<chrome-extension-id>   # Chrome
     $ ts-browser-ext --install=F                        # Firefox

To unregister it again:

     $ ts-browser-ext --uninstall
`)
		return
	}

	hostinfo.SetApp("ts-browser-ext")

	h := newHost(os.Stdin, os.Stdout)
	logPath := setupLogging()
	h.logf("ts-browser-ext starting, pid %v", os.Getpid())

	ln, err := h.getProxyListener()
	if err != nil {
		// The extension can't do anything without a proxy port, but it can at
		// least say why rather than reporting the backend as missing.
		h.logf("could not start proxy: %v", err)
		h.send(&reply{ProcRunning: &procRunningResult{
			Pid:             os.Getpid(),
			Error:           err.Error(),
			LogPath:         logPath,
			ProtocolVersion: protocolVersion,
			Version:         backendVersion(),
		}})
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	h.logf("Proxy listening on localhost:%v", port)

	h.send(&reply{ProcRunning: &procRunningResult{
		Port:            port,
		Pid:             os.Getpid(),
		LogPath:         logPath,
		ProtocolVersion: protocolVersion,
		Version:         backendVersion(),
	}})
	h.logf("Starting readMessages loop")
	err = h.readMessages()
	h.logf("readMessage loop ended: %v", err)
}

const (
	// maxLogSize is how large the log file may grow before rolling over, and
	// keepLogs how many rolled-over files are retained beside it.
	maxLogSize = 2 << 20
	keepLogs   = 3
)

// protocolVersion is the version of the native messaging protocol this backend
// speaks, reported to the extension on connect so it can compare.
//
// The two halves are updated separately: the extension through the browser's
// store, the backend by re-running --install. They drift apart, and without
// this the mismatch surfaces as commands that quietly do nothing rather than
// as something the user can act on.
//
// Bump it when a change would leave an older peer misreading a message.
const protocolVersion = 1

// backendVersion describes this build, for display and for bug reports. It
// deliberately has no part in the version check: what matters there is whether
// the two sides speak the same protocol, not which build is newer.
func backendVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				modified = "-dirty"
			}
		}
	}
	if revision == "" {
		// Built without VCS information, as "go build" does from a module
		// cache; the module version is the best available.
		if v := info.Main.Version; v != "" {
			return v
		}
		return "unknown"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	return revision + modified
}

// setupLogging sends the standard logger, which is what [host.logf] uses, to a
// rotating file and, if --syslog was given, to that listener as well. It
// returns the log file's path, or the empty string if none could be opened.
//
// Until this runs, logs go to stderr, which the browser discards.
func setupLogging() (logPath string) {
	var sinks []io.Writer

	dir, err := logDirFor()
	if err != nil {
		log.Printf("no log directory: %v", err)
	} else {
		path := filepath.Join(dir, "backend.log")
		f, err := newRotatingFile(path, maxLogSize, keepLogs)
		if err != nil {
			log.Printf("opening %v: %v", path, err)
		} else {
			sinks = append(sinks, f)
			logPath = path
		}
	}

	if addr := *syslogFlag; addr != "" {
		w, err := dialSyslog(addr)
		if err != nil {
			log.Printf("syslog: %v", err)
		} else {
			sinks = append(sinks, w)
		}
	}

	if len(sinks) > 0 {
		log.SetOutput(io.MultiWriter(sinks...))
	}
	return logPath
}

// The native messaging host names, as used by both the manifest and the
// browser-side connectNative call.
const (
	chromeHostName  = "com.tailscale.browserext.chrome"
	firefoxHostName = "com.tailscale.browserext.firefox"
)

// hostName returns the native messaging host name for a browser byte.
func hostName(browserByte string) (string, error) {
	switch browserByte {
	case "C":
		return chromeHostName, nil
	case "F":
		return firefoxHostName, nil
	}
	return "", fmt.Errorf("unknown browser prefix byte %q", browserByte)
}

// targetBinName is what we call our copy of ourselves in the target directory.
func targetBinName() string {
	if runtime.GOOS == "windows" {
		return "ts-browser-ext.exe"
	}
	return "ts-browser-ext"
}

// getTargetDir returns the directory to install the binary and the host
// manifest into, creating it if needed.
//
// On macOS and Linux this is the per-user directory the browser scans for
// native messaging hosts. Windows browsers find the manifest through the
// registry instead (see registerHost), so there the location is ours to pick.
func getTargetDir(browserByte string) (string, error) {
	if _, err := hostName(browserByte); err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	var dir string
	switch runtime.GOOS {
	case "linux":
		if browserByte == "C" {
			dir = filepath.Join(home, ".config", "google-chrome", "NativeMessagingHosts")
		} else {
			dir = filepath.Join(home, ".mozilla", "native-messaging-hosts")
		}
	case "darwin":
		if browserByte == "C" {
			dir = filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "NativeMessagingHosts")
		} else {
			dir = filepath.Join(home, "Library", "Application Support", "Mozilla", "NativeMessagingHosts")
		}
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = filepath.Join(home, "AppData", "Local")
		}
		if browserByte == "C" {
			dir = filepath.Join(base, "Tailscale", "BrowserExt", "Chrome")
		} else {
			dir = filepath.Join(base, "Tailscale", "BrowserExt", "Firefox")
		}
	default:
		return "", fmt.Errorf("installing is not supported on %q", runtime.GOOS)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return dir, nil
}

// getTargetJSON returns the path of the native messaging host manifest.
func getTargetJSON(browserByte, targetDir string) (string, error) {
	name, err := hostName(browserByte)
	if err != nil {
		return "", err
	}
	return filepath.Join(targetDir, name+".json"), nil
}

func uninstall() error {
	for _, browserByte := range []string{"C", "F"} {
		if err := unregisterHost(browserByte); err != nil {
			return err
		}
		targetDir, err := getTargetDir(browserByte)
		if err != nil {
			return err
		}
		targetJSON, err := getTargetJSON(browserByte, targetDir)
		if err != nil {
			return err
		}
		targetBin := filepath.Join(targetDir, targetBinName())
		if err := os.Remove(targetBin); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Remove(targetJSON); err != nil && !os.IsNotExist(err) {
			return err
		}
		// Leftovers from [replaceBinary]: a .old that was still running when
		// an upgrade displaced it, or a .new from a write that failed partway.
		// Neither is running now, in the usual case where the browser has
		// released the backend before an uninstall, but if one still is, say
		// so rather than reporting a clean uninstall.
		for _, leftover := range []string{targetBin + ".old", targetBin + ".new"} {
			if err := os.Remove(leftover); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// chromeExtensionIDRE matches a Chrome extension ID: 32 letters from a-p,
// though we accept any lowercase alphanumerics.
var chromeExtensionIDRE = regexp.MustCompile(`^[a-z0-9]{32}$`)

// parseInstallArg splits the --install value into its browser byte and, for
// Chrome, the extension ID. Firefox keys on a fixed add-on ID instead, so it
// takes no ID.
func parseInstallArg(installArg string) (browserByte, extension string, err error) {
	if installArg == "" {
		return "", "", errors.New("empty --install value")
	}
	browserByte, extension = installArg[0:1], installArg[1:]
	switch browserByte {
	case "C":
		if !chromeExtensionIDRE.MatchString(extension) {
			return "", "", fmt.Errorf("invalid extension ID %q", extension)
		}
	case "F":
		if extension != "" {
			return "", "", fmt.Errorf("unexpected extension ID %q after Firefox prefix", extension)
		}
	default:
		return "", "", fmt.Errorf("unknown browser prefix byte %q", browserByte)
	}
	return browserByte, extension, nil
}

func install(installArg string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return installFrom(exe, installArg)
}

// installFrom registers the browser extension, copying the backend binary from
// exe. install passes the running executable; tests pass a stand-in, so the
// installation can be exercised without copying a large binary around.
func installFrom(exe, installArg string) error {
	browserByte, extension, err := parseInstallArg(installArg)
	if err != nil {
		return err
	}

	targetDir, err := getTargetDir(browserByte)
	if err != nil {
		return err
	}
	binary, err := os.ReadFile(exe)
	if err != nil {
		return err
	}
	targetBin := filepath.Join(targetDir, targetBinName())
	if err := replaceBinary(targetBin, binary); err != nil {
		return err
	}
	log.SetFlags(0)
	log.Printf("copied binary to %v", targetBin)

	name, err := hostName(browserByte)
	if err != nil {
		return err
	}
	targetJSON, err := getTargetJSON(browserByte, targetDir)
	if err != nil {
		return err
	}
	manifest := &hostManifest{
		Name:        name,
		Description: "Tailscale Browser Extension",
		Path:        targetBin,
		Type:        "stdio",
	}
	switch browserByte {
	case "C":
		manifest.AllowedOrigins = []string{"chrome-extension://" + extension + "/"}
	case "F":
		manifest.AllowedExtensions = []string{firefoxExtensionID}
	}
	// Marshal rather than format the JSON: on Windows targetBin contains
	// backslashes, which have to be escaped.
	jsonConf, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(targetJSON, jsonConf, 0644); err != nil {
		return err
	}
	log.Printf("wrote registration to %v", targetJSON)

	return registerHost(browserByte, targetJSON)
}

// replaceBinary installs content as the executable at path, replacing whatever
// is there.
//
// It writes alongside and renames into place rather than writing over the
// target. Windows refuses to open a running executable for writing, so a
// straight write fails for the whole time the browser has the backend
// started, which is exactly when someone re-runs --install to upgrade. It
// does allow renaming a running executable, since executables are mapped
// with FILE_SHARE_DELETE, so moving the old one aside clears the way.
//
// Renaming into place also removes the window in which a partially written
// file sits at the path the browser is about to launch.
func replaceBinary(path string, content []byte) error {
	newPath := path + ".new"
	if err := os.WriteFile(newPath, content, 0755); err != nil {
		return err
	}
	defer os.Remove(newPath) // no-op once the rename below succeeds

	if err := os.Rename(newPath, path); err == nil {
		// Nothing was holding the old binary. Clear out any leftover from a
		// previous upgrade that could not delete it at the time.
		os.Remove(path + ".old")
		return nil
	}

	// Busy, or otherwise not replaceable in one step: move the old one out of
	// the way and try again.
	oldPath := path + ".old"
	if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %v: %w", oldPath, err)
	}
	if err := os.Rename(path, oldPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("moving %v aside: %w", path, err)
	}
	if err := os.Rename(newPath, path); err != nil {
		return fmt.Errorf("installing %v: %w", path, err)
	}

	// The displaced binary is still running, so this usually fails; the next
	// install removes it. Nothing depends on it being gone now.
	os.Remove(oldPath)
	return nil
}

// firefoxExtensionID is the add-on ID from firefox/manifest.json. Unlike
// Chrome, where the ID depends on how the extension was loaded, it is fixed.
const firefoxExtensionID = "browser-ext@tailscale.com"

// hostManifest is a native messaging host manifest, as documented at
// https://developer.chrome.com/docs/extensions/develop/concepts/native-messaging
// and the Firefox equivalent. Chrome keys on the extension origin, Firefox on
// the add-on ID, so exactly one of the two allowed_ lists is set.
type hostManifest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
	Type        string `json:"type"`

	AllowedOrigins    []string `json:"allowed_origins,omitempty"`
	AllowedExtensions []string `json:"allowed_extensions,omitempty"`
}

type host struct {
	br   *bufio.Reader
	w    io.Writer
	logf logger.Logf

	wmu sync.Mutex // guards writing to w

	lenBuf [4]byte // owned by readMessage

	mu              sync.Mutex
	watchDead       bool
	lastNetmap      *netmap.NetworkMap
	lastState       ipn.State
	lastBrowseToURL string
	ctx             context.Context // for IPN bus; canceled by cancelCtx
	cancelCtx       context.CancelFunc
	ts              *tsnet.Server
	ws              *web.Server
	ln              net.Listener
	wantUp          bool
	fatalErr        string // non-empty once the proxy has stopped serving
	// ...
}

func newHost(r io.Reader, w io.Writer) *host {
	h := &host{
		br:   bufio.NewReaderSize(r, 1<<20),
		w:    w,
		logf: log.Printf,
	}
	h.ts = &tsnet.Server{
		RunWebClient: true,

		// late-binding, so caller can adjust h.logf.
		Logf: func(f string, a ...any) {
			h.logf(f, a...)
		},
	}
	return h
}

const maxMsgSize = 1 << 20

// maxLogPreview bounds how much of a message body reaches the log. Messages
// run up to [maxMsgSize], and a megabyte on a single line is more than a log
// file or a CI log viewer should have to swallow to tell you what happened.
const maxLogPreview = 1024

// logPreview returns b for logging, shortened if it is unreasonably long.
func logPreview(b []byte) string {
	if len(b) <= maxLogPreview {
		return string(b)
	}
	return fmt.Sprintf("%s... (%d bytes total)", b[:maxLogPreview], len(b))
}

func (h *host) readMessages() error {
	for {
		msg, err := h.readMessage()
		if err != nil {
			return err
		}
		if err := h.handleMessage(msg); err != nil {
			h.logf("error handling message %v: %v", msg, err)
			return err
		}
	}
}

func (h *host) handleMessage(msg *request) error {
	switch msg.Cmd {
	case CmdInit:
		return h.handleInit(msg)
	case CmdGetStatus:
		h.sendStatus()
	case CmdUp:
		return h.handleUp()
	case CmdDown:
		return h.handleDown()
	default:
		h.logf("unknown command %q", msg.Cmd)
	}
	return nil
}

func (h *host) handleUp() error {
	return h.setWantRunning(true)
}

func (h *host) handleDown() error {
	return h.setWantRunning(false)
}

func (h *host) setWantRunning(want bool) error {
	defer h.sendStatus()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ts.Sys() == nil {
		return fmt.Errorf("not init")
	}
	h.wantUp = want
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lc, err := h.ts.LocalClient()
	if err != nil {
		return err
	}
	if _, err := lc.EditPrefs(ctx, &ipn.MaskedPrefs{
		WantRunningSet: true,
		Prefs: ipn.Prefs{
			WantRunning: want,
		},
	}); err != nil {
		return fmt.Errorf("EditPrefs to wantRunning=%v: %w", want, err)
	}
	return nil
}

func (h *host) handleInit(msg *request) (ret error) {
	defer func() {
		var errMsg string
		if ret != nil {
			errMsg = ret.Error()
		}
		h.send(&reply{
			Init: &initResult{Error: errMsg},
		})
	}()
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.cancelCtx != nil {
		h.cancelCtx()
	}
	h.ctx, h.cancelCtx = context.WithCancel(context.Background())

	id := msg.InitID
	if err := validInitID(id); err != nil {
		return err
	}

	if h.ts.Sys() != nil {
		return fmt.Errorf("already running")
	}
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("getting current user: %w", err)
	}
	h.ts.Hostname = u.Username + "-browser-ext"

	confDir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("getting user config dir: %w", err)
	}
	h.ts.Dir = filepath.Join(confDir, "tailscale-browser-ext", id)

	h.logf("Starting...")
	if err := h.ts.Start(); err != nil {
		return fmt.Errorf("starting tsnet.Server: %w", err)
	}
	h.logf("Started")

	lc, err := h.ts.LocalClient()
	if err != nil {
		return fmt.Errorf("getting local client: %w", err)
	}

	wc, err := lc.WatchIPNBus(h.ctx, ipn.NotifyInitialState|ipn.NotifyRateLimit)
	if err != nil {
		return fmt.Errorf("watching IPN bus: %w", err)
	}
	go h.watchIPNBus(wc)

	h.ws, err = web.NewServer(web.ServerOpts{
		Mode:        web.LoginServerMode, // TODO: manage?
		LocalClient: lc,
	})
	if err != nil {
		return fmt.Errorf("NewServer: %w", err)
	}

	return nil
}

// validInitID reports whether id is safe to use as a directory name, since it
// comes from JavaScript and ends up in a filesystem path. See [request.InitID].
func validInitID(id string) error {
	if len(id) == 0 {
		return errors.New("missing initID")
	}
	if len(id) > 60 {
		return errors.New("initID too long")
	}
	for i := range len(id) {
		b := id[i]
		if b == '-' || (b >= 'a' && b <= 'f') || (b >= '0' && b <= '9') {
			continue
		}
		return errors.New("invalid initID character")
	}
	return nil
}

func (h *host) watchIPNBus(wc *tailscale.IPNBusWatcher) {
	h.mu.Lock()
	h.watchDead = false
	h.mu.Unlock()

	for h.updateFromWatcher(wc) {
		// Keep going.
	}
}

func (h *host) updateFromWatcher(wc *tailscale.IPNBusWatcher) bool {
	n, err := wc.Next()

	defer h.sendStatus()

	h.mu.Lock()
	defer h.mu.Unlock()

	if err != nil {
		log.Printf("watchIPNBus: %v", err)
		h.watchDead = true
		return false
	}

	if n.NetMap != nil {
		h.lastNetmap = n.NetMap
	}
	if n.State != nil {
		h.lastState = *n.State
	}

	if n.BrowseToURL != nil {
		h.lastBrowseToURL = *n.BrowseToURL
		// TODO: pop a browser for Tailscale SSH check mode etc, even
		// if already logged in.
	}
	return true
}

func (h *host) send(msg *reply) error {
	msgb, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("json encoding of message: %w", err)
	}
	h.logf("sent reply: %s", logPreview(msgb))
	return h.writeFramed(msgb)
}

// writeFramed writes msgb with the 4-byte little-endian length prefix the
// native messaging protocol uses.
func (h *host) writeFramed(msgb []byte) error {
	if len(msgb) > maxMsgSize {
		return fmt.Errorf("message too big (%v)", len(msgb))
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(msgb)))
	h.wmu.Lock()
	defer h.wmu.Unlock()
	if _, err := h.w.Write(lenBuf[:]); err != nil {
		return err
	}
	if _, err := h.w.Write(msgb); err != nil {
		return err
	}
	return nil
}

func (h *host) getProxyListener() (net.Listener, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.getProxyListenerLocked()
}

func (h *host) getProxyListenerLocked() (net.Listener, error) {
	if h.ln != nil {
		return h.ln, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listening on localhost: %w", err)
	}
	h.ln = ln
	socksListener, httpListener := proxymux.SplitSOCKSAndHTTP(h.ln)

	hs := &http.Server{Handler: h.httpProxyHandler()}
	go func() {
		h.proxyDied("HTTP proxy", hs.Serve(httpListener))
	}()
	ss := &socks5.Server{
		Logf:   logger.WithPrefix(h.logf, "socks5: "),
		Dialer: h.userDial,
	}
	go func() {
		h.proxyDied("SOCKS5 server", ss.Serve(socksListener))
	}()
	return h.ln, nil
}

// proxyDied records that one of the proxy servers stopped serving and tells
// the extension. Without a working proxy the backend is useless, but killing
// the process only shows the user a dropped connection, which is
// indistinguishable from the backend never having been installed.
func (h *host) proxyDied(what string, err error) {
	h.logf("%s exited: %v", what, err)
	h.mu.Lock()
	if h.fatalErr == "" {
		h.fatalErr = fmt.Sprintf("%s stopped: %v", what, err)
	}
	h.mu.Unlock()
	h.sendStatus()
}

func (h *host) userDial(ctx context.Context, netw, addr string) (net.Conn, error) {
	h.mu.Lock()
	sys := h.ts.Sys()
	h.mu.Unlock()

	if sys == nil {
		h.logf("userDial to %v/%v without a tsnet.Server started", netw, addr)
		return nil, fmt.Errorf("no tsnet.Server")
	}

	return sys.Dialer.Get().UserDial(ctx, netw, addr)
}

func (h *host) sendStatus() {
	st := &status{}
	h.mu.Lock()
	st.Running = h.lastState == ipn.Running
	if nm := h.lastNetmap; nm != nil {
		st.Tailnet = nm.Domain
	}
	if h.lastState == ipn.NeedsLogin {
		st.NeedsLogin = true
		st.BrowseToURL = h.lastBrowseToURL
	} else if !st.Running {
		st.Error = "State: " + h.lastState.String()
	}
	if h.watchDead {
		st.Error = "WatchIPNBus stopped"
	}
	// A dead proxy outranks the other errors: nothing works without it.
	if h.fatalErr != "" {
		st.Error = h.fatalErr
	}
	h.mu.Unlock()

	if err := h.send(&reply{Status: st}); err != nil {
		h.logf("failed to send status: %v", err)
	}
}

type Cmd string

const (
	CmdInit      Cmd = "init"
	CmdUp        Cmd = "up"
	CmdDown      Cmd = "down"
	CmdGetStatus Cmd = "get-status"
)

// request is a message from the browser extension.
type request struct {
	// Cmd is the request type.
	Cmd Cmd `json:"cmd"`

	// InitID is the unique ID made by the extension (in its local storage) to
	// distinguish between different browser profiles using the same extension.
	// A given Go process will correspond to a single browser profile.
	// This lets us store tsnet state in different directories.
	// This string, coming from JavaScript, should not be trusted. It must be
	// UUID-ish: hex and hyphens only, and too long.
	InitID string `json:"initID,omitempty"`

	// ...
}

// reply is a message to the browser extension.
type reply struct {
	// ProcRunning is set on the first message when the Go process starts up.
	// It's the message that makes the browser recognize that the native
	// messaging port is up.
	ProcRunning *procRunningResult `json:"procRunning,omitempty"`

	// Status is sent in response to a [CmdGetStatus] [request.Cmd].
	Status *status `json:"status,omitempty"`

	Init *initResult `json:"init,omitempty"`
}

type procRunningResult struct {
	Port  int    `json:"port"` // HTTP+SOCKS5 localhost proxy port
	Pid   int    `json:"pid"`
	Error string `json:"error"`

	// LogPath is where the backend is writing its log file, for the popup to
	// show the user when something goes wrong. Empty if none could be opened.
	LogPath string `json:"logPath,omitempty"`

	// ProtocolVersion is [protocolVersion], for the extension to compare
	// against its own.
	ProtocolVersion int `json:"protocolVersion"`

	// Version describes this build, for the popup and for bug reports. It is
	// not what the version check uses.
	Version string `json:"version,omitempty"`
}

type initResult struct {
	Error string `json:"error"` // empty for none
}

type status struct {
	Running bool   `json:"running"`
	Tailnet string `json:"tailnet"`
	Error   string `json:"error,omitempty"`

	NeedsLogin  bool   `json:"needsLogin,omitempty"` // true if the user needs to log in
	BrowseToURL string `json:"browseToURL"`
}

func (h *host) readMessage() (*request, error) {
	if _, err := io.ReadFull(h.br, h.lenBuf[:]); err != nil {
		return nil, err
	}
	msgSize := binary.LittleEndian.Uint32(h.lenBuf[:])
	if msgSize > maxMsgSize {
		return nil, fmt.Errorf("message size too big (%v)", msgSize)
	}
	msgb := make([]byte, msgSize)
	if n, err := io.ReadFull(h.br, msgb); err != nil {
		return nil, fmt.Errorf("read %v of %v bytes in message with error %v", n, msgSize, err)
	}
	msg := new(request)
	if err := json.Unmarshal(msgb, msg); err != nil {
		return nil, fmt.Errorf("invalid JSON decoding of message: %w", err)
	}
	h.logf("got command %q: %s", msg.Cmd, msgb)
	return msg, nil
}

// httpProxyHandler returns an HTTP proxy http.Handler using the
// provided backend dialer.
func (h *host) httpProxyHandler() http.Handler {
	rp := &httputil.ReverseProxy{
		Director: func(r *http.Request) {}, // no change
		Transport: &http.Transport{
			DialContext: h.userDial,
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "100.100.100.100" {
			h.ws.ServeHTTP(w, csrf.PlaintextHTTPRequest(r))
			return
		}

		if r.Method != "CONNECT" {
			backURL := r.RequestURI
			if strings.HasPrefix(backURL, "/") || backURL == "*" {
				http.Error(w, "bogus RequestURI; must be absolute URL or CONNECT", 400)
				return
			}
			rp.ServeHTTP(w, r)
			return
		}

		// CONNECT support:

		dst := r.RequestURI
		c, err := h.userDial(r.Context(), "tcp", dst)
		if err != nil {
			w.Header().Set("Tailscale-Connect-Error", err.Error())
			http.Error(w, err.Error(), 500)
			return
		}
		defer c.Close()

		cc, ccbuf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer cc.Close()

		io.WriteString(cc, "HTTP/1.1 200 OK\r\n\r\n")

		var clientSrc io.Reader = ccbuf
		if ccbuf.Reader.Buffered() == 0 {
			// In the common case (with no
			// buffered data), read directly from
			// the underlying client connection to
			// save some memory, letting the
			// bufio.Reader/Writer get GC'ed.
			clientSrc = cc
		}

		errc := make(chan error, 1)
		go func() {
			_, err := io.Copy(cc, c)
			errc <- err
		}()
		go func() {
			_, err := io.Copy(c, clientSrc)
			errc <- err
		}()
		<-errc
	})
}
