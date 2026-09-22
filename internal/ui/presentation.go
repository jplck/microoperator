//go:build linux

package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jplck/microoperator/internal/state"
)

type uiCell struct {
	Text, URL string
	Status    bool
}

type uiTable struct {
	Title   string
	Columns []string
	Rows    [][]uiCell
}

type uiStat struct{ Label, Value string }

type uiAgent struct {
	Name, Role, State, Parent, Model, Activity, Assignment, Reason, Result, TaskURL string
	Depth, Turns                                                                    int
}

type uiEvent struct{ Time, Title, Route, State, Reason string }

type tableSpec struct {
	Key     string
	Columns []uiStat
}

var sectionTables = map[string]tableSpec{
	"agents":            {"agents", []uiStat{{"Agent", "name"}, {"State", "state"}, {"Model", "definition.model"}, {"Parent", "parent_id"}, {"Token budget", "token_budget"}, {"Revision", "revision"}}},
	"tasks":             {"tasks", []uiStat{{"Task", "task_id"}, {"Agent", "agent_id"}, {"State", "state"}, {"Control", "control"}, {"Turns", "turns"}, {"Reason", "reason"}}},
	"events":            {"events", []uiStat{{"Event", "type"}, {"State", "state"}, {"From", "source"}, {"To", "recipient"}, {"Created", "created_at_ms"}, {"Reason", "reason"}}},
	"model-calls":       {"calls", []uiStat{{"Model", "model"}, {"Agent", "agent_id"}, {"State", "state"}, {"Input tokens", "input_tokens"}, {"Output tokens", "output_tokens"}, {"Reason", "reason"}}},
	"tool-calls":        {"calls", []uiStat{{"Tool", "name"}, {"State", "state"}, {"Task", "task_id"}, {"Version", "version"}}},
	"artifacts":         {"artifacts", []uiStat{{"Artifact", "artifact_id"}, {"Bytes", "bytes"}, {"Task", "task_id"}, {"Goal", "goal_id"}}},
	"tools":             {"tools", []uiStat{{"Tool", "tool_id"}, {"Kind", "kind"}, {"State", "state"}, {"Version", "version"}, {"Origin", "origin"}, {"Description", "description"}}},
	"memory":            {"entries", []uiStat{{"Note", "content"}, {"Scope", "scope"}, {"State", "state"}, {"Confidence", "confidence"}, {"Evidence", "evidence"}}},
	"schedules":         {"schedules", []uiStat{{"Schedule", "id"}, {"State", "state"}, {"Cron", "cron"}, {"Timezone", "timezone"}, {"Next due", "next_due_ms"}, {"Remaining", "remaining"}}},
	"subscriptions":     {"subscriptions", []uiStat{{"Subscription", "id"}, {"Event", "type"}, {"Scope", "scope"}, {"State", "state"}, {"Remaining", "remaining"}}},
	"learning":          {"items", []uiStat{{"Evaluation", "evaluation_id"}, {"Tool", "tool_id"}, {"Version", "version"}, {"State", "state"}, {"Reason", "reason"}}},
	"learning-checks":   {"items", []uiStat{{"Check", "check_id"}, {"Cases", "cases"}, {"Created", "created_at"}}},
	"learning-feedback": {"items", []uiStat{{"Feedback", "content"}, {"Rating", "rating"}, {"Task", "task_id"}, {"Created", "created_at"}}},
	"broker":            {"quota_groups", []uiStat{{"Quota group", "group"}, {"In flight", "in_flight"}, {"Queued", "queued"}, {"Concurrency limit", "limits.max_concurrent"}, {"Attempts", "dispatched_attempts"}, {"Throttles", "throttles"}}},
}

