//go:build darwin || linux

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const usageChunk = `{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}`

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
		{"oversized", false, 200, strings.Repeat(" ", maxProviderBytes+1), false},
		{"tool call", false, 200, `{"choices":[{"index":0,"message":{"content":"","tool_calls":[{}]},"finish_reason":"tool_calls"}],"usage":` + usageChunk + `}`, false},
		{"unsupported status", false, 503, fixtureProviderSecret, false},
		{"explicit rejection", false, 401, fixtureProviderSecret, true},
		{"stream", true, 200, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: {\"choices\":[],\"usage\":" + usageChunk + "}\n\ndata: [DONE]\n\n", true},
		{"stream disconnect", true, 200, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n", false},
		{"stream missing usage", true, 200, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", false},
		{"oversized event", true, 200, "data: " + strings.Repeat("x", maxFrame+1) + "\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) }))
			defer server.Close()
			client := providerClient()
			defer client.CloseIdleConnections()
			result := requestModel(context.Background(), client, providerConfig{BaseURL: server.URL}, modelConfig{MaxOutputTokens: 1024},
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
	client := providerClient()
	defer client.CloseIdleConnections()
	result := requestModel(context.Background(), client, providerConfig{BaseURL: source.URL}, modelConfig{MaxOutputTokens: 1024},
		[]byte("{}"), false, fixtureProviderSecret, time.Now(), 1)
	if result.Known || redirected.Load() != 0 {
		t.Fatal("provider redirect escaped configured destination")
	}
	success := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { completion(w, "provider echoed "+fixtureProviderSecret) }))
	defer success.Close()
	result = requestModel(context.Background(), client, providerConfig{BaseURL: success.URL}, modelConfig{MaxOutputTokens: 1024},
		[]byte("{}"), false, fixtureProviderSecret, time.Now(), 1)
	if !result.Known || strings.Contains(result.Text, fixtureProviderSecret) || !strings.Contains(result.Text, "[redacted]") {
		t.Fatalf("success credential handling: %+v", result)
	}
}

func TestProviderDeadlineAndStreamSlot(t *testing.T) {
	entered := make(chan struct{}, 2)
	closed := make(chan struct{}, 2)
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
		w.(http.Flusher).Flush()
		entered <- struct{}{}
		<-r.Context().Done()
		closed <- struct{}{}
	})
	q := cfg.QuotaGroups["account"]
	q.BurstRequests, q.MaxConcurrent = 2, 1
	cfg.QuotaGroups["account"] = q
	b, _, id := fixtureBroker(t, cfg)
	first := fixtureCall(t, b, id, "stream", true)
	second := queuedCall(t, b, id, "queued")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan executionRecord, 1)
	go func() {
		result, err := b.call(ctx, first)
		if err != nil {
			t.Error(err)
		}
		done <- result
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("stream did not start")
	}
	admitCall(t, b, second, false)
	if count.Load() != 1 {
		t.Fatal("stream released concurrency before close")
	}
	cancel()
	var result executionRecord
	select {
	case result = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not cancel")
	}
	if result.State != "unknown" || result.GoalReserved != first.Reservation {
		t.Fatalf("cancellation accounting: %+v", result)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("transport not closed")
	}
	admitCall(t, b, second, true)
	if count.Load() != 1 {
		t.Fatal("admission unexpectedly sent a second request")
	}
}

func TestBrokerBoundsThrottlingRetries(t *testing.T) {
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "0"); w.WriteHeader(429) })
	q := cfg.QuotaGroups["account"]
	q.BurstRequests = 4
	cfg.QuotaGroups["account"] = q
	b, _, id := fixtureBroker(t, cfg)
	e := fixtureCall(t, b, id, "throttle", false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := b.call(ctx, e)
	if err != nil || result.State != "failed" || result.Attempts != 3 || result.Retries != 2 || result.GoalReserved != 0 || count.Load() != 3 {
		t.Fatalf("retry bound: %+v %v; requests=%d", result, err, count.Load())
	}
	quotas, err := b.quotas(ctx)
	if err != nil || quotas[0].Throttles != 3 || quotas[0].ReservedRateTokens != 3*e.Reservation {
		t.Fatalf("throttling metrics: %+v %v", quotas, err)
	}
}
