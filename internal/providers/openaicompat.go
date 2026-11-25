package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

// ChatStream asks for usage in the final chunk (stream_options.include_usage),
// which OpenAI-compatible servers otherwise omit from streams.
func (p *OpenAICompat) ChatStream(ctx context.Context, req *api.ChatRequest) (Stream, error) {
	up := *req
	up.Stream = true
	up.StreamOptions = &api.StreamOptions{IncludeUsage: true}
	resp, err := post(ctx, p.client, p.name, p.baseURL+"/chat/completions", sseHeaders(p.hdr), &up)
	if err != nil {
		return nil, err
	}
	return newSSEStream(ctx, p.name, resp, func(_ string, data []byte) (*api.ChatChunk, error) {
		if string(data) == "[DONE]" {
			return nil, io.EOF
		}
		var c struct {
			api.ChatChunk
			Error *api.ErrorBody `json:"error"` // some servers report mid-stream failures in-band
		}
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, &ProviderError{Provider: p.name, Retryable: true, Message: "decode chunk: " + err.Error()}
		}
		if c.Error != nil {
			return nil, &ProviderError{Provider: p.name, Status: http.StatusBadGateway, Retryable: true, Message: c.Error.Message, FromEnvelope: true}
		}
		return &c.ChatChunk, nil
	}), nil
}

// AsProviderError is a small helper for callers using errors.As.
func AsProviderError(err error) (*ProviderError, bool) {
	var pe *ProviderError
	ok := errors.As(err, &pe)
	return pe, ok
}
