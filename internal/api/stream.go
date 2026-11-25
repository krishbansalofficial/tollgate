package api

import "encoding/json"

// StreamOptions mirrors OpenAI's stream_options.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatChunk is one OpenAI chat.completion.chunk, the unit of a streamed reply.
type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"` // null until the last chunk
}

type Delta struct {
	Role      string          `json:"role,omitempty"`
	Content   string          `json:"content,omitempty"`
	ToolCalls json.RawMessage `json:"tool_calls,omitempty"` // passed through from OpenAI-compatible upstreams
}

// HasContent reports whether the chunk carries generated text (TTFT marker).
func (c *ChatChunk) HasContent() bool {
	for _, ch := range c.Choices {
		if ch.Delta.Content != "" || len(ch.Delta.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// TextChunk builds a single-choice chunk; finish may be "".
func TextChunk(role, content, finish string) *ChatChunk {
	ch := ChunkChoice{Delta: Delta{Role: role, Content: content}}
	if finish != "" {
		ch.FinishReason = &finish
	}
	return &ChatChunk{Object: "chat.completion.chunk", Choices: []ChunkChoice{ch}}
}
