//go:build linux

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jplck/microoperator/internal/protocol"
	"github.com/jplck/microoperator/internal/state"
)

const usageChunk = `{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}`
const fixtureProviderSecret = "fixture-provider-secret-not-real"

type deadlineTransport func(*http.Request) (*http.Response, error)

func (transport deadlineTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestProviderRequestDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seconds int64
		parent  time.Duration
		want    time.Duration
	}{
		{"default", 0, 0, 60 * time.Second},
		{"configured", 600, 0, 600 * time.Second},
		{"earlier parent", 600, 5 * time.Second, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.parent != 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.parent)
				defer cancel()
			}
			client := NewClient()
			defer client.CloseIdleConnections()
			transport := client.Transport.(*http.Transport)
			if client.Timeout != 0 || transport.ResponseHeaderTimeout != 0 {
				t.Fatal("shared HTTP client overrides the configured request deadline")
			}
			client.Transport = deadlineTransport(func(r *http.Request) (*http.Response, error) {
				deadline, ok := r.Context().Deadline()
				if remaining := time.Until(deadline); !ok || remaining > tc.want || remaining < tc.want-time.Second {
					t.Fatalf("request deadline = %s, want %s", remaining, tc.want)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(
					`{"choices":[{"index":0,"message":{"content":"ok"},"finish_reason":"stop"}],"usage":` + usageChunk + `}`)),
					Header: make(http.Header)}, nil
			})
			result := Request(ctx, client, state.ProviderConfig{BaseURL: "http://127.0.0.1/v1", TimeoutSeconds: tc.seconds},
				state.ModelConfig{MaxOutputTokens: 1024}, []byte("{}"), false, "", time.Now(), 1)
			if !result.Known || result.Reason != "" || result.Text != "ok" {
				t.Fatalf("request failed: %+v", result)
			}
		})
	}
}

func TestProviderDeadlineAndCancellationReasons(t *testing.T) {
	for _, phase := range []string{"headers", "body", "stream"} {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deadline-%t", phase, deadline), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				entered := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					if phase != "headers" {
						w.WriteHeader(http.StatusOK)
						w.(http.Flusher).Flush()
					}
					close(entered)
					if !deadline {
						cancel()
					}
					<-r.Context().Done()
				}))
				defer server.Close()
				if deadline {
					var finish context.CancelFunc
					ctx, finish = context.WithTimeout(ctx, 200*time.Millisecond)
					defer finish()
				}
				client := NewClient()
				defer client.CloseIdleConnections()
				result := Request(ctx, client, state.ProviderConfig{BaseURL: server.URL, TimeoutSeconds: 600},
					state.ModelConfig{MaxOutputTokens: 1024}, []byte("{}"), phase == "stream", fixtureProviderSecret, time.Now(), 1)
				select {
				case <-entered:
				default:
					t.Fatal("request was not dispatched")
				}
				want := "provider request canceled after admission; outcome unknown and reservation retained"
				if deadline {
					want = "provider request deadline exceeded after admission; outcome unknown and reservation retained"
				}
				if result.Known || result.Retry || result.Reason != want {
					t.Fatalf("failure lost its cause or was retried: %+v", result)
				}
			})
		}
	}
}

func completion(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	data, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": text}, "finish_reason": "stop"}},
		"usage":   json.RawMessage(usageChunk),
	})
	w.Write(data)
}

func TestProviderBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream bool
		status int
		body   string
		known  bool
	}{
		{"json", false, 200, `{"choices":[{"index":0,"message":{"content":"ok"},"finish_reason":"stop"}],"usage":` + usageChunk + `}`, true},
		{"missing usage", false, 200, `{"choices":[{"index":0,"message":{"content":"ok"},"finish_reason":"stop"}]}`, false},
		{"duplicate usage", false, 200, `{"usage":null,"usage":` + usageChunk + `}`, false},
		{"malformed", false, 200, `{"choices":`, false},
		{"oversized", false, 200, strings.Repeat(" ", state.MaxProviderBytes+1), false},
		{"tool call", false, 200, `{"choices":[{"index":0,"message":{"content":"","tool_calls":[{}]},"finish_reason":"tool_calls"}],"usage":` + usageChunk + `}`, false},
		{"unsupported status", false, 503, fixtureProviderSecret, false},
		{"explicit rejection", false, 401, fixtureProviderSecret, true},
		{"stream", true, 200, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: {\"choices\":[],\"usage\":" + usageChunk + "}\n\ndata: [DONE]\n\n", true},
		{"stream disconnect", true, 200, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n", false},
		{"stream missing usage", true, 200, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", false},
		{"oversized event", true, 200, "data: " + strings.Repeat("x", protocol.MaxFrame+1) + "\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) }))
			defer server.Close()
			client := NewClient()
			defer client.CloseIdleConnections()
			result := Request(context.Background(), client, state.ProviderConfig{BaseURL: server.URL}, state.ModelConfig{MaxOutputTokens: 1024},
				[]byte(`{"messages":[]}`), tc.stream, fixtureProviderSecret, time.Now(), 1)
			if result.Known != tc.known {
				t.Fatalf("usage certainty: %+v", result)
			}
			if strings.Contains(fmt.Sprint(result), fixtureProviderSecret) {
				t.Fatal("provider error leaked credentials")
			}
			if tc.name == "json" || tc.name == "stream" {
				if result.Text != "ok" || result.Input != 11 || result.Output != 7 || result.Reason != "" {
					t.Fatalf("completion: %+v", result)
				}
			}
		})
	}
}

func TestProviderRejectsRedirectsAndRedactsSuccess(t *testing.T) {
	var redirected atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client := NewClient()
	defer client.CloseIdleConnections()
	result := Request(context.Background(), client, state.ProviderConfig{BaseURL: source.URL}, state.ModelConfig{MaxOutputTokens: 1024},
		[]byte("{}"), false, fixtureProviderSecret, time.Now(), 1)
	if result.Known || redirected.Load() != 0 {
		t.Fatal("provider redirect escaped configured destination")
	}
	success := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { completion(w, "provider echoed "+fixtureProviderSecret) }))
	defer success.Close()
	result = Request(context.Background(), client, state.ProviderConfig{BaseURL: success.URL}, state.ModelConfig{MaxOutputTokens: 1024},
		[]byte("{}"), false, fixtureProviderSecret, time.Now(), 1)
	if !result.Known || strings.Contains(result.Text, fixtureProviderSecret) || !strings.Contains(result.Text, "[redacted]") {
		t.Fatalf("success credential handling: %+v", result)
	}
}

