// Package cursor wires GoModel's OpenAI-compatible surface to a local
// cursor-sdk-bridge subprocess. The bridge implements the versioned
// `sdk.v1` Connect contract over loopback HTTP/1.1; this file owns the
// process lifecycle (spawn, ready-line handshake, stderr drain, shutdown).
package cursor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/goccy/go-json"
)

// readyLinePrefix is the literal stderr prefix the bridge writes once it
// is listening. The trailing space is significant — the discovery JSON
// follows immediately. Defined by cursor/sdk-bridge docs/protocol.md.
const readyLinePrefix = "cursor-sdk-bridge ready "

// bridgeControlShutdown is the Connect RPC path the bridge's
// SdkBridgeControlService exposes for graceful termination.
const bridgeControlShutdown = "/sdk.v1.SdkBridgeControlService/Shutdown"

// defaultStartupTimeout bounds how long Start waits for the ready line.
const defaultStartupTimeout = 30 * time.Second

// shutdownGrace lets the bridge drain in-flight RPCs before SIGTERM.
const shutdownGrace = 5 * time.Second

// execLookPath is indirection to keep tests free of side-effects.
var execLookPath = exec.LookPath

// homeDir is indirection for the default-binary fallback path.
var homeDir = os.UserHomeDir

// BridgeManager owns the cursor-sdk-bridge subprocess. It is safe to call
// Start concurrently; the first call wins, later calls return the same
// endpoint/token. Close is idempotent and may be called from a defer.
type BridgeManager struct {
	// endpoint is the attach-mode base URL. When non-empty, Start does
	// not spawn a process; it returns (endpoint, CURSOR_BRIDGE_TOKEN).
	endpoint string
	// tokenEnv is the env var name to read the bearer from in attach mode.
	tokenEnv string
	// apiKey is forwarded to the spawned child as CURSOR_API_KEY.
	apiKey string
	// startupTimeout overrides defaultStartupTimeout in tests.
	startupTimeout time.Duration
	// shutdownTimeout overrides shutdownGrace in tests.
	shutdownTimeout time.Duration
	// httpClient is used for the Shutdown RPC. nil == http.DefaultClient.
	httpClient *http.Client
	// stderrSink receives bridge stderr once it is ready. Defaults to
	// io.Discard so a full pipe can never block the bridge.
	stderrSink io.Writer

	mu      sync.Mutex
	cmd     *exec.Cmd
	started bool
	closed  bool
	endpt   string
	tok     string
	// workspaceDir is the MkdirTemp workspace passed to --workspace.
	// Removed on Close (best-effort).
	workspaceDir string
}

// BridgeManagerOption configures a BridgeManager.
type BridgeManagerOption func(*BridgeManager)

// WithStartupTimeout overrides the default 30s startup timeout. Tests
// use a short timeout to cover the timeout-fires path.
func WithStartupTimeout(d time.Duration) BridgeManagerOption {
	return func(b *BridgeManager) { b.startupTimeout = d }
}

// WithShutdownTimeout overrides the 5s graceful-stop window used by Close.
func WithShutdownTimeout(d time.Duration) BridgeManagerOption {
	return func(b *BridgeManager) { b.shutdownTimeout = d }
}

// WithHTTPClient overrides the http.Client used for the Shutdown RPC.
func WithHTTPClient(hc *http.Client) BridgeManagerOption {
	return func(b *BridgeManager) { b.httpClient = hc }
}

// WithStderrSink routes the bridge's stderr after the ready line (the
// ready line itself is never forwarded). Defaults to io.Discard.
func WithStderrSink(w io.Writer) BridgeManagerOption {
	return func(b *BridgeManager) { b.stderrSink = w }
}