func label(value string) string {
	value = strings.NewReplacer("-", " ", "_", " ", ".", " ").Replace(value)
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func preview(value string) string {
	trimmed := strings.TrimSpace(value)
	if (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) && json.Valid([]byte(trimmed)) {
		return "Structured content - open Properties"
	}
	runes := []rune(value)
	if len(runes) > 240 {
		return string(runes[:240]) + "..."
	}
	return value
}

func tableCell(value any, key string) string {
	switch value := value.(type) {
	case nil:
		return "-"
	case string:
		if value == "" {
			return "-"
		}
		return preview(value)
	case json.Number:
		if strings.HasSuffix(key, "_ms") || key == "created_at" {
			if ms, err := value.Int64(); err == nil {
				if ms == 0 {
					return "-"
				}
				return time.UnixMilli(ms).UTC().Format("Jan 02 15:04:05 UTC")
			}
		}
		return value.String()
	case bool:
		if value {
			return "Yes"
		}
		return "No"
	case []any:
		return fmt.Sprintf("%d items", len(value))
	case map[string]any:
		return "Open Properties"
	default:
		return fmt.Sprint(value)
	}
}

func collectionTable(data []byte, section, base string) (uiTable, error) {
	spec, found := sectionTables[section]
	if !found {
		return uiTable{}, fmt.Errorf("unsupported table %q", section)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return uiTable{}, err
	}
	raw, found := envelope[spec.Key]
	if !found {
		return uiTable{}, fmt.Errorf("missing %s in daemon response", spec.Key)
	}
	var records []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&records); err != nil {
		return uiTable{}, err
	}
	table := uiTable{Title: label(section)}
	for _, column := range spec.Columns {
		table.Columns = append(table.Columns, column.Label)
	}
	for _, record := range records {
		if record == nil {
			return uiTable{}, fmt.Errorf("invalid %s row", section)
		}
		row := []uiCell{}
		for _, column := range spec.Columns {
			var value any = record
			for _, key := range strings.Split(column.Value, ".") {
				fields, ok := value.(map[string]any)
				if !ok {
					value = nil
					break
				}
				value = fields[key]
			}
			cell := uiCell{Text: tableCell(value, column.Value), Status: column.Value == "state" || column.Value == "control"}
			if section == "model-calls" && (column.Value == "input_tokens" || column.Value == "output_tokens") && record["usage_known"] != true {
				cell.Text = "Not settled / unknown"
			}
			if section == "artifacts" && column.Value == "artifact_id" {
				id, ok := value.(string)
				if value != nil && !ok {
					return uiTable{}, fmt.Errorf("invalid artifact ID")
				}
				if id != "" {
					cell.URL = base + "/" + url.PathEscape(id)
				}
			}
			row = append(row, cell)
		}
		table.Rows = append(table.Rows, row)
	}
	return table, nil
}

func systemSummary(page *uiPage, record state.SystemRecord) {
	page.State = record.State
	page.Stats = []uiStat{
		{"Model", record.Configuration.Operator.Model},
		{"Tokens remaining", strconv.FormatInt(record.RemainingTokens, 10)},
		{"Tokens used", strconv.FormatInt(record.UsedTokens, 10)},
		{"Active agent limit", strconv.FormatInt(record.Configuration.Limits.MaxActiveAgents, 10)},
	}
	if record.Goal != nil {
		page.Goal, page.GoalLabel = preview(record.Goal.Prompt), "Initial goal"
		if record.Goal.State == "pending" {
			page.GoalLabel = "Saved initial goal"
		}
	}
	if record.Execution != nil {
		page.Goal, page.GoalLabel = preview(record.Execution.Prompt), "Latest goal"
		page.Outcome = preview(record.Execution.Response)
		if record.Execution.Reason != "" {
			page.Error = record.Execution.Reason
		}
		if record.Execution.TaskState == "waiting" && record.State == "running" {
			page.Notice = "Waiting for input or an authorized event. Open Activity to see the task and its blocker."
		}
	}
	if record.BlockedReason != "" {
		page.Error = record.BlockedReason
	}
	if record.Continuous {
		page.GoalLabel = "Continuous goal"
		if record.Idle && record.State == "running" {
			page.State = "waiting"
			page.Notice = "Continuous assistant is idle; no model calls or workers are running. Send input or wait for an authorized event."
		}
	}
}

