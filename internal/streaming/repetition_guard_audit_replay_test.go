package streaming

// Real-world replay tests derived from production audit captures.
//
// Provenance: GoModel audit log at forge.weselben.de, 2026-09-03. Two
// streaming requests to the zai/glm-5.3-flash route (log ids 4dbca6f2 and
// 80351615) hung for 205 s and 522 s respectively and were terminated by
// the client with "context canceled". The audit log stores request bodies
// but omits response bodies, so the SSE payloads below replay the hang
// *signature* (legitimate answer degenerating into a prose loop, no [DONE])
// rather than the exact repeated tokens.
//
// Sanitization: no session ids, request ids, auth keys, client IPs, local
// file paths, or verbatim user turns appear here. Model labels and the
// loop-phrase shapes are generic. Loop phrases are representative stutters
// a coding model emits; they are not copies of the captured content.
//
// End-of-turn contract: when the guard triggers it appends
// `data: [DONE]\n\n` — byte-identical to the OpenAI chat-completions
// terminator a clean upstream would emit. Any OpenAI-compatible SSE
// consumer therefore sees a normal end-of-stream and proceeds without
// intervention. There is no `finish_reason` distinguishing a guard
// truncation from a natural completion; this is intentional, so that the
// cancellation is indistinguishable from a normal answer ending.
//
// Groups:
//
//	A. Normal traffic  — guard ON, stream completes, output byte-identical,
//	   trigger callback never fires.
//	B. Legitimate repetition — guard ON, shape LOOKS like a loop (fenced
//	   code, tables, whitespace runs, encoded blobs) but must NOT trigger;
//	   output byte-identical.
//	C. Definite loops  — guard ON, stream degenerates; trigger fires exactly
//	   once, upstream is cut early, output ends with a synthetic [DONE].

import (
	"io"
	"strings"
	"sync/atomic"
	"testing"
)

// counterSink counts trigger-callback invocations.
type counterSink struct{ n atomic.Int32 }

func (c *counterSink) inc() { c.n.Add(1) }

// mustReplaySource builds a guard over a fixed SSE payload with an armed
// trigger callback, and fails the test if io.ReadAll errors.
func mustReplaySource(t *testing.T, payload string, limit, maxPattern int, model string) (string, *recordingReadCloser, *counterSink) {
	t.Helper()
	src := newSource(payload)
	sink := &counterSink{}
	stream := NewRepetitionGuardStream(src, limit, maxPattern, model,
		WithTriggerCallback(sink.inc))
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return string(out), src, sink
}

// ---------------------------------------------------------------------------
// Group A — normal traffic: guard ON, everything passes through untouched.
// ---------------------------------------------------------------------------