// NewManagedBridgeManager creates a BridgeManager that spawns the bridge
// subprocess on first Start. Resolve order for the binary: env
// CURSOR_SDK_BRIDGE_BIN, then exec.LookPath, then
// ~/.local/share/gomodel/bin/cursor-sdk-bridge. The apiKey is forwarded
// to the child as CURSOR_API_KEY.
func NewManagedBridgeManager(apiKey string, opts ...BridgeManagerOption) (*BridgeManager, error) {
	bin, err := resolveBridgeBinary()
	if err != nil {
		return nil, err
	}
	b := &BridgeManager{
		apiKey:          apiKey,
		startupTimeout:  defaultStartupTimeout,
		shutdownTimeout: shutdownGrace,
		stderrSink:      io.Discard,
	}
	for _, opt := range opts {
		opt(b)
	}
	b.cmd = exec.Command(bin, "--workspace", "{workspace}")
	b.cmd.Env = scrubbedBridgeEnv(apiKey)
	// The actual workspace directory is filled in by Start; the placeholder
	// keeps the field reference valid even if Start is never called.
	return b, nil
}

// NewAttachedBridgeManager creates a BridgeManager in attach mode: no
// subprocess is spawned. Start returns (endpoint, CURSOR_BRIDGE_TOKEN).
// The endpoint must be a valid base URL (non-empty); Close is a no-op.
func NewAttachedBridgeManager(endpoint, tokenEnv string, opts ...BridgeManagerOption) (*BridgeManager, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, errors.New("attach mode requires a non-empty endpoint URL")
	}
	b := &BridgeManager{
		endpoint:        endpoint,
		tokenEnv:        tokenEnv,
		startupTimeout:  defaultStartupTimeout,
		shutdownTimeout: shutdownGrace,
		stderrSink:      io.Discard,
	}
	for _, opt := range opts {
		opt(b)
	}
	if b.stderrSink == nil {
		b.stderrSink = io.Discard
	}
	return b, nil
}

// Start starts the bridge (or returns the attached endpoint) and returns
// the endpoint URL and the bearer token to use on every RPC. It is safe
// to call Start multiple times; subsequent calls return the cached pair.
// Start is single-attempt: a failed spawn leaves b.cmd in a partial state
// (placeholder args replaced, child already killed); create a fresh
// BridgeManager to retry.
func (b *BridgeManager) Start(ctx context.Context) (string, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.started {
		return b.endpt, b.tok, nil
	}
	if b.endpoint != "" {
		b.endpt = b.endpoint
		b.tok = os.Getenv(b.tokenEnv)
		b.started = true
		return b.endpt, b.tok, nil
	}
	endpt, tok, err := b.spawn(ctx)
	if err != nil {
		return "", "", err
	}
	b.endpt = endpt
	b.tok = tok
	b.started = true
	return b.endpt, b.tok, nil
}

// Close implements io.Closer. In attach mode it is a no-op (we do not own
// the process). For a managed bridge it asks the bridge to shut down
// gracefully, escalates to SIGTERM, then SIGKILL, and removes the
// workspace dir. Close is idempotent; repeat calls are no-ops.
func (b *BridgeManager) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.endpoint != "" || b.cmd == nil || b.cmd.Process == nil {
		return nil
	}
	b.closed = true
	err := b.shutdown()
	if b.workspaceDir != "" {
		_ = os.RemoveAll(b.workspaceDir)
		b.workspaceDir = ""
	}
	return err
}

// shutdown performs the graceful→SIGTERM→SIGKILL sequence. Caller must
// hold b.mu.
func (b *BridgeManager) shutdown() error {
	timeout := b.shutdownTimeout
	if b.endpt != "" && b.tok != "" {
		// Best-effort graceful Shutdown RPC. We do not fail Close on
		// network errors — SIGTERM is the authoritative fallback.
		req, err := http.NewRequest(http.MethodPost, b.endpt+bridgeControlShutdown, bytes.NewReader([]byte("{}")))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+b.tok)
			req.Header.Set("Connect-Protocol-Version", "1")
			client := b.httpClient
			if client == nil {
				client = http.DefaultClient
			}
			shutCtx, cancel := context.WithTimeout(context.Background(), timeout)
			_, _ = client.Do(req.WithContext(shutCtx))
			cancel()
		}
	}
	// Wait briefly for the bridge to exit on its own.
	done := make(chan struct{})
	go func() { _ = b.cmd.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
	}
	// SIGTERM, then wait again.
	_ = b.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
	}
	// SIGKILL — last resort.
	_ = b.cmd.Process.Kill()
	<-done
	return nil
}

