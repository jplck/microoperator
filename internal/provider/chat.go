//go:build darwin || linux

package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	rand "math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jplck/microoperator/internal/protocol"
	"github.com/jplck/microoperator/internal/state"
)

func NewClient() *http.Client {
	return &http.Client{
		Timeout: state.ProviderTimeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: state.ProviderTimeout,
			IdleConnTimeout: 30 * time.Second, MaxIdleConns: 64,
		},
		// Never forward credentials to a redirect or bypass a configured gateway.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type chatUsage struct {
	Input  *int64 `json:"prompt_tokens"`
	Output *int64 `json:"completion_tokens"`
	Total  *int64 `json:"total_tokens"`
}

type chatReply struct {
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Content      *string         `json:"content"`
			ToolCalls    json.RawMessage `json:"tool_calls"`
			FunctionCall json.RawMessage `json:"function_call"`
		} `json:"message"`
		Delta struct {
			Content      *string         `json:"content"`
			ToolCalls    json.RawMessage `json:"tool_calls"`
			FunctionCall json.RawMessage `json:"function_call"`
		} `json:"delta"`
		Finish *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage      `json:"usage"`
	Error json.RawMessage `json:"error"`
}

func present(data json.RawMessage) bool {
	return len(data) != 0 && string(data) != "null" && string(data) != "[]"
}

