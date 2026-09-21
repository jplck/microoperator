//go:build darwin || linux

package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

var (
	errSystemNotFound   = errors.New("system not found")
	errCommandConflict  = errors.New("idempotency key was already used for a different command")
	errRevisionConflict = errors.New("system revision has changed")
)

type toolPin struct {
	Name    string `json:"name"`
	Version int64  `json:"version"`
	Digest  string `json:"digest"`
}

type systemGrants struct {
	Model             string    `json:"model"`
	SandboxProfile    string    `json:"sandbox_profile"`
	SystemTools       []toolPin `json:"system_tools"`
	OperatorTools     []toolPin `json:"operator_tools"`
	DefinitionsDigest string    `json:"definitions_digest"`
}

type initialGoal struct {
	ID          string `json:"goal_id"`
	SystemID    string `json:"system_id"`
	Prompt      string `json:"prompt"`
	State       string `json:"state"`
	TokenBudget int64  `json:"token_budget"`
}

type systemRecord struct {
	ID              string       `json:"system_id"`
	OperatorID      string       `json:"operator_id"`
	Launch          string       `json:"launch"`
	State           string       `json:"state"`
	Revision        int64        `json:"revision"`
	ConfigurationID string       `json:"configuration_id"`
	Configuration   systemConfig `json:"configuration"`
	Grants          systemGrants `json:"grants"`
	UsedTokens      int64        `json:"used_tokens"`
	RemainingTokens int64        `json:"remaining_tokens"`
	Goal            *initialGoal `json:"initial_goal,omitempty"`
	CreatedAt       string       `json:"created_at"`
	BlockedReason   string       `json:"blocked_reason,omitempty"`
}

type createSystemCommand struct {
	Launch string  `json:"launch"`
	Goal   *string `json:"goal,omitempty"`
}

type reviseSystemCommand struct {
	ExpectedRevision int64         `json:"expected_revision"`
	Configuration    *systemConfig `json:"configuration"`
}

type stateStore struct{ db *sql.DB }

const schemaV1 = `
CREATE TABLE config_snapshots (
  config_id TEXT PRIMARY KEY,
  configuration BLOB NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE systems (
  system_id TEXT PRIMARY KEY,
  owner TEXT NOT NULL,
  operator_id TEXT NOT NULL UNIQUE,
  launch TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state = 'inactive'),
  revision INTEGER NOT NULL CHECK (revision > 0),
  used_tokens INTEGER NOT NULL DEFAULT 0 CHECK (used_tokens >= 0),
  created_at TEXT NOT NULL,
  FOREIGN KEY (system_id, revision) REFERENCES system_revisions(system_id, revision)
    DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE system_revisions (
  system_id TEXT NOT NULL REFERENCES systems(system_id),
  revision INTEGER NOT NULL CHECK (revision > 0),
  config_id TEXT NOT NULL REFERENCES config_snapshots(config_id),
  definition BLOB NOT NULL,
  grants BLOB NOT NULL,
  token_budget INTEGER NOT NULL CHECK (token_budget > 0),
  created_at TEXT NOT NULL,
  PRIMARY KEY (system_id, revision)
);
CREATE TABLE goals (
  goal_id TEXT PRIMARY KEY,
  system_id TEXT NOT NULL UNIQUE REFERENCES systems(system_id),
  owner TEXT NOT NULL,
  prompt TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state = 'pending'),
  token_budget INTEGER NOT NULL CHECK (token_budget > 0)
);
CREATE TABLE audit (
  audit_id INTEGER PRIMARY KEY,
  system_id TEXT NOT NULL,
  principal TEXT NOT NULL,
  action TEXT NOT NULL CHECK (action IN ('system.create', 'system.revise')),
  revision INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY (system_id, revision) REFERENCES system_revisions(system_id, revision)
);
CREATE TABLE command_receipts (
  principal TEXT NOT NULL,
  command_key TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  system_id TEXT NOT NULL REFERENCES systems(system_id),
  response BLOB NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (principal, command_key)
);
CREATE INDEX systems_owner ON systems(owner, system_id);
CREATE TRIGGER immutable_revision_update BEFORE UPDATE ON system_revisions
  BEGIN SELECT RAISE(ABORT, 'system revisions are immutable'); END;
CREATE TRIGGER immutable_revision_delete BEFORE DELETE ON system_revisions
  BEGIN SELECT RAISE(ABORT, 'system revisions are immutable'); END;
CREATE TRIGGER immutable_config_update BEFORE UPDATE ON config_snapshots
  BEGIN SELECT RAISE(ABORT, 'configuration snapshots are immutable'); END;
CREATE TRIGGER immutable_config_delete BEFORE DELETE ON config_snapshots
  BEGIN SELECT RAISE(ABORT, 'configuration snapshots are immutable'); END;
CREATE TRIGGER immutable_audit_update BEFORE UPDATE ON audit
  BEGIN SELECT RAISE(ABORT, 'audit is append-only'); END;
CREATE TRIGGER immutable_audit_delete BEFORE DELETE ON audit
  BEGIN SELECT RAISE(ABORT, 'audit is append-only'); END;
PRAGMA user_version = 1;
PRAGMA application_id = 1297043538;
`

