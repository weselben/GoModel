// Package thinkextract rewrites assistant message text that carries legacy
// <think>...</think> reasoning blocks (and configured equivalents) into the
// structured reasoning field expected by GoModel's downstream surfaces.
//
// The package operates on the gateway's response wire format after a model
// returns. A model that emits <think>reasoning</think>answer produces, after
// translation, a response with reasoning_content="reasoning" and content="answer".
//
// The translation is lossless on the wire: no model-visible character is
// dropped. If a <think> block opens but the matching </think> does not yet
// appear in the input the package buffers the partial text and waits for more
// bytes; a never-closed block is forwarded verbatim with no rewrite applied,
// so a chunk-boundary cut can never strand user content as reasoning.
package thinkextract

import (
	"strings"
)

// FieldReasoning is the OpenAI chat-completions field name that carries the
// extracted reasoning text on the wire. The Anthropic messages endpoint and
// OpenAI responses API surface the same string as their native reasoning
// payload via their respective dialect converters.
const FieldReasoning = "reasoning_content"

// defaultMaxBufferBytes is the cross-chunk cap for an unclosed reasoning
// block. 64 KiB clears any legacy variant the package is documented to
// recognise without risking unbounded growth on a runaway open tag.
const defaultMaxBufferBytes = 64 * 1024

// Options configures the tag delimiters and the per-stream buffer cap.
//
// The zero value is valid: it defaults to "<think>" / "</think>" with the
// 64 KiB cross-chunk buffer.
type Options struct {
	// TagOpen opens a reasoning block. Default "<think>".
	TagOpen string
	// TagClose closes a reasoning block. Default "</think>".
	TagClose string
	// MaxBufferBytes caps the size of an unclosed block held in streaming
	// state. Once exceeded the buffered text is flushed as ordinary content
	// and no further reasoning is emitted on that stream. Default 64 KiB.
	MaxBufferBytes int
}

func (o Options) withDefaults() Options {
	if o.TagOpen == "" {
		o.TagOpen = "<think>"
	}
	if o.TagClose == "" {
		o.TagClose = "</think>"
	}
	if o.MaxBufferBytes <= 0 {
		o.MaxBufferBytes = defaultMaxBufferBytes
	}
	return o
}

// Extract returns the input with every recognised reasoning block removed
// (cleaned), the concatenated reasoning text, and a flag indicating whether
// any block was rewritten. The flag lets callers skip re-serialisation when
// the input was already clean.
//
// When the input ends inside an open block with no matching close, Extract
// reports found=false and returns the input unchanged: the text might still
// receive the closing tag from a future chunk and we cannot risk dropping it.
func Extract(text string, opts Options) (cleaned string, reasoning string, found bool) {
	o := opts.withDefaults()
	if text == "" || !strings.Contains(text, o.TagOpen) {
		return text, "", false
	}
	openLen := len(o.TagOpen)
	closeLen := len(o.TagClose)

	var (
		cursor    int
		out       strings.Builder
		outSet    bool
		reason    strings.Builder
		reasonSet bool
	)
	for cursor < len(text) {
		rel := strings.Index(text[cursor:], o.TagOpen)
		if rel == -1 {
			out.WriteString(text[cursor:])
			outSet = true
			break
		}
		openIdx := cursor + rel
		if openIdx > cursor {
			out.WriteString(text[cursor:openIdx])
			outSet = true
		}
		relClose := strings.Index(text[openIdx+openLen:], o.TagClose)
		if relClose == -1 {
			// Open block with no matching close. Treat the whole input as
			// ordinary content: the close may yet arrive in a future chunk.
			return text, "", false
		}
		body := strings.TrimSpace(text[openIdx+openLen : openIdx+openLen+relClose])
		if reasonSet && body != "" {
			reason.WriteString("\n\n")
		}
		reason.WriteString(body)
		reasonSet = true
		cursor = openIdx + openLen + relClose + closeLen
	}
	if !outSet {
		out.WriteString(text)
	}
	if !reasonSet {
		return text, "", false
	}
	cleaned = strings.TrimSpace(out.String())
	reasoning = strings.TrimSpace(reason.String())
	return cleaned, reasoning, true
}

// State holds the running state of a streaming rewrite. One State is created
// per stream by TransformStream; a State is not safe for concurrent use.
type State struct {
	opts Options

	// inThink is true after an open tag has been seen but before its close.
	inThink bool
	// buffer holds text seen after the last emitted boundary that has not yet
	// been classified. While inThink the buffer holds only reasoning text;
	// otherwise it holds only visible text.
	buffer strings.Builder
	// reasoning accumulates the body of every closed think block, separated
	// by a blank line so multiple blocks round-trip distinctly. The first
	// block is appended without a leading separator.
	reasoning strings.Builder
	// emitted tracks how many bytes of `reasoning` have already been returned
	// via a Feed/Flush call, so the caller sees each block as a discrete delta.
	emitted int
}

