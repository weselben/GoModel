package server

import (
	"github.com/labstack/echo/v5"

	"gomodel/internal/core"
	"gomodel/internal/providers"
)

// PassthroughHeaderCapture returns Echo middleware that filters incoming
// request headers for safe forwarding and stores the filtered copy in the
// request context. The original request headers are not mutated.
func PassthroughHeaderCapture(userPathAlias string) echo.MiddlewareFunc {
	userPathAlias = core.UserPathHeaderName(userPathAlias)
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			req := c.Request()
			filtered := providers.FilterIncomingHeaders(req.Header, userPathAlias)
			ctx := providers.WithPassthroughHeaders(req.Context(), filtered)
			c.SetRequest(req.WithContext(ctx))
			return next(c)
		}
	}
}