// openStore uses one connection so mutations serialize through SQLite instead of
// a second in-memory lock. The daemon must hold its data-directory lock first.
// Per-connection pragmas preserve foreign keys and durability if the driver ever
// replaces that connection; contexts also cancel requests waiting for the pool.
func openStore(ctx context.Context, filename string) (*stateStore, error) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		flags := os.O_RDWR
		if suffix == "" {
			flags |= os.O_CREATE
		}
		file, err := openOwnedFile(filename+suffix, flags, true)
		if suffix != "" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("open database file: %w", err)
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
	}
	dsn := url.URL{Scheme: "file", Path: filepath.ToSlash(filename)}
	query := url.Values{}
	for _, pragma := range []string{"foreign_keys(1)", "busy_timeout(5000)", "synchronous(FULL)"} {
		query.Add("_pragma", pragma)
	}
	dsn.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	// ponytail: serialize this local control API; add a read pool only if measured contention warrants it.
	db.SetMaxOpenConns(1)
	store := &stateStore{db: db}
	if err := store.migrate(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		return nil, errors.Join(fmt.Errorf("enable SQLite WAL: %w", err), db.Close())
	}
	if journalMode != "wal" {
		return nil, errors.Join(errors.New("SQLite did not enable WAL"), db.Close())
	}
	return store, nil
}

func (store *stateStore) migrate(ctx context.Context) (err error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer rollback(tx, &err)
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	var applicationID int
	if err := tx.QueryRowContext(ctx, "PRAGMA application_id").Scan(&applicationID); err != nil {
		return fmt.Errorf("read database ownership marker: %w", err)
	}
	if (version != 0 && applicationID != 0x4d4f5052) ||
		(version == 0 && applicationID != 0) {
		return errors.New("database is not a recognized Microoperator state database")
	}
	switch version {
	case 0:
		var objects int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'").Scan(&objects); err != nil {
			return fmt.Errorf("inspect initial database schema: %w", err)
		}
		if objects != 0 {
			return errors.New("refusing to initialize a nonempty, unrecognized database")
		}
		if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
			return fmt.Errorf("apply schema migration 1: %w", err)
		}
	case 1:
	default:
		return fmt.Errorf("unsupported database schema version %d", version)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}

func rollback(tx *sql.Tx, result *error) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		*result = errors.Join(*result, fmt.Errorf("roll back transaction: %w", err))
	}
}

func (store *stateStore) rememberConfiguration(ctx context.Context, cfg configuration) (string, error) {
	id, data, err := jsonDigest(cfg)
	if err != nil {
		return "", err
	}
	_, err = store.db.ExecContext(ctx,
		"INSERT OR IGNORE INTO config_snapshots(config_id, configuration, created_at) VALUES (?, ?, ?)",
		id, data, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return "", fmt.Errorf("persist administrative configuration: %w", err)
	}
	return id, nil
}

func newID(prefix string) (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate identity: %w", err)
	}
	return prefix + hex.EncodeToString(bytes[:]), nil
}

func (cfg configuration) grantsFor(definition systemConfig) (systemGrants, error) {
	grants := systemGrants{
		Model: definition.Operator.Model, SandboxProfile: definition.Operator.SandboxProfile,
		SystemTools: []toolPin{}, OperatorTools: []toolPin{},
	}
	model := cfg.Models[definition.Operator.Model]
	quotas := make(map[string]quotaConfig)
	for _, name := range model.QuotaGroups {
		quotas[name] = cfg.QuotaGroups[name]
	}
	tools := make(map[string]toolConfig)
	pins := make(map[string]toolPin)
	for _, name := range definition.Tools {
		tool := cfg.Tools[name]
		digest, _, err := jsonDigest(tool)
		if err != nil {
			return grants, err
		}
		pins[name] = toolPin{Name: name, Version: tool.Version, Digest: digest}
		grants.SystemTools = append(grants.SystemTools, pins[name])
		tools[name] = tool
	}
	for _, name := range definition.Operator.Tools {
		grants.OperatorTools = append(grants.OperatorTools, pins[name])
	}
	digest, _, err := jsonDigest(struct {
		Model    modelConfig            `json:"model"`
		Provider providerConfig         `json:"provider"`
		Quotas   map[string]quotaConfig `json:"quotas"`
		Profile  sandboxConfig          `json:"profile"`
		Tools    map[string]toolConfig  `json:"tools"`
	}{model, cfg.Providers[model.Provider], quotas, cfg.SandboxProfiles[definition.Operator.SandboxProfile], tools})
	grants.DefinitionsDigest = digest
	return grants, err
}