func TestRealWorld_NormalTraffic(t *testing.T) {
	const limit = 4

	cases := []struct {
		name  string
		model string
		body  string
	}{
		{
			name:  "short conversational answer",
			model: "gpt-4o",
			body: chatEvent("Hello! How can I help you today?") + doneEvent(),
		},
		{
			name:  "structured failover-attempt answer with markdown table",
			model: "zai-weselben/glm-5.3-flash",
			body: chatEvent("Here is the failover-attempt table for that request:\n\n") +
				chatEvent("| seq | model | status | error |\n") +
				chatEvent("|-----|-------|--------|-------|\n") +
				chatEvent("| 1 | kilo/minimax-m3:free | 429 | rate limit |\n") +
				chatEvent("| 2 | openrouter/minimax-m3:free | 200 | ok |\n") +
				chatEvent("\nThe red squares mark failed attempts; the row list adds model names inline.\n") +
				doneEvent(),
		},
		{
			name:  "code answer with fenced block and varied prose",
			model: "gpt-4o",
			body: chatEvent("To reverse a slice in Go:\n\n") +
				chatEvent("```go\nfor i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {\n") +
				chatEvent("    s[i], s[j] = s[j], s[i]\n}\n```\n\n") +
				chatEvent("Runs in O(n) time with O(1) extra space.\n") +
				doneEvent(),
		},
		{
			name:  "long prose without repetition",
			model: "zai-weselben/glm-5.3-flash",
			body: chatEvent("The audit log shows request timing, token usage, and per-attempt failover details. ") +
				chatEvent("Each attempt records the provider, model, status code, and error message. ") +
				chatEvent("A circuit-breaker error marks the provider as temporarily unavailable. ") +
				chatEvent("The client sees only the final serving model unless the detail view is opened.\n") +
				doneEvent(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, src, sink := mustReplaySource(t, tc.body, limit, 8, tc.model)
			if out != tc.body {
				t.Fatalf("normal traffic altered\nwant: %q\ngot:  %q", tc.body, out)
			}
			if sink.n.Load() != 0 {
				t.Fatalf("trigger fired on normal traffic (%d times)", sink.n.Load())
			}
			if src.closeCount != 1 {
				t.Fatalf("expected one source close on clean EOF, got %d", src.closeCount)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Group B — legitimate repetition: shapes that look like loops but must pass.
// ---------------------------------------------------------------------------

func TestRealWorld_LegitimateRepetition(t *testing.T) {
	const limit = 3 // aggressive threshold on purpose

	cases := []struct {
		name    string
		model   string
		limitOv int // >0 overrides the group threshold for this case
		body    string
	}{
		{
			name:  "fenced code with identical lines repeated",
			model: "gpt-4o",
			body: chatEvent("Here is the attempt-recording helper:\n\n```go\n") +
				chatEvent("attempts = append(attempts, Attempt{Model: target})\n") +
				chatEvent("attempts = append(attempts, Attempt{Model: target})\n") +
				chatEvent("attempts = append(attempts, Attempt{Model: target})\n") +
				chatEvent("attempts = append(attempts, Attempt{Model: target})\n") +
				chatEvent("attempts = append(attempts, Attempt{Model: target})\n") +
				chatEvent("```\n\nFive identical appends, all inside the fence.\n") + doneEvent(),
		},
		{
			name:  "markdown table with repeated rows",
			model: "zai-weselben/glm-5.3-flash",
			body: chatEvent("| a | b |\n") + chatEvent("| a | b |\n") +
				chatEvent("| a | b |\n") + chatEvent("| a | b |\n") + doneEvent(),
		},
		{
			name:  "whitespace-heavy output",
			model: "gpt-4o",
			body: chatEvent(strings.Repeat("\n", 40)) + chatEvent(strings.Repeat(" \t", 30)) + doneEvent(),
		},
		{
			name:  "base64-looking blob output",
			model: "zai-weselben/glm-5.3-flash",
			body: chatEvent(strings.Repeat("QUJDRA==", 24)) +
				chatEvent(strings.Repeat("QUJDRA==", 24)) + doneEvent(),
		},
		{
			// Known model -> token detector (no byte floor), so limit must
			// sit above the repeat count for this to count as edgy. Uses the
			// per-case limit override below.
			name:    "repeated single short word outside code",
			model:   "gpt-4o",
			limitOv: 4,
			body:    chatEvent("ok ") + chatEvent("ok ") + chatEvent("ok ") + doneEvent(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limit := limit
			if tc.limitOv > 0 {
				limit = tc.limitOv
			}
			out, _, sink := mustReplaySource(t, tc.body, limit, 8, tc.model)
			if out != tc.body {
				t.Fatalf("legitimate repetition was altered\nwant: %q\ngot:  %q", tc.body, out)
			}
			if sink.n.Load() != 0 {
				t.Fatalf("trigger fired on legitimate repetition (%d times)", sink.n.Load())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Group C — definite loops: the guard must cut early and append [DONE].
// ---------------------------------------------------------------------------

func TestRealWorld_DefiniteLoops(t *testing.T) {
	cases := []struct {
		name       string
		model      string
		limit      int
		maxPattern int
		// build returns (payload, loopUnit); assertions count loopUnit
		// occurrences and require the output to end with [DONE].
		build func() (payload, loopUnit string, maxAllowed int)
	}{
		{
			// The audit-hang signature: legitimate intro, then a prose loop,
			// no [DONE] from the upstream. Unknown model -> byte fallback.
			name:       "prose loop at unknown model (audit 4dbca6f2 signature)",
			model:      "zai-weselben/glm-5.3-flash",
			limit:      4,
			maxPattern: 8,
			build: func() (string, string, int) {
				const unit = " Let me consider that. "
				var b strings.Builder
				b.WriteString(chatEvent("Looking at the failover display question:\n\n"))
				b.WriteString(chatEvent("```go\nattempts = append(attempts, Attempt{Model: target})\n```\n\n"))
				b.WriteString(chatEvent("Each red square is one failed attempt.\n"))
				for i := 0; i < 40; i++ {
					b.WriteString(chatEvent(unit))
				}
				return b.String(), unit, 6 // byte floor (96B / 21B unit) may leak one past limit*unit
			},
		},
		{
			name:       "single-token stutter at known model",
			model:      "gpt-4o",
			limit:      3,
			maxPattern: 8,
			build: func() (string, string, int) {
				var b strings.Builder
				b.WriteString(chatEvent("Counting: "))
				for i := 0; i < 30; i++ {
					b.WriteString(chatEvent("a"))
				}
				return b.String(), chatEvent("a"), 3
			},
		},
		{
			name:       "token-chain loop at known model",
			model:      "gpt-4o",
			limit:      2,
			maxPattern: 8,
			build: func() (string, string, int) {
				var b strings.Builder
				b.WriteString(chatEvent("start "))
				for i := 0; i < 20; i++ {
					b.WriteString(chatEvent("ab"))
				}
				return b.String(), chatEvent("ab"), 2
			},
		},
		{
			name:       "long-unit prose loop at unknown model with higher limit",
			model:      "some-provider/some-model",
			limit:      6,
			maxPattern: 8,
			build: func() (string, string, int) {
				// 28-byte unit stays inside the 32-byte byte-fallback period cap.
				const unit = "Thinking through this step, "
				var b strings.Builder
				b.WriteString(chatEvent("Investigating the gateway behavior:\n\n"))
				for i := 0; i < 50; i++ {
					b.WriteString(chatEvent(unit))
				}
				return b.String(), unit, 8 // 96B floor / 28B unit -> may leak up to ~4 past limit
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, unit, maxAllowed := tc.build()
			src := newSource(payload)
			sink := &counterSink{}
			stream := NewRepetitionGuardStream(src, tc.limit, tc.maxPattern, tc.model,
				WithTriggerCallback(sink.inc))
			out, err := io.ReadAll(stream)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if err := stream.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			if !strings.HasSuffix(string(out), doneEvent()) {
				t.Fatalf("output does not end with synthetic [DONE]: tail=%q", tail(string(out), 80))
			}
			if n := sink.n.Load(); n != 1 {
				t.Fatalf("trigger callback fired %d times, want exactly 1", n)
			}
			if got := strings.Count(string(out), unit); got > maxAllowed {
				t.Fatalf("loop unit emitted %d times, want <= %d (accepted leak)", got, maxAllowed)
			}
			if src.closeCount != 1 {
				t.Fatalf("expected upstream closed exactly once on trigger, got %d", src.closeCount)
			}
		})
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
