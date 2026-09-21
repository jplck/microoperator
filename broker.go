//go:build darwin || linux

package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"math"
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

func authorizeCall(ctx context.Context, tx *sql.Tx, session executionRecord) (executionRecord, error) {
	current, err := readExecution(ctx, tx, session.SystemID, session.CallID)
	if err != nil {
		return current, err
	}
	if current.ActivationID != session.ActivationID || current.ActivationOwner != session.ActivationOwner ||
		current.Revision != session.Revision || current.GoalID != session.GoalID {
		return current, invalid("session", "model call is outside this activation")
	}
	return current, nil
}

func (b *modelBroker) queue(ctx context.Context, session executionRecord) (err error) {
	tx, err := b.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx, &err)
	e, err := authorizeCall(ctx, tx, session)
	if err != nil {
		return err
	}
	if e.State == "awaiting_worker" {
		if _, err := tx.ExecContext(ctx, `UPDATE model_calls SET state='queued' WHERE call_id=?`, e.CallID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE goals SET state='waiting' WHERE system_id=? AND goal_id=?`, e.SystemID, e.GoalID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func terminalCall(ctx context.Context, tx *sql.Tx, e executionRecord, state, reason string) error {
	_, err := tx.ExecContext(ctx, `UPDATE model_calls SET state=?,reason=? WHERE system_id=? AND call_id=?`,
		state, reason, e.SystemID, e.CallID)
	return err
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
	tx, err := b.store.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer rollback(tx, &err)
	result, err = authorizeCall(ctx, tx, session)
	if err != nil {
		return result, err
	}
	if result.State == "queued" || result.State == "awaiting_worker" {
		if err := terminalCall(ctx, tx, result, "canceled", "canceled before dispatch; no budget charged"); err != nil {
			return result, err
		}
		result.State, result.Reason = "canceled", "canceled before dispatch; no budget charged"
	}
	if err := tx.Commit(); err != nil {
		b.failLocked(err)
		return result, err
	}
	b.notifyLocked()
	return result, nil
}

func (b *modelBroker) perform(ctx context.Context, e executionRecord) providerResult {
	unknown := providerResult{Reason: "provider outcome unknown; reservation retained"}
	record, err := b.store.getSystem(ctx, localAdministrator, e.SystemID)
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

// admit is a single durable decision: grants, fair ordering, every shared quota,
// and both budgets pass before any attempt is recorded or network request starts.
// One operator/goal per system makes system round-robin also goal-fair in phase 2.
func (b *modelBroker) admit(ctx context.Context, session executionRecord, now time.Time) (result executionRecord, admitted bool, err error) {
	tx, err := b.store.db.BeginTx(ctx, nil)
	if err != nil {
		return result, false, err
	}
	defer rollback(tx, &err)
	result, err = authorizeCall(ctx, tx, session)
	if err != nil {
		return result, false, err
	}
	if result.State != "queued" {
		return result, false, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.system_id,c.call_id FROM model_calls c JOIN systems s USING(system_id)
	 WHERE c.state='queued' ORDER BY s.last_admission,c.sequence LIMIT ?`, maxOperators)
	if err != nil {
		return result, false, err
	}
	var ids [][2]string
	for rows.Next() {
		var id [2]string
		if err := rows.Scan(&id[0], &id[1]); err != nil {
			return result, false, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return result, false, err
	}
	for _, id := range ids {
		e, err := readExecution(ctx, tx, id[0], id[1])
		if err != nil {
			return result, false, err
		}
		record, err := readSystem(ctx, tx, localAdministrator, e.SystemID)
		if err != nil {
			return result, false, err
		}
		record, err = b.cfg.inspect(record)
		if err != nil {
			return result, false, err
		}
		reason := ""
		switch {
		case record.State != "running":
			reason = "system stopped or stopping"
		case record.Revision != e.Revision || record.BlockedReason != "":
			reason = "model grant revoked or administrative definitions changed"
		case now.UnixMilli() >= e.WaitDeadline || now.UnixMilli() >= e.Deadline:
			reason = "queue deadline exceeded"
		case e.Reservation > record.RemainingTokens || e.Reservation > e.GoalBudget-e.GoalUsed-e.GoalReserved:
			reason = "budget exhausted"
		}
		if reason != "" {
			if err := terminalCall(ctx, tx, e, "rejected", reason); err != nil {
				return result, false, err
			}
			continue
		}
		allowed := true
		dispatchAt := now.UnixMilli()
		model := b.cfg.Models[e.Model]
		for _, group := range model.QuotaGroups {
			quota := b.cfg.QuotaGroups[group]
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO quota_state(group_name,credit,updated_at) VALUES(?,?,?)`,
				group, quota.BurstRequests, now.UnixMilli()); err != nil {
				return result, false, err
			}
			var credit float64
			var updated, cooldown int64
			if err := tx.QueryRowContext(ctx, `SELECT credit,updated_at,cooldown_until FROM quota_state WHERE group_name=?`, group).
				Scan(&credit, &updated, &cooldown); err != nil {
				return result, false, err
			}
			effective := max(now.UnixMilli(), updated)
			dispatchAt = max(dispatchAt, effective)
			credit = math.Min(float64(quota.BurstRequests), credit+float64(effective-updated)*float64(quota.RequestsPerMinute)/60000)
			if _, err := tx.ExecContext(ctx, `UPDATE quota_state SET credit=?,updated_at=? WHERE group_name=?`, credit, effective, group); err != nil {
				return result, false, err
			}
			var tokens, inflight int64
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN a.dispatched_at>? THEN a.reservation ELSE 0 END),0),
			 COALESCE(SUM(CASE WHEN a.state='running' THEN 1 ELSE 0 END),0)
			 FROM model_attempts a JOIN call_groups g USING(call_id) WHERE g.group_name=?`,
				effective-60000, group).Scan(&tokens, &inflight); err != nil {
				return result, false, err
			}
			if credit < 1 || cooldown > now.UnixMilli() || tokens+e.Reservation > quota.TokensPerMinute || inflight >= quota.MaxConcurrent {
				allowed = false
			}
		}
		if !allowed {
			if _, err := tx.ExecContext(ctx, `UPDATE model_calls SET reason='waiting for shared rate, cooldown, or concurrency capacity' WHERE call_id=?`, e.CallID); err != nil {
				return result, false, err
			}
			continue
		}
		// A later caller cannot leapfrog an earlier eligible system, even if
		// its goroutine wins the mutex. Wakeups never themselves grant capacity.
		if e.CallID != session.CallID {
			break
		}
		for _, group := range model.QuotaGroups {
			if _, err := tx.ExecContext(ctx, `UPDATE quota_state SET credit=credit-1 WHERE group_name=?`, group); err != nil {
				return result, false, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE systems SET reserved_tokens=reserved_tokens+? WHERE system_id=?`, e.Reservation, e.SystemID); err != nil {
			return result, false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE goals SET reserved_tokens=reserved_tokens+?,state='running' WHERE system_id=? AND goal_id=?`,
			e.Reservation, e.SystemID, e.GoalID); err != nil {
			return result, false, err
		}
		insert, err := tx.ExecContext(ctx, `INSERT INTO model_attempts(call_id,system_id,dispatched_at,reservation,state) VALUES(?,?,?,?,'running')`,
			e.CallID, e.SystemID, dispatchAt, e.Reservation)
		if err != nil {
			return result, false, err
		}
		seq, err := insert.LastInsertId()
		if err != nil {
			return result, false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE systems SET last_admission=? WHERE system_id=?`, seq, e.SystemID); err != nil {
			return result, false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE model_calls SET state='running',reason='',usage_known=0,attempts=attempts+1,
		 dispatched_at=?,queue_wait_ms=queue_wait_ms+? WHERE call_id=?`,
			dispatchAt, max(now.UnixMilli()-e.QueuedAt, 0), e.CallID); err != nil {
			return result, false, err
		}
		if err := auditExecution(ctx, tx, e.SystemID, e.ActivationID, "model.dispatch", e.Revision, now); err != nil {
			return result, false, err
		}
		admitted = true
		break
	}
	result, err = readExecution(ctx, tx, session.SystemID, session.CallID)
	if err != nil {
		return result, false, err
	}
	if err := tx.Commit(); err != nil {
		return result, false, err
	}
	return result, admitted, nil
}

func (b *modelBroker) settle(ctx context.Context, session executionRecord, p providerResult, now time.Time) (err error) {
	tx, err := b.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx, &err)
	e, err := authorizeCall(ctx, tx, session)
	if err != nil {
		return err
	}
	if e.State != "running" {
		return nil
	}
	state := "unknown"
	if p.Known {
		state = "completed"
		if p.Reason != "" {
			state = "failed"
		}
		actual := p.Input + p.Output
		if _, err := tx.ExecContext(ctx, `UPDATE systems SET reserved_tokens=reserved_tokens-?,used_tokens=used_tokens+? WHERE system_id=?`,
			e.Reservation, actual, e.SystemID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE goals SET reserved_tokens=reserved_tokens-?,used_tokens=used_tokens+? WHERE system_id=? AND goal_id=?`,
			e.Reservation, actual, e.SystemID, e.GoalID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE model_attempts SET state='known',input_tokens=?,output_tokens=?,reservation=MAX(reservation,?),throttled=?
		 WHERE call_id=? AND state='running'`, p.Input, p.Output, actual, p.Retry, e.CallID); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE model_attempts SET state='unknown' WHERE call_id=? AND state='running'`, e.CallID); err != nil {
			return err
		}
	}
	if p.Retry {
		cooldown := p.Cooldown
		if cooldown.IsZero() {
			cooldown = now.Add(p.RetryDelay)
		}
		wait := int64(3600)
		queueAvailable := true
		for _, group := range b.cfg.Models[e.Model].QuotaGroups {
			wait = min(wait, b.cfg.QuotaGroups[group].MaxWaitSeconds)
			if _, err := tx.ExecContext(ctx, `UPDATE quota_state SET cooldown_until=MAX(cooldown_until,?) WHERE group_name=?`, cooldown.UnixMilli(), group); err != nil {
				return err
			}
			var queued int64
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM model_calls c JOIN call_groups g USING(call_id)
			 WHERE g.group_name=? AND c.state IN ('awaiting_worker','queued')`, group).Scan(&queued); err != nil {
				return err
			}
			if queued >= b.cfg.QuotaGroups[group].QueueCapacity {
				queueAvailable = false
			}
		}
		var systemState string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM systems WHERE system_id=?`, e.SystemID).Scan(&systemState); err != nil {
			return err
		}
		if !queueAvailable {
			p.Reason = "provider throttled request; retry queue capacity exhausted"
		}
		if e.Attempts < 3 && now.UnixMilli() < e.Deadline && systemState == "running" && queueAvailable {
			state = "queued"
			if _, err := tx.ExecContext(ctx, `UPDATE model_calls SET wait_deadline=?,queued_at=? WHERE call_id=?`,
				min(e.Deadline, now.Add(time.Duration(wait)*time.Second).UnixMilli()), now.UnixMilli(), e.CallID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE goals SET state='waiting' WHERE system_id=? AND goal_id=?`, e.SystemID, e.GoalID); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE model_calls SET state=?,reason=?,response=?,input_tokens=input_tokens+?,
	 output_tokens=output_tokens+?,usage_known=? WHERE call_id=?`,
		state, p.Reason, p.Text, p.Input, p.Output, p.Known, e.CallID); err != nil {
		return err
	}
	if err := auditExecution(ctx, tx, e.SystemID, e.ActivationID, "model."+state, e.Revision, now); err != nil {
		return err
	}
	return tx.Commit()
}

type quotaView struct {
	Group              string      `json:"group"`
	RequestCredit      float64     `json:"request_credit"`
	CooldownUntil      int64       `json:"cooldown_until_ms"`
	ReservedRateTokens int64       `json:"reserved_rate_tokens"`
	InFlight           int64       `json:"in_flight"`
	Queued             int64       `json:"queued"`
	Limits             quotaConfig `json:"limits"`
	Attempts           int64       `json:"dispatched_attempts"`
	Throttles          int64       `json:"throttles"`
}

func (b *modelBroker) quotas(ctx context.Context) (views []quotaView, err error) {
	tx, err := b.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer rollback(tx, &err)
	views = make([]quotaView, 0, len(b.cfg.QuotaGroups))
	for name, quota := range b.cfg.QuotaGroups {
		v := quotaView{Group: name, Limits: quota, RequestCredit: float64(quota.BurstRequests)}
		var updated int64
		err := tx.QueryRowContext(ctx, `SELECT credit,updated_at,cooldown_until FROM quota_state WHERE group_name=?`, name).Scan(&v.RequestCredit, &updated, &v.CooldownUntil)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		now := max(b.now().UnixMilli(), updated)
		if err == nil {
			v.RequestCredit = math.Min(float64(quota.BurstRequests), v.RequestCredit+float64(now-updated)*float64(quota.RequestsPerMinute)/60000)
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN dispatched_at>? THEN reservation ELSE 0 END),0),
		 COALESCE(SUM(CASE WHEN a.state='running' THEN 1 ELSE 0 END),0),count(*),COALESCE(SUM(a.throttled),0)
		 FROM model_attempts a JOIN call_groups g USING(call_id) WHERE group_name=?`,
			now-60000, name).Scan(&v.ReservedRateTokens, &v.InFlight, &v.Attempts, &v.Throttles); err != nil {
			return nil, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM model_calls c JOIN call_groups g USING(call_id)
		 WHERE group_name=? AND state IN ('awaiting_worker','queued')`, name).Scan(&v.Queued); err != nil {
			return nil, err
		}
		views = append(views, v)
	}
	return views, nil
}