// NewState constructs a streaming State with the given options applied.
func NewState(opts Options) *State {
	return &State{opts: opts.withDefaults()}
}

// Feed pushes a chunk of assistant delta content and returns the
// (content, reasoning) deltas to emit immediately. Any text that opens or
// closes a tag is held in the State until the matching boundary arrives.
//
// The returned deltas are non-empty only when there is data to emit right
// now; buffered text (an unclosed tail) yields two empty strings.
func (s *State) Feed(chunk string) (contentDelta string, reasoningDelta string) {
	if chunk == "" {
		return "", ""
	}
	if s.opts.MaxBufferBytes > 0 && s.buffer.Len()+len(chunk) > s.opts.MaxBufferBytes {
		// Cap exceeded: drop any buffered text on the floor as content so
		// nothing is lost and a runaway open tag cannot leak memory.
		rest := s.buffer.String() + chunk
		s.buffer.Reset()
		s.opts.MaxBufferBytes = 0 // disable further buffering
		_ = s.reasoning.String()
		return rest, ""
	}
	s.buffer.WriteString(chunk)
	for {
		cd, rd, advanced := s.tryAdvance()
		if !advanced {
			break
		}
		contentDelta += cd
		reasoningDelta += rd
	}
	// Outside a think block, drain any text whose tail cannot be a partial
	// open tag — emit it as content so the client sees progress.
	if !s.inThink {
		bs := s.buffer.String()
		if bs == "" {
			return contentDelta, reasoningDelta
		}
		safeEnd := safeEmitPrefix(bs, s.opts.TagOpen)
		if safeEnd > 0 {
			contentDelta += bs[:safeEnd]
			rest := bs[safeEnd:]
			s.buffer.Reset()
			s.buffer.WriteString(rest)
		}
	}
	return contentDelta, reasoningDelta
}

// Flush releases any buffered text at end-of-stream. An unclosed tag at the
// tail of the last chunk is emitted as ordinary content so nothing is lost.
// Reasoning accumulated in earlier Feed calls but not yet emitted is returned.
func (s *State) Flush() (contentDelta string, reasoningDelta string) {
	full := s.reasoning.String()
	if len(full) > s.emitted {
		reasoningDelta = full[s.emitted:]
		s.emitted = len(full)
	}
	if s.buffer.Len() == 0 {
		return contentDelta, reasoningDelta
	}
	bs := s.buffer.String()
	if s.inThink {
		// Unclosed block. Re-emit the open tag literal plus the buffered body
		// so the original bytes round-trip even without a close.
		contentDelta += s.opts.TagOpen + bs
		s.buffer.Reset()
		return contentDelta, reasoningDelta
	}
	// End of stream: emit the tail verbatim. A partial open tag can no
	// longer be completed by a later chunk, so it is literal content now.
	contentDelta += bs
	s.buffer.Reset()
	return contentDelta, reasoningDelta
}

// tryAdvance consumes one open-or-close boundary if present in the buffer.
// Returns advanced=false when the buffer is too short to decide (an open
// boundary may be cut by the chunk edge).
func (s *State) tryAdvance() (contentDelta string, reasoningDelta string, advanced bool) {
	bs := s.buffer.String()
	if s.inThink {
		i := strings.Index(bs, s.opts.TagClose)
		if i == -1 {
			return "", "", false
		}
		body := strings.TrimSpace(bs[:i])
		rest := bs[i+len(s.opts.TagClose):]
		if s.reasoning.Len() > 0 && body != "" {
			s.reasoning.WriteString("\n\n")
		}
		if body != "" {
			s.reasoning.WriteString(body)
		}
		s.buffer.Reset()
		s.buffer.WriteString(rest)
		s.inThink = false
		full := s.reasoning.String()
		if len(full) > s.emitted {
			reasoningDelta = full[s.emitted:]
			s.emitted = len(full)
		}
		return "", reasoningDelta, true
	}
	i := strings.Index(bs, s.opts.TagOpen)
	if i == -1 {
		return "", "", false
	}
	contentDelta = bs[:i]
	rest := bs[i+len(s.opts.TagOpen):]
	s.buffer.Reset()
	s.buffer.WriteString(rest)
	s.inThink = true
	return contentDelta, "", true
}

// safeEmitPrefix returns the largest prefix length of bs whose tail cannot
// form a prefix of openTag. Used to keep partial open tags in the buffer
// until the next chunk arrives.
func safeEmitPrefix(bs, openTag string) int {
	if openTag == "" {
		return len(bs)
	}
	for k := 1; k < len(openTag); k++ {
		if strings.HasSuffix(bs, openTag[:k]) {
			return len(bs) - k
		}
	}
	return len(bs)
}