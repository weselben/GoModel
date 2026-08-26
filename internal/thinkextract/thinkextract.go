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

// TagPair is a matched open/close delimiter pair that brackets a reasoning
// block. Both fields are matched literally; no regex is used so the scanner
// stays allocation-free and chunk-boundary-safe.
type TagPair struct {
	Open  string
	Close string
}

// DefaultTagPairs is the evidence-backed default recognition list. It mirrors
// the union of what vLLM, SGLang, and Open WebUI treat as standard legacy
// reasoning markers (T9 research: docs.vllm.ai reasoning outputs, SGLang
// separate-reasoning docs, Open WebUI reasoning-models docs). Granite's
// plain-English delimiters and ERNIE's <response> answer-wrapper are
// deliberately excluded: the former are false-positive-prone natural language,
// the latter marks the answer rather than the reasoning.
func DefaultTagPairs() []TagPair {
	return []TagPair{
		{Open: "<think>", Close: "</think>"},
		{Open: "<thinking>", Close: "</thinking>"},
		{Open: "<reasoning>", Close: "</reasoning>"},
		{Open: "<reason>", Close: "</reason>"},
		{Open: "<thought>", Close: "</thought>"},
		{Open: "<|begin_of_thought|>", Close: "<|end_of_thought|>"},
		{Open: "◁think▷", Close: "◁/think▷"},
		{Open: "[THINK]", Close: "[/THINK]"},
		{Open: "<|channel|>analysis<|message|>", Close: "<|end|>"},
	}
}

// ParseTagPairs parses a comma-separated list of "<open>...</close>" entries
// into TagPairs, e.g. "<think>...</think>,<thinking>...</thinking>".
// Malformed entries (missing the "..." separator, empty open or close) are
// skipped so one bad entry never breaks the whole list.
func ParseTagPairs(list string) []TagPair {
	if strings.TrimSpace(list) == "" {
		return nil
	}
	var pairs []TagPair
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		open, close, ok := strings.Cut(entry, "...")
		if !ok || open == "" || close == "" {
			continue
		}
		pairs = append(pairs, TagPair{Open: open, Close: close})
	}
	return pairs
}

// Options configures the tag delimiters, the per-stream buffer cap, and the
// per-surface enable gates.
//
// The zero value is valid: it defaults to DefaultTagPairs with the 64 KiB
// cross-chunk buffer and all surfaces enabled.
type Options struct {
	// TagPairs is the recognition list. When empty, DefaultTagPairs applies.
	// TagOpen/TagClose below, when set, override TagPairs with a single pair.
	TagPairs []TagPair
	// TagOpen opens a reasoning block. Legacy single-pair override; prefer
	// TagPairs for new configuration.
	TagOpen string
	// TagClose closes a reasoning block. Legacy single-pair override; prefer
	// TagPairs for new configuration.
	TagClose string
	// MaxBufferBytes caps the size of an unclosed block held in streaming
	// state. Once exceeded the buffered text is flushed as ordinary content
	// and no further reasoning is emitted on that stream. Default 64 KiB.
	MaxBufferBytes int
	// ChatEnabled gates the translation on the chat completions surface.
	// Nil means on.
	ChatEnabled *bool
	// ResponsesEnabled gates the translation on the OpenAI responses surface.
	// Nil means on.
	ResponsesEnabled *bool
	// MessagesPolicy gates the translation on the Anthropic messages surface.
	// Values: "off" (default), "unsigned", "redacted". Empty means off,
	// matching the messages endpoint's default of no synthesized thinking
	// blocks.
	MessagesPolicy string
}

// EnabledFor reports whether the translation runs on the given surface.
// The empty surface (no surface set on the request context) is treated as
// enabled, matching the global-on default.
func (o Options) EnabledFor(surface Surface) bool {
	switch surface {
	case SurfaceChat:
		if o.ChatEnabled != nil {
			return *o.ChatEnabled
		}
	case SurfaceResponses:
		if o.ResponsesEnabled != nil {
			return *o.ResponsesEnabled
		}
	case SurfaceMessages:
		return ParseMessagesPolicy(o.MessagesPolicy) != MessagesPolicyOff
	}
	return true
}

// pairs resolves the effective recognition list for these options.
func (o Options) pairs() []TagPair {
	if len(o.TagPairs) > 0 {
		return o.TagPairs
	}
	if o.TagOpen != "" && o.TagClose != "" {
		return []TagPair{{Open: o.TagOpen, Close: o.TagClose}}
	}
	return DefaultTagPairs()
}