func decodeProvider(data []byte, target any) error {
	if !utf8.Valid(data) {
		return errors.New("invalid provider UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := protocol.ScanJSONValue(d, 0); err != nil {
		return errors.New("invalid provider JSON")
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("invalid provider JSON")
	}
	if err := json.Unmarshal(data, target); err != nil {
		return errors.New("invalid provider response shape")
	}
	return nil
}

func validUsage(usage *chatUsage, maxOutput int64) bool {
	return usage != nil && usage.Input != nil && usage.Output != nil && usage.Total != nil &&
		*usage.Input >= 0 && *usage.Input <= 1_000_000_000 &&
		*usage.Output >= 0 && *usage.Output <= maxOutput && *usage.Total == *usage.Input+*usage.Output
}

func Request(ctx context.Context, client *http.Client, provider state.ProviderConfig, model state.ModelConfig,
	body []byte, stream bool, key string, now time.Time, attempt int64) state.ProviderResult {
	unknown := state.ProviderResult{Reason: "provider outcome or usage unknown; reservation retained"}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(provider.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return state.ProviderResult{Known: true, Reason: "provider request invalid before sending"}
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		unknown.Reason = "provider transport failed; outcome unknown and reservation retained"
		if ctx.Err() != nil {
			unknown.Reason = "provider call canceled or deadline exceeded after admission; outcome unknown and reservation retained"
		}
		return unknown
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		delay := time.Duration(1<<min(attempt, 6))*time.Second + time.Duration(rand.IntN(1000))*time.Millisecond
		var cooldown time.Time
		value := response.Header.Get("Retry-After")
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 && seconds <= 86400*365 {
			delay = time.Duration(seconds) * time.Second
		} else if date, err := http.ParseTime(value); err == nil {
			cooldown, delay = date, 0
			if cooldown.Before(now) {
				cooldown = now
			}
		}
		return state.ProviderResult{Known: true, Reason: "provider throttled request", Retry: true, Cooldown: cooldown, RetryDelay: delay}
	}
	switch response.StatusCode {
	case 400, 401, 403, 404, 422:
		return state.ProviderResult{Known: true, Reason: "provider rejected request (HTTP " + strconv.Itoa(response.StatusCode) + ")"}
	}
	if response.StatusCode != http.StatusOK {
		unknown.Reason = "provider returned HTTP " + strconv.Itoa(response.StatusCode) + "; usage unknown and reservation retained"
		return unknown
	}
	var text string
	var actions []state.ModelToolCall
	var usage *chatUsage
	if stream {
		text, usage, err = readChatStream(response.Body)
	} else {
		var data []byte
		data, err = io.ReadAll(io.LimitReader(response.Body, state.MaxProviderBytes+1))
		if err == nil && len(data) > state.MaxProviderBytes {
			err = errors.New("provider response too large")
		}
		var reply chatReply
		if err == nil {
			err = decodeProvider(data, &reply)
		}
		if err == nil {
			if present(reply.Error) || len(reply.Choices) != 1 || reply.Choices[0].Index != 0 ||
				present(reply.Choices[0].Message.FunctionCall) || reply.Choices[0].Finish == nil ||
				(*reply.Choices[0].Finish != "stop" && *reply.Choices[0].Finish != "length" && *reply.Choices[0].Finish != "tool_calls") {
				err = errors.New("unsupported provider completion")
			} else {
				usage = reply.Usage
				if reply.Choices[0].Message.Content != nil {
					text = *reply.Choices[0].Message.Content
				}
				if *reply.Choices[0].Finish == "tool_calls" {
					err = decodeProvider(reply.Choices[0].Message.ToolCalls, &actions)
					var request struct {
						Tools []state.ModelFunction `json:"tools"`
					}
					if decodeErr := json.Unmarshal(body, &request); decodeErr != nil {
						err = decodeErr
					}
					if len(actions) != 1 || len(request.Tools) == 0 {
						err = errors.New("unrequested or parallel tool call")
					} else {
						a := &actions[0]
						allowed := false
						var selected state.FunctionSchema
						for _, function := range request.Tools {
							if function.Function.Name == a.Function.Name {
								allowed = true
								selected = function.Function
							}
						}
						if !allowed || a.Type != "function" || !state.CommandKeyPattern.MatchString(a.ID) || strings.Contains(a.ID, key) || len(a.Function.Arguments) > state.MaxEventBytes {
							err = errors.New("invalid model tool call")
						}
						var arguments map[string]any
						if decodeErr := protocol.DecodeJSON([]byte(a.Function.Arguments), &arguments); decodeErr != nil {
							err = decodeErr
						} else {
							redactArgumentStrings(arguments, key)
							encoded, encodeErr := json.Marshal(arguments)
							if encodeErr != nil {
								err = encodeErr
							} else {
								a.Function.Arguments = string(encoded)
							}
						}
						if allowed && err == nil {
							err = state.ValidateArguments(selected, a.Function.Arguments)
						}
					}
				} else if present(reply.Choices[0].Message.ToolCalls) || reply.Choices[0].Message.Content == nil {
					err = errors.New("invalid completion content")
				}
			}
		}
	}
	if err != nil || len(text) > state.MaxModelText {
		unknown.Reason = "provider response malformed, incomplete, unsupported, or oversized; reservation retained"
		return unknown
	}
	if !validUsage(usage, model.MaxOutputTokens) {
		unknown.Reason = "provider usage missing or invalid; reservation retained"
		return unknown
	}
	text = strings.ReplaceAll(text, key, "[redacted]")
	if len(text) > state.MaxModelText || protocol.CheckFrame(protocol.Message{Type: "model.result", ID: "call_" + strings.Repeat("0", 32), Data: text}) != nil {
		return state.ProviderResult{Known: true, Input: *usage.Input, Output: *usage.Output, Reason: "encoded model output exceeds the worker response limit"}
	}
	return state.ProviderResult{Text: text, Input: *usage.Input, Output: *usage.Output, Known: true, Actions: actions}
}

func redactArgumentStrings(value any, key string) {
	switch value := value.(type) {
	case map[string]any:
		for name, item := range value {
			if text, ok := item.(string); ok {
				value[name] = strings.ReplaceAll(text, key, "[redacted]")
			} else {
				redactArgumentStrings(item, key)
			}
		}
	case []any:
		for i, item := range value {
			if text, ok := item.(string); ok {
				value[i] = strings.ReplaceAll(text, key, "[redacted]")
			} else {
				redactArgumentStrings(item, key)
			}
		}
	}
}

// readChatStream consumes SSE through [DONE], including a separate usage chunk.
// A partial stream is never a successful completion or a reason to replay it.
// The broker keeps its provider slot until this body has been closed.
func readChatStream(body io.Reader) (string, *chatUsage, error) {
	limited := &io.LimitedReader{R: body, N: state.MaxProviderBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), protocol.MaxFrame+1)
	var text strings.Builder
	var event strings.Builder
	var usage *chatUsage
	finished, done := false, false
	consume := func() error {
		data := strings.TrimSuffix(event.String(), "\n")
		event.Reset()
		if data == "" {
			return nil
		}
		if data == "[DONE]" {
			done = true
			return nil
		}
		if done {
			return errors.New("data after stream completion")
		}
		var reply chatReply
		if err := decodeProvider([]byte(data), &reply); err != nil {
			return err
		}
		if present(reply.Error) || len(reply.Choices) > 1 {
			return errors.New("invalid stream choice")
		}
		if reply.Usage != nil {
			if usage != nil {
				return errors.New("duplicate stream usage")
			}
			usage = reply.Usage
		}
		if len(reply.Choices) == 1 {
			choice := reply.Choices[0]
			if choice.Index != 0 || present(choice.Delta.ToolCalls) || present(choice.Delta.FunctionCall) || finished {
				return errors.New("unsupported stream delta")
			}
			if choice.Delta.Content != nil {
				text.WriteString(*choice.Delta.Content)
			}
			if choice.Finish != nil {
				if *choice.Finish != "stop" && *choice.Finish != "length" {
					return errors.New("unsupported stream finish")
				}
				finished = true
			}
		}
		if text.Len() > state.MaxModelText {
			return errors.New("stream output too large")
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := consume(); err != nil {
				return "", nil, err
			}
			if done {
				break
			}
		} else if strings.HasPrefix(line, "data:") {
			event.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			event.WriteByte('\n')
			if event.Len() > protocol.MaxFrame {
				return "", nil, errors.New("SSE event too large")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", nil, err
	}
	if !done || !finished || limited.N <= 0 {
		return "", nil, errors.New("incomplete or oversized provider stream")
	}
	return text.String(), usage, nil
}
