package kimi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gomodel/internal/core"
	"gomodel/internal/llmclient"
	"gomodel/internal/providers"
)

// expectedHeaders maps all spoofed headers to their expected values.
var expectedHeaders = map[string]string{
	"Accept":                     "application/json",
	"Accept-Encoding":            "gzip, deflate",
	"Accept-Language":            "*",
	"Content-Type":               "application/json",
	"Http-Referer":               "https://github.com/Zoo-Code-Org/Zoo-Code",
	"Sec-Fetch-Mode":             "cors",
	"User-Agent":                 "ZooCode/3.62.0",
	"X-Stainless-Arch":           "x64",
	"X-Stainless-Lang":           "js",
	"X-Stainless-Os":             "Linux",
	"X-Stainless-Package-Version": "5.12.2",
	"X-Stainless-Retry-Count":    "0",
	"X-Stainless-Runtime":        "node",
	"X-Stainless-Runtime-Version": "v22.22.1",
	"X-Title":                    "Zoo Code",
}

func verifySpoofedHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	for key, expected := range expectedHeaders {
		if got := r.Header.Get(key); got != expected {
			t.Errorf("%s = %q, want %q", key, got, expected)
		}
	}
	authHeader := r.Header.Get("Authorization")
	if authHeader != "Bearer test-api-key" {
		t.Errorf("Authorization = %q, want %q", authHeader, "Bearer test-api-key")
	}
}

func TestNew(t *testing.T) {
	provider := NewWithHTTPClient("test-api-key", nil, llmclient.Hooks{})
	if provider.compat == nil {
		t.Error("compat provider should not be nil")
	}
}

func TestNew_ReturnsProvider(t *testing.T) {
	provider := New(providers.ProviderConfig{APIKey: "test-api-key"}, providers.ProviderOptions{})
	if provider == nil {
		t.Error("provider should not be nil")
	}
}

func TestChatCompletion(t *testing.T) {
	tests := []struct {
		name          string
		statusCode    int
		responseBody  string
		expectedError bool
		checkResponse func(*testing.T, *core.ChatResponse)
	}{
		{
			name:       "successful request",
			statusCode: http.StatusOK,
			responseBody: `{
				"id": "chatcmpl-123",
				"object": "chat.completion",
				"created": 1677652288,
				"model": "kimi-for-coding",
				"choices": [{
					"index": 0,
					"message": {
						"role": "assistant",
						"content": "Hello! How can I help you today?"
					},
					"finish_reason": "stop"
				}],
				"usage": {
					"prompt_tokens": 10,
					"completion_tokens": 20,
					"total_tokens": 30
				}
			}`,
			expectedError: false,
			checkResponse: func(t *testing.T, resp *core.ChatResponse) {
				if resp.ID != "chatcmpl-123" {
					t.Errorf("ID = %q, want %q", resp.ID, "chatcmpl-123")
				}
				if resp.Model != "kimi-for-coding" {
					t.Errorf("Model = %q, want %q", resp.Model, "kimi-for-coding")
				}
				if len(resp.Choices) != 1 {
					t.Fatalf("len(Choices) = %d, want 1", len(resp.Choices))
				}
				if resp.Choices[0].Message.Content != "Hello! How can I help you today?" {
					t.Errorf("Message content = %q, want %q", resp.Choices[0].Message.Content, "Hello! How can I help you today?")
				}
				if resp.Usage.PromptTokens != 10 {
					t.Errorf("PromptTokens = %d, want 10", resp.Usage.PromptTokens)
				}
				if resp.Usage.CompletionTokens != 20 {
					t.Errorf("CompletionTokens = %d, want 20", resp.Usage.CompletionTokens)
				}
				if resp.Usage.TotalTokens != 30 {
					t.Errorf("TotalTokens = %d, want 30", resp.Usage.TotalTokens)
				}
			},
		},
		{
			name:          "auth error",
			statusCode:    http.StatusUnauthorized,
			responseBody:  `{"error": {"message": "Invalid API key"}}`,
			expectedError: true,
		},
		{
			name:          "rate limit error",
			statusCode:    http.StatusTooManyRequests,
			responseBody:  `{"error": {"message": "Rate limit exceeded"}}`,
			expectedError: true,
		},
		{
			name:          "server error",
			statusCode:    http.StatusInternalServerError,
			responseBody:  `{"error": {"message": "Internal server error"}}`,
			expectedError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				verifySpoofedHeaders(t, r)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("failed to read request body: %v", err)
				}
				var req core.ChatRequest
				if err := json.Unmarshal(body, &req); err != nil {
					t.Fatalf("failed to unmarshal request: %v", err)
				}
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.responseBody))
			}))
			defer server.Close()

			provider := NewWithHTTPClient("test-api-key", nil, llmclient.Hooks{})
			provider.SetBaseURL(server.URL)

			req := &core.ChatRequest{
				Model: "kimi-for-coding",
				Messages: []core.Message{
					{Role: "user", Content: "Hello"},
				},
			}

			resp, err := provider.ChatCompletion(context.Background(), req)

			if tt.expectedError {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if tt.checkResponse != nil {
					tt.checkResponse(t, resp)
				}
			}
		})
	}
}