func (app *controlUI) activity(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("system_id")
	if !state.SystemIDPattern.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	data, ok := app.fetch(w, r, "/v1/systems/"+id+"/activity")
	if !ok {
		return
	}
	var snapshot state.ActivitySnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		app.render(w, 502, uiPage{Title: "Invalid activity response", Data: err.Error()})
		return
	}
	base := "/systems/" + id
	page := uiPage{Title: "Activity / " + snapshot.System.Configuration.Name, Subtitle: "Recorded work, not inferred model reasoning.",
		Data: prettyJSON(data), SystemID: id, Refresh: r.URL.Query().Get("refresh") != "off",
		Links: []uiLink{{"System overview", base}, {"All agents", base + "/agents"}, {"All tasks", base + "/tasks"}}}
	if page.Refresh {
		page.Links = append(page.Links, uiLink{"Pause live updates", base + "/activity?refresh=off"})
	} else {
		page.Links = append(page.Links, uiLink{"Resume live updates", base + "/activity"})
	}
	systemSummary(&page, snapshot.System)
	page.Notice = "Current or latest goal. Up to 32 agents and the 20 most recent calls and event deliveries. Full histories are available from the system overview."
	if snapshot.Truncated {
		page.Notice += " More agents exist; open All agents for the complete directory."
	}
	names := map[string]string{snapshot.System.OperatorID: "Operator"}
	for _, agent := range snapshot.Agents {
		names[agent.ID] = agent.Name
	}
	agentName := func(id string) string {
		if name, ok := names[id]; ok {
			return name
		}
		return id
	}
	latest := map[string]state.ActivityCall{}
	calls := uiTable{Title: "Recent model and tool work", Columns: []string{"Time", "Agent", "Model", "State", "Tool", "Tool state", "Tokens used"}}
	for _, call := range snapshot.Calls {
		if _, exists := latest[call.TaskID]; !exists {
			latest[call.TaskID] = call
		}
		tokens := "Not settled / unknown"
		if call.UsageKnown {
			tokens = strconv.FormatInt(call.Input+call.Output, 10)
		}
		calls.Rows = append(calls.Rows, []uiCell{
			{Text: time.UnixMilli(call.CreatedAt).UTC().Format("15:04:05 UTC")},
			{Text: agentName(call.AgentID)}, {Text: call.Model}, {Text: call.State, Status: true},
			{Text: call.Tool}, {Text: call.ToolState, Status: call.ToolState != ""},
			{Text: tokens},
		})
	}
	page.Tables = []uiTable{calls}
	for _, agent := range snapshot.Agents {
		card := uiAgent{Name: agent.Name, Role: "Agent", State: agent.TaskState, Parent: agentName(agent.Parent),
			Depth: min(agent.Depth, 8), Model: agent.Model, Turns: agent.Turns, Assignment: preview(agent.Assignment),
			Reason: preview(agent.Reason), Result: preview(agent.Response), TaskURL: base + "/tasks",
			Activity: "No task recorded for this goal"}
		if agent.ID == snapshot.System.OperatorID {
			card.Name, card.Role, card.Depth = "Operator", "Coordinator", 0
		}
		if agent.TaskState != "" {
			card.Activity = label(agent.TaskState)
		} else {
			card.State = agent.State
		}
		if call, ok := latest[agent.TaskID]; ok && !state.TaskTerminal(agent.TaskState) && agent.TaskState != "waiting" {
			switch call.State {
			case "running":
				card.Activity = "Calling model " + call.Model
			case "queued", "awaiting_worker":
				card.Activity = "Waiting for model admission or a worker"
			case "completed":
				card.Activity = "Handling model response"
			}
			if call.Tool != "" {
				card.Activity = "Requested tool: " + call.Tool
				if call.ToolState != "" {
					card.Activity = "Tool " + call.ToolState + ": " + call.Tool
				}
			}
		}
		if agent.State == "stopped" || agent.Control == "stopped" {
			card.State = "stopped"
			card.Activity = "Stopped; any in-flight work must finish cleanup"
		} else if agent.State == "paused" || agent.Control == "paused" {
			card.State = "paused"
			card.Activity += " (paused; no new activations)"
		} else if agent.State != "active" {
			card.State = agent.State
			card.Activity = label(agent.State)
		}
		if snapshot.System.State == "paused" {
			card.Activity += " (system paused; no new activations)"
		}
		page.Agents = append(page.Agents, card)
	}
	if len(page.Agents) == 0 {
		page.Agents = []uiAgent{{Name: "Operator", Role: "Coordinator", State: "inactive",
			Activity: "Not started. Start the saved goal from the system overview.", Model: snapshot.System.Configuration.Operator.Model}}
	}
	for _, event := range snapshot.Events {
		page.Events = append(page.Events, uiEvent{Time: time.UnixMilli(event.CreatedAt).UTC().Format("15:04:05 UTC"),
			Title: label(event.Type), Route: agentName(event.Source) + " -> " + agentName(event.Recipient),
			State: event.State, Reason: preview(event.Reason)})
	}
	app.render(w, 200, page)
}
