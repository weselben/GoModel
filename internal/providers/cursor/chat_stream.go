package cursor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/goccy/go-json"

	"github.com/enterpilot/gomodel/internal/core"
	"github.com/enterpilot/gomodel/internal/providers"
	"github.com/enterpilot/gomodel/internal/streaming"
)

// streamConverter wraps a Connect envelope frame stream and renders it as
// OpenAI chat.completion.chunk SSE. The contract is:
//
//   - Assistant text deltas become one chunk each (the first carries
//     delta.role=assistant in addition to delta.content).
//   - A terminal result frame yields a final chunk with finish_reason
//     "stop" and an optional top-level "usage" payload.
//   - On a malformed envelope frame the converter returns a GatewayError
//     with status 502 after any already-buffered chunks have been read
//     out (mirrors anthropic's TestStreamChatCompletion_MalformedEventReturnsError).
//   - Clean end-of-stream → "data: [DONE]\n\n" and EOF.
//   - closeAgent is invoked exactly once, on terminal frame, malformed
//     frame, or explicit Close, so the bridge releases the local agent
//     whether the stream is drained, errors, or abandoned.
//
// Tracking note: the Connect wire spec carries an offset per frame, but
// the cursor bridge delivers incremental text deltas — concatenating the
// deltas reproduces the cumulative text — so we deliberately ignore it
// (matching cursor_wire.go's runStreamEnvelope contract).
type streamConverter struct {
	stream     *StreamReader
	model      string
	created    int64
	msgID      string
	buffer     streaming.StreamBuffer
	closed     bool
	emitted    bool   // whether the leading role chunk has gone out
	closeAgent func() // idempotent agent release
	ctx        context.Context
}

func newStreamConverter(ctx context.Context, stream *StreamReader, model string, closeAgent func()) *streamConverter {
	return &streamConverter{
		stream:     stream,
		model:      model,
		created:    time.Now().Unix(),
		buffer:     streaming.NewStreamBuffer(1024),
		closeAgent: closeAgent,
		ctx:        ctx,
	}
}

// Read implements io.Reader: it fills p with the next chunk of OpenAI
// SSE bytes, materialising one or more Connect frames per call. It is
// safe to call Read in a tight loop until EOF.
func (c *streamConverter) Read(p []byte) (int, error) {
	if c.buffer.Len() > 0 {
		return c.buffer.Read(p), nil
	}
	if c.closed {
		c.buffer.Release()
		return 0, io.EOF
	}

	frame, err := c.stream.Next(c.ctx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			c.releaseAgent()
			c.closed = true
			c.buffer.AppendString("data: [DONE]\n\n")
			return c.buffer.Read(p), nil
		}
		c.releaseAgent()
		c.closed = true
		c.buffer.Release()
		return 0, err
	}

	env := runStreamEnvelope{}
	if err := json.Unmarshal(frame, &env); err != nil {
		// Malformed frame → 502 after any prior chunks have been drained
		// by the caller. The buffer is empty because we only ever append
		// after a successful parse; any preceding chunks were already
		// handed back to the caller.
		c.releaseAgent()
		c.closed = true
		return 0, core.NewProviderError("cursor", http.StatusBadGateway,
			"cursor: decode stream frame: "+err.Error(), err)
	}

	switch {
	case env.Result != nil:
		if err := c.handleResult(env.Result); err != nil {
			c.releaseAgent()
			c.closed = true
			c.buffer.Release()
			return 0, err
		}
	case env.SDKMessage != nil && env.SDKMessage.Type == "assistant":
		c.appendAssistant(env.SDKMessage.Message)
	}
	// env.Done and other sdkMessage types are no-ops on the wire.

	if c.buffer.Len() > 0 {
		return c.buffer.Read(p), nil
	}
	// No bytes produced for this frame — recurse to read the next one.
	return c.Read(p)
}

// handleResult renders the terminal result frame: emit a final chunk
// carrying finish_reason "stop" and (when present) the usage payload,
// then release the agent so the bridge can free local resources. A
// non-OK run status returns a GatewayError mirroring runSend.
func (c *streamConverter) handleResult(r *runStreamResult) error {
	if !terminalStatusOK(r.Status) {
		return cursorRunError(r)
	}
	if c.msgID == "" {
		c.msgID = r.RunID
	}
	var usage map[string]any
	if u := r.Result.Usage; u != nil {
		usage = map[string]any{
			"prompt_tokens":     int(u.InputTokens),
			"completion_tokens": int(u.OutputTokens),
			"total_tokens":      int(u.TotalTokens),
		}
	}
	c.buffer.AppendString(providers.FormatChatChunkSSE(
		c.msgID, c.created, c.model, "cursor",
		map[string]any{}, "stop", usage,
	))
	return nil
}

// appendAssistant extracts every text block from an assistant SDK
// message and renders it as one OpenAI chunk. The first assistant chunk
// in a stream also carries delta.role=assistant; subsequent chunks are
// content-only. Unknown block types are skipped silently.
func (c *streamConverter) appendAssistant(payload json.RawMessage) {
	var msg assistantMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return
	}
	for _, block := range msg.Content {
		if block.Type != "text" || block.Text == "" {
			continue
		}
		delta := map[string]any{"content": block.Text}
		if !c.emitted {
			delta["role"] = "assistant"
			c.emitted = true
		}
		c.buffer.AppendString(providers.FormatChatChunkSSE(
			c.msgID, c.created, c.model, "cursor", delta, nil, nil,
		))
	}
}

// Close releases the underlying frame stream and releases the agent
// exactly once. Safe to call multiple times.
func (c *streamConverter) Close() error {
	if c.closed {
		c.buffer.Release()
		return nil
	}
	c.closed = true
	c.buffer.Release()
	c.releaseAgent()
	return c.stream.Close()
}

// releaseAgent runs the CloseAgent callback exactly once. A panic in the
// caller-supplied callback is not recovered: Close is the terminal call
// and propagating a Close panic keeps the deferred error visible to the
// caller instead of being swallowed by the reader.
func (c *streamConverter) releaseAgent() {
	if c.closeAgent == nil {
		return
	}
	fn := c.closeAgent
	c.closeAgent = nil
	fn()
}
