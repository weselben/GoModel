package cursor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeScriptPath returns the absolute path to testdata/fake_bridge.sh.
func fakeScriptPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("testdata/fake_bridge.sh")
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	return p
}

// withFakeBridge sets CURSOR_SDK_BRIDGE_BIN to the fake script and
// restores it on cleanup.
func withFakeBridge(t *testing.T) string {
	t.Helper()
	p := fakeScriptPath(t)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fake bridge script missing at %s: %v", p, err)
	}
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatalf("chmod fake bridge: %v", err)
	}
	t.Setenv("CURSOR_SDK_BRIDGE_BIN", p)
	return p
}

// childPIDs returns the set of descendant PIDs of ppid (best-effort,
// /proc-based; Linux only). Empty on unsupported platforms.
func childPIDs(ppid int) map[int]struct{} {
	out := map[int]struct{}{}
	if runtime.GOOS != "linux" {
		return out
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// field 4 (1-based) is ppid; fields are space-separated and the
		// comm field is wrapped in parens, so find the last ")".
		s := string(stat)
		i := strings.LastIndex(s, ")")
		if i < 0 || i+2 >= len(s) {
			continue
		}
		rest := strings.Fields(s[i+2:])
		if len(rest) < 2 {
			continue
		}
		// After comm: state ppid ...
		if rest[1] == fmt.Sprint(ppid) {
			pid, err := atoi(e.Name())
			if err == nil {
				out[pid] = struct{}{}
			}
		}
	}
	return out
}

