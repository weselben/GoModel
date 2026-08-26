package thinkextract

import (
	"encoding/json"
	"strings"

	"github.com/enterpilot/gomodel/internal/core"
)

// TransformChatResponse rewrites every choice of resp whose message text
// carries legacy think-block tags: the tags and their bodies move from the
// message content into ExtraFields["reasoning_content"], leaving the visible
// text as content. Choices that already carry a reasoning_content field are
// left untouched — upstream-structured reasoning always wins over extracted
// tags.
//
// The function returns the number of choices rewritten, so callers can skip
// downstream bookkeeping when nothing changed.
func TransformChatResponse(resp *core.ChatResponse, opts Options) int {
	if resp == nil {
		return 0
	}
	rewritten := 0
	for i := range resp.Choices {
		if transformMessage(&resp.Choices[i].Message, opts) {
			rewritten++
		}
	}
	return rewritten
}

// transformMessage applies the think-block extraction to one response
// message. It returns true when the message was rewritten.
func transformMessage(msg *core.ResponseMessage, opts Options) bool {
	if msg == nil {
		return false
	}
	if len(msg.ExtraFields.Lookup(FieldReasoning)) > 0 {
		return false
	}
	switch content := msg.Content.(type) {
	case string:
		cleaned, reasoning, found := Extract(content, opts)
		if !found {
			return false
		}
		msg.Content = cleaned
		// The content was rewritten regardless of whether the block carried
		// any reasoning text; count it.
		setReasoning(msg, reasoning)
		return true
	case []core.ContentPart:
		return transformContentParts(msg, content, opts)
	default:
		return false
	}
}

// transformContentParts extracts think blocks from every text part of a
// structured content array. Non-text parts are untouched.
func transformContentParts(msg *core.ResponseMessage, parts []core.ContentPart, opts Options) bool {
	var reasoning strings.Builder
	changed := false
	for i := range parts {
		if parts[i].Type != "text" || parts[i].Text == "" {
			continue
		}
		cleaned, reason, found := Extract(parts[i].Text, opts)
		if !found {
			continue
		}
		parts[i].Text = cleaned
		if reason != "" {
			if reasoning.Len() > 0 {
				reasoning.WriteString("\n\n")
			}
			reasoning.WriteString(reason)
		}
		changed = true
	}
	if !changed {
		return false
	}
	msg.Content = parts
	// The content parts were rewritten even when every extracted block was
	// empty; count the rewrite and only set reasoning when there is some.
	setReasoning(msg, reasoning.String())
	return true
}

// setReasoning stores the extracted reasoning text on the message's extra
// fields, preserving every other unknown field the upstream set.
func setReasoning(msg *core.ResponseMessage, reasoning string) bool {
	if reasoning == "" {
		return false
	}
	encoded, err := json.Marshal(reasoning)
	if err != nil {
		return false
	}
	merged, err := core.MergeUnknownJSONFields(msg.ExtraFields, map[string]json.RawMessage{
		FieldReasoning: encoded,
	})
	if err != nil {
		return false
	}
	msg.ExtraFields = merged
	return true
}