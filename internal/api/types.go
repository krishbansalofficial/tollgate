// Package api holds Tollgate's canonical request/response types. They are the
// OpenAI chat completions schema; every provider adapter translates to and
// from these at its boundary.
//
// Fields the gateway does not inspect but must forward intact (content parts,
// tools, stop) are kept as json.RawMessage so their exact shape survives.
// Fields not listed here are dropped: a documented limitation.
package api

import "encoding/json"

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

// TextContent returns the message content as a string when it is a plain JSON
// string, and "" otherwise (content-part arrays).
func (m Message) TextContent() string {
	var s string
	if json.Unmarshal(m.Content, &s) != nil {
		return ""
	}
	return s
}