// inspect retains the saved revision. Administrative changes can block it, but
// cannot silently substitute a new model, profile, tool version, or quota policy.
// A later explicit revision binds the user's selection to the new definitions.
func (cfg configuration) inspect(record systemRecord) (systemRecord, error) {
	record.RemainingTokens = record.Configuration.Limits.TokenBudget - record.UsedTokens
	if err := cfg.validateSystem(record.Configuration); err != nil {
		record.BlockedReason = err.Error()
		return record, nil
	}
	current, err := cfg.grantsFor(record.Configuration)
	if err != nil {
		return record, err
	}
	if current.DefinitionsDigest != record.Grants.DefinitionsDigest {
		record.BlockedReason = "administrative definitions changed; explicitly revise the system before execution"
	}
	return record, nil
}

// command commits the resource, immutable revision, audit entry, and replay
// receipt together. A lost HTTP response can therefore be retried without
// creating a second system or applying a revision twice, even after a restart.
func (store *stateStore) command(ctx context.Context, principal, key, requestHash string,
	mutate func(*sql.Tx) (systemRecord, error),
) (result systemRecord, err error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("begin command: %w", err)
	}
	defer rollback(tx, &err)
	var savedHash string
	var response []byte
	err = tx.QueryRowContext(ctx,
		"SELECT request_hash, response FROM command_receipts WHERE principal = ? AND command_key = ?",
		principal, key).Scan(&savedHash, &response)
	if err == nil {
		if savedHash != requestHash {
			return result, errCommandConflict
		}
		if err := json.Unmarshal(response, &result); err != nil {
			return result, fmt.Errorf("decode command receipt: %w", err)
		}
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, fmt.Errorf("read command receipt: %w", err)
	}
	result, err = mutate(tx)
	if err != nil {
		return result, err
	}
	response, err = json.Marshal(result)
	if err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO command_receipts
		(principal, command_key, request_hash, system_id, response, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		principal, key, requestHash, result.ID, response, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return result, fmt.Errorf("record command receipt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("commit command: %w", err)
	}
	return result, nil
}

