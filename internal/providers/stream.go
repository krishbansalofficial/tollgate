package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/krishbansalofficial/tollgate/internal/api"
)

// sseStream is the shared plumbing for every SSE-based adapter: it owns the
// response body and turns read failures into the right error type. Each
// adapter supplies decode, which maps one SSE event to zero or one chunks
// (or io.EOF when the provider signals the end).
type sseStream struct {
	ctx      context.Context
	provider string
	body     io.ReadCloser
	sse      *sseReader
	decode   func(event string, data []byte) (*api.ChatChunk, error)
	// endOnEOF: the provider has no explicit end marker (Gemini), so a clean
	// EOF between events is the normal end. Others treat it as truncation.
	endOnEOF bool
}

func newSSEStream(ctx context.Context, provider string, resp *http.Response, decode func(string, []byte) (*api.ChatChunk, error)) *sseStream {
	return &sseStream{ctx: ctx, provider: provider, body: resp.Body, sse: newSSEReader(resp.Body), decode: decode}
}

func (s *sseStream) Recv() (*api.ChatChunk, error) {
	for {
		event, data, err := s.sse.next()
		if err != nil {
			if err == io.EOF && s.endOnEOF {
				return nil, io.EOF
			}
			return nil, s.readErr(err)
		}
		chunk, err := s.decode(event, data)
		if err != nil || chunk != nil {
			return chunk, err
		}
		// nil, nil: an event with nothing to forward (ping, block start...).
	}
}

func (s *sseStream) Close() error { return s.body.Close() }

// readErr: a cancelled request is the caller's doing, not the provider's;
// anything else mid-stream is an upstream fault.
func (s *sseStream) readErr(err error) error {
	if ctxErr := s.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &ProviderError{Provider: s.provider, Retryable: true, Message: "stream ended unexpectedly"}
	}
	return &ProviderError{Provider: s.provider, Retryable: true, Message: "stream read: " + err.Error()}
}

// streamError turns an in-stream {"error":{...}} payload into a ProviderError.
func streamError(provider string, data []byte) error {
	var env api.ErrorResponse
	if json.Unmarshal(data, &env) == nil && env.Error.Message != "" {
		return &ProviderError{Provider: provider, Status: http.StatusBadGateway, Retryable: true, Message: env.Error.Message, FromEnvelope: true}
	}
	return &ProviderError{Provider: provider, Status: http.StatusBadGateway, Retryable: true, Message: "malformed stream error event"}
}

func sseHeaders(h http.Header) http.Header {
	h = h.Clone()
	h.Set("Accept", "text/event-stream")
	return h
}
