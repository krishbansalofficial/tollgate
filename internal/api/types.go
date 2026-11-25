// Package api holds Tollgate's canonical request/response types. They are the
// OpenAI chat completions schema; every provider adapter translates to and
// from these at its boundary.
//
// Fields the gateway does not inspect but must forward intact (content parts,
// tools, stop) are kept as json.RawMessage so their exact shape survives.
// Fields not listed here are dropped: a documented limitation.
package api

import (
	"encoding/json"
	"fmt"
	"strings"
)

type ChatRequest struct {
	Model            string          `json:"model"`
	Messages         []Message       `json:"messages"`
	Temperature      *float64        `json:"temperature,omitempty"` // pointer: absent ≠ 0 (matters for caching)
	TopP             *float64        `json:"top_p,omitempty"`
	MaxTokens        *int            `json:"max_tokens,omitempty"`
	Stop             json.RawMessage `json:"stop,omitempty"` // string or []string
	Seed             *int64          `json:"seed,omitempty"`
	PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
	Stream           bool            `json:"stream,omitempty"`
	Tools            json.RawMessage `json:"tools,omitempty"`
	ToolChoice       json.RawMessage `json:"tool_choice,omitempty"`
	ResponseFormat   json.RawMessage `json:"response_format,omitempty"`
	User             string          `json:"user,omitempty"`
}

type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"` // string or []content part
	Name       string          `json:"name,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ErrorResponse is the OpenAI error envelope; SDKs parse error.message from it.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// TextContent returns the message text, joining text parts of a content-part
// array; it returns "" when content holds anything else.
func (m Message) TextContent() string {
	s, _ := m.Text()
	return s
}

// Text returns the message content as plain text. Content is either a JSON
// string or an array of parts; only "text" parts can be represented as text,
// so any other part type (images, audio) is an error.
func (m Message) Text() (string, error) {
	if len(m.Content) == 0 || string(m.Content) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return "", fmt.Errorf("content must be a string or an array of parts")
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			return "", fmt.Errorf("content part type %q is not supported", p.Type)
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

// StringContent encodes s as a JSON string for Message.Content.
func StringContent(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// EstimatePromptTokens is a deliberately crude estimate (~4 bytes/token) used
// only to size the rate-limit reservation, which is reconciled with real
// usage afterwards. A real tokenizer per model isn't worth hot-path CPU.
func EstimatePromptTokens(req *ChatRequest) int {
	n := 0
	for _, m := range req.Messages {
		n += len(m.Content)/4 + 4 // +4: per-message role/formatting overhead
	}
	return n + len(req.Tools)/4
}
