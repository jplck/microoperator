// Package state owns durable state, scoped artifacts and transactional workflows.
// Its public contract contains domain values, not database handles or callbacks.
// Network requests, process supervision and HTTP transport stay in the daemon.
package state

import (
	"context"
	"database/sql"
	"maps"
	"time"
)

const localAdministrator = "local-admin"

// workflow contains operation-local policy and ownership, never worker processes,
// HTTP clients or goroutines. The daemon serializes its live activation snapshot.
type workflow struct {
	store        *Store
	cfg          Configuration
	owner        string
	active       map[string]string
	shuttingDown bool
}

type admission struct {
	store *Store
	cfg   Configuration
	now   func() time.Time
}

func (store *Store) Close() error { return store.db.Close() }

func (store *Store) Claim(ctx context.Context, cfg Configuration, owner string, active map[string]string, now time.Time) (SystemRecord, ExecutionRecord, bool, error) {
	w := workflow{store: store, cfg: cfg, owner: owner, active: maps.Clone(active)}
	return w.claim(ctx, now)
}

func (store *Store) FinishDelivery(ctx context.Context, cfg Configuration, owner string, session ExecutionRecord, workerErr error, canceled, shuttingDown bool) error {
	w := workflow{store: store, cfg: cfg, owner: owner, shuttingDown: shuttingDown}
	return w.finishDelivery(ctx, session, workerErr, canceled)
}

func (store *Store) Task(ctx context.Context, systemID, taskID string) (TaskRecord, error) {
	return (&workflow{store: store}).taskSnapshot(ctx, systemID, taskID)
}

func (store *Store) QueueModelCall(ctx context.Context, session ExecutionRecord) error {
	return (&admission{store: store}).queue(ctx, session)
}

func (store *Store) AdmitModelCall(ctx context.Context, cfg Configuration, session ExecutionRecord, now time.Time) (ExecutionRecord, bool, error) {
	return (&admission{store: store, cfg: cfg}).admit(ctx, session, now)
}

func (store *Store) SettleModelCall(ctx context.Context, cfg Configuration, session ExecutionRecord, result ProviderResult, now time.Time) error {
	return (&admission{store: store, cfg: cfg}).settle(ctx, session, result, now)
}

func (store *Store) Quotas(ctx context.Context, cfg Configuration, now time.Time) ([]QuotaView, error) {
	return (&admission{store: store, cfg: cfg, now: func() time.Time { return now }}).quotas(ctx)
}

// commitFailed distinguishes an uncertain durable outcome from a rejected
// session or pre-commit failure; only the former disables shared admission.
func (store *Store) CancelModelCall(ctx context.Context, session ExecutionRecord) (result ExecutionRecord, commitFailed bool, err error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return result, false, err
	}
	defer rollback(tx, &err)
	result, err = authorizeCall(ctx, tx, session)
	if err != nil {
		return result, false, err
	}
	if result.State == "queued" || result.State == "awaiting_worker" {
		if err := terminalCall(ctx, tx, result, "canceled", "canceled before dispatch; no budget charged"); err != nil {
			return result, false, err
		}
		result.State, result.Reason = "canceled", "canceled before dispatch; no budget charged"
	}
	err = tx.Commit()
	return result, err != nil, err
}

func (store *Store) SearchMemory(ctx context.Context, systemID string, query MemoryQuery, now time.Time) (result MemorySearchResult, err error) {
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer rollback(tx, &err)
	record, err := readSystem(ctx, tx, localAdministrator, systemID)
	if err != nil {
		return result, err
	}
	return searchMemory(ctx, tx, TaskRecord{SystemID: systemID, AgentID: record.OperatorID}, query, true, now)
}
