package state

import (
	"database/sql"
	"encoding/json"
	"errors"
)

func readIDs(rows *sql.Rows) (ids []string, err error) {
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	return ids, errors.Join(rows.Err(), rows.Close())
}

type EventView struct {
	Sequence       int64           `json:"sequence"`
	ID             string          `json:"event_id"`
	SystemID       string          `json:"system_id"`
	Type           string          `json:"type"`
	Version        int             `json:"version"`
	Source         string          `json:"source"`
	Recipient      string          `json:"recipient"`
	GoalID         string          `json:"goal_id"`
	TaskID         string          `json:"task_id"`
	Correlation    string          `json:"correlation_id"`
	Causation      string          `json:"causation_id"`
	CreatedAt      int64           `json:"created_at_ms"`
	ExpiresAt      int64           `json:"expires_at_ms"`
	Classification string          `json:"classification"`
	Authorization  string          `json:"authorization_ref"`
	Depth          int             `json:"depth"`
	Payload        json.RawMessage `json:"payload"`
	State          string          `json:"delivery_state"`
	Attempts       int             `json:"attempts"`
	LeaseUntil     int64           `json:"lease_until_ms"`
	Reason         string          `json:"reason,omitempty"`
}
