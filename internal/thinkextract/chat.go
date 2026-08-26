package thinkextract

import (
	"encoding/json"
	"strings"

	"github.com/enterpilot/gomodel/internal/core"
)

// MessagesThinkingPolicy is the policy applied on the Anthropic messages
// surface to synthesized-from-tags reasoning. Valid values are the empty
// string (treated as Off), "off", "unsigned", and "redacted".
type MessagesThinkingPolicy string

const (
	MessagesPolicyOff       MessagesThinkingPolicy = "off"
	MessagesPolicyUnsigned  MessagesThinkingPolicy = "unsigned"
	MessagesPolicyRedacted  MessagesThinkingPolicy = "redacted"
)

// ParseMessagesPolicy normalizes a raw config value. Unknown values fall back
// to off so a typo cannot silently change wire behaviour.
func ParseMessagesPolicy(raw string) MessagesThinkingPolicy {
	switch MessagesThinkingPolicy(raw) {
	case MessagesPolicyUnsigned, MessagesPolicyRedacted:
		return MessagesThinkingPolicy(raw)
	default:
		return MessagesPolicyOff
	}
}

// SynthesizedMarkerKey is the ExtraFields key that thinkextract sets on a
// chat response message when its reasoning_content came from tag extraction.
// The messages converters read it to apply MessagesThinkingPolicy; the
// marker is only emitted on the messages surface so chat-surface responses
// never carry it.
const SynthesizedMarkerKey = "thinkextract_synthesized"

// TransformChatResponse rewrites every choice of resp whose message text
// carries legacy think-block tags: the tags and their bodies move from the
// message content into ExtraFields["reasoning_content"], leaving the visible
// text as content. Choices that already carry a reasoning_content field are
// left untouched — upstream-structured reasoning always wins over extracted
// tags.
//
// The function returns the number of choices rewritten, so callers can skip
// downstream bookkeeping when nothing changed.
//
// On the messages surface, extracted reasoning is also marked via the
// SynthesizedMarkerKey so the Anthropic dialect converter can apply
// MessagesThinkingPolicy without affecting native provider reasoning.
func TransformChatResponse(resp *core.ChatResponse, opts Options) int {
	return TransformChatResponseForSurface(resp, opts, "")
}

// TransformChatResponseForSurface is TransformChatResponse with an explicit
// surface so synthesized reasoning on the messages surface can be marked.
func TransformChatResponseForSurface(resp *core.ChatResponse, opts Options, surface Surface) int {
	if resp == nil {
		return 0
	}
	markSynthesized := surface == SurfaceMessages
	rewritten := 0
	for i := range resp.Choices {
		if transformMessage(&resp.Choices[i].Message, opts) {
			rewritten++
			if markSynthesized {
				markSynthesizedOnMessage(&resp.Choices[i].Message)
			}
		}
	}
	return rewritten
}

// markSynthesizedOnMessage sets the SynthesizedMarkerKey ExtraFields flag on
// the message so the messages converter can apply MessagesThinkingPolicy.
func markSynthesizedOnMessage(msg *core.ResponseMessage) {
	raw, err := json.Marshal(true)
	if err != nil {
		return
	}
	merged, err := core.MergeUnknownJSONFields(msg.ExtraFields, map[string]json.RawMessage{
		SynthesizedMarkerKey: raw,
	})
	if err != nil {
		return
	}
	msg.ExtraFields = merged
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