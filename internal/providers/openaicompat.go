package providers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/krishbansalofficial/tollgate/internal/api"
)

// OpenAICompat talks to any provider exposing the OpenAI chat completions API
// (Groq, Ollama, the mock). No translation is needed, only transport.
type OpenAICompat struct {
	name    string
	baseURL string // e.g. https://api.groq.com/openai/v1
	hdr     http.Header
	client  *http.Client
}

func NewOpenAICompat(name, baseURL, apiKey string, client *http.Client) *OpenAICompat {
	hdr := http.Header{}
	if apiKey != "" {
		hdr.Set("Authorization", "Bearer "+apiKey)
	}
	return &OpenAICompat{name: name, baseURL: strings.TrimRight(baseURL, "/"), hdr: hdr, client: client}
}

func (p *OpenAICompat) Name() string { return p.name }

func (p *OpenAICompat) Chat(ctx context.Context, req *api.ChatRequest) (*api.ChatResponse, error) {
	resp, err := post(ctx, p.client, p.name, p.baseURL+"/chat/completions", p.hdr, req)
	if err != nil {
		return nil, err
	}
	var out api.ChatResponse
	if err := decode(ctx, p.name, resp, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AsProviderError is a small helper for callers using errors.As.
func AsProviderError(err error) (*ProviderError, bool) {
	var pe *ProviderError
	ok := errors.As(err, &pe)
	return pe, ok
}
