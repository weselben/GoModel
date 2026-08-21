// Package cursor hosts the GoModel provider type that lets a user's Cursor
// subscription serve inference through the official cursor-sdk-bridge
// subprocess (Connect-over-HTTP/1.1, JSON encoding).
//
// This file implements the wire-level transport only: a hand-rolled
// Connect-over-HTTP/1.1 client with JSON encoding, built on top of
// llmclient.Client. It is consumed by the provider core in a sibling file.
//
// Why hand-rolled: the bridge speaks HTTP/1.1 only and the Connect wire
// format for JSON is small enough to implement without protobuf codegen or
// the connectrpc.com/connect runtime. We ride llmclient.Client for retries,
// circuit breaking, and observability hooks: unary calls go through DoRaw,
// whose error path (core.ParseProviderError) already extracts Connect's
// top-level "code"/"message" error envelope and retries 429/502/503/504;
// streaming calls go through DoStream, with the Connect envelope framing
// (1 byte flags + 4 byte big-endian length + payload) handled here.
package cursor

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-json"

	"github.com/enterpilot/gomodel/internal/core"
	"github.com/enterpilot/gomodel/internal/llmclient"
)

const (
	connectProtocolVersion   = "1"
	connectContentTypeUnary  = "application/json"
	connectContentTypeStream = "application/connect+json"

	// Frame flag bits (RFC: connectrpc.com — Connect over HTTP/1.1 wire format).
	frameFlagCompressed  byte = 0x01
	frameFlagEndOfStream byte = 0x02

	// maxUnaryBodyBytes caps a successful unary response body. Bridge
	// unary responses are small JSON objects; this bound exists only to
	// keep a misbehaving upstream from buffering us into the ground.
	maxUnaryBodyBytes = 1 << 20

	// maxStreamFrameBytes caps a single streaming envelope frame. Bridge
	// streaming payloads can be large — multi-MB assistant texts, tool
	// output blobs — so the streaming cap is intentionally an order of
	// magnitude larger than the unary cap.
	maxStreamFrameBytes = 8 << 20
)

// Transport issues Connect RPCs against a cursor-sdk-bridge endpoint.
// It is safe for concurrent use.
type Transport struct {
	client *llmclient.Client
}

// NewTransport returns a Transport that talks to the bridge at baseURL,
// authenticating with bearer token. Pass a nil httpClient to use
// llmclient's default; tests inject a client that targets httptest.Server.
//
// The bearer token is captured in the headerSetter closure; if it
// contains CR/LF Go's http package rejects the request at write time
// with a confusing error. Guard the constructor: strip CR/LF and log
// a warning so the operator sees the issue during boot, not deep
// inside a request.
func NewTransport(httpClient *http.Client, baseURL, token string) *Transport {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if strings.ContainsAny(token, "\r\n") {
		slog.Warn("cursor: bearer token contained CR/LF; stripping before use — set CURSOR_BRIDGE_TOKEN to a clean value")
		token = strings.NewReplacer("\r", "", "\n", "").Replace(token)
	}
	client := llmclient.NewWithHTTPClient(
		httpClient,
		llmclient.DefaultConfig("cursor", baseURL),
		func(req *http.Request) {
			req.Header.Set("Connect-Protocol-Version", connectProtocolVersion)
			// The bearer token is captured in this closure and never
			// surfaces through logs — this headerSetter is the single place
			// that touches it. Do not move it elsewhere without an explicit
			// scrub step.
			req.Header.Set("Authorization", "Bearer "+token)
		},
	)
	return &Transport{client: client}
}

func connectEndpoint(service, method string) string {
	return fmt.Sprintf("/sdk.v1.%s/%s", service, method)
}

// Unary calls a Connect unary RPC and unmarshals the response into resp.
// Non-2xx responses are mapped to a *core.GatewayError by
// core.ParseProviderError, which already understands Connect's
// {"code","message"} error envelope and preserves the Connect "code" on the
// returned error.
func (t *Transport) Unary(ctx context.Context, service, method string, req, resp any) error {
	var body []byte
	if req != nil {
		b, err := json.Marshal(req)
		if err != nil {
			return core.NewInvalidRequestError("cursor: marshal unary request: "+err.Error(), err)
		}
		body = b
	}

	httpResp, err := t.client.DoRaw(ctx, llmclient.Request{
		Method:   http.MethodPost,
		Endpoint: connectEndpoint(service, method),
		RawBody:  body,
		Headers: http.Header{
			"Content-Type": {connectContentTypeUnary},
		},
	})
	if err != nil {
		return err
	}

	// Reject oversized successful bodies with a clear error rather than
	// letting the subsequent unmarshal fail with a confusing syntax
	// complaint.
	if len(httpResp.Body) > maxUnaryBodyBytes {
		return core.NewProviderError("cursor", http.StatusBadGateway,
			fmt.Sprintf("cursor: unary response exceeds %d bytes", maxUnaryBodyBytes), nil)
	}

	if resp != nil {
		if err := json.Unmarshal(httpResp.Body, resp); err != nil {
			return core.NewProviderError("cursor", http.StatusBadGateway, "cursor: unmarshal unary response: "+err.Error(), err)
		}
	}
	return nil
}