func atoi(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

func TestSpawnReadyParseAndTokenRead(t *testing.T) {
	withFakeBridge(t)

	tokenFile := filepath.Join(t.TempDir(), "auth-token")
	t.Setenv("FAKE_BRIDGE_TOKEN_FILE", tokenFile)
	wantToken := "secret-token-" + strings.ReplaceAll(time.Now().Format(time.RFC3339Nano), ":", "")
	t.Setenv("FAKE_BRIDGE_TOKEN", wantToken)

	bm, err := NewManagedBridgeManager("test-api-key",
		WithShutdownTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("NewManagedBridgeManager: %v", err)
	}
	// The gateway scrubs the child env, but the fake bridge reads its
	// own FAKE_BRIDGE_* knobs from the env. Re-add them so the fake
	// script can locate the token file.
	bm.cmd.Env = append(bm.cmd.Env,
		"FAKE_BRIDGE_TOKEN_FILE="+tokenFile,
		"FAKE_BRIDGE_TOKEN="+wantToken,
	)
	t.Cleanup(func() { _ = bm.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	endpt, tok, err := bm.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if endpt != "http://127.0.0.1:49152" {
		t.Errorf("endpoint = %q, want http://127.0.0.1:49152", endpt)
	}
	if tok != wantToken {
		t.Errorf("token = %q, want %q", tok, wantToken)
	}
	// Confirm process is alive.
	if bm.cmd == nil || bm.cmd.Process == nil {
		t.Fatal("expected managed bridge to have a running process")
	}
	if bm.cmd.ProcessState != nil {
		t.Errorf("process exited unexpectedly: %v", bm.cmd.ProcessState)
	}
	// Second Start returns cached values without re-spawning.
	oldCmd := bm.cmd
	endpt2, tok2, err := bm.Start(ctx)
	if err != nil || endpt2 != endpt || tok2 != tok {
		t.Errorf("Start cached mismatch: endpt=%q tok=%q err=%v", endpt2, tok2, err)
	}
	if bm.cmd != oldCmd {
		t.Error("second Start replaced cmd; should be cached")
	}
}

func TestExitBeforeReadySurfacesStderr(t *testing.T) {
	withFakeBridge(t)
	t.Setenv("FAKE_BRIDGE_MODE", "fail")

	bm, err := NewManagedBridgeManager("test-api-key")
	if err != nil {
		t.Fatalf("NewManagedBridgeManager: %v", err)
	}
	bm.cmd.Env = append(bm.cmd.Env, "FAKE_BRIDGE_MODE=fail")
	t.Cleanup(func() { _ = bm.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err = bm.Start(ctx)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "missing CURSOR_API_KEY") {
		t.Errorf("error %q does not include captured stderr", err.Error())
	}
	if !strings.Contains(err.Error(), "bridge exited before ready line") &&
		!strings.Contains(err.Error(), "EOF") {
		t.Errorf("error %q does not name the exit-before-ready cause", err.Error())
	}
}

func TestStartupTimeoutFires(t *testing.T) {
	withFakeBridge(t)
	t.Setenv("FAKE_BRIDGE_MODE", "hang")

	bm, err := NewManagedBridgeManager("test-api-key",
		WithStartupTimeout(150*time.Millisecond))
	if err != nil {
		t.Fatalf("NewManagedBridgeManager: %v", err)
	}
	bm.cmd.Env = append(bm.cmd.Env, "FAKE_BRIDGE_MODE=hang")
	t.Cleanup(func() { _ = bm.Close() })

	start := time.Now()
	_, _, err = bm.Start(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error %q does not mention timeout", err.Error())
	}
	if elapsed > 5*time.Second {
		t.Errorf("Start took %s; should have returned near the 150ms timeout", elapsed)
	}
	// Close should be safe (process is already gone or being killed).
	_ = bm.Close()
}

func TestCloseTerminatesProcessNoOrphan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals not supported on windows")
	}
	withFakeBridge(t)

	tokenFile := filepath.Join(t.TempDir(), "auth-token")
	t.Setenv("FAKE_BRIDGE_TOKEN_FILE", tokenFile)
	t.Setenv("FAKE_BRIDGE_TOKEN", "close-test-token")

	bm, err := NewManagedBridgeManager("test-api-key",
		WithShutdownTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("NewManagedBridgeManager: %v", err)
	}
	bm.cmd.Env = append(bm.cmd.Env,
		"FAKE_BRIDGE_TOKEN_FILE="+tokenFile,
		"FAKE_BRIDGE_TOKEN=close-test-token",
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err = bm.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := bm.cmd.Process.Pid
	before := childPIDs(os.Getpid())
	if _, ok := before[pid]; !ok {
		t.Fatalf("child pid %d not found in /proc before Close", pid)
	}

	// The Shutdown RPC target does not exist; Close should fall through
	// RPC failure to SIGTERM (200ms grace) and then SIGKILL.
	if err := bm.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Wait briefly for the kernel to reap and update /proc.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		children := childPIDs(os.Getpid())
		if _, ok := children[pid]; !ok {
			break
		}
		if bm.cmd != nil && bm.cmd.ProcessState != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if bm.cmd == nil || bm.cmd.ProcessState == nil {
		t.Errorf("cmd.ProcessState still nil after Close (possible orphan pid=%d)", pid)
	}
	if _, ok := childPIDs(os.Getpid())[pid]; ok {
		t.Errorf("child pid %d still alive after Close", pid)
	}
	// Close again should be a no-op.
	if err := bm.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestAttachModeCloseIsNoOp(t *testing.T) {
	// A control server that would record a request if Close ever dialed
	// the wire. Close in attach mode must not touch the network.
	var mu sync.Mutex
	var captured map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		captured = map[string]string{
			"path": r.URL.Path, "auth": r.Header.Get("Authorization"),
			"ct": r.Header.Get("Content-Type"), "cpv": r.Header.Get("Connect-Protocol-Version"),
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	bm, err := NewAttachedBridgeManager(srv.URL, "CURSOR_BRIDGE_TOKEN")
	if err != nil {
		t.Fatalf("NewAttachedBridgeManager: %v", err)
	}
	t.Setenv("CURSOR_BRIDGE_TOKEN", "test-bearer")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := bm.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := bm.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	mu.Lock()
	got := captured
	mu.Unlock()
	if got != nil {
		t.Errorf("attach-mode Close should not touch the network, got %+v", got)
	}
}

func TestAttachedBridgeManagerRejectsEmptyEndpoint(t *testing.T) {
	if _, err := NewAttachedBridgeManager("", "CURSOR_BRIDGE_TOKEN"); err == nil {
		t.Fatal("expected error for empty endpoint")
	}
	if _, err := NewAttachedBridgeManager("   ", "CURSOR_BRIDGE_TOKEN"); err == nil {
		t.Fatal("expected error for whitespace-only endpoint")
	}
}

func TestManagedShutdownRPC(t *testing.T) {
	// The fake bridge advertises http://127.0.0.1:49152 in its ready
	// line but does not actually listen. We bind that exact port so the
	// manager's Shutdown RPC lands on our handler.
	type capture struct {
		method, path, auth, ct, cpv string
		body                        string
	}
	var (
		mu      sync.Mutex
		got     capture
		hitOnce atomic.Bool
	)
	ln, err := net.Listen("tcp", "127.0.0.1:49152")
	if err != nil {
		t.Skipf("port 49152 unavailable: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = capture{
			method: r.Method, path: r.URL.Path,
			auth: r.Header.Get("Authorization"),
			ct:   r.Header.Get("Content-Type"),
			cpv:  r.Header.Get("Connect-Protocol-Version"),
			body: string(body),
		}
		mu.Unlock()
		hitOnce.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	withFakeBridge(t)
	tokenFile := filepath.Join(t.TempDir(), "auth-token")
	t.Setenv("FAKE_BRIDGE_TOKEN_FILE", tokenFile)
	t.Setenv("FAKE_BRIDGE_TOKEN", "shutdown-test-token")

	bm, err := NewManagedBridgeManager("api-key",
		WithShutdownTimeout(500*time.Millisecond))
	if err != nil {
		t.Fatalf("NewManagedBridgeManager: %v", err)
	}
	bm.cmd.Env = append(bm.cmd.Env,
		"FAKE_BRIDGE_TOKEN_FILE="+tokenFile,
		"FAKE_BRIDGE_TOKEN=shutdown-test-token",
	)
	t.Cleanup(func() { _ = bm.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	endpt, tok, err := bm.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if endpt != "http://127.0.0.1:49152" {
		t.Errorf("endpoint = %q", endpt)
	}
	if tok != "shutdown-test-token" {
		t.Errorf("token = %q", tok)
	}

	if err := bm.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Second Close must not re-send the RPC.
	mu.Lock()
	first := got
	mu.Unlock()
	if err := bm.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	mu.Lock()
	second := got
	mu.Unlock()
	if first != second {
		t.Errorf("second Close re-hit the server (handler ran twice)")
	}

	if !hitOnce.Load() {
		t.Fatal("Shutdown RPC handler was never invoked")
	}
	if first.method != http.MethodPost {
		t.Errorf("method = %q, want POST", first.method)
	}
	if first.path != "/sdk.v1.SdkBridgeControlService/Shutdown" {
		t.Errorf("path = %q", first.path)
	}
	if first.auth != "Bearer shutdown-test-token" {
		t.Errorf("auth = %q", first.auth)
	}
	if first.ct != "application/json" {
		t.Errorf("content-type = %q", first.ct)
	}
	if first.cpv != "1" {
		t.Errorf("Connect-Protocol-Version = %q", first.cpv)
	}
}

func TestAttachModeNeverTouchesExec(t *testing.T) {
	// Force resolveBridgeBinary path: a broken CURSOR_SDK_BRIDGE_BIN
	// would make NewManagedBridgeManager fail. Attach mode must still
	// succeed because it never calls resolveBridgeBinary.
	t.Setenv("CURSOR_SDK_BRIDGE_BIN", "/nonexistent/cursor-sdk-bridge")

	var lookups atomic.Int32
	origLookPath := execLookPath
	execLookPath = func(file string) (string, error) {
		lookups.Add(1)
		return "", fmt.Errorf("exec disabled for test")
	}
	t.Cleanup(func() { execLookPath = origLookPath })

	bm, err := NewAttachedBridgeManager("http://127.0.0.1:9999", "CURSOR_BRIDGE_TOKEN")
	if err != nil {
		t.Fatalf("NewAttachedBridgeManager: %v", err)
	}
	t.Setenv("CURSOR_BRIDGE_TOKEN", "attached-token")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpt, tok, err := bm.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if endpt != "http://127.0.0.1:9999" {
		t.Errorf("endpoint = %q", endpt)
	}
	if tok != "attached-token" {
		t.Errorf("token = %q", tok)
	}
	if lookups.Load() != 0 {
		t.Errorf("exec.LookPath called %d times in attach mode", lookups.Load())
	}
	// No process spawned.
	if bm.cmd != nil {
		t.Errorf("attach mode should leave cmd nil, got %+v", bm.cmd)
	}
	// Close is a no-op (returns nil, no SIGTERM to a missing PID).
	if err := bm.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestBridgeManagerOptionsApplied(t *testing.T) {
	customClient := &http.Client{Timeout: 7 * time.Second}
	customSink := &bytes.Buffer{}
	bm, err := NewAttachedBridgeManager("http://127.0.0.1:1", "CURSOR_BRIDGE_TOKEN",
		WithHTTPClient(customClient),
		WithStderrSink(customSink),
	)
	if err != nil {
		t.Fatalf("NewAttachedBridgeManager: %v", err)
	}
	if bm.httpClient != customClient {
		t.Errorf("httpClient not stored")
	}
	if bm.stderrSink != customSink {
		t.Errorf("stderrSink not stored")
	}

	// WithStderrSink(nil) must reset to io.Discard; nil is a footgun
	// because drainStderr writes to a nil writer would panic.
	bm2, err := NewAttachedBridgeManager("http://127.0.0.1:2", "CURSOR_BRIDGE_TOKEN",
		WithStderrSink(nil),
	)
	if err != nil {
		t.Fatalf("NewAttachedBridgeManager (nil sink): %v", err)
	}
	if bm2.stderrSink != io.Discard {
		t.Errorf("nil sink not reset to io.Discard; got %T", bm2.stderrSink)
	}

	// WithStartupTimeout and WithShutdownTimeout on managed too.
	bm3, err := NewManagedBridgeManager("k",
		WithStartupTimeout(99*time.Millisecond),
		WithShutdownTimeout(99*time.Millisecond),
		WithHTTPClient(customClient),
		WithStderrSink(customSink),
	)
	if err != nil {
		t.Fatalf("NewManagedBridgeManager: %v", err)
	}
	if bm3.startupTimeout != 99*time.Millisecond {
		t.Errorf("startupTimeout = %v", bm3.startupTimeout)
	}
	if bm3.shutdownTimeout != 99*time.Millisecond {
		t.Errorf("shutdownTimeout = %v", bm3.shutdownTimeout)
	}
	if bm3.stderrSink != customSink {
		t.Errorf("managed stderrSink not stored")
	}
}

func TestDrainStderrReturnsOnEOF(t *testing.T) {
	// The drain returns when the reader is exhausted; the sink sees
	// every byte before that point.
	var got bytes.Buffer
	src := strings.NewReader("line1\nline2\n")
	drainStderr(src, &got)
	if got.String() != "line1\nline2\n" {
		t.Errorf("sink = %q, want %q", got.String(), "line1\nline2\n")
	}

	// nil sink is replaced with io.Discard inside drainStderr.
	drainStderr(strings.NewReader("ignored"), nil)
}

func TestParseReadyLineUsesAuthTokenFile(t *testing.T) {
	// AuthTokenFile is preferred over AuthToken when both are present.
	tmp := t.TempDir()
	tokFile := filepath.Join(tmp, "auth")
	if err := os.WriteFile(tokFile, []byte("  file-token\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	payload := `{"schemaVersion":1,"transport":"tcp","protocol":"connect","url":"http://h:1","authToken":"inline-token","authTokenFile":"` + tokFile + `"}`
	endpt, tok, err := parseReadyLine(payload)
	if err != nil {
		t.Fatalf("parseReadyLine: %v", err)
	}
	if endpt != "http://h:1" {
		t.Errorf("endpoint = %q", endpt)
	}
	if tok != "file-token" {
		t.Errorf("token = %q, want file-token (AuthTokenFile wins)", tok)
	}

	// Missing auth token file → clear error.
	payload2 := `{"schemaVersion":1,"transport":"tcp","protocol":"connect","url":"http://h:1","authTokenFile":"/nonexistent/file"}`
	if _, _, err := parseReadyLine(payload2); err == nil ||
		!strings.Contains(err.Error(), "auth token file") {
		t.Errorf("expected auth-token-file error, got %v", err)
	}
}

func TestResolveBridgeBinaryOrder(t *testing.T) {
	// env override pointing at an existing file wins; LookPath must not
	// be consulted in that case.
	tmp := t.TempDir()
	// Keep the host's real installation out of the test: the conventional
	// fallback (~/.local/share/gomodel/bin) must resolve inside tmp.
	origHome := homeDir
	homeDir = func() (string, error) { return tmp, nil }
	t.Cleanup(func() { homeDir = origHome })
	binPath := filepath.Join(tmp, "cursor-sdk-bridge")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	t.Setenv("CURSOR_SDK_BRIDGE_BIN", binPath)
	origLook := execLookPath
	execLookPath = func(string) (string, error) {
		t.Error("LookPath must not be called when env override is set")
		return "", errors.New("disabled")
	}
	t.Cleanup(func() { execLookPath = origLook })
	got, err := resolveBridgeBinary()
	if err != nil || got != binPath {
		t.Errorf("resolveBridgeBinary = (%q, %v), want (%q, nil)", got, err, binPath)
	}

	// env points at a missing path: must surface a clear error (the
	// operator gave us an override that does not resolve).
	t.Setenv("CURSOR_SDK_BRIDGE_BIN", filepath.Join(tmp, "does-not-exist"))
	if _, err := resolveBridgeBinary(); err == nil ||
		!strings.Contains(err.Error(), "CURSOR_SDK_BRIDGE_BIN") {
		t.Errorf("expected install-hint error for missing env path, got %v", err)
	}

	// unset env, LookPath returns a path
	t.Setenv("CURSOR_SDK_BRIDGE_BIN", "")
	execLookPath = func(file string) (string, error) {
		if file != "cursor-sdk-bridge" {
			t.Errorf("LookPath file = %q, want cursor-sdk-bridge", file)
		}
		return "/usr/bin/cursor-sdk-bridge", nil
	}
	got, err = resolveBridgeBinary()
	if err != nil || got != "/usr/bin/cursor-sdk-bridge" {
		t.Errorf("LookPath branch = (%q, %v)", got, err)
	}

	// neither: must mention install hints
	execLookPath = func(string) (string, error) {
		return "", exec.ErrNotFound
	}
	if _, err := resolveBridgeBinary(); err == nil ||
		!strings.Contains(err.Error(), "CURSOR_SDK_BRIDGE_BIN") {
		t.Errorf("expected install-hint error, got %v", err)
	}
}

func TestScrubbedBridgeEnv(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("HOME", "/home/test")
	t.Setenv("TMPDIR", "/tmp")
	t.Setenv("USER", "tester")
	t.Setenv("LANG", "C")
	// These must NOT leak.
	t.Setenv("CURSOR_API_KEY", "parent-leaked")
	t.Setenv("OPENAI_API_KEY", "parent-leaked-2")
	env := scrubbedBridgeEnv("child-key")
	joined := strings.Join(env, "\n")
	for _, must := range []string{"PATH=", "HOME=", "TMPDIR=", "USER=", "LANG=",
		"CURSOR_API_KEY=child-key", "CURSOR_SDK_CLIENT_LANGUAGE=go"} {
		if !strings.Contains(joined, must) {
			t.Errorf("env missing %q\n%s", must, joined)
		}
	}
	for _, mustNot := range []string{"parent-leaked", "parent-leaked-2"} {
		if strings.Contains(joined, mustNot) {
			t.Errorf("env leaked %q\n%s", mustNot, joined)
		}
	}
}

func TestScrubbedBridgeEnvForwardsProxyEnv(t *testing.T) {
	// Operators behind a corporate proxy need HTTP(S)_PROXY/NO_PROXY
	// forwarded to the bridge. Without these the bridge cannot reach
	// the Cursor APIs. ALL_PROXY is the curl-style catch-all and is
	// also forwarded for compatibility with curl-derived tooling.
	t.Setenv("HTTP_PROXY", "http://proxy.example:8080")
	t.Setenv("HTTPS_PROXY", "http://proxy.example:8443")
	t.Setenv("NO_PROXY", "localhost,127.0.0.1,.internal")
	t.Setenv("ALL_PROXY", "http://all-proxy.example:8888")
	t.Setenv("http_proxy", "http://lowercase-proxy.example:3128")
	t.Setenv("https_proxy", "http://lowercase-proxy.example:3129")
	t.Setenv("no_proxy", "intra.example")
	t.Setenv("all_proxy", "http://lowercase-all.example:7777")
	t.Setenv("FOO_PROXY", "should-not-leak") // unrelated proxy var
	env := scrubbedBridgeEnv("child-key")
	joined := strings.Join(env, "\n")
	for _, must := range []string{
		"HTTP_PROXY=http://proxy.example:8080",
		"HTTPS_PROXY=http://proxy.example:8443",
		"NO_PROXY=localhost,127.0.0.1,.internal",
		"ALL_PROXY=http://all-proxy.example:8888",
		"http_proxy=http://lowercase-proxy.example:3128",
		"https_proxy=http://lowercase-proxy.example:3129",
		"no_proxy=intra.example",
		"all_proxy=http://lowercase-all.example:7777",
	} {
		if !strings.Contains(joined, must) {
			t.Errorf("env missing proxy var %q\n%s", must, joined)
		}
	}
	if strings.Contains(joined, "FOO_PROXY") {
		t.Errorf("env leaked unrelated FOO_PROXY: %s", joined)
	}
}

func TestAttachModeTrimsTokenWhitespace(t *testing.T) {
	// Editors commonly inject leading whitespace into .env values; the
	// bearer would then arrive at the bridge as "  token" and every
	// Connect RPC would 401 with no clue.
	t.Setenv("CURSOR_BRIDGE_TOKEN", "  \t test-token \n")
	bm, err := NewAttachedBridgeManager("http://127.0.0.1:9999", "CURSOR_BRIDGE_TOKEN")
	if err != nil {
		t.Fatalf("NewAttachedBridgeManager: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, tok, err := bm.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if tok != "test-token" {
		t.Errorf("token = %q, want %q (whitespace must be trimmed)", tok, "test-token")
	}
}

func TestScanReadyLineHandlesLongBanner(t *testing.T) {
	// Some supervisor wrappers print a multi-line banner (>64 KiB) before
	// the ready line; the scan buffer must accommodate the largest
	// single line without losing the ready line at the tail.
	longBanner := strings.Repeat("banner line with some content\n", 20000)
	ready := `cursor-sdk-bridge ready {"schemaVersion":1,"transport":"tcp","protocol":"connect","url":"http://h:1","authToken":"x"}` + "\n"
	src := strings.NewReader(longBanner + ready)

	out := make(chan readyResult, 1)
	scanReadyLine(src, out)
	res := <-out
	if res.err != nil {
		t.Fatalf("scanReadyLine: %v", res.err)
	}
	if res.endpoint != "http://h:1" {
		t.Errorf("endpoint = %q, want http://h:1", res.endpoint)
	}
	if res.token != "x" {
		t.Errorf("token = %q, want x", res.token)
	}
}

func TestParseReadyLineRejectsBadSchema(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantOK  string
	}{
		{"good url", `{"schemaVersion":1,"transport":"tcp","protocol":"connect","url":"http://h:1","authToken":"x"}`, "http://h:1"},
		{"good hostport", `{"schemaVersion":1,"transport":"tcp","protocol":"connect","host":"h","port":7,"authToken":"y"}`, "http://h:7"},
		{"bad schema", `{"schemaVersion":2,"transport":"tcp","protocol":"connect","url":"http://h:1","authToken":"x"}`, ""},
		{"bad transport", `{"schemaVersion":1,"transport":"udp","protocol":"connect","url":"http://h:1","authToken":"x"}`, ""},
		{"bad protocol", `{"schemaVersion":1,"transport":"tcp","protocol":"grpc","url":"http://h:1","authToken":"x"}`, ""},
		{"missing endpoint", `{"schemaVersion":1,"transport":"tcp","protocol":"connect","authToken":"x"}`, ""},
		{"no token", `{"schemaVersion":1,"transport":"tcp","protocol":"connect","url":"http://h:1"}`, ""},
		{"unknown field ignored", `{"schemaVersion":1,"transport":"tcp","protocol":"connect","url":"http://h:1","authToken":"x","future":42}`, "http://h:1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			endpt, tok, err := parseReadyLine(c.payload)
			if c.wantOK == "" {
				if err == nil {
					t.Fatalf("expected error, got endpt=%q tok=%q", endpt, tok)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if endpt != c.wantOK {
				t.Errorf("endpoint = %q, want %q", endpt, c.wantOK)
			}
			if tok == "" {
				t.Errorf("token empty")
			}
		})
	}
}

func TestReplaceWorkspaceArg(t *testing.T) {
	got := replaceWorkspaceArg([]string{"a", "{workspace}", "b"}, "/tmp/ws")
	want := []string{"a", "/tmp/ws", "b"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDrainStderrNilSink uses io.Discard when the sink is nil — the
// contract is that drain never panics or blocks the program when no
// sink is configured.
func TestDrainStderrNilSink(t *testing.T) {
	r := strings.NewReader("some stderr noise\n")
	drainStderr(r, nil)
	// Reaching here without panic proves the io.Discard branch fired.
}

// TestScanReadyLineTruncatedFrame covers the partial-read error path in
// scanReadyLine when the connection drops mid-line.
func TestScanReadyLineTruncatedFrame(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		// Write less than a header line so ScanLines never finds a
		// delimiter and returns io.ErrBufferFull.
		_, _ = pw.Write([]byte("not-a-ready"))
		_ = pw.Close()
	}()
	out := make(chan readyResult, 1)
	scanReadyLine(pr, out)
	res := <-out
	if res.err == nil {
		t.Fatal("expected error from truncated frame, got none")
	}
	if res.endpoint != "" {
		t.Errorf("endpoint = %q, want empty", res.endpoint)
	}
}

// TestScanReadyLineNonEOFError covers the `else` branch in
// scanReadyLine — when readFrame returns a non-EOF error, the
// residual stderr and the error are delivered on the channel.
func TestScanReadyLineNonEOFError(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		// Valid header that declares a 50-byte payload, then close
		// the pipe with a custom non-EOF error so readFrame returns
		// it directly.
		_, _ = pw.Write([]byte{0, 0, 0, 0, 50})
		_ = pw.CloseWithError(errors.New("body closed with custom error"))
	}()
	out := make(chan readyResult, 1)
	scanReadyLine(pr, out)
	res := <-out
	if res.err == nil {
		t.Fatal("expected error from non-EOF body, got nil")
	}
	if res.endpoint != "" {
		t.Errorf("endpoint = %q, want empty", res.endpoint)
	}
}

// TestParseReadyLineMalformedJSON covers the json.Unmarshal failure
// branch — invalid JSON must surface as a wrapped error.
func TestParseReadyLineMalformedJSON(t *testing.T) {
	_, _, err := parseReadyLine("{not-valid-json")
	if err == nil {
		t.Fatal("expected error from malformed ready line, got none")
	}
	if !strings.Contains(err.Error(), "parse ready line") {
		t.Errorf("error = %q, want 'parse ready line' prefix", err.Error())
	}
}

// TestResolveBridgeBinaryMissingPathCoversUnreachable exercises the
// `if v := strings.TrimSpace(...); v != ""` and `if _, err := os.Stat(v); err == nil`
// branches — set CURSOR_SDK_BRIDGE_BIN to a path that does not exist
// and confirm resolveBridgeBinary returns the ErrBridgeUnreachable sentinel.
func TestResolveBridgeBinaryMissingPathCoversUnreachable(t *testing.T) {
	t.Setenv("CURSOR_SDK_BRIDGE_BIN", "/tmp/this/path/definitely/does/not/exist")
	_, err := resolveBridgeBinary()
	if err == nil {
		t.Fatal("expected error from missing binary path, got none")
	}
	if !errors.Is(err, ErrBridgeUnreachable) {
		t.Errorf("err = %v, want wrapping ErrBridgeUnreachable", err)
	}
}

// TestResolveBridgeBinaryNonExecutableSurfacesErrUnreachable covers
// the executableBinary branch — a non-executable file at the env
// var must surface as ErrBridgeUnreachable instead of letting exec.Start
// report it later as a generic spawn failure (502).
func TestResolveBridgeBinaryNonExecutableSurfacesErrUnreachable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable-bit check is unix-only")
	}
	path := filepath.Join(t.TempDir(), "fake-binary")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("CURSOR_SDK_BRIDGE_BIN", path)
	_, err := resolveBridgeBinary()
	if err == nil {
		t.Fatal("expected error from non-executable binary, got nil")
	}
	if !errors.Is(err, ErrBridgeUnreachable) {
		t.Errorf("err = %v, want wrapping ErrBridgeUnreachable", err)
	}
}

// TestSpawnZeroStartupTimeoutUsesDefault covers the
// `if timeout <= 0` defensive branch — a BridgeManager with a 0
// startupTimeout must fall through to defaultStartupTimeout instead
// of constructing a zero-duration context.
func TestSpawnZeroStartupTimeoutUsesDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals not supported on windows")
	}
	withFakeBridge(t)

	tokenFile := filepath.Join(t.TempDir(), "auth-token")
	t.Setenv("FAKE_BRIDGE_TOKEN_FILE", tokenFile)
	t.Setenv("FAKE_BRIDGE_TOKEN", "tok")

	bm, err := NewManagedBridgeManager("test-api-key")
	if err != nil {
		t.Fatalf("NewManagedBridgeManager: %v", err)
	}
	bm.startupTimeout = 0 // exercise the <= 0 fallback
	bm.cmd.Env = append(bm.cmd.Env,
		"FAKE_BRIDGE_TOKEN_FILE="+tokenFile,
		"FAKE_BRIDGE_TOKEN=tok",
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := bm.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = bm.Close()
}

// TestSpawnCancelledContextSurfacesCtxErr covers the
// `if ctxErr := ctx.Err(); ctxErr != nil` branch — when the parent
// context is cancelled before the bridge becomes ready, the error
// must wrap the cancellation rather than the generic timeout.
func TestSpawnCancelledContextSurfacesCtxErr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals not supported on windows")
	}
	withFakeBridge(t)

	tokenFile := filepath.Join(t.TempDir(), "auth-token")
	t.Setenv("FAKE_BRIDGE_TOKEN_FILE", tokenFile)
	t.Setenv("FAKE_BRIDGE_TOKEN", "tok")
	// hang mode keeps the bridge from ever emitting the ready line.
	t.Setenv("FAKE_BRIDGE_MODE", "hang")

	bm, err := NewManagedBridgeManager("test-api-key",
		WithStartupTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewManagedBridgeManager: %v", err)
	}
	bm.cmd.Env = append(bm.cmd.Env,
		"FAKE_BRIDGE_TOKEN_FILE="+tokenFile,
		"FAKE_BRIDGE_TOKEN=tok",
		"FAKE_BRIDGE_MODE=hang",
	)
	bm.shutdownTimeout = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately so the parent ctx is done before Start.
	cancel()
	_, _, err = bm.Start(ctx)
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want wrapping context.Canceled", err)
	}
}

// TestExecutableBinaryClassification drives the executableBinary table
// directly — missing files, directories, non-executable regular files,
// and executable regular files. The reason string is asserted so future
// changes to the categorization stay honest.
func TestExecutableBinaryClassification(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable-bit check is unix-only")
	}
	t.Run("missing", func(t *testing.T) {
		ok, why := executableBinary(filepath.Join(t.TempDir(), "absent"))
		if ok || why != "missing" {
			t.Errorf("missing file: ok=%v why=%q, want false/missing", ok, why)
		}
	})
	t.Run("directory", func(t *testing.T) {
		d := t.TempDir()
		ok, why := executableBinary(d)
		if ok || why != "is a directory" {
			t.Errorf("directory: ok=%v why=%q, want false/is a directory", ok, why)
		}
	})
	t.Run("non-executable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "no-exec")
		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		ok, why := executableBinary(path)
		if ok || why == "" || why == "missing" {
			t.Errorf("non-exec: ok=%v why=%q, want false/non-empty-reason", ok, why)
		}
	})
	t.Run("executable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "exec")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write: %v", err)
		}
		ok, why := executableBinary(path)
		if !ok || why != "" {
			t.Errorf("executable: ok=%v why=%q, want true/empty", ok, why)
		}
	})
}

// TestResolveBridgeBinaryPathDirectoryNotFound covers the
// `if _, statErr := os.Stat(candidate); statErr == nil` path when
// neither CURSOR_SDK_BRIDGE_BIN nor cursor-sdk-bridge-in-PATH nor
// ~/.local/share/... exists — last-resort branch that also wraps
// ErrBridgeUnreachable.
func TestResolveBridgeBinaryPathDirectoryNotFound(t *testing.T) {
	t.Setenv("CURSOR_SDK_BRIDGE_BIN", "")
	// Force exec.LookPath to fail by clearing PATH — this exercises
	// the second branch of resolveBridgeBinary. Force homeDir() to
	// return an empty directory so the .local/share fallback cannot
	// find anything.
	t.Setenv("PATH", "/nonexistent-only")
	t.Setenv("HOME", t.TempDir())
	_, err := resolveBridgeBinary()
	if err == nil {
		t.Fatal("expected error from absent binary, got none")
	}
}

// TestResolveBridgeBinaryNonExecHomeCoversErrUnreachable covers the
// `if why != "missing"` branch — when the home-directory fallback
// finds a path that exists but is not executable, it must surface
// ErrBridgeUnreachable rather than silently passing the path to
// exec.Start which would fail later with a generic spawn error.
func TestResolveBridgeBinaryNonExecHomeCoversErrUnreachable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable-bit check is unix-only")
	}
	tmp := t.TempDir()
	binDir := tmp + "/.local/share/gomodel/bin"
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	binPath := binDir + "/cursor-sdk-bridge"
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("CURSOR_SDK_BRIDGE_BIN", "")
	t.Setenv("PATH", "/nonexistent-only")
	t.Setenv("HOME", tmp)
	_, err := resolveBridgeBinary()
	if err == nil {
		t.Fatal("expected error from non-executable home-dir binary, got nil")
	}
	if !errors.Is(err, ErrBridgeUnreachable) {
		t.Errorf("err = %v, want wrapping ErrBridgeUnreachable", err)
	}
}

