package thinkextract

import "context"

// Surface identifies the API surface a request is being processed on. The
// translation is gated per surface so an operator can disable it on one
// surface without affecting the others.
type Surface string

const (
	// SurfaceChat is the OpenAI chat completions surface (/v1/chat/completions).
	SurfaceChat Surface = "chat"
	// SurfaceMessages is the Anthropic messages surface (/v1/messages).
	SurfaceMessages Surface = "messages"
	// SurfaceResponses is the OpenAI responses surface (/v1/responses).
	// Currently not exercised by the chat-side hook; tracked for the
	// follow-up native Responses transformer.
	SurfaceResponses Surface = "responses"
)

// surfaceKey is the unexported context key under which a Surface value is
// stored. Using an unexported empty struct prevents external packages from
// colliding on the same key.
type surfaceKey struct{}

// WithSurface returns a context that carries the given Surface for use by
// the orchestrator's extraction gate.
func WithSurface(ctx context.Context, surface Surface) context.Context {
	return context.WithValue(ctx, surfaceKey{}, surface)
}

// SurfaceFrom returns the Surface stored on ctx, or the empty string when
// no surface has been set. The empty string is the "default" surface and
// is treated as enabled when the global translation is on.
func SurfaceFrom(ctx context.Context) Surface {
	if v, ok := ctx.Value(surfaceKey{}).(Surface); ok {
		return v
	}
	return ""
}