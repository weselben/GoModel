package server

import (
	"bytes"
	"io"
	"net/http"

	"github.com/goccy/go-json"
	"github.com/labstack/echo/v5"

	"github.com/enterpilot/gomodel/internal/auditlog"
	"github.com/enterpilot/gomodel/internal/core"
	"github.com/enterpilot/gomodel/internal/modelnormalizer"
)

// ModelNormalizerMiddleware rewrites the chat request body's `model` field
// and injects the per-alias thinking policy before workflow resolution sees
// the request. It runs after authentication (so rewriters only see
// authenticated traffic) and before WorkflowResolutionWithResolverAndPolicy
// (so the rewritten model is the one resolution operates on — otherwise
// ApplyResolvedSelector would re-stamp the original alias over the rewrite).
//
// The middleware is fail-closed: a rewrite error from the normalizer aborts
// the request with HTTP 400. The unchanged body falls through unchanged,
// keeping the seam invisible to clients when no rule matches.
func ModelNormalizerMiddleware(normalizer *modelnormalizer.Normalizer, auditLogger auditlog.LoggerInterface) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if normalizer == nil {
				return next(c)
			}
			if c.Request().Method != http.MethodPost {
				return next(c)
			}
			if c.Request().URL.Path != "/v1/chat/completions" {
				return next(c)
			}

			body, err := requestBodyBytes(c)
			if err != nil {
				return handleError(c, core.NewInvalidRequestError("failed to read request body", err))
			}
			if len(body) == 0 {
				return next(c)
			}

			var req core.ChatRequest
			if err := json.Unmarshal(body, &req); err != nil {
				// Malformed body — let downstream handlers report it.
				return next(c)
			}
			adapted, rewritten, err := normalizer.AdaptChatRequest(&req)
			if err != nil {
				return handleError(c, core.NewInvalidRequestError("model normalizer: "+err.Error(), err))
			}
			if !rewritten {
				return next(c)
			}

			out, err := json.Marshal(adapted)
			if err != nil {
				return handleError(c, core.NewInvalidRequestError("model normalizer: marshal: "+err.Error(), err))
			}
			pinNormalizerOriginalAuditBody(c, auditLogger)
			req2 := c.Request()
			req2.Body = io.NopCloser(bytes.NewReader(out))
			req2.ContentLength = int64(len(out))
			storeRequestBodySnapshot(c, out)
			if auditLogger != nil && auditLogger.Config().Enabled {
				auditlog.EnrichEntryWithRequestRevision(c, auditlog.RequestRevisionSnapshot{
					Rewriter:    "model_normalizer",
					BytesBefore: len(body),
					BytesAfter:  len(out),
				})
			}
			return next(c)
		}
	}
}

// pinNormalizerOriginalAuditBody captures the pre-rewrite request body so
// the audit entry shows what the client actually sent (the canonical alias),
// not the rewritten upstream ID.
func pinNormalizerOriginalAuditBody(c *echo.Context, auditLogger auditlog.LoggerInterface) {
	if auditLogger == nil || !auditLogger.Config().Enabled {
		return
	}
	entry, ok := c.Get(string(auditlog.LogEntryKey)).(*auditlog.LogEntry)
	if !ok || entry == nil {
		return
	}
	auditlog.PopulateRequestData(entry, c.Request(), auditLogger.Config())
}