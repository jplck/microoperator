//go:build darwin || linux

package main

import (
	"context"

	"errors"
	"log"

	"net/http"
	"strings"
	"sync"
	"time"
)

type modelBroker struct {
	mu      sync.Mutex
	changed chan struct{}
	broken  bool
	store   *stateStore
	cfg     configuration
	client  *http.Client
	lookup  func(string) (string, bool)
	now     func() time.Time
	logger  *log.Logger
}

var errBrokerUnavailable = errors.New("model broker unavailable after accounting failure; new dispatch is disabled")

func newModelBroker(store *stateStore, cfg configuration, logger *log.Logger) *modelBroker {
	return &modelBroker{changed: make(chan struct{}), store: store, cfg: cfg, client: providerClient(), now: time.Now, logger: logger}
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

func (b *modelBroker) call(ctx context.Context, session executionRecord) (executionRecord, error) {
	if err := b.queue(ctx, session); err != nil {
		return executionRecord{}, err
	}
	for {
		b.mu.Lock()
		if b.broken {
			b.mu.Unlock()
			return executionRecord{}, errBrokerUnavailable
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

func (b *modelBroker) cancelQueued(session executionRecord) (result executionRecord, err error) {
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

func (b *modelBroker) queue(ctx context.Context, session executionRecord) error {
	return b.store.QueueModelCall(ctx, session)
}

func (b *modelBroker) admit(ctx context.Context, session executionRecord, now time.Time) (executionRecord, bool, error) {
	return b.store.AdmitModelCall(ctx, b.cfg, session, now)
}

func (b *modelBroker) settle(ctx context.Context, session executionRecord, result providerResult, now time.Time) error {
	return b.store.SettleModelCall(ctx, b.cfg, session, result, now)
}

func (b *modelBroker) quotas(ctx context.Context) ([]quotaView, error) {
	return b.store.Quotas(ctx, b.cfg, b.now())
}

func (b *modelBroker) perform(ctx context.Context, e executionRecord) providerResult {
	unknown := providerResult{Reason: "provider outcome unknown; reservation retained"}
	record, err := b.store.GetSystem(ctx, localAdministrator, e.SystemID)
	if err != nil {
		return unknown
	}
	model := b.cfg.Models[e.Model]
	provider := b.cfg.Providers[model.Provider]
	key, ok := b.lookup(provider.APIKeyEnv)
	if !ok || strings.TrimSpace(key) == "" || strings.ContainsAny(key, "\r\n") {
		return providerResult{Known: true, Reason: "provider credential unavailable before request"}
	}
	body, _, err := modelRequest(b.cfg, record, e.Prompt, e.Stream)
	if len(e.Request) > 0 {
		body, err = e.Request, nil
	}
	if err != nil {
		return providerResult{Known: true, Reason: "provider request rejected before sending"}
	}
	requestCtx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	if requestCtx.Err() != nil {
		return providerResult{Known: true, Reason: "canceled before sending provider request"}
	}
	return requestModel(requestCtx, b.client, provider, model, body, e.Stream, key, b.now(), e.Attempts)
}