// Stream calls a Connect server-streaming RPC and returns a reader over the
// envelope frames. The request body is sent as exactly one envelope frame
// (1 byte flags=0x00 + 4 bytes big-endian length + JSON payload), per the
// Connect wire format; conforming servers parse streaming request bodies as
// envelope frames. Each Next returns one data payload as json.RawMessage,
// skipping empty and "{}" keepalive frames. The terminal end-of-stream
// frame yields io.EOF (clean) or a typed error parsed from its JSON
// payload.
func (t *Transport) Stream(ctx context.Context, service, method string, req any) (*StreamReader, error) {
	frame, err := marshalStreamRequest(req)
	if err != nil {
		return nil, err
	}

	httpResp, err := t.client.DoStream(ctx, llmclient.Request{
		Method:   http.MethodPost,
		Endpoint: connectEndpoint(service, method),
		RawBody:  frame,
		Headers: http.Header{
			"Content-Type": {connectContentTypeStream},
		},
	})
	if err != nil {
		return nil, err
	}
	return newStreamReader(httpResp), nil
}

// marshalStreamRequest returns the framed request body for a streaming RPC:
// the JSON payload (nil req becomes "{}", the zero-value JSON message)
// wrapped in one Connect envelope frame.
func marshalStreamRequest(req any) ([]byte, error) {
	payload := []byte("{}")
	if req != nil {
		b, err := json.Marshal(req)
		if err != nil {
			return nil, core.NewInvalidRequestError("cursor: marshal stream request: "+err.Error(), err)
		}
		payload = b
	}
	return encodeRequestFrame(payload)
}

// encodeRequestFrame wraps a streaming request payload in one Connect
// envelope frame: 1 byte flags (always 0x00 — we never send compressed) +
// 4 bytes big-endian length + payload.
func encodeRequestFrame(payload []byte) ([]byte, error) {
	if len(payload) > 0xFFFFFFFF {
		// Marshal of a caller request never legitimately reaches 4 GiB, but
		// fail loudly rather than silently truncating the frame length.
		return nil, core.NewInvalidRequestError(
			fmt.Sprintf("cursor: stream request payload %d bytes exceeds 4 GiB frame limit", len(payload)), nil)
	}
	buf := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	return buf, nil
}

// StreamReader yields one envelope frame payload at a time. The end-of-stream
// frame is consumed exactly once and surfaces as io.EOF (clean) or as a typed
// error parsed from its payload.
type StreamReader struct {
	body io.ReadCloser
	done bool
}

// newStreamReader wraps an already-open response body. The caller must Close
// the returned StreamReader when finished.
func newStreamReader(body io.ReadCloser) *StreamReader {
	return &StreamReader{body: body}
}

// Next returns the next envelope payload. It returns io.EOF on a clean
// end-of-stream frame, or a typed error parsed from an error-bearing end
// frame. The ctx parameter is honoured: a cancellation closes the body
// so an in-progress block read returns immediately.
func (r *StreamReader) Next(ctx context.Context) (json.RawMessage, error) {
	if r.done {
		// We already consumed the terminal frame on a previous call; never
		// hand it back twice.
		return nil, io.EOF
	}
	// Wire ctx into the body so a caller cancel unblocks a stalled read.
	// AfterFunc is no-op when ctx is already cancelled or done; using it
	// keeps the happy path cheap (no extra goroutine unless we are
	// actively blocking on ctx.Done).
	stop := context.AfterFunc(ctx, func() {
		_ = r.Close()
	})
	defer stop()
	for {
		flags, payload, err := readFrame(r.body)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Server closed the body without an explicit end frame —
				// treat as a clean stream end. Any end-frame error has
				// already been surfaced on the call that consumed it.
				return nil, io.EOF
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, err
		}
		if flags&frameFlagCompressed != 0 {
			return nil, &UnsupportedError{Reason: "cursor: compressed Connect frames are not supported"}
		}
		if flags&frameFlagEndOfStream != 0 {
			r.done = true
			if endErr := parseEndStream(payload); endErr != nil {
				return nil, endErr
			}
			return nil, io.EOF
		}
		// Keepalive frames are either empty or the empty JSON object {}
		// per the Connect wire format. Skip both; the first real data
		// frame is what callers want.
		if len(payload) == 0 || string(payload) == "{}" {
			continue
		}
		return json.RawMessage(payload), nil
	}
}