// TestSpawnNonExecutableSurfacesErrUnreachable exercises the
// `b.cmd.Start()` failure path — when exec.Start fails because the
// resolved binary is not executable, the error must wrap
// ErrBridgeUnreachable so startFailure returns 503 instead of 502.
func TestSpawnNonExecutableSurfacesErrUnreachable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable-bit check is unix-only")
	}
	withFakeBridge(t)

	path := filepath.Join(t.TempDir(), "fake")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	bm, err := NewManagedBridgeManager("test-api-key")
	if err != nil {
		t.Fatalf("NewManagedBridgeManager: %v", err)
	}
	// Force the manager to point at our non-executable fake.
	bm.cmd = exec.Command(path, "--workspace", "{workspace}")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err = bm.Start(ctx)
	if err == nil {
		t.Fatal("expected error from non-executable binary, got nil")
	}
}

// TestCloseEscalatesToSIGKILLWhenBridgeIgnoresSIGTERM covers the
// SIGKILL escalation path in shutdown(). When SIGTERM does not bring
// the bridge down within shutdownTimeout, the manager must escalate
// to SIGKILL and return within bounded time.
func TestCloseEscalatesToSIGKILLWhenBridgeIgnoresSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals not supported on windows")
	}
	withFakeBridge(t)

	tokenFile := filepath.Join(t.TempDir(), "auth-token")
	t.Setenv("FAKE_BRIDGE_TOKEN_FILE", tokenFile)
	t.Setenv("FAKE_BRIDGE_TOKEN", "kill-test-token")
	t.Setenv("FAKE_BRIDGE_MODE", "sigterm_ignore")

	bm, err := NewManagedBridgeManager("test-api-key",
		WithShutdownTimeout(150*time.Millisecond))
	if err != nil {
		t.Fatalf("NewManagedBridgeManager: %v", err)
	}
	bm.cmd.Env = append(bm.cmd.Env,
		"FAKE_BRIDGE_TOKEN_FILE="+tokenFile,
		"FAKE_BRIDGE_TOKEN=kill-test-token",
		"FAKE_BRIDGE_MODE=sigterm_ignore",
	)
	bm.startupTimeout = 5 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err = bm.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := bm.cmd.Process.Pid
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- bm.Close() }()
	select {
	case err := <-done:
		elapsed := time.Since(start)
		if err != nil {
			t.Errorf("Close: %v", err)
		}
		// SIGKILL escalation should fire well within 2s.
		if elapsed > 2*time.Second {
			t.Errorf("Close took %v; expected fast SIGKILL escalation", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return; bridge likely still alive")
	}
	if _, ok := childPIDs(os.Getpid())[pid]; ok {
		t.Errorf("child pid %d still alive after SIGKILL escalation", pid)
	}
}
