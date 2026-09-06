package streaming

import (
	"bytes"
	"math"
	"strings"
)

// detectTokenRun appends the delta's token IDs to the rolling tail (capped at
// limit*maxPattern) and reports whether the smallest period p in 1..maxPattern
// yields limit consecutive copies at the tail.
func (st *choiceState) detectTokenRun(content []byte, counter TokenCounter, limit, maxPattern int) bool {
	st.tokenTail = append(st.tokenTail, counter.Tokens(string(content))...)
	if capacity := limit * maxPattern; len(st.tokenTail) > capacity {
		st.tokenTail = append(st.tokenTail[:0], st.tokenTail[len(st.tokenTail)-capacity:]...)
	}

	tail := st.tokenTail
	for p := 1; p <= maxPattern; p++ {
		need := p * limit
		if len(tail) < need {
			break
		}
		if tokenTailRepeats(tail, p, need) {
			return true
		}
	}
	return false
}

// tokenTailRepeats reports whether the last need token IDs are limit copies
// of the final p IDs.
func tokenTailRepeats(tail []int, p, need int) bool {
	n := len(tail)
	if n < need {
		return false
	}
	for i := n - need; i < n; i++ {
		if tail[i] != tail[n-p+(i-(n-need))%p] {
			return false
		}
	}
	return true
}

// detectByteRun appends content bytes to the rolling tail (capped so the
// longest checked run always fits) and reports whether a period of at most
// fallbackMaxUnitBytes bytes repeats limit times AND extends for at least
// fallbackMinRunBytes at the tail. The generous minimum run keeps natural
// text repeats from tripping the byte fallback.
func (st *choiceState) detectByteRun(content []byte, limit int) bool {
	st.byteTail = append(st.byteTail, content...)
	keep := fallbackMinRunBytes
	if maxNeed := fallbackMaxUnitBytes * limit; maxNeed > keep {
		keep = maxNeed
	}
	if len(st.byteTail) > keep {
		st.byteTail = append(st.byteTail[:0], st.byteTail[len(st.byteTail)-keep:]...)
	}

	tail := st.byteTail
	for p := 1; p <= fallbackMaxUnitBytes; p++ {
		need := p * limit
		if len(tail) < need {
			break
		}
		if !byteTailRepeats(tail, p, need) {
			continue
		}
		if run := byteRunLength(tail, p); run >= need && run >= fallbackMinRunBytes {
			return true
		}
	}
	return false
}

// byteTailRepeats reports whether the last need bytes are limit copies of the
// final p bytes.
func byteTailRepeats(tail []byte, p, need int) bool {
	n := len(tail)
	if n < need {
		return false
	}
	for i := n - need; i < n; i++ {
		if tail[i] != tail[n-p+(i-(n-need))%p] {
			return false
		}
	}
	return true
}

// byteRunLength measures the maximal periodic suffix run of tail with period
// p, scanning backwards from the end.
func byteRunLength(tail []byte, p int) int {
	n := len(tail)
	if n < p {
		return 0
	}
	run := p
	for i := n - 1 - p; i >= 0; i-- {
		if tail[i] != tail[i+p] {
			break
		}
		run++
	}
	return run
}

var codeFenceMarker = []byte("```")

