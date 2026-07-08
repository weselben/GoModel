package run

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"gomodel/config"
	"gomodel/internal/core"
	"gomodel/internal/providers"
)

// capturedProvider is a test double that records ProviderOptions passed to its
// constructor so wiring assertions can observe values that would otherwise be
// buried inside provider implementations.
type capturedProvider struct {
	userPathHeader string
}

func (c *capturedProvider) ChatCompletion(_ context.Context, _ *core.ChatRequest) (*core.ChatResponse, error) {
	return nil, nil
}

func (c *capturedProvider) StreamChatCompletion(_ context.Context, _ *core.ChatRequest) (io.ReadCloser, error) {
	return nil, nil
}

func (c *capturedProvider) ListModels(_ context.Context) (*core.ModelsResponse, error) {
	return nil, nil
}

func (c *capturedProvider) Responses(_ context.Context, _ *core.ResponsesRequest) (*core.ResponsesResponse, error) {
	return nil, nil
}

func (c *capturedProvider) StreamResponses(_ context.Context, _ *core.ResponsesRequest) (io.ReadCloser, error) {
	return nil, nil
}

func (c *capturedProvider) Embeddings(_ context.Context, _ *core.EmbeddingRequest) (*core.EmbeddingResponse, error) {
	return nil, nil
}

type stubLifecycleApp struct {
	mu            sync.Mutex
	startErr      error
	shutdownErr   error
	startCalls    int
	shutdownCalls int
	shutdownCtx   context.Context
	shutdownBlock <-chan struct{}
}

func (s *stubLifecycleApp) Start(_ context.Context, _ string) error {
	s.mu.Lock()
	s.startCalls++
	s.mu.Unlock()
	return s.startErr
}

func (s *stubLifecycleApp) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.shutdownCalls++
	s.shutdownCtx = ctx
	s.mu.Unlock()
	if s.shutdownBlock != nil {
		<-s.shutdownBlock
	}
	return s.shutdownErr
}

func (s *stubLifecycleApp) startCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startCalls
}

func (s *stubLifecycleApp) shutdownCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shutdownCalls
}

func (s *stubLifecycleApp) capturedShutdownContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shutdownCtx
}

func TestStartApplication_ShutsDownOnStartFailure(t *testing.T) {
	startErr := errors.New("listen tcp :8080: bind: address already in use")
	app := &stubLifecycleApp{startErr: startErr}

	err := startApplication(app, ":8080")
	if !errors.Is(err, startErr) {
		t.Fatalf("error = %v, want start error %v", err, startErr)
	}
	if calls := app.startCallCount(); calls != 1 {
		t.Fatalf("startCalls = %d, want 1", calls)
	}
	if calls := app.shutdownCallCount(); calls != 1 {
		t.Fatalf("shutdownCalls = %d, want 1", calls)
	}
	shutdownCtx := app.capturedShutdownContext()
	if shutdownCtx == nil {
		t.Fatal("shutdown context was not captured")
	}
	deadline, ok := shutdownCtx.Deadline()
	if !ok {
		t.Fatal("shutdown context should have a deadline")
	}
	if time.Until(deadline) <= 0 {
		t.Fatal("shutdown context deadline should be in the future")
	}
}

func TestStartApplication_ReportsShutdownFailure(t *testing.T) {
	startErr := errors.New("listen failed")
	shutdownErr := errors.New("close failed")
	app := &stubLifecycleApp{
		startErr:    startErr,
		shutdownErr: shutdownErr,
	}

	err := startApplication(app, ":8080")
	if !errors.Is(err, startErr) {
		t.Fatalf("error = %v, want start error %v", err, startErr)
	}
	if !errors.Is(err, shutdownErr) {
		t.Fatalf("error = %v, want shutdown error %v", err, shutdownErr)
	}
	if calls := app.shutdownCallCount(); calls != 1 {
		t.Fatalf("shutdownCalls = %d, want 1", calls)
	}
}

func TestStartApplication_DoesNotShutdownOnSuccess(t *testing.T) {
	app := &stubLifecycleApp{}

	if err := startApplication(app, ":8080"); err != nil {
		t.Fatalf("startApplication() error = %v, want nil", err)
	}
	if calls := app.startCallCount(); calls != 1 {
		t.Fatalf("startCalls = %d, want 1", calls)
	}
	if calls := app.shutdownCallCount(); calls != 0 {
		t.Fatalf("shutdownCalls = %d, want 0", calls)
	}
}

func TestStartApplication_StopsWaitingWhenShutdownTimesOut(t *testing.T) {
	previousTimeout := shutdownTimeout
	shutdownTimeout = 10 * time.Millisecond
	defer func() {
		shutdownTimeout = previousTimeout
	}()

	startErr := errors.New("listen failed")
	shutdownBlock := make(chan struct{})
	defer close(shutdownBlock)

	app := &stubLifecycleApp{
		startErr:      startErr,
		shutdownBlock: shutdownBlock,
	}

	err := startApplication(app, ":8080")
	if !errors.Is(err, startErr) {
		t.Fatalf("error = %v, want start error %v", err, startErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
	if calls := app.shutdownCallCount(); calls != 1 {
		t.Fatalf("shutdownCalls = %d, want 1", calls)
	}
}

func TestMain_KimicodeProviderRegistration(t *testing.T) {
	factory := defaultProviderFactory(&config.Config{})

	registered := factory.RegisteredTypes()
	found := false
	for _, typ := range registered {
		if typ == "kimicode" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("kimicode not in RegisteredTypes() = %v", registered)
	}

	provider, err := factory.Create(providers.ProviderConfig{Type: "kimicode", APIKey: "test"})
	if err != nil {
		t.Fatalf("factory.Create(kimicode) error = %v, want nil", err)
	}
	if provider == nil {
		t.Fatal("factory.Create(kimicode) returned nil provider")
	}
}

func TestMain_SetUserPathHeaderWiring(t *testing.T) {
	factory := providers.NewProviderFactory()
	factory.SetUserPathHeader("X-Tenant")

	var captured *capturedProvider
	factory.Add(providers.Registration{
		Type: "captured",
		New: func(cfg providers.ProviderConfig, opts providers.ProviderOptions) core.Provider {
			p := &capturedProvider{userPathHeader: opts.UserPathHeader}
			captured = p
			return p
		},
	})

	_, err := factory.Create(providers.ProviderConfig{Type: "captured"})
	if err != nil {
		t.Fatalf("factory.Create() error = %v", err)
	}
	if captured == nil {
		t.Fatal("provider constructor was not called")
	}
	if captured.userPathHeader != "X-Tenant" {
		t.Fatalf("UserPathHeader = %q, want X-Tenant", captured.userPathHeader)
	}
}