func TestStreamChatCompletion(t *testing.T) {
	tests := []struct {
		name          string
		statusCode    int
		responseBody  string
		expectedError bool
	}{
		{
			name:       "successful streaming request",
			statusCode: http.StatusOK,
			responseBody: `data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1677652288,"model":"kimi-for-coding","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1677652288,"model":"kimi-for-coding","choices":[{"index":0,"delta":{"content":"!"},"finish_reason":null}]}

data: [DONE]
`,
			expectedError: false,
		},
		{
			name:          "API error",
			statusCode:    http.StatusUnauthorized,
			responseBody:  `{"error": {"message": "Invalid API key"}}`,
			expectedError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				verifySpoofedHeaders(t, r)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("failed to read request body: %v", err)
				}
				var req core.ChatRequest
				if err := json.Unmarshal(body, &req); err != nil {
					t.Fatalf("failed to unmarshal request: %v", err)
				}
				if !req.Stream {
					t.Error("Stream should be true in request")
				}
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.responseBody))
			}))
			defer server.Close()

			provider := NewWithHTTPClient("test-api-key", nil, llmclient.Hooks{})
			provider.SetBaseURL(server.URL)

			req := &core.ChatRequest{
				Model: "kimi-for-coding",
				Messages: []core.Message{
					{Role: "user", Content: "Hello"},
				},
			}

			body, err := provider.StreamChatCompletion(context.Background(), req)

			if tt.expectedError {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if body == nil {
					t.Fatal("body should not be nil")
				}
				defer func() { _ = body.Close() }()

				respBody, err := io.ReadAll(body)
				if err != nil {
					t.Fatalf("failed to read response body: %v", err)
				}
				if string(respBody) != tt.responseBody {
					t.Errorf("response body = %q, want %q", string(respBody), tt.responseBody)
				}
			}
		})
	}
}

