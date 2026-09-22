//go:build darwin || linux

package daemon

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jplck/microoperator/internal/state"
)

func TestExecutionControlValidationAndUnavailableAccounting(t *testing.T) {
	cfg := fixtureConfiguration(t)
	store, id := fixtureStore(t, cfg)
	broker := newModelBroker(store, cfg, log.New(io.Discard, "", 0))
	broker.broken = true
	engine := &executionEngine{ctx: context.Background(), store: store, cfg: cfg, broker: broker}
	handler := newControlHandler(store, cfg, id, fixtureControlToken, log.New(io.Discard, "", 0), engine)
	record, err := store.CreateSystem(context.Background(), localAdministrator, "create", state.CreateSystemCommand{Launch: "research"}, cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, body, token string
		want              int
	}{
		{"/start", `{"expected_revision":1}`, "wrong", 401},
		{"/start", `{"expected_revision":1,"system_id":"foreign"}`, fixtureControlToken, 400},
		{"/start", `{"expected_revision":0}`, fixtureControlToken, 400},
		{"/stop", `null`, fixtureControlToken, 400},
		{"/stop", `{"actor":"other-user"}`, fixtureControlToken, 400},
		{"/start", `{"expected_revision":1,"goal":"unused"}`, fixtureControlToken, 503},
	} {
		response := controlRequest(handler, "POST", "/v1/systems/"+record.ID+tc.path, "command", tc.body, tc.token)
		if response.Code != tc.want {
			t.Fatalf("%s %s: %d %s", tc.path, tc.body, response.Code, response.Body.String())
		}
	}
	health := controlRequest(handler, "GET", "/v1/health", "", "", fixtureControlToken)
	if !strings.Contains(health.Body.String(), `"status":"degraded"`) {
		t.Fatalf("accounting outage hidden: %s", health.Body.String())
	}
	assertCount(t, store, "model_attempts", 0)
	// Cancellation remains a separate, available operation even when admission
	// has closed after an accounting error.
	response := controlRequest(handler, http.MethodPost, "/v1/systems/"+record.ID+"/stop", "safe-stop", `{}`, fixtureControlToken)
	if response.Code != 202 {
		t.Fatalf("emergency stop unavailable: %d", response.Code)
	}
}

func TestProviderTimeoutRetainsReservation(t *testing.T) {
	entered := make(chan struct{})
	cfg, count := providerConfigFor(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
	})
	b, _, id := fixtureBroker(t, cfg)
	e := fixtureCall(t, b, id, "timeout", false)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	result, err := b.call(ctx, e)
	select {
	case <-entered:
	default:
		t.Fatal("deadline test never dispatched")
	}
	if err != nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) || result.State != "unknown" ||
		result.GoalReserved != e.Reservation || count.Load() != 1 {
		t.Fatalf("timeout was refunded or retried: %+v %v", result, err)
	}
}