// spawn creates the workspace, starts the child, waits for the ready
// line, and reads the bearer token. Caller must hold b.mu.
func (b *BridgeManager) spawn(ctx context.Context) (string, string, error) {
	workspace, err := os.MkdirTemp("", "cursor-sdk-bridge-")
	if err != nil {
		return "", "", fmt.Errorf("create bridge workspace: %w", err)
	}
	// Replace the placeholder the ctor stashed in Args. The path is
	// computed on Start so a stale path can never be reused.
	b.cmd.Args = replaceWorkspaceArg(b.cmd.Args, workspace)

	stderr, err := b.cmd.StderrPipe()
	if err != nil {
		_ = os.RemoveAll(workspace)
		return "", "", fmt.Errorf("bridge stderr pipe: %w", err)
	}
	if err := b.cmd.Start(); err != nil {
		_ = os.RemoveAll(workspace)
		return "", "", fmt.Errorf("start bridge: %w", err)
	}

	timeout := b.startupTimeout
	if timeout <= 0 {
		timeout = defaultStartupTimeout
	}
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	readyCh := make(chan readyResult, 1)
	go scanReadyLine(stderr, readyCh)

	var result readyResult
	select {
	case result = <-readyCh:
	case <-readyCtx.Done():
		// Bridge did not become ready in time. Make sure we do not leak
		// a runaway child; wait briefly so the post-kill stderr drain
		// does not race with our error message.
		_ = b.cmd.Process.Kill()
		_, _ = b.cmd.Process.Wait()
		_ = os.RemoveAll(workspace)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", "", fmt.Errorf("start bridge: %w", ctxErr)
		}
		return "", "", fmt.Errorf("start bridge: timeout after %s waiting for ready line", timeout)
	}
	if result.err != nil {
		// The bridge exited before emitting the ready line. Surface its
		// captured stderr so operators can diagnose without rerunning.
		_, _ = b.cmd.Process.Wait()
		_ = os.RemoveAll(workspace)
		return "", "", fmt.Errorf("start bridge: %v: %s", result.err, strings.TrimSpace(result.stderr))
	}
	// Drain stderr forever after ready so a full pipe never blocks the
	// bridge. The raw ready line is never written to the sink.
	go drainStderr(result.follow, b.stderrSink)
	b.workspaceDir = workspace
	return result.endpoint, result.token, nil
}

// readyResult carries the handshake outcome plus the residual stderr
// reader so the caller can keep draining it after success.
type readyResult struct {
	endpoint string
	token    string
	stderr   string
	follow   io.Reader
	err      error
}

// resolveBridgeBinary implements the documented search order.
func resolveBridgeBinary() (string, error) {
	if v := strings.TrimSpace(os.Getenv("CURSOR_SDK_BRIDGE_BIN")); v != "" {
		if _, err := os.Stat(v); err == nil {
			return v, nil
		}
		return "", fmt.Errorf("CURSOR_SDK_BRIDGE_BIN=%q does not exist", v)
	}
	if path, err := execLookPath("cursor-sdk-bridge"); err == nil {
		return path, nil
	}
	home, err := homeDir()
	if err == nil {
		candidate := filepath.Join(home, ".local", "share", "gomodel", "bin", "cursor-sdk-bridge")
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, nil
		}
	}
	return "", errors.New("cursor-sdk-bridge not found: set CURSOR_SDK_BRIDGE_BIN, " +
		"add cursor-sdk-bridge to PATH, or install it under " +
		"~/.local/share/gomodel/bin/cursor-sdk-bridge")
}