func (o Options) withDefaults() Options {
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
// With multiple tag pairs configured, the earliest opening tag wins; an
// unclosed block of any pair aborts the whole extraction conservatively.
func Extract(text string, opts Options) (cleaned string, reasoning string, found bool) {
	o := opts.withDefaults()
	pairs := o.pairs()
	if text == "" || !containsAnyOpen(text, pairs) {
		return text, "", false
	}

	var (
		cursor    int
		out       strings.Builder
		outSet    bool
		reason    strings.Builder
		reasonSet bool
	)
	for cursor < len(text) {
		openIdx, pairIdx := earliestOpen(text, cursor, pairs)
		if openIdx == -1 {
			out.WriteString(text[cursor:])
			outSet = true
			break
		}
		if openIdx > cursor {
			out.WriteString(text[cursor:openIdx])
			outSet = true
		}
		pair := pairs[pairIdx]
		bodyStart := openIdx + len(pair.Open)
		relClose := strings.Index(text[bodyStart:], pair.Close)
		if relClose == -1 {
			// Open block with no matching close. Treat the whole input as
			// ordinary content: the close may yet arrive in a future chunk.
			return text, "", false
		}
		body := strings.TrimSpace(text[bodyStart : bodyStart+relClose])
		if reasonSet && body != "" {
			reason.WriteString("\n\n")
		}
		reason.WriteString(body)
		reasonSet = true
		cursor = bodyStart + relClose + len(pair.Close)
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

// containsAnyOpen reports whether text contains any configured open tag.
func containsAnyOpen(text string, pairs []TagPair) bool {
	for _, p := range pairs {
		if strings.Contains(text, p.Open) {
			return true
		}
	}
	return false
}

// earliestOpen finds the first occurrence of any open tag at or after cursor.
// It returns the absolute index and the pair index, or -1 when none matches.
func earliestOpen(text string, cursor int, pairs []TagPair) (int, int) {
	best, bestPair := -1, -1
	for i, p := range pairs {
		rel := strings.Index(text[cursor:], p.Open)
		if rel == -1 {
			continue
		}
		abs := cursor + rel
		if best == -1 || abs < best {
			best, bestPair = abs, i
		}
	}
	return best, bestPair
}

// State holds the running state of a streaming rewrite. One State is created
// per stream by TransformStream; a State is not safe for concurrent use.
type State struct {
	opts  Options
	pairs []TagPair

	// inThink is true after an open tag has been seen but before its close.
	// activePair is the pair that opened the current block.
	inThink    bool
	activePair int
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
	o := opts.withDefaults()
	return &State{opts: o, pairs: o.pairs()}
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
	// open tag of any configured pair — emit it as content so the client sees
	// progress.
	if !s.inThink {
		bs := s.buffer.String()
		if bs == "" {
			return contentDelta, reasoningDelta
		}
		safeEnd := safeEmitPrefixAll(bs, s.pairs)
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
// tail of the last chunk is re-emitted with its open tag as ordinary content
// so nothing is lost. Reasoning accumulated in earlier Feed calls but not yet
// emitted is returned.
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
		contentDelta += s.pairs[s.activePair].Open + bs
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
		closeTag := s.pairs[s.activePair].Close
		i := strings.Index(bs, closeTag)
		if i == -1 {
			return "", "", false
		}
		body := strings.TrimSpace(bs[:i])
		rest := bs[i+len(closeTag):]
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
	openIdx, pairIdx := earliestOpenIn(bs, s.pairs)
	if openIdx == -1 {
		return "", "", false
	}
	contentDelta = bs[:openIdx]
	rest := bs[openIdx+len(s.pairs[pairIdx].Open):]
	s.buffer.Reset()
	s.buffer.WriteString(rest)
	s.inThink = true
	s.activePair = pairIdx
	return contentDelta, "", true
}

// earliestOpenIn finds the first occurrence of any open tag in bs.
func earliestOpenIn(bs string, pairs []TagPair) (int, int) {
	best, bestPair := -1, -1
	for i, p := range pairs {
		idx := strings.Index(bs, p.Open)
		if idx == -1 {
			continue
		}
		if best == -1 || idx < best {
			best, bestPair = idx, i
		}
	}
	return best, bestPair
}

// safeEmitPrefixAll returns the largest prefix length of bs whose tail cannot
// form a prefix of any configured open tag.
func safeEmitPrefixAll(bs string, pairs []TagPair) int {
	safeEnd := len(bs)
	for _, p := range pairs {
		if end := safeEmitPrefix(bs, p.Open); end < safeEnd {
			safeEnd = end
		}
	}
	return safeEnd
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