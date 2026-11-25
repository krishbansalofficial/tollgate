package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/krishbansalofficial/tollgate/internal/api"
)

func TestAnthropicTranslate(t *testing.T) {
	p := NewAnthropic("http://x", "k", 1024, nil)
	tests := []struct {
		name    string
		in      string
		want    string // expected JSON of the translated request
		wantErr string
	}{
		{
			name: "system hoisted, defaults filled, sampling dropped",
			in:   `{"model":"claude-haiku-5-5","temperature":0,"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`,
			want: `{"model":"claude-haiku-5-5","max_tokens":1024,"system":"be brief","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
		},
		{
			name: "content parts, stop string, explicit max_tokens",
			in:   `{"model":"m","max_tokens":5,"stop":"END","messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]},{"role":"assistant","content":"c"}]}`,
			want: `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":[{"type":"text","text":"ab"}]},{"role":"assistant","content":[{"type":"text","text":"c"}]}],"stop_sequences":["END"]}`,
		},
		{name: "image part rejected", in: `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, wantErr: "image_url"},
		{name: "tools rejected", in: `{"model":"m","tools":[{}],"messages":[{"role":"user","content":"hi"}]}`, wantErr: "tool calling"},
		{name: "tool role rejected", in: `{"model":"m","messages":[{"role":"tool","content":"x"}]}`, wantErr: "role tool"},
		{name: "only system", in: `{"model":"m","messages":[{"role":"system","content":"x"}]}`, wantErr: "user message"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, perr := p.translate(req2(t, tt.in))
			if tt.wantErr != "" {
				if perr == nil || !strings.Contains(perr.Message, tt.wantErr) || !perr.ClientFault() {
					t.Fatalf("want client-fault error containing %q, got %v", tt.wantErr, perr)
				}
				return
			}
			if perr != nil {
				t.Fatal(perr)
			}
			b, _ := json.Marshal(got)
			if string(b) != tt.want {
				t.Errorf("got  %s\nwant %s", b, tt.want)
			}
		})
	}
}

func TestAnthropicChat(t *testing.T) {
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant",
			"content":[{"type":"thinking","thinking":""},{"type":"text","text":"Hello"},{"type":"text","text":" there"}],
			"stop_reason":"max_tokens","usage":{"input_tokens":7,"output_tokens":3}}`)
	}))
	defer srv.Close()

	resp, err := NewAnthropic(srv.URL, "secret", 1024, srv.Client()).Chat(context.Background(), req2(t, `{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Get("x-api-key") != "secret" || hdr.Get("anthropic-version") != anthropicVersion {
		t.Errorf("headers: %v", hdr)
	}
	c := resp.Choices[0]
	if c.Message.TextContent() != "Hello there" || c.FinishReason != "length" || resp.Usage.TotalTokens != 10 {
		t.Errorf("resp: %+v %s", resp, c.Message.Content)
	}
}

func TestAnthropicErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(529)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	}))
	defer srv.Close()
	_, err := NewAnthropic(srv.URL, "k", 1024, srv.Client()).Chat(context.Background(), req2(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	pe, ok := AsProviderError(err)
	if !ok || pe.Status != 529 || !pe.Retryable || pe.Message != "Overloaded" || !pe.FromEnvelope {
		t.Fatalf("got %+v", pe)
	}
}

func req2(t *testing.T, s string) *api.ChatRequest {
	t.Helper()
	var r api.ChatRequest
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