// Close releases the underlying body. Safe to call multiple times.
func (r *StreamReader) Close() error {
	if r.body == nil {
		return nil
	}
	err := r.body.Close()
	r.body = nil
	return err
}

// UnsupportedError signals a Connect feature the transport deliberately
// refuses to implement (currently: compressed frames). Callers should not
// retry.
type UnsupportedError struct {
	Reason string
}

func (e *UnsupportedError) Error() string { return e.Reason }

// readFrame parses one Connect envelope frame: 1 byte flags + 4 bytes
// big-endian length + payload.
func readFrame(r io.Reader) (flags byte, payload []byte, err error) {
	var hdr [5]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	flags = hdr[0]
	length := binary.BigEndian.Uint32(hdr[1:5])
	if length == 0 {
		return flags, nil, nil
	}
	if int64(length) > maxStreamFrameBytes {
		return flags, nil, fmt.Errorf("cursor: envelope frame length %d exceeds %d bytes", length, maxStreamFrameBytes)
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(r, payload); err != nil {
		return flags, nil, fmt.Errorf("cursor: read frame payload: %w", err)
	}
	return flags, payload, nil
}

// connectError is the JSON shape of a Connect error envelope carried in an
// end-of-stream frame (or, in the broader protocol, an HTTP error body).
type connectError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// endStreamResponse is the JSON object carried in an end-of-stream frame.
// A non-nil Error signals a server-side stream failure.
type endStreamResponse struct {
	Error *connectError `json:"error,omitempty"`
}

func parseEndStream(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	var es endStreamResponse
	if err := json.Unmarshal(payload, &es); err != nil {
		// Malformed end-frame payload is treated as a clean end: the stream
		// itself was not in error, we just cannot decode the trailing
		// metadata. Log a warning so operators can spot a buggy bridge;
		// raw bytes are scrubbed (capped + binary-truncated) to keep this
		// log safe to ship.
		slog.Warn("cursor: malformed end-of-stream frame from bridge",
			"err", err.Error(),
			"raw_preview", scrubForLog(payload, 256))
		return nil
	}
	if es.Error == nil || (es.Error.Code == "" && es.Error.Message == "") {
		return nil
	}
	// Connect end-of-stream errors do not carry an HTTP status; tag them as
	// provider errors so they survive downstream error rendering.
	gw := core.NewProviderError("cursor", http.StatusBadGateway, es.Error.Message, nil)
	if es.Error.Code != "" {
		gw = gw.WithCode(es.Error.Code)
	}
	return gw
}

// scrubForLog returns a printable preview of payload, capped at max bytes
// and with non-printable / control runes replaced so it is safe to drop
// into a log line. Used for the parseEndStream warning where the raw
// bytes might contain bearer tokens or binary garbage.
//
// Bytes that are not ASCII printable are emitted as `\xNN` escapes. UTF-8
// runes outside printable ASCII (e.g. U+2028 line-separator, bidi
// control runes) are emitted as `\uNNNN` escapes. This keeps secrets and
// terminal-breaking glyphs out of the preview.
func scrubForLog(payload []byte, max int) string {
	if len(payload) > max {
		payload = payload[:max]
	}
	var b strings.Builder
	b.Grow(len(payload) * 4)
	for i := 0; i < len(payload); {
		c := payload[i]
		switch {
		case c == '\t' || c == '\n' || c == '\r':
			b.WriteByte(' ')
			i++
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", c)
			i++
		case c >= 0x80:
			// Decode the UTF-8 rune so a multi-byte sequence escapes as
			// one \uNNNN — a per-byte escape would still be safe but
			// noisier and harder to grep for.
			r, size := utf8.DecodeRune(payload[i:])
			if r == utf8.RuneError && size <= 1 {
				fmt.Fprintf(&b, "\\x%02x", c)
				i++
			} else {
				fmt.Fprintf(&b, "\\u%04x", r)
				i += size
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}
