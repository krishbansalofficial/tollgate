package providers

import (
	"bytes"
	"context"
	"encoding/json"
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

// post sends a JSON body and returns the response only when it is 200 OK;
// any other status becomes a *ProviderError (body consumed and closed).
// The caller owns closing a returned response's body.
func post(ctx context.Context, c *http.Client, provider, url string, hdr http.Header, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%s: encode request: %w", provider, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", provider, err)
	}
	req.Header = hdr.Clone()
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Do(req)
	if err != nil {
		// Our own deadline or the client hanging up is not an upstream fault:
		// return the context error so callers don't retry or trip a breaker.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &ProviderError{Provider: provider, Retryable: true, Message: err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, errorFrom(provider, resp)
	}
	return resp, nil
}

// decode reads a 200 response body into out and closes it.
func decode(ctx context.Context, provider string, resp *http.Response, out any) error {
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(out); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		// A 200 with a body we can't parse: treat as an upstream fault.
		return &ProviderError{Provider: provider, Status: resp.StatusCode, Retryable: true, Message: "decode response: " + err.Error()}
	}
	return nil
}

// errorFrom parses {"error":{"message":...}}, the shape used by OpenAI,
// Groq, Ollama, Anthropic and (inside its own envelope) Gemini.
func errorFrom(provider string, resp *http.Response) *ProviderError {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	pe := &ProviderError{
		Provider:   provider,
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

// badRequest is a translation failure: the request can't be expressed in
// this provider's API. It is a client fault (400), not provider ill-health.
func badRequest(provider, msg string) *ProviderError {
	return &ProviderError{Provider: provider, Status: http.StatusBadRequest, Message: provider + " adapter: " + msg, FromEnvelope: true}
}
