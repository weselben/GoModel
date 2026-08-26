package thinkextract

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/enterpilot/gomodel/internal/core"
)

func TestWithSurface_AndSurfaceFrom(t *testing.T) {
	if got := SurfaceFrom(context.Background()); got != "" {
		t.Errorf("empty context: SurfaceFrom=%q, want empty", got)
	}
	ctx := WithSurface(context.Background(), SurfaceChat)
	if got := SurfaceFrom(ctx); got != SurfaceChat {
		t.Errorf("SurfaceFrom(chat ctx)=%q, want %q", got, SurfaceChat)
	}
	ctx2 := WithSurface(context.Background(), SurfaceMessages)
	if got := SurfaceFrom(ctx2); got != SurfaceMessages {
		t.Errorf("SurfaceFrom(messages ctx)=%q, want %q", got, SurfaceMessages)
	}
}

func TestMarkSynthesizedOnMessage(t *testing.T) {
	msg := &core.ResponseMessage{
		Role:    "assistant",
		Content: "answer",
	}
	markSynthesizedOnMessage(msg)
	raw := msg.ExtraFields.Lookup(SynthesizedMarkerKey)
	if len(raw) == 0 {
		t.Fatalf("synthesized marker not set")
	}
	var got bool
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got {
		t.Errorf("synthesized marker=%v, want true", got)
	}
}

func TestMarkSynthesizedOnMessage_PreservesExistingExtras(t *testing.T) {
	msg := &core.ResponseMessage{
		Content: "answer",
		ExtraFields: mustUnknownFields(map[string]json.RawMessage{
			"x_custom": json.RawMessage(`"keepme"`),
		}),
	}
	markSynthesizedOnMessage(msg)
	if len(msg.ExtraFields.Lookup("x_custom")) == 0 {
		t.Errorf("existing extra dropped during mark")
	}
	if len(msg.ExtraFields.Lookup(SynthesizedMarkerKey)) == 0 {
		t.Errorf("synthesized marker not set")
	}
}

func mustUnknownFields(m map[string]json.RawMessage) core.UnknownJSONFields {
	return core.UnknownJSONFieldsFromMap(m)
}
