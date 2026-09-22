//go:build darwin || linux

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