func TestKeylessCompletionsAndToolCalls(t *testing.T) {
	tool, _ := state.BuiltinTool("runtime.text.analyze")
	schema, err := state.ExecutableSchema("runtime.text.analyze", tool)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"tools": []state.ModelFunction{schema}})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"text", "stream", "tool"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if len(r.Header.Values("Authorization")) != 0 {
					t.Error("keyless request sent an Authorization header")
				}
				switch mode {
				case "text":
					completion(w, "local text")
				case "stream":
					fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"local text\"},\"finish_reason\":\"stop\"}]}\n\n"+
						"data: {\"choices\":[],\"usage\":%s}\n\ndata: [DONE]\n\n", usageChunk)
				case "tool":
					action := state.ModelToolCall{ID: "local-action", Type: "function"}
					action.Function.Name, action.Function.Arguments = schema.Function.Name, `{"text":"local text"}`
					json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"tool_calls": []state.ModelToolCall{action}}, "finish_reason": "tool_calls"}}, "usage": json.RawMessage(usageChunk)})
				}
			}))
			defer server.Close()
			client := NewClient()
			defer client.CloseIdleConnections()
			result := Request(context.Background(), client, state.ProviderConfig{Adapter: "ollama", BaseURL: server.URL},
				state.ModelConfig{MaxOutputTokens: 1024}, body, mode == "stream", "", time.Now(), 1)
			if !result.Known || result.Reason != "" {
				t.Fatalf("keyless completion failed: %+v", result)
			}
			if mode == "tool" {
				if len(result.Actions) != 1 || result.Actions[0].ID != "local-action" || result.Actions[0].Function.Arguments != `{"text":"local text"}` {
					t.Fatalf("keyless tool call corrupted: %+v", result)
				}
			} else if result.Text != "local text" {
				t.Fatalf("keyless text corrupted: %q", result.Text)
			}
		})
	}
}
func TestToolDecisionsValidateAndRedactBeforeWorkerDelivery(t *testing.T) {
	tool, _ := state.BuiltinTool("runtime.text.analyze")
	schema, err := state.ExecutableSchema("runtime.text.analyze", tool)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"tools": []state.ModelFunction{schema}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, id, args string
		known          bool
	}{
		{"escaped credential", "action", `{"text":"\u0066ixture-secret"}`, true},
		{"credential in id", "fixture-secret", `{"text":"safe"}`, false},
		{"credential in field", "action", `{"text":"safe","fixture-secret":"value"}`, false},
		{"invalid shape", "action", `{"text":{"nested":"fixture-secret"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			action := state.ModelToolCall{ID: tc.id, Type: "function"}
			action.Function.Name = schema.Function.Name
			action.Function.Arguments = tc.args
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"content": nil, "tool_calls": []state.ModelToolCall{action}}, "finish_reason": "tool_calls"}}, "usage": json.RawMessage(usageChunk)})
				if err != nil {
					t.Error(err)
					return
				}
				w.Write(data)
			}))
			defer server.Close()
			client := NewClient()
			defer client.CloseIdleConnections()
			result := Request(context.Background(), client, state.ProviderConfig{BaseURL: server.URL}, state.ModelConfig{MaxOutputTokens: 1024}, body, false, "fixture-secret", time.Now(), 1)
			if result.Known != tc.known || strings.Contains(fmt.Sprint(result), "fixture-secret") {
				t.Fatalf("unsafe action response: %+v", result)
			}
			if tc.known && (len(result.Actions) != 1 || !strings.Contains(result.Actions[0].Function.Arguments, "[redacted]")) {
				t.Fatalf("escaped credential not redacted: %+v", result)
			}
		})
	}
}
