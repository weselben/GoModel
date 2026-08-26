package thinkextract

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// TransformStream wraps an SSE stream of OpenAI chat-completion chunks,
// rewriting each event's delta.content that carries legacy think-block tags
// into a paired reasoning_content + content delta. Non-data lines and the
// data: [DONE] sentinel are passed through unchanged. Invalid JSON in a data
// line is forwarded byte-for-byte so a malformed upstream event cannot break
// the stream.
//
// A line is treated as data when its trimmed prefix equals "data:". Any
// other SSE field name (event:, id:, retry:, comment lines starting with ":")
// is forwarded untouched.
//
// Multi-choice streams are supported: a State is maintained per choice index
// so each choice's tag boundaries resolve independently.
func TransformStream(in io.ReadCloser, opts Options) io.ReadCloser {
	o := opts.withDefaults()
	pr, pw := io.Pipe()
	go transformLoop(in, pw, o)
	return pr
}

func transformLoop(in io.ReadCloser, pw *io.PipeWriter, o Options) {
	defer pw.Close()
	defer in.Close()

	states := map[int]*State{}
	scanner := bufio.NewScanner(in)
	// 1 MiB per-line cap: enough for any sane SSE event, low enough to
	// bound per-event memory.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		trimmed := bytes.TrimLeft(line, " \t")
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			if !writeLine(pw, line) {
				return
			}
			continue
		}
		payload := bytes.TrimPrefix(trimmed, []byte("data:"))
		payload = bytes.TrimPrefix(payload, []byte(" "))

		if string(payload) == "[DONE]" {
			for idx, st := range states {
				cd, rd := st.Flush()
				if cd == "" && rd == "" {
					continue
				}
				if !writeFlushDelta(pw, idx, cd, rd) {
					return
				}
			}
			if !writeLine(pw, line) {
				return
			}
			continue
		}

		rewritten, err := rewriteChunk(payload, states, o)
		if err != nil {
			if !writeLine(pw, line) {
				return
			}
			continue
		}
		for _, out := range rewritten {
			if !writeEvent(pw, out) {
				return
			}
		}
	}
	// Defensive drain for streams that terminate without a [DONE] sentinel.
	for idx, st := range states {
		cd, rd := st.Flush()
		if cd == "" && rd == "" {
			continue
		}
		_ = writeFlushDelta(pw, idx, cd, rd)
	}
}

// rewriteChunk decodes a single OpenAI chat-completion chunk and rewrites each
// choice's delta.content for think-block markers. The chunk is returned as a
// slice of JSON payloads, one per output event. A single input chunk can yield
// 0, 1, or 2 output chunks per choice (content + reasoning).
//
// All non-delta fields on every choice are preserved byte-for-byte: the
// rewriter only mutates delta.content and delta.reasoning_content.
func rewriteChunk(payload []byte, states map[int]*State, o Options) ([][]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, err
	}
	rawChoices, ok := root["choices"]
	if !ok {
		return [][]byte{payload}, nil
	}
	var choices []json.RawMessage
	if err := json.Unmarshal(rawChoices, &choices); err != nil {
		return nil, err
	}
	if len(choices) == 0 {
		return [][]byte{payload}, nil
	}

	type pendingDelta struct {
		choiceIdx    int
		contentDelta string
		reasonDelta  string
	}
	var pending []pendingDelta
	for i, choiceRaw := range choices {
		var meta struct {
			Index int `json:"index"`
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(choiceRaw, &meta); err != nil {
			continue
		}
		// Existing upstream reasoning wins; never stomp on structured data.
		if meta.Delta.ReasoningContent != "" {
			continue
		}
		state := states[meta.Index]
		if state == nil {
			state = NewState(o)
			states[meta.Index] = state
		}
		cd, rd := state.Feed(meta.Delta.Content)
		if cd == "" && rd == "" {
			continue
		}
		pending = append(pending, pendingDelta{
			choiceIdx:    i,
			contentDelta: cd,
			reasonDelta:  rd,
		})
	}
	if len(pending) == 0 {
		return [][]byte{payload}, nil
	}

	out := make([][]byte, 0, len(pending))
	for _, p := range pending {
		newChoice, err := rewriteChoiceDelta(choices[p.choiceIdx], p.contentDelta, p.reasonDelta)
		if err != nil {
			continue
		}
		newChoices := make([]json.RawMessage, len(choices))
		copy(newChoices, choices)
		newChoices[p.choiceIdx] = newChoice
		newRoot := make(map[string]json.RawMessage, len(root))
		for k, v := range root {
			if k == "choices" {
				continue
			}
			newRoot[k] = v
		}
		newChoicesJSON, err := json.Marshal(newChoices)
		if err != nil {
			continue
		}
		newRoot["choices"] = newChoicesJSON
		encoded, err := json.Marshal(newRoot)
		if err != nil {
			continue
		}
		out = append(out, encoded)
	}
	return out, nil
}

// rewriteChoiceDelta returns a copy of choice with delta.content and
// delta.reasoning_content replaced. Every other field on the choice is
// preserved verbatim.
func rewriteChoiceDelta(choice json.RawMessage, content, reasoning string) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(choice, &m); err != nil {
		return nil, err
	}
	delta := map[string]any{}
	if raw, ok := m["delta"]; ok && len(raw) > 0 {
		_ = json.Unmarshal(raw, &delta)
	}
	delta["content"] = content
	delta["reasoning_content"] = reasoning
	deltaJSON, err := json.Marshal(delta)
	if err != nil {
		return nil, err
	}
	m["delta"] = deltaJSON
	return json.Marshal(m)
}

// writeLine writes a single SSE line followed by "\n".
func writeLine(pw *io.PipeWriter, line []byte) bool {
	if _, err := pw.Write(line); err != nil {
		return false
	}
	if len(line) == 0 || line[len(line)-1] != '\n' {
		if _, err := pw.Write([]byte("\n")); err != nil {
			return false
		}
	}
	return true
}

// writeEvent writes a complete SSE event (data: <payload>\n\n).
func writeEvent(pw *io.PipeWriter, payload []byte) bool {
	if _, err := pw.Write([]byte("data: ")); err != nil {
		return false
	}
	if _, err := pw.Write(payload); err != nil {
		return false
	}
	if _, err := pw.Write([]byte("\n\n")); err != nil {
		return false
	}
	return true
}

// writeFlushDelta emits a final cleanup event for a stream that ended without
// a [DONE] sentinel. The event carries only the flushed content / reasoning.
func writeFlushDelta(pw *io.PipeWriter, idx int, contentDelta, reasonDelta string) bool {
	payload := mustEncode(map[string]any{
		"choices": []map[string]any{{
			"index": idx,
			"delta": map[string]any{
				"content":           contentDelta,
				"reasoning_content": reasonDelta,
			},
		}},
	})
	return writeEvent(pw, payload)
}

func mustEncode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"choices":[]}`)
	}
	return b
}

// ReadAll is a small convenience that drains an io.ReadCloser produced by
// TransformStream into a single string. Tests use it; production code keeps
// using io.Copy.
func ReadAll(rc io.ReadCloser) (string, error) {
	defer rc.Close()
	var sb strings.Builder
	if _, err := io.Copy(&sb, rc); err != nil {
		return "", err
	}
	return sb.String(), nil
}