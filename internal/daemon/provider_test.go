//go:build darwin || linux

package daemon

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/jplck/microoperator/internal/state"
)

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
	done := make(chan state.ExecutionRecord, 1)
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
	var result state.ExecutionRecord
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
