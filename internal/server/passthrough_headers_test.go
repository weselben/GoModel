package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gomodel/internal/providers"
)

func TestPassthroughHeaderCapture_FiltersBlockedHeaders(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Api-Key", "secret-key")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("X-GoModel-User-Path", "acme")
	req.Header.Set("X-Custom-Alias", "acme")
	req.Header.Set("X-Allowed-Header", "keep-me")

	c := e.NewContext(req, httptest.NewRecorder())
	var captured http.Header
	handler := PassthroughHeaderCapture("X-Custom-Alias")(func(c *echo.Context) error {
		captured = providers.PassthroughHeadersFromContext(c.Request().Context())
		return nil
	})

	err := handler(c)
	require.NoError(t, err)

	assert.Empty(t, captured.Get("Authorization"))
	assert.Empty(t, captured.Get("X-Api-Key"))
	assert.Empty(t, captured.Get("Accept-Encoding"))
	assert.Empty(t, captured.Get("Connection"))
	assert.Empty(t, captured.Get("X-GoModel-User-Path"))
	assert.Empty(t, captured.Get("X-Custom-Alias"))
	assert.Equal(t, "keep-me", captured.Get("X-Allowed-Header"))

	assert.Equal(t, "Bearer secret", req.Header.Get("Authorization"))
	assert.Equal(t, "secret-key", req.Header.Get("X-Api-Key"))
	assert.Equal(t, "gzip", req.Header.Get("Accept-Encoding"))
	assert.Equal(t, "keep-alive", req.Header.Get("Connection"))
	assert.Equal(t, "acme", req.Header.Get("X-GoModel-User-Path"))
	assert.Equal(t, "acme", req.Header.Get("X-Custom-Alias"))
	assert.Equal(t, "keep-me", req.Header.Get("X-Allowed-Header"))
}
