package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/krishbansalofficial/tollgate/internal/api"
)

const (
	maxErrorBody    = 4 << 10  // enough for an error message; don't buffer giant HTML error pages
	maxResponseBody = 10 << 20 // completions are text; 10 MiB is far above any real response
)

// OpenAICompat talks to any provider exposing the OpenAI chat completions API
// (Groq, Ollama, the mock). No translation is needed, only transport.
type OpenAICompat struct {
	name    string
	baseURL string // e.g. https://api.groq.com/openai/v1
	apiKey  string
	client  *http.Client
}

func NewOpenAICompat(name, baseURL, apiKey string, client *http.Client) *OpenAICompat {
	return &OpenAICompat{name: name, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, client: client}
}

func (p *OpenAICompat) Name() string { return p.name }

func (p *OpenAICompat) Chat(ctx context.Context, req *api.ChatRequest) (*api.ChatResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("%s: encode request: %w", p.name, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", p.name, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		// Our own deadline or the client hanging up is not an upstream fault:
		// return the context error so callers don't retry or trip a breaker.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &ProviderError{Provider: p.name, Retryable: true, Message: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, p.errorFrom(resp)
	}
	var out api.ChatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&out); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// A 200 with a body we can't parse: treat as an upstream fault.
		return nil, &ProviderError{Provider: p.name, Status: resp.StatusCode, Retryable: true, Message: "decode response: " + err.Error()}
	}
	return &out, nil
}

func (p *OpenAICompat) errorFrom(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	pe := &ProviderError{
		Provider:   p.name,
		Status:     resp.StatusCode,
		Retryable:  retryableStatus(resp.StatusCode),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		Message:    strings.TrimSpace(string(raw)),
	}
	var env api.ErrorResponse
	if json.Unmarshal(raw, &env) == nil && env.Error.Message != "" {
		pe.Message, pe.FromEnvelope = env.Error.Message, true
	}
	return pe
}

// AsProviderError is a small helper for callers using errors.As.
func AsProviderError(err error) (*ProviderError, bool) {
	var pe *ProviderError
	ok := errors.As(err, &pe)
	return pe, ok
}
