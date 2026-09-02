package streaming

import (
	"github.com/ron2111/omnitoken"
)

// TokenCounter returns a fresh slice of token IDs for a given text using the
// tokenizer appropriate for the model the counter was constructed for.
//
// The interface stays intentionally narrow: callers that drive per-chunk
// repetition detection only need the token sequence, not decode or count
// helpers. Counters are safe for concurrent use; the underlying omnitoken
// engine caches itself per encoding so a single TokenCounter instance per
// model is sufficient.
type TokenCounter interface {
	Tokens(text string) []int
}

// NewTokenCounter resolves model to a tokenizer via omnitoken and returns a
// TokenCounter backed by that engine.
//
// When omnitoken cannot resolve model (unknown name, empty string, or any
// other resolution failure) NewTokenCounter returns (nil, nil) so the caller
// can fall back to a byte-period heuristic without branching on error values.
// ForEncoding failures after a successful ResolveModel are surfaced as a real
// error: the registry claims the encoding exists but the engine could not be
// built, which is a configuration bug worth surfacing.
func NewTokenCounter(model string) (TokenCounter, error) {
	info, err := omnitoken.ResolveModel(model)
	if err != nil {
		return nil, nil
	}
	engine, err := omnitoken.ForEncoding(info.Encoding)
	if err != nil {
		return nil, err
	}
	return &engineCounter{engine: engine}, nil
}

// engineCounter adapts an omnitoken ModelEngine to the TokenCounter
// interface. It deliberately owns no cache: omnitoken's engineCache already
// memoises engines per encoding, so duplicating it here would only waste
// memory.
type engineCounter struct {
	engine omnitoken.ModelEngine
}

// Tokens encodes text as ordinary text (no special-token interpretation) and
// returns the resulting token IDs.
//
// Empty input short-circuits to a non-nil empty slice without invoking the
// underlying tokenizer; omnitoken's EncodeOrdinary would otherwise allocate a
// tokens slice with capacity len(text)/4+1 and then run the segmenter over
// zero bytes, which is wasted work for the streaming hot path.
func (c *engineCounter) Tokens(text string) []int {
	if text == "" {
		return []int{}
	}
	return c.engine.EncodeOrdinary(text)
}