// scrubbedBridgeEnv returns the minimal env passed to the bridge child.
// The gateway process holds every provider API key and the master key,
// so none of that may cross the bridge boundary. Mirror
// internal/mcpgateway/upstream.go:180-196.
func scrubbedBridgeEnv(apiKey string) []string {
	env := []string{}
	keep := []string{"PATH", "HOME", "TMPDIR", "USER", "LANG"}
	for _, key := range keep {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	if apiKey != "" {
		env = append(env, "CURSOR_API_KEY="+apiKey)
	}
	env = append(env, "CURSOR_SDK_CLIENT_LANGUAGE=go")
	return env
}

// replaceWorkspaceArg returns args with the "{workspace}" placeholder
// replaced by dir. Errors are reported as a mutated slice rather than a
// return value to keep the call site small.
func replaceWorkspaceArg(args []string, dir string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, a := range out {
		if a == "{workspace}" {
			out[i] = dir
		}
	}
	return out
}

// scanReadyLine reads stderr line-by-line until it sees the ready-line
// prefix or the child closes the pipe. Exactly one result is delivered.
// The follow reader is the same bufio.Reader used for scanning, so bytes
// already buffered past the ready line are handed to the drain intact.
func scanReadyLine(r io.Reader, out chan<- readyResult) {
	br := bufio.NewReaderSize(r, 64*1024)
	var leftover strings.Builder
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\n")
		if payload, ok := strings.CutPrefix(line, readyLinePrefix); ok {
			endpt, tok, parseErr := parseReadyLine(payload)
			out <- readyResult{endpoint: endpt, token: tok, follow: br, err: parseErr}
			return
		}
		if line != "" {
			leftover.WriteString(line)
			leftover.WriteByte('\n')
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				out <- readyResult{stderr: leftover.String(), err: errors.New("bridge exited before ready line")}
			} else {
				out <- readyResult{stderr: leftover.String(), err: err}
			}
			return
		}
	}
}

// drainStderr forwards everything after the ready line to sink. It
// returns when the bridge closes stderr.
func drainStderr(r io.Reader, sink io.Writer) {
	if sink == nil {
		sink = io.Discard
	}
	if _, err := io.Copy(sink, r); err != nil {
		// Swallow: the bridge is shutting down or the pipe is closing.
		_ = err
	}
}

// readyLine mirrors the discovery JSON the bridge writes to stderr.
// Field names are pinned to docs/protocol.md (cursor/sdk-bridge); unknown
// fields are ignored. If a future bridge version renames a field, this
// is the one place to fix it.
type readyLine struct {
	SchemaVersion int    `json:"schemaVersion"`
	ServerVersion string `json:"serverVersion"`
	Transport     string `json:"transport"`
	Protocol      string `json:"protocol"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	URL           string `json:"url"`
	AuthTokenFile string `json:"authTokenFile"`
	// AuthToken is the legacy fallback: older bridges inlined the bearer
	// in the ready line. We prefer AuthTokenFile when present.
	AuthToken string `json:"authToken,omitempty"`
}

// parseReadyLine validates the discovery JSON and returns the endpoint
// URL and bearer token. The endpoint is the bridge's url field, or
// "http://host:port" if url is missing.
func parseReadyLine(payload string) (string, string, error) {
	var r readyLine
	if err := json.Unmarshal([]byte(payload), &r); err != nil {
		return "", "", fmt.Errorf("parse ready line: %w", err)
	}
	if r.SchemaVersion != 1 {
		return "", "", fmt.Errorf("unsupported ready-line schemaVersion %d (want 1)", r.SchemaVersion)
	}
	if r.Transport != "tcp" {
		return "", "", fmt.Errorf("unsupported transport %q (want tcp)", r.Transport)
	}
	if r.Protocol != "connect" {
		return "", "", fmt.Errorf("unsupported protocol %q (want connect)", r.Protocol)
	}
	endpt := r.URL
	if endpt == "" {
		if r.Host == "" || r.Port == 0 {
			return "", "", fmt.Errorf("ready line missing endpoint (need url or host+port)")
		}
		u := url.URL{Scheme: "http", Host: net.JoinHostPort(r.Host, fmt.Sprintf("%d", r.Port))}
		endpt = u.String()
	}
	tok := r.AuthToken
	if r.AuthTokenFile != "" {
		buf, err := os.ReadFile(r.AuthTokenFile)
		if err != nil {
			return "", "", fmt.Errorf("read auth token file %q: %w", r.AuthTokenFile, err)
		}
		tok = strings.TrimSpace(string(buf))
	}
	if tok == "" {
		return "", "", errors.New("ready line: no bearer token (set authTokenFile or legacy authToken)")
	}
	return endpt, tok, nil
}
