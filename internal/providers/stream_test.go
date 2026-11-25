package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/krishbansalofficial/tollgate/internal/api"
	"github.com/krishbansalofficial/tollgate/internal/mockupstream"
)

// sseServer replays a fixed SSE body and records the request.
func sseServer(t *testing.T, body string, got *http.Request, gotBody *[]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got != nil {
			*got = *r
		}
		if gotBody != nil {
			*gotBody, _ = io.ReadAll(r.Body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// collect drains a stream into its text, last finish reason and usage.
func collect(t *testing.T, s Stream) (text, finish string, usage *api.Usage, err error) {
	t.Helper()
	defer func() { _ = s.Close() }()
	var b strings.Builder
	for {
		c, rerr := s.Recv()
		if rerr == io.EOF {
			return b.String(), finish, usage, nil
		}
		if rerr != nil {
			return b.String(), finish, usage, rerr
		}
		for _, ch := range c.Choices {
			b.WriteString(ch.Delta.Content)
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
		if c.Usage != nil {
			usage = c.Usage
		}
	}
}

func TestOpenAICompatStream(t *testing.T) {
	body := `data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}

data: [DONE]

`
	var reqBody []byte
	srv := sseServer(t, body, nil, &reqBody)
	s, err := NewOpenAICompat("p", srv.URL, "", srv.Client()).ChatStream(context.Background(), req("m"))
	if err != nil {
		t.Fatal(err)
	}
	text, finish, usage, err := collect(t, s)
	if err != nil || text != "Hello" || finish != "stop" || usage == nil || usage.TotalTokens != 5 {
		t.Fatalf("text=%q finish=%q usage=%+v err=%v", text, finish, usage, err)
	}
	if !strings.Contains(string(reqBody), `"stream":true`) || !strings.Contains(string(reqBody), `"include_usage":true`) {
		t.Errorf("request body: %s", reqBody)
	}
}

func TestOpenAICompatStreamTruncated(t *testing.T) {
	srv := sseServer(t, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n", nil, nil) // no [DONE]
	s, _ := NewOpenAICompat("p", srv.URL, "", srv.Client()).ChatStream(context.Background(), req("m"))
	_, _, _, err := collect(t, s)
	if pe, ok := AsProviderError(err); !ok || !pe.Retryable {
		t.Fatalf("want retryable ProviderError for a truncated stream, got %v", err)
	}
}

func TestOpenAICompatStreamAgainstMock(t *testing.T) {
	m := mockupstream.New(mockupstream.Config{StreamChunks: 4})
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	s, err := NewOpenAICompat("mock", srv.URL+"/v1", "", srv.Client()).ChatStream(context.Background(), req("m"))
	if err != nil {
		t.Fatal(err)
	}
	text, finish, usage, err := collect(t, s)
	if err != nil || text == "" || finish != "stop" || usage == nil {
		t.Fatalf("text=%q finish=%q usage=%+v err=%v", text, finish, usage, err)
	}
}

func TestOpenAICompatStreamClientCancel(t *testing.T) {
	m := mockupstream.New(mockupstream.Config{StreamChunks: 100, ChunkDelay: 20 * time.Millisecond})
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := NewOpenAICompat("mock", srv.URL+"/v1", "", srv.Client()).ChatStream(ctx, req("m"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recv(); err != nil {
		t.Fatal(err)
	}
	cancel()
	_, _, _, err = collect(t, s)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	for range 100 { // the upstream sees the disconnect and stops generating
		if m.Canceled() == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("upstream never observed the cancellation")
}

func TestAnthropicStream(t *testing.T) {
	body := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":9,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hi"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":" there"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":4}}

event: message_stop
data: {"type":"message_stop"}

`
	var got http.Request
	var reqBody []byte
	srv := sseServer(t, body, &got, &reqBody)
	s, err := NewAnthropic(srv.URL, "k", 1024, srv.Client()).ChatStream(context.Background(), req("claude-haiku-5-5"))
	if err != nil {
		t.Fatal(err)
	}
	text, finish, usage, err := collect(t, s)
	if err != nil || text != "Hi there" || finish != "length" || usage == nil || usage.PromptTokens != 9 || usage.CompletionTokens != 4 {
		t.Fatalf("text=%q finish=%q usage=%+v err=%v", text, finish, usage, err)
	}
	if !strings.Contains(string(reqBody), `"stream":true`) || got.Header.Get("anthropic-version") != anthropicVersion {
		t.Errorf("request: %s %v", reqBody, got.Header)
	}
}

func TestAnthropicStreamErrorEvent(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	srv := sseServer(t, body, nil, nil)
	s, _ := NewAnthropic(srv.URL, "k", 1024, srv.Client()).ChatStream(context.Background(), req("m"))
	_, _, _, err := collect(t, s)
	if pe, ok := AsProviderError(err); !ok || pe.Message != "Overloaded" || !pe.Retryable {
		t.Fatalf("got %v", err)
	}
}

func TestGeminiTranslate(t *testing.T) {
	p := NewGemini("http://x", "k", nil)
	in := `{"model":"gemini-2.5-flash","max_tokens":50,"temperature":0.2,"top_p":0.9,"stop":["END"],
		"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":[{"type":"text","text":"more"}]}]}`
	got, perr := p.translate(req2(t, in))
	if perr != nil {
		t.Fatal(perr)
	}
	b, _ := json.Marshal(got)
	want := `{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"text":"hello"}]},{"role":"user","parts":[{"text":"more"}]}],` +
		`"systemInstruction":{"parts":[{"text":"be brief"}]},"generationConfig":{"maxOutputTokens":50,"stopSequences":["END"],"temperature":0.2,"topP":0.9}}`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
	for _, bad := range []string{
		`{"model":"m","tools":[{}],"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","messages":[{"role":"tool","content":"x"}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url"}]}]}`,
	} {
		if _, perr := p.translate(req2(t, bad)); perr == nil || !perr.ClientFault() {
			t.Errorf("want client fault for %s", bad)
		}
	}
}

func TestGeminiChat(t *testing.T) {
	var got http.Request
	srv := sseServer(t, `{"responseId":"r1","candidates":[{"content":{"role":"model","parts":[{"text":"plan","thought":true},{"text":"Answer"}]},"finishReason":"MAX_TOKENS"}],
		"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"thoughtsTokenCount":10}}`, &got, nil)
	resp, err := NewGemini(srv.URL, "secret", srv.Client()).Chat(context.Background(), req("gemini-2.5-flash"))
	if err != nil {
		t.Fatal(err)
	}
	if got.URL.Path != "/models/gemini-2.5-flash:generateContent" || got.URL.RawQuery != "" || got.Header.Get("x-goog-api-key") != "secret" {
		t.Errorf("request: %s ? %s %v", got.URL.Path, got.URL.RawQuery, got.Header)
	}
	c := resp.Choices[0]
	if c.Message.TextContent() != "Answer" || c.FinishReason != "length" || resp.Usage.CompletionTokens != 12 {
		t.Errorf("resp %+v usage %+v", c, resp.Usage)
	}
}

func TestGeminiStream(t *testing.T) {
	body := `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}],"usageMetadata":{"promptTokenCount":4}}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2}}

`
	var got http.Request
	srv := sseServer(t, body, &got, nil)
	s, err := NewGemini(srv.URL, "k", srv.Client()).ChatStream(context.Background(), req("gemini-2.5-flash"))
	if err != nil {
		t.Fatal(err)
	}
	text, finish, usage, err := collect(t, s)
	if err != nil || text != "Hello" || finish != "stop" || usage == nil || usage.TotalTokens != 6 {
		t.Fatalf("text=%q finish=%q usage=%+v err=%v", text, finish, usage, err)
	}
	if got.URL.Path != "/models/gemini-2.5-flash:streamGenerateContent" || got.URL.RawQuery != "alt=sse" {
		t.Errorf("url %s?%s", got.URL.Path, got.URL.RawQuery)
	}
}

func TestGeminiErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}`)
	}))
	defer srv.Close()
	_, err := NewGemini(srv.URL, "k", srv.Client()).Chat(context.Background(), req("m"))
	if pe, ok := AsProviderError(err); !ok || pe.Status != 400 || pe.Message != "Request contains an invalid argument." || !pe.ClientFault() {
		t.Fatalf("got %v", err)
	}
}