// contentDeltas extracts (choiceIndex, text) pairs from a decoded SSE
// payload across the three supported wire shapes:
//
//   - Chat completions: choices[].delta.content, skipping tool_calls and
//     function_call deltas.
//   - Anthropic messages: content_block_delta events with
//     delta.type "text_delta" yield delta.text; thinking deltas and
//     input_json_delta (tool use) are never inspected. The content-block
//     index is the choice index.
//   - Responses API: response.output_text.delta events yield the delta
//     string; function-call and reasoning deltas are never inspected.
//     The state key is the composite output/content index, since deltas
//     from different output items can interleave.
//
// Payloads that match no shape yield nil and are ignored.
func contentDeltas(payload map[string]any) []struct {
	choiceIndex int
	content     []byte
} {
	if t, _ := payload["type"].(string); t != "" || payload["choices"] == nil {
		switch {
		case strings.HasPrefix(t, "content_block_"):
			if t != "content_block_delta" {
				return nil
			}
			delta, ok := payload["delta"].(map[string]any)
			if !ok || delta["type"] != "text_delta" {
				return nil
			}
			text, ok := delta["text"].(string)
			if !ok || text == "" {
				return nil
			}
			idx, _ := payload["index"].(float64)
			return []struct {
				choiceIndex int
				content     []byte
			}{{choiceIndex: int(idx), content: []byte(text)}}
		case strings.HasPrefix(t, "response."):
			if t != "response.output_text.delta" {
				return nil
			}
			text, ok := payload["delta"].(string)
			if !ok || text == "" {
				return nil
			}
			// Deltas from different output items can interleave; key the
			// guard state by the composite output/content index so
			// independent tails never merge into a false repetition.
			// Negative keys cannot collide with the non-negative
			// choice/block indices of the other dialects, and the
			// Responses termination ignores the index anyway.
			outIdx, _ := payload["output_index"].(float64)
			contentIdx, _ := payload["content_index"].(float64)
			key := -1 - (int(outIdx)*65536 + int(contentIdx))
			return []struct {
				choiceIndex int
				content     []byte
			}{{choiceIndex: key, content: []byte(text)}}
		default:
			return nil
		}
	}

	choicesRaw, ok := payload["choices"].([]any)
	if !ok {
		return nil
	}
	var out []struct {
		choiceIndex int
		content     []byte
	}
	for i, c := range choicesRaw {
		choiceMap, ok := c.(map[string]any)
		if !ok {
			continue
		}
		// Key the guard state by the choice's own index, not its position
		// in the choices array; the two differ only for out-of-order
		// choice delivery, which OpenAI permits.
		choiceIndex := i
		if idx, ok := choiceMap["index"].(float64); ok {
			choiceIndex = int(idx)
		}
		delta, ok := choiceMap["delta"].(map[string]any)
		if !ok {
			continue
		}
		// tool_calls/function_call deltas are never inspected, even when they
		// also carry a content field.
		if _, ok := delta["tool_calls"]; ok {
			continue
		}
		if _, ok := delta["function_call"]; ok {
			continue
		}
		content, ok := delta["content"].(string)
		if !ok || content == "" {
			// DeepSeek-style reasoning streams carry the visible thinking
			// in delta.reasoning_content; a hang loops there exactly like
			// in content, so inspect it with the same limit.
			content, ok = delta["reasoning_content"].(string)
			if !ok || content == "" {
				continue
			}
		}
		out = append(out, struct {
			choiceIndex int
			content     []byte
		}{choiceIndex: choiceIndex, content: []byte(content)})
	}
	return out
}

// isMarkdownTableRow reports whether content, after leading whitespace, starts
// with '|': a markdown table row, which repeats structurally.
func isMarkdownTableRow(content []byte) bool {
	trimmed := bytes.TrimLeft(content, " \t")
	return len(trimmed) > 0 && trimmed[0] == '|'
}

// hasLongWhitespaceRun reports whether content contains a run of at least 8
// consecutive whitespace bytes.
func hasLongWhitespaceRun(content []byte) bool {
	run := 0
	for _, b := range content {
		if isSpaceByte(b) {
			run++
			if run >= 8 {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}

func isSpaceByte(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}

// looksLikeEncodedBlob reports whether the trailing encodedWindowBytes-byte
// window of content looks like base64 or hex: symbol density >= 0.85 and
// Shannon entropy < 4.5 bits/char. Structured encodings repeat tokens by
// construction and must not trip the guard.
func looksLikeEncodedBlob(content []byte) bool {
	if len(content) < encodedWindowBytes {
		return false
	}
	window := content[len(content)-encodedWindowBytes:]
	if encodedSymbolDensity(window) < 0.85 {
		return false
	}
	return shannonEntropy(window) < 4.5
}

func encodedSymbolDensity(window []byte) float64 {
	symbols := 0
	for _, b := range window {
		if isEncodedSymbol(b) {
			symbols++
		}
	}
	return float64(symbols) / float64(len(window))
}

// isEncodedSymbol matches the base64 alphabet (and its url-safe variant) plus
// '=' padding; hex digits are a subset.
func isEncodedSymbol(b byte) bool {
	switch {
	case b >= '0' && b <= '9', b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z':
		return true
	case b == '+', b == '/', b == '-', b == '_', b == '=':
		return true
	}
	return false
}

// shannonEntropy returns the per-byte Shannon entropy of window in bits.
func shannonEntropy(window []byte) float64 {
	var freq [256]int
	for _, b := range window {
		freq[b]++
	}
	n := float64(len(window))
	entropy := 0.0
	for _, count := range freq {
		if count == 0 {
			continue
		}
		p := float64(count) / n
		entropy -= p * math.Log2(p)
	}
	return entropy
}
