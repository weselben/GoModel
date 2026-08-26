package anthropicapi

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/enterpilot/gomodel/internal/core"
	"github.com/enterpilot/gomodel/internal/thinkextract"
)

func chatRespWithReasoning(reasoning string, synthesized bool) *core.ChatResponse {
	extra := core.UnknownJSONFields{}
	fields := map[string]json.RawMessage{
		"reasoning_content": json.RawMessage(`"` + reasoning + `"`),
	}
	if synthesized {
		fields[thinkextract.SynthesizedMarkerKey] = json.RawMessage("true")
	}
	merged, err := core.MergeUnknownJSONFields(extra, fields)
	if err != nil {
		panic(err)
	}
	return &core.ChatResponse{
		Choices: []core.Choice{{
			Message: core.ResponseMessage{
				Role:        "assistant",
				Content:     "answer",
				ExtraFields: merged,
			},
		}},
	}
}

func TestFromChatResponse_NativeReasoningUnchangedByPolicy(t *testing.T) {
	// Provider-supplied reasoning (no synthesized marker) always renders as a
	// thinking block regardless of the policy.
	for _, policy := range []thinkextract.MessagesThinkingPolicy{
		thinkextract.MessagesPolicyOff,
		thinkextract.MessagesPolicyUnsigned,
		thinkextract.MessagesPolicyRedacted,
	} {
		out := FromChatResponseWithPolicy(chatRespWithReasoning("native", false), policy)
		if len(out.Content) != 2 || out.Content[0].Type != "thinking" || out.Content[0].Thinking != "native" {
			t.Errorf("policy=%q: native reasoning must render as thinking block, got %+v", policy, out.Content)
		}
	}
}

func TestFromChatResponse_SynthesizedOff(t *testing.T) {
	out := FromChatResponseWithPolicy(chatRespWithReasoning("synth", true), thinkextract.MessagesPolicyOff)
	for _, b := range out.Content {
		if b.Type == "thinking" || b.Type == "redacted_thinking" {
			t.Errorf("off policy: synthesized reasoning leaked as %q", b.Type)
		}
	}
	// Content text survives.
	if len(out.Content) != 1 || out.Content[0].Type != "text" || out.Content[0].Text != "answer" {
		t.Errorf("off policy: content=%+v, want single text block", out.Content)
	}
}

func TestFromChatResponse_SynthesizedUnsigned(t *testing.T) {
	out := FromChatResponseWithPolicy(chatRespWithReasoning("synth", true), thinkextract.MessagesPolicyUnsigned)
	if len(out.Content) != 2 || out.Content[0].Type != "thinking" || out.Content[0].Thinking != "synth" {
		t.Errorf("unsigned policy: got %+v, want thinking block first", out.Content)
	}
}

func TestFromChatResponse_SynthesizedRedacted(t *testing.T) {
	out := FromChatResponseWithPolicy(chatRespWithReasoning("synth", true), thinkextract.MessagesPolicyRedacted)
	if len(out.Content) != 2 || out.Content[0].Type != "redacted_thinking" {
		t.Fatalf("redacted policy: got %+v, want redacted_thinking first", out.Content)
	}
	var data string
	if err := json.Unmarshal(out.Content[0].Data, &data); err != nil {
		t.Fatalf("redacted data unmarshal: %v", err)
	}
	if data != "synth" {
		t.Errorf("redacted data=%q, want %q", data, "synth")
	}
}

func TestParseMessagesPolicy(t *testing.T) {
	tests := []struct {
		raw  string
		want thinkextract.MessagesThinkingPolicy
	}{
		{"", thinkextract.MessagesPolicyOff},
		{"off", thinkextract.MessagesPolicyOff},
		{"unsigned", thinkextract.MessagesPolicyUnsigned},
		{"redacted", thinkextract.MessagesPolicyRedacted},
		{"garbage", thinkextract.MessagesPolicyOff},
	}
	for _, tt := range tests {
		if got := thinkextract.ParseMessagesPolicy(tt.raw); got != tt.want {
			t.Errorf("ParseMessagesPolicy(%q)=%q, want %q", tt.raw, got, tt.want)
		}
	}
}

func TestStreamConverter_PolicyOffDropsSynthesized(t *testing.T) {
	input := "data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"r\",\"thinkextract_synthesized\":true}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"}}]}\n\n" +
		"data: [DONE]\n"
	rc := NewStreamConverterWithPolicy(io.NopCloser(strings.NewReader(input)), "m", 0, thinkextract.MessagesPolicyOff)
	defer rc.Close()
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "thinking_delta") {
		t.Errorf("off policy: synthesized thinking leaked: %q", s)
	}
	if !strings.Contains(s, "answer") {
		t.Errorf("content text dropped: %q", s)
	}
}

func TestStreamConverter_PolicyUnsignedEmitsThinking(t *testing.T) {
	input := "data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"r\",\"thinkextract_synthesized\":true}}]}\n\n" +
		"data: [DONE]\n"
	rc := NewStreamConverterWithPolicy(io.NopCloser(strings.NewReader(input)), "m", 0, thinkextract.MessagesPolicyUnsigned)
	defer rc.Close()
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "\"thinking\"") {
		t.Errorf("unsigned policy: no thinking block: %q", s)
	}
	if strings.Contains(s, "thinkextract_synthesized") {
		t.Errorf("marker leaked to wire: %q", s)
	}
}

func TestStreamConverter_PolicyRedactedEmitsRedacted(t *testing.T) {
	input := "data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"r\",\"thinkextract_synthesized\":true}}]}\n\n" +
		"data: [DONE]\n"
	rc := NewStreamConverterWithPolicy(io.NopCloser(strings.NewReader(input)), "m", 0, thinkextract.MessagesPolicyRedacted)
	defer rc.Close()
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "redacted_thinking") {
		t.Errorf("redacted policy: no redacted_thinking block: %q", s)
	}
}

func TestStreamConverter_NativeReasoningUnaffected(t *testing.T) {
	// No marker: provider-native reasoning always renders as thinking.
	input := "data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"native\"}}]}\n\n" +
		"data: [DONE]\n"
	rc := NewStreamConverterWithPolicy(io.NopCloser(strings.NewReader(input)), "m", 0, thinkextract.MessagesPolicyOff)
	defer rc.Close()
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(string(out), "thinking_delta") {
		t.Errorf("native reasoning dropped under off policy: %q", string(out))
	}
}
