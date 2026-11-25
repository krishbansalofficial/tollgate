package providers

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An upstream that sends an endless event split across many short data:
// lines must hit the per-event cap, not grow memory without bound.
func TestSSEEventCap(t *testing.T) {
	line := "data: " + strings.Repeat("x", 1000) + "\n"
	body := strings.Repeat(line, (maxSSEEvent/1000)+10) // ~1 MiB+ in one event, no blank line
	_, _, err := newSSEReader(strings.NewReader(body)).next()
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("want ErrTooLong, got %v", err)
	}
}

func geminiSSE(t *testing.T, status int, body string) *Gemini {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return NewGemini(srv.URL, "k", srv.Client())
}

// A Gemini stream that ends before any finishReason is truncation, and must
// surface as a retryable error rather than a clean (silently cut) reply.
func TestGeminiTruncatedStreamIsError(t *testing.T) {
	p := geminiSSE(t, 200, `data: {"candidates":[{"content":{"parts":[{"text":"Hel"}]}}]}`+"\n\n")
	s, err := p.ChatStream(context.Background(), req2(t, `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Recv(); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	_, err = s.Recv()
	if pe, ok := AsProviderError(err); !ok || !pe.Retryable {
		t.Fatalf("want retryable truncation error, got %v", err)
	}
}

func TestGeminiCompleteStreamEndsCleanly(t *testing.T) {
	p := geminiSSE(t, 200, `data: {"candidates":[{"content":{"parts":[{"text":"Hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1}}`+"\n\n")
	s, _ := p.ChatStream(context.Background(), req2(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	defer func() { _ = s.Close() }()
	if _, err := s.Recv(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recv(); err != io.EOF {
		t.Fatalf("want io.EOF after finishReason, got %v", err)
	}
}

// Gemini reports a bad API key as 400; it must not be treated (or relayed)
// as the client's fault.
func TestGeminiInvalidKeyIsNotClientFault(t *testing.T) {
	p := geminiSSE(t, 400, `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT"}}`)
	_, err := p.Chat(context.Background(), req2(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	pe, ok := AsProviderError(err)
	if !ok || pe.ClientFault() || pe.Status != http.StatusUnauthorized {
		t.Fatalf("want non-client-fault 401, got %+v", pe)
	}
	if _, relay := pe.ClientMessage(); relay {
		t.Fatal("credential message must not be relayable")
	}
}

func TestClientMessageFilter(t *testing.T) {
	tests := []struct {
		pe    ProviderError
		relay bool
	}{
		{ProviderError{Message: "max_tokens must be <= 8192", FromEnvelope: true}, true},
		{ProviderError{Message: "max_tokens must be <= 8192", FromEnvelope: false}, false},
		{ProviderError{Message: "Quota exceeded for project 1234", FromEnvelope: true}, false},
		{ProviderError{Message: "Your credit balance is too low; check Billing", FromEnvelope: true}, false},
	}
	for _, tt := range tests {
		if _, ok := tt.pe.ClientMessage(); ok != tt.relay {
			t.Errorf("%q envelope=%v: relay=%v, want %v", tt.pe.Message, tt.pe.FromEnvelope, ok, tt.relay)
		}
	}
}