func TestResponses(t *testing.T) {
	tests := []struct {
		name          string
		statusCode    int
		responseBody  string
		expectedError bool
		checkResponse func(*testing.T, *core.ResponsesResponse)
	}{
		{
			name:       "successful request",
			statusCode: http.StatusOK,
			responseBody: `{
				"id": "resp_123",
				"object": "response",
				"created_at": 1677652288,
				"model": "kimi-for-coding",
				"status": "completed",
				"output": [{
					"id": "msg_123",
					"type": "message",
					"role": "assistant",
					"status": "completed",
					"content": [{
						"type": "output_text",
						"text": "Hello! How can I help you today?"
					}]
				}],
				"usage": {
					"input_tokens": 10,
					"output_tokens": 20,
					"total_tokens": 30
				}
			}`,
			expectedError: false,
			checkResponse: func(t *testing.T, resp *core.ResponsesResponse) {
				if resp.ID != "resp_123" {
					t.Errorf("ID = %q, want %q", resp.ID, "resp_123")
				}
				if resp.Object != "response" {
					t.Errorf("Object = %q, want %q", resp.Object, "response")
				}
				if resp.Model != "kimi-for-coding" {
					t.Errorf("Model = %q, want %q", resp.Model, "kimi-for-coding")
				}
				if resp.Status != "completed" {
					t.Errorf("Status = %q, want %q", resp.Status, "completed")
				}
				if len(resp.Output) != 1 {
					t.Fatalf("len(Output) = %d, want 1", len(resp.Output))
				}
				if len(resp.Output[0].Content) != 1 {
					t.Fatalf("len(Output[0].Content) = %d, want 1", len(resp.Output[0].Content))
				}
				if resp.Output[0].Content[0].Text != "Hello! How can I help you today?" {
					t.Errorf("Output text = %q, want %q", resp.Output[0].Content[0].Text, "Hello! How can I help you today?")
				}
				if resp.Usage == nil {
					t.Fatal("Usage should not be nil")
				}
				if resp.Usage.InputTokens != 10 {
					t.Errorf("InputTokens = %d, want 10", resp.Usage.InputTokens)
				}
				if resp.Usage.OutputTokens != 20 {
					t.Errorf("OutputTokens = %d, want 20", resp.Usage.OutputTokens)
				}
				if resp.Usage.TotalTokens != 30 {
					t.Errorf("TotalTokens = %d, want 30", resp.Usage.TotalTokens)
				}
			},
		},
		{
			name:          "auth error",
			statusCode:    http.StatusUnauthorized,
			responseBody:  `{"error": {"message": "Invalid API key"}}`,
			expectedError: true,
		},
		{
			name:          "rate limit error",
			statusCode:    http.StatusTooManyRequests,
			responseBody:  `{"error": {"message": "Rate limit exceeded"}}`,
			expectedError: true,
		},
		{
			name:          "server error",
			statusCode:    http.StatusInternalServerError,
			responseBody:  `{"error": {"message": "Internal server error"}}`,
			expectedError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				verifySpoofedHeaders(t, r)
				if r.URL.Path != "/responses" {
					t.Errorf("Path = %q, want %q", r.URL.Path, "/responses")
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("failed to read request body: %v", err)
				}
				var req core.ResponsesRequest
				if err := json.Unmarshal(body, &req); err != nil {
					t.Fatalf("failed to unmarshal request: %v", err)
				}
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.responseBody))
			}))
			defer server.Close()

			provider := NewWithHTTPClient("test-api-key", nil, llmclient.Hooks{})
			provider.SetBaseURL(server.URL)

			req := &core.ResponsesRequest{
				Model: "kimi-for-coding",
				Input: "Hello",
			}

			resp, err := provider.Responses(context.Background(), req)

			if tt.expectedError {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if tt.checkResponse != nil {
					tt.checkResponse(t, resp)
				}
			}
		})
	}
}

func TestStreamResponses(t *testing.T) {
	tests := []struct {
		name          string
		statusCode    int
		responseBody  string
		expectedError bool
		checkStream   func(*testing.T, io.ReadCloser)
	}{
		{
			name:       "successful streaming request",
			statusCode: http.StatusOK,
			responseBody: `event: response.created
data: {"type":"response.created","response":{"id":"resp_123","object":"response","status":"in_progress","model":"kimi-for-coding"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"Hello"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"!"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_123","object":"response","status":"completed","model":"kimi-for-coding"}}
`,
			expectedError: false,
			checkStream: func(t *testing.T, body io.ReadCloser) {
				if body == nil {
					t.Fatal("body should not be nil")
				}
				defer func() { _ = body.Close() }()

				respBody, err := io.ReadAll(body)
				if err != nil {
					t.Fatalf("failed to read response body: %v", err)
				}

				responseStr := string(respBody)
				if !strings.Contains(responseStr, "response.created") {
					t.Error("response should contain response.created event")
				}
				if !strings.Contains(responseStr, "response.output_text.delta") {
					t.Error("response should contain response.output_text.delta event")
				}
				if !strings.Contains(responseStr, "[DONE]") {
					t.Error("response should end with [DONE]")
				}
			},
		},
		{
			name:          "API error",
			statusCode:    http.StatusUnauthorized,
			responseBody:  `{"error": {"message": "Invalid API key"}}`,
			expectedError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				verifySpoofedHeaders(t, r)
				if r.URL.Path != "/responses" {
					t.Errorf("Path = %q, want %q", r.URL.Path, "/responses")
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("failed to read request body: %v", err)
				}
				var req core.ResponsesRequest
				if err := json.Unmarshal(body, &req); err != nil {
					t.Fatalf("failed to unmarshal request: %v", err)
				}
				if !req.Stream {
					t.Error("Stream should be true in request")
				}
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.responseBody))
			}))
			defer server.Close()

			provider := NewWithHTTPClient("test-api-key", nil, llmclient.Hooks{})
			provider.SetBaseURL(server.URL)

			req := &core.ResponsesRequest{
				Model: "kimi-for-coding",
				Input: "Hello",
			}

			body, err := provider.StreamResponses(context.Background(), req)

			if tt.expectedError {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if tt.checkStream != nil {
					tt.checkStream(t, body)
				}
			}
		})
	}
}

func TestSetBaseURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-123",
			"object": "chat.completion",
			"created": 1677652288,
			"model": "kimi-for-coding",
			"choices": [{
				"index": 0,
				"message": {"role": "assistant", "content": "Hello!"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`))
	}))
	defer server.Close()

	provider := NewWithHTTPClient("test-api-key", nil, llmclient.Hooks{})
	provider.SetBaseURL(server.URL)

	req := &core.ChatRequest{
		Model: "kimi-for-coding",
		Messages: []core.Message{
			{Role: "user", Content: "Hello"},
		},
	}

	resp, err := provider.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("response should not be nil")
	}
	if resp.ID != "chatcmpl-123" {
		t.Errorf("ID = %q, want %q", resp.ID, "chatcmpl-123")
	}
}

func TestChatCompletionWithContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		w.WriteHeader(http.StatusRequestTimeout)
	}))
	defer server.Close()

	provider := NewWithHTTPClient("test-api-key", nil, llmclient.Hooks{})
	provider.SetBaseURL(server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := &core.ChatRequest{
		Model: "kimi-for-coding",
		Messages: []core.Message{
			{Role: "user", Content: "Hello"},
		},
	}

	_, err := provider.ChatCompletion(ctx, req)
	if err == nil {
		t.Error("expected error when context is cancelled, got nil")
	}
}

func TestHeaderSpoofing(t *testing.T) {
	var called bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		verifySpoofedHeaders(t, r)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-123",
			"object": "chat.completion",
			"created": 1677652288,
			"model": "kimi-for-coding",
			"choices": [{
				"index": 0,
				"message": {"role": "assistant", "content": "Hello!"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`))
	}))
	defer server.Close()

	provider := NewWithHTTPClient("test-api-key", nil, llmclient.Hooks{})
	provider.SetBaseURL(server.URL)

	req := &core.ChatRequest{
		Model: "kimi-for-coding",
		Messages: []core.Message{
			{Role: "user", Content: "Hello"},
		},
	}

	_, err := provider.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("expected mock server to be called, but it was not")
	}
}

func TestListModels_Upstream(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"kimi-for-coding","object":"model","owned_by":"kimi"}]}`))
	}))
	defer server.Close()

	provider := NewWithHTTPClient("test-api-key", server.Client(), llmclient.Hooks{})
	provider.SetBaseURL(server.URL)

	resp, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.Object != "list" {
		t.Errorf("Object = %q, want %q", resp.Object, "list")
	}
	if len(resp.Data) != 1 {
		t.Fatalf("len(Data) = %d, want 1", len(resp.Data))
	}
	if resp.Data[0].ID != "kimi-for-coding" {
		t.Errorf("Data[0].ID = %q, want %q", resp.Data[0].ID, "kimi-for-coding")
	}
	if resp.Data[0].Object != "model" {
		t.Errorf("Data[0].Object = %q, want %q", resp.Data[0].Object, "model")
	}
	if resp.Data[0].OwnedBy != "kimi" {
		t.Errorf("Data[0].OwnedBy = %q, want %q", resp.Data[0].OwnedBy, "kimi")
	}
	if !called {
		t.Fatal("expected mock server handler to be called")
	}
}
