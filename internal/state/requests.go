package state

import (
	"encoding/json"

	"time"
)

const (
	ProviderTimeout  = 60 * time.Second
	MaxProviderBytes = 1 << 20
	MaxModelText     = 32768
)

type ChatMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolCalls  []ModelToolCall `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

func ModelRequest(cfg Configuration, record SystemRecord, prompt string, stream bool) ([]byte, int64, error) {
	return ConversationRequest(cfg, record, []ChatMessage{{Role: "user", Content: prompt}}, stream)
}

func ConversationRequest(cfg Configuration, record SystemRecord, conversation []ChatMessage, stream bool) ([]byte, int64, error) {
	cfg = cfg.WithLocalTools(record.LocalTools)
	model, ok := cfg.Models[record.Grants.Model]
	if !ok {
		return nil, 0, Invalid("model", "grant no longer exists")
	}
	messages := []ChatMessage{{Role: "system", Content: record.Configuration.Operator.Prompt}}
	var functions []ModelFunction
	for _, pin := range record.Grants.OperatorTools {
		Tool, ok := cfg.Tool(pin.Name)
		if !ok {
			return nil, 0, Invalid("tools", "pinned definition is unavailable")
		}
		if Tool.Kind == "executable" {
			function, err := ExecutableSchema(pin.Name, Tool)
			if err != nil {
				return nil, 0, err
			}
			functions = append(functions, function)
			continue
		}
		messages = append(messages, ChatMessage{Role: "system", Content: Tool.Content})
	}
	if stream && len(functions) > 0 {
		return nil, 0, Invalid("stream", "streaming executable tool calls is not enabled; use non-streaming turns")
	}
	messages = append(messages, conversation...)
	request := struct {
		Model             string          `json:"model"`
		Messages          []ChatMessage   `json:"messages"`
		MaxTokens         int64           `json:"max_tokens"`
		Stream            bool            `json:"stream"`
		Tools             []ModelFunction `json:"tools,omitempty"`
		ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
		StreamOptions     *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options,omitempty"`
	}{Model: model.Model, Messages: messages, MaxTokens: model.MaxOutputTokens, Stream: stream, Tools: functions}
	if len(functions) > 0 {
		disabled := false
		request.ParallelToolCalls = &disabled
	}
	if stream {
		request.StreamOptions = &struct {
			IncludeUsage bool `json:"include_usage"`
		}{true}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, 0, err
	}
	if len(body) > maxFrame {
		return nil, 0, Invalid("model.call", "encoded provider request exceeds 64 KiB")
	}
	headroom := model.InputHeadroomPercent
	if headroom == 0 {
		headroom = 20
	}
	// One token per serialized byte deliberately overestimates ordinary text.
	// This is not a tokenizer or a price guarantee; unknown usage stays reserved.
	input := (int64(len(body))*(100+headroom) + 99) / 100
	return body, input + model.MaxOutputTokens, nil
}

type ProviderResult struct {
	Text          string
	Input, Output int64
	Known         bool
	Reason        string
	Retry         bool
	Cooldown      time.Time
	RetryDelay    time.Duration
	Actions       []ModelToolCall
}