func (store *stateStore) createSystem(ctx context.Context, principal, key string,
	command createSystemCommand, cfg configuration, configID string,
) (systemRecord, error) {
	hash, _, err := jsonDigest(struct {
		Operation string              `json:"operation"`
		Command   createSystemCommand `json:"command"`
	}{"system.create", command})
	if err != nil {
		return systemRecord{}, err
	}
	return store.command(ctx, principal, key, hash, func(tx *sql.Tx) (systemRecord, error) {
		var record systemRecord
		definition, ok := cfg.Systems[command.Launch]
		if !ok {
			return record, invalid("launch", "unknown launch configuration")
		}
		if err := cfg.validateSystem(definition); err != nil {
			return record, err
		}
		record.ID, err = newID("sys_")
		if err != nil {
			return record, err
		}
		record.OperatorID, err = newID("agent_")
		if err != nil {
			return record, err
		}
		record.Launch, record.State, record.Revision = command.Launch, "inactive", 1
		record.ConfigurationID, record.Configuration = configID, definition
		record.Grants, err = cfg.grantsFor(definition)
		if err != nil {
			return record, err
		}
		record.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `INSERT INTO systems
			(system_id, owner, operator_id, launch, state, revision, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			record.ID, principal, record.OperatorID, record.Launch, record.State, record.Revision, record.CreatedAt); err != nil {
			return record, fmt.Errorf("create system: %w", err)
		}
		if err := insertRevision(ctx, tx, record, principal, "system.create"); err != nil {
			return record, err
		}
		if command.Goal != nil {
			goalID, err := newID("goal_")
			if err != nil {
				return record, err
			}
			record.Goal = &initialGoal{goalID, record.ID, *command.Goal, "pending", definition.Limits.TokenBudget}
			if _, err := tx.ExecContext(ctx, `INSERT INTO goals
				(goal_id, system_id, owner, prompt, state, token_budget) VALUES (?, ?, ?, ?, ?, ?)`,
				goalID, record.ID, principal, *command.Goal, "pending", record.Goal.TokenBudget); err != nil {
				return record, fmt.Errorf("record initial goal: %w", err)
			}
		}
		return record, nil
	})
}

func (store *stateStore) reviseSystem(ctx context.Context, principal, key, systemID string,
	command reviseSystemCommand, cfg configuration, configID string,
) (systemRecord, error) {
	hash, _, err := jsonDigest(struct {
		Operation string              `json:"operation"`
		SystemID  string              `json:"system_id"`
		Command   reviseSystemCommand `json:"command"`
	}{"system.revise", systemID, command})
	if err != nil {
		return systemRecord{}, err
	}
	return store.command(ctx, principal, key, hash, func(tx *sql.Tx) (systemRecord, error) {
		record, err := readSystem(ctx, tx, principal, systemID)
		if err != nil {
			return record, err
		}
		if command.ExpectedRevision != record.Revision {
			return record, errRevisionConflict
		}
		if command.Configuration == nil {
			return record, invalid("configuration", "is required")
		}
		if err := cfg.validateSystem(*command.Configuration); err != nil {
			return record, err
		}
		if command.Configuration.Limits.TokenBudget < record.UsedTokens {
			return record, invalid("limits.token_budget", "cannot be lower than consumed usage")
		}
		record.Configuration, record.ConfigurationID = *command.Configuration, configID
		record.Revision++
		record.Grants, err = cfg.grantsFor(record.Configuration)
		if err != nil {
			return record, err
		}
		if err := insertRevision(ctx, tx, record, principal, "system.revise"); err != nil {
			return record, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE systems SET revision = ? WHERE system_id = ? AND owner = ?",
			record.Revision, record.ID, principal); err != nil {
			return record, fmt.Errorf("select new system revision: %w", err)
		}
		return record, nil
	})
}

func insertRevision(ctx context.Context, tx *sql.Tx, record systemRecord, principal, action string) error {
	definition, err := json.Marshal(record.Configuration)
	if err != nil {
		return err
	}
	grants, err := json.Marshal(record.Grants)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO system_revisions
		(system_id, revision, config_id, definition, grants, token_budget, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.Revision, record.ConfigurationID, definition, grants, record.Configuration.Limits.TokenBudget, now); err != nil {
		return fmt.Errorf("persist system revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit
		(system_id, principal, action, revision, created_at) VALUES (?, ?, ?, ?, ?)`,
		record.ID, principal, action, record.Revision, now); err != nil {
		return fmt.Errorf("audit system command: %w", err)
	}
	return nil
}

func readSystem(ctx context.Context, tx *sql.Tx, principal, systemID string) (systemRecord, error) {
	var record systemRecord
	var definition, grants []byte
	err := tx.QueryRowContext(ctx, `SELECT s.system_id, s.operator_id, s.launch, s.state,
		s.revision, s.used_tokens, s.created_at, r.config_id, r.definition, r.grants
		FROM systems s JOIN system_revisions r ON r.system_id = s.system_id AND r.revision = s.revision
		WHERE s.system_id = ? AND s.owner = ?`, systemID, principal).Scan(
		&record.ID, &record.OperatorID, &record.Launch, &record.State, &record.Revision,
		&record.UsedTokens, &record.CreatedAt, &record.ConfigurationID, &definition, &grants)
	if errors.Is(err, sql.ErrNoRows) {
		return record, errSystemNotFound
	}
	if err != nil {
		return record, fmt.Errorf("read system: %w", err)
	}
	if err := json.Unmarshal(definition, &record.Configuration); err != nil {
		return record, fmt.Errorf("decode persisted revision: %w", err)
	}
	if err := json.Unmarshal(grants, &record.Grants); err != nil {
		return record, fmt.Errorf("decode persisted grants: %w", err)
	}
	var goal initialGoal
	err = tx.QueryRowContext(ctx, `SELECT goal_id, system_id, prompt, state, token_budget
		FROM goals WHERE system_id = ? AND owner = ?`, systemID, principal).Scan(
		&goal.ID, &goal.SystemID, &goal.Prompt, &goal.State, &goal.TokenBudget)
	if err == nil {
		record.Goal = &goal
	} else if !errors.Is(err, sql.ErrNoRows) {
		return record, fmt.Errorf("read initial goal: %w", err)
	}
	return record, nil
}

func (store *stateStore) getSystem(ctx context.Context, principal, systemID string) (record systemRecord, err error) {
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return record, err
	}
	defer rollback(tx, &err)
	return readSystem(ctx, tx, principal, systemID)
}

func (store *stateStore) listSystems(ctx context.Context, principal, after string) (records []systemRecord, next string, err error) {
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, "", err
	}
	defer rollback(tx, &err)
	rows, err := tx.QueryContext(ctx,
		"SELECT system_id FROM systems WHERE owner = ? AND system_id > ? ORDER BY system_id LIMIT 21", principal, after)
	if err != nil {
		return nil, "", err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, "", errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, "", err
	}
	if len(ids) > 20 {
		ids = ids[:20]
		next = ids[len(ids)-1]
	}
	records = make([]systemRecord, 0, len(ids))
	for _, id := range ids {
		record, err := readSystem(ctx, tx, principal, id)
		if err != nil {
			return nil, "", err
		}
		records = append(records, record)
	}
	return records, next, nil
}
