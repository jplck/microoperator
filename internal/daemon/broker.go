//go:build darwin || linux

package daemon

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	modelprovider "github.com/jplck/microoperator/internal/provider"
	"github.com/jplck/microoperator/internal/state"
)

type modelBroker struct {
	mu      sync.Mutex
	changed chan struct{}
	broken  bool
	store   *state.Store
	cfg     state.Configuration
	client  *http.Client
	lookup  func(string) (string, bool)
	now     func() time.Time
	logger  *log.Logger
}

var errBrokerUnavailable = errors.New("model broker unavailable after accounting failure; new dispatch is disabled")

func newModelBroker(store *state.Store, cfg state.Configuration, logger *log.Logger) *modelBroker {
	return &modelBroker{changed: make(chan struct{}), store: store, cfg: cfg, client: modelprovider.NewClient(), now: time.Now, logger: logger}
}

func (b *modelBroker) notifyLocked() { close(b.changed); b.changed = make(chan struct{}) }

func (b *modelBroker) failLocked(err error) {
	b.broken = true
	b.logger.Printf("model accounting failed; new dispatch disabled: %v", err)
	b.notifyLocked()
}

func (b *modelBroker) available() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.broken
}

func (b *modelBroker) call(ctx context.Context, session state.ExecutionRecord) (state.ExecutionRecord, error) {
	if err := b.queue(ctx, session); err != nil {
		return state.ExecutionRecord{}, err
	}
	for {
		b.mu.Lock()
		if b.broken {
			b.mu.Unlock()
			return state.ExecutionRecord{}, errBrokerUnavailable
		}
		changed := b.changed
		current, admitted, err := b.admit(ctx, session, b.now())
		if err != nil {
			if ctx.Err() == nil {
				b.failLocked(err)
			}
			b.mu.Unlock()
			if ctx.Err() != nil {
				return b.cancelQueued(session)
			}
			return current, err
		}
		if admitted {
			b.notifyLocked()
		}
		b.mu.Unlock()
		if admitted {
			result := b.perform(ctx, current)
			b.mu.Lock()
			finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err = b.settle(finishCtx, current, result, b.now())
			cancel()
			if err != nil {
				b.failLocked(err)
			} else {
				b.notifyLocked()
			}
			b.mu.Unlock()
			if err != nil {
				return current, err
			}
			continue
		}
		if current.State != "queued" && current.State != "running" {
			return current, nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return b.cancelQueued(session)
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (b *modelBroker) cancelQueued(session state.ExecutionRecord) (result state.ExecutionRecord, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, commitFailed, err := b.store.CancelModelCall(ctx, session)
	if err != nil {
		if commitFailed {
			b.failLocked(err)
		}
		return result, err
	}
	b.notifyLocked()
	return result, nil
}

func (b *modelBroker) queue(ctx context.Context, session state.ExecutionRecord) error {
	return b.store.QueueModelCall(ctx, session)
}

func (b *modelBroker) admit(ctx context.Context, session state.ExecutionRecord, now time.Time) (state.ExecutionRecord, bool, error) {
	return b.store.AdmitModelCall(ctx, b.cfg, session, now)
}

func (b *modelBroker) settle(ctx context.Context, session state.ExecutionRecord, result state.ProviderResult, now time.Time) error {
	return b.store.SettleModelCall(ctx, b.cfg, session, result, now)
}

func (b *modelBroker) quotas(ctx context.Context) ([]state.QuotaView, error) {
	return b.store.Quotas(ctx, b.cfg, b.now())
}

func (b *modelBroker) perform(ctx context.Context, e state.ExecutionRecord) state.ProviderResult {
	unknown := state.ProviderResult{Reason: "provider outcome unknown; reservation retained"}
	record, err := b.store.GetSystem(ctx, localAdministrator, e.SystemID)
	if err != nil {
		return unknown
	}
	model := b.cfg.Models[e.Model]
	provider := b.cfg.Providers[model.Provider]
	key, ok := b.lookup(provider.APIKeyEnv)
	if !ok || strings.TrimSpace(key) == "" || strings.ContainsAny(key, "\r\n") {
		return state.ProviderResult{Known: true, Reason: "provider credential unavailable before request"}
	}
	body, _, err := state.ModelRequest(b.cfg, record, e.Prompt, e.Stream)
	if len(e.Request) > 0 {
		body, err = e.Request, nil
	}
	if err != nil {
		return state.ProviderResult{Known: true, Reason: "provider request rejected before sending"}
	}
	requestCtx, cancel := context.WithTimeout(ctx, state.ProviderTimeout)
	defer cancel()
	if requestCtx.Err() != nil {
		return state.ProviderResult{Known: true, Reason: "canceled before sending provider request"}
	}
	return modelprovider.Request(requestCtx, b.client, provider, model, body, e.Stream, key, b.now(), e.Attempts)
}
