package thinkextract

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/enterpilot/gomodel/internal/core"
)

// TransformResponsesResponse rewrites each output item of resp whose message
// content carries legacy reasoning tags. For every item rewritten, a fresh
// reasoning output item is prepended in place and the message item keeps
// only the cleaned text. Items that already carry a reasoning item ahead of
// them are left untouched.
//
// Returns the number of message items rewritten. The function is the
// Responses API analogue of TransformChatResponse; both use the same reasoning
// item shape that the gateway emits for native reasoning providers.
func TransformResponsesResponse(resp *core.ResponsesResponse, opts Options) int {
	if resp == nil {
		return 0
	}
	rewritten := 0
	for i := 0; i < len(resp.Output); i++ {
		item := &resp.Output[i]
		cleaned, reasoning, changed := transformResponsesOutputItem(item, opts)
		if !changed {
			continue
		}
		item.Content = cleaned
		rewritten++
		reasonItem := core.ResponsesOutputItem{
			ID:     "rs_" + shortID(),
			Type:   "reasoning",
			Status: "completed",
			Content: []core.ResponsesContentItem{
				{Type: "reasoning_text", Text: reasoning},
			},
			ExtraFields: core.UnknownJSONFieldsFromMap(map[string]json.RawMessage{
				"summary": json.RawMessage(`[]`),
			}),
		}
		resp.Output = append(resp.Output[:i], append([]core.ResponsesOutputItem{reasonItem}, resp.Output[i:]...)...)
		i++ // // skip the inserted item on the next iteration
	}
	return rewritten
}

// transformResponsesOutputItem rewrites the text content of a single
// Responses output item. It returns the cleaned Content slice, the
// concatenated reasoning text, and a changed flag.
func transformResponsesOutputItem(item *core.ResponsesOutputItem, opts Options) ([]core.ResponsesContentItem, string, bool) {
	if item == nil || item.Type != "message" || len(item.Content) == 0 {
		return nil, "", false
	}
	cleaned := make([]core.ResponsesContentItem, len(item.Content))
	copy(cleaned, item.Content)
	var reasoning strings.Builder
	changed := false
	for ci := range cleaned {
		part := &cleaned[ci]
		if part.Type != "output_text" || part.Text == "" {
			continue
		}
		newText, body, found := Extract(part.Text, opts)
		if !found {
			continue
		}
		part.Text = newText
		if body != "" {
			if reasoning.Len() > 0 {
				reasoning.WriteString("\n\n")
			}
			reasoning.WriteString(body)
		}
		changed = true
	}
	return cleaned, reasoning.String(), changed
}

// shortID returns a short opaque hex identifier. Real production code should
// use uuid.NewString; crypto/rand is used here so the package stays
// dependency-free for tests.
func shortID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(b[:])
}