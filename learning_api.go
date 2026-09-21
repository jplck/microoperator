//go:build darwin || linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

type evaluateCommand struct {
	ToolID   string   `json:"tool_id"`
	Version  int64    `json:"version"`
	CheckID  string   `json:"check_id"`
	TaskID   string   `json:"task_id"`
	Baseline *toolPin `json:"baseline,omitempty"`
}

type evaluationBaseline struct {
	Prompt string   `json:"prompt"`
	Tool   *toolPin `json:"tool,omitempty"`
}

func (api *controlAPI) registerLearningRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/systems/{system_id}/learning", api.learningView)
	mux.HandleFunc("GET /v1/systems/{system_id}/learning/{section}", api.learningView)
	mux.HandleFunc("POST /v1/systems/{system_id}/learning/checks", api.learningChecks)
	mux.HandleFunc("POST /v1/systems/{system_id}/learning/feedback", api.learningFeedback)
	mux.HandleFunc("POST /v1/systems/{system_id}/learning/evaluate", api.learningEvaluate)
	mux.HandleFunc("POST /v1/systems/{system_id}/learning/{evaluation_id}/approve", api.learningApprove)
}

func (api *controlAPI) learningChecks(w http.ResponseWriter, r *http.Request) {
	var command struct {
		Cases []protectedCase `json:"cases"`
	}
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	data, err := json.Marshal(command.Cases)
	if err != nil {
		api.failure(w, err)
		return
	}
	if _, err := protectedCases(data); err != nil {
		api.failure(w, err)
		return
	}
	record, err := api.mutation(r.Context(), key, r.PathValue("system_id"), "learning.checks", command, func(tx *sql.Tx, s systemRecord) (any, error) {
		var count int
		if err := tx.QueryRowContext(r.Context(), `SELECT count(*) FROM learning_checks WHERE system_id=?`, s.ID).Scan(&count); err != nil {
			return nil, err
		}
		if count >= 64 {
			return nil, invalid("checks", "protected check capacity exhausted")
		}
		id, err := newID("check_")
		if err != nil {
			return nil, err
		}
		digest := artifactDigest(data)
		_, err = tx.ExecContext(r.Context(), `INSERT INTO learning_checks(system_id,check_id,cases,digest,created_at) VALUES(?,?,?,?,?)`, s.ID, id, data, digest, time.Now().UnixMilli())
		return map[string]string{"check_id": id, "digest": digest}, err
	})
	api.systemResponse(w, 201, record, err)
}

func (api *controlAPI) learningFeedback(w http.ResponseWriter, r *http.Request) {
	var command struct {
		TaskID  string `json:"task_id"`
		Rating  string `json:"rating"`
		Content string `json:"content"`
	}
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	if (command.Rating != "success" && command.Rating != "failure") || strings.TrimSpace(command.Content) == "" || len(command.Content) > 2048 {
		api.failure(w, invalid("feedback", "requires success/failure and 1-2048 bytes of evidence"))
		return
	}
	record, err := api.mutation(r.Context(), key, r.PathValue("system_id"), "learning.feedback", command, func(tx *sql.Tx, s systemRecord) (any, error) {
		if _, err := readTask(r.Context(), tx, s.ID, command.TaskID); err != nil {
			return nil, err
		}
		var count int
		if err := tx.QueryRowContext(r.Context(), `SELECT count(*) FROM learning_feedback WHERE system_id=?`, s.ID).Scan(&count); err != nil {
			return nil, err
		}
		if count >= 512 {
			return nil, invalid("feedback", "feedback capacity exhausted")
		}
		id, err := newID("feedback_")
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(r.Context(), `INSERT INTO learning_feedback(system_id,feedback_id,task_id,rating,content,created_at) VALUES(?,?,?,?,?,?)`, s.ID, id, command.TaskID, command.Rating, command.Content, time.Now().UnixMilli())
		return map[string]string{"feedback_id": id}, err
	})
	api.systemResponse(w, 201, record, err)
}

func (api *controlAPI) learningEvaluate(w http.ResponseWriter, r *http.Request) {
	if !api.executionAvailable(w) {
		return
	}
	var command evaluateCommand
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	record, err := api.mutation(r.Context(), key, r.PathValue("system_id"), "learning.evaluate", command, func(tx *sql.Tx, s systemRecord) (any, error) {
		return queueLearning(r.Context(), tx, api.engine, s, command, localAdministrator, time.Now())
	})
	if err == nil {
		api.engine.notify()
	}
	api.systemResponse(w, 202, record, err)
}

func queueLearning(ctx context.Context, tx *sql.Tx, engine *executionEngine, s systemRecord, command evaluateCommand, source string, now time.Time) (any, error) {
	if s.State != "running" || s.Configuration.Limits.TokenBudget <= s.UsedTokens+s.ReservedTokens {
		return nil, errExecutionConflict
	}
	origin, err := readTask(ctx, tx, s.ID, command.TaskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errSystemNotFound
	}
	if err != nil {
		return nil, err
	}
	if taskTerminal(origin.State) || origin.Control != "active" || origin.LearningID != "" {
		return nil, errExecutionConflict
	}
	parent, err := readAgent(ctx, tx, s.ID, origin.AgentID, origin.Revision)
	if err != nil {
		return nil, err
	}
	if parent.State != "active" || parent.Depth >= 8 {
		return nil, errExecutionConflict
	}
	var deadline, budget, used, reserved int64
	var control string
	if err := tx.QueryRowContext(ctx, `SELECT deadline,token_budget,used_tokens,reserved_tokens,control FROM goals WHERE system_id=? AND goal_id=?`, s.ID, origin.GoalID).Scan(&deadline, &budget, &used, &reserved, &control); err != nil {
		return nil, err
	}
	if control != "active" || deadline <= now.UnixMilli() || budget <= used+reserved {
		return nil, invalid("goal", "evaluation exceeds remaining goal lifetime or budget")
	}
	draft, err := readLearningDraft(ctx, tx, s.ID, command.ToolID, command.Version)
	if err != nil {
		return nil, err
	}
	var data []byte
	if err := tx.QueryRowContext(ctx, `SELECT cases FROM learning_checks WHERE system_id=? AND check_id=?`, s.ID, command.CheckID).Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errSystemNotFound
		}
		return nil, err
	}
	cases, err := protectedCases(data)
	if err != nil {
		return nil, err
	}
	if err := authorizePins(ctx, tx, engine.cfg, s.ID, origin.Tools); err != nil {
		return nil, err
	}
	profile := engine.cfg.SandboxProfiles[parent.Definition.SandboxProfile]
	profileDigest, _, err := jsonDigest(profile)
	if err != nil {
		return nil, err
	}
	toolchain := ""
	if draft.Kind == "executable" {
		if len(draft.Requires) != 0 {
			return nil, invalid("capabilities", "generated tools cannot call brokers or other tools")
		}
		if err := generatedProfile(profile); err != nil {
			return nil, err
		}
		if engine.cfg.Learning == nil {
			return nil, invalid("learning", "a pinned local Go toolchain is required")
		}
		if err := validateGeneratedSource(draft.Content); err != nil {
			return nil, err
		}
		toolchain = engine.cfg.Learning.ToolchainDigest
	}
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM learning_evaluations WHERE system_id=? AND tool_id=? AND version=?`, s.ID, command.ToolID, command.Version).Scan(&existing); err != nil {
		return nil, err
	}
	if existing != 0 {
		return nil, invalid("evaluation", "this revision was already evaluated; submit a new immutable version")
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM learning_evaluations e JOIN tasks t ON t.system_id=e.system_id AND t.task_id=e.origin_task WHERE e.system_id=? AND t.goal_id=?`, s.ID, origin.GoalID).Scan(&existing); err != nil {
		return nil, err
	}
	if existing >= 4 {
		return nil, invalid("evaluation", "four evaluations per goal allowed")
	}
	var agents, tasks int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agents WHERE system_id=? AND state!='stopped'`, s.ID).Scan(&agents); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE system_id=? AND goal_id=?`, s.ID, origin.GoalID).Scan(&tasks); err != nil {
		return nil, err
	}
	if agents >= s.Configuration.Limits.MaxAgents || tasks+int64(2*len(cases)) > 64 {
		return nil, invalid("evaluation", "ordinary agent/task capacity exhausted")
	}
	locals, err := loadLocalTools(ctx, tx, s.ID, origin.Tools)
	if err != nil {
		return nil, err
	}
	cfg := engine.cfg.withLocalTools(locals)
	baselinePrompt, candidatePrompt := parent.Definition.Prompt, parent.Definition.Prompt
	baselineFound := command.Baseline == nil
	for _, pin := range origin.Tools {
		tool, _ := cfg.tool(pin.Name)
		replaced := command.Baseline != nil && pin == *command.Baseline
		if replaced {
			baselineFound = true
			if tool.Kind != draft.Kind {
				return nil, invalid("baseline", "baseline kind differs from candidate")
			}
			if draft.Kind == "executable" && (tool.BinaryDigest == "" || tool.ProfileDigest != profileDigest) {
				return nil, invalid("baseline", "requires an approved generated tool with the same capabilities")
			}
		}
		if tool.Kind == "skill" {
			baselinePrompt += "\n\n" + tool.Content
			if !replaced {
				candidatePrompt += "\n\n" + tool.Content
			}
		}
	}
	if !baselineFound {
		return nil, invalid("baseline", "baseline must be an exact originating task pin")
	}
	candidatePrompt += "\n\n" + draft.Content
	if len(baselinePrompt) > 32768 || (draft.Kind == "skill" && len(candidatePrompt) > 32768) {
		return nil, invalid("evaluation", "combined prompts exceed context bound")
	}
	for _, required := range draft.Requires {
		found := false
		for _, pin := range origin.Tools {
			if pin.Name == required {
				found = true
			}
		}
		if !found {
			return nil, invalid("requires_tools", "evaluation cannot expand originating task capabilities")
		}
	}
	id, err := newID("evaluation_")
	if err != nil {
		return nil, err
	}
	agentID, err := newID("agent_")
	if err != nil {
		return nil, err
	}
	def := parent.Definition
	def.Tools = []string{}
	def.Prompt = baselinePrompt
	agent := agentRecord{SystemID: s.ID, ID: agentID, Parent: parent.ID, GoalID: origin.GoalID, Name: "protected-evaluation", Revision: 1, State: "active", Depth: parent.Depth + 1, TokenBudget: min(parent.TokenBudget, budget-used-reserved), MaxTurns: 8, Definition: def, Tools: []toolPin{}}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agents(system_id,agent_id,parent_id,goal_id,name,revision,state,depth,token_budget,max_turns) VALUES(?,?,?,?,?,1,'active',?,?,8)`, s.ID, agent.ID, parent.ID, origin.GoalID, agent.Name, agent.Depth, agent.TokenBudget); err != nil {
		return nil, err
	}
	if err := insertAgentRevision(ctx, tx, agent); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_usage(system_id,agent_id,goal_id) VALUES(?,?,?)`, s.ID, agent.ID, origin.GoalID); err != nil {
		return nil, err
	}
	if draft.Kind == "skill" {
		agent.Revision = 2
		agent.Definition.Prompt = candidatePrompt
		if err := insertAgentRevision(ctx, tx, agent); err != nil {
			return nil, err
		}
	}
	baseline, err := json.Marshal(evaluationBaseline{Prompt: baselinePrompt, Tool: command.Baseline})
	if err != nil {
		return nil, err
	}
	state := "queued"
	if draft.Kind == "skill" {
		state = "evaluating"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO learning_evaluations(system_id,evaluation_id,tool_id,version,check_id,origin_task,agent_id,baseline,profile_digest,toolchain_digest,runtime_digest,system_revision,configuration_digest,notify,state)
	 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, s.ID, id, command.ToolID, command.Version, command.CheckID, origin.ID, agent.ID, baseline, profileDigest, toolchain, engine.cfg.runtimeDigest, s.Revision, s.Grants.DefinitionsDigest, source != localAdministrator, state); err != nil {
		return nil, err
	}
	for index, check := range cases {
		roles := []string{"baseline", "candidate"}
		if draft.Kind == "executable" {
			roles = []string{"build"}
		}
		for _, role := range roles {
			taskID, err := newID("task_")
			if err != nil {
				return nil, err
			}
			revision := int64(1)
			if role == "candidate" {
				revision = 2
			}
			conversation, err := json.Marshal([]chatMessage{{Role: "user", Content: check.Input}})
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(system_id,task_id,goal_id,agent_id,agent_revision,parent_task,state,tools,conversation,turns,call_id,learning_id,learning_role)
			 VALUES(?,?,?,?,?,?,'queued','[]',?,0,'',?,?)`, s.ID, taskID, origin.GoalID, agent.ID, revision, origin.ID, conversation, id, role+string(rune('0'+index))); err != nil {
				return nil, err
			}
			task, err := readTask(ctx, tx, s.ID, taskID)
			if err != nil {
				return nil, err
			}
			kind := "task.ready"
			if role == "build" {
				kind = "tool.evaluate"
			} else if err := prepareNextCall(ctx, tx, engine.cfg, task, engine.owner, now); err != nil {
				return nil, err
			}
			if _, err := emitEvent(ctx, tx, task, kind, source, "", struct{}{}, now); err != nil {
				return nil, err
			}
		}
		if draft.Kind == "executable" {
			break
		}
	}
	return map[string]string{"evaluation_id": id, "state": state}, nil
}

func (api *controlAPI) learningApprove(w http.ResponseWriter, r *http.Request) {
	var command struct {
		Digest         string `json:"digest"`
		ExpiresSeconds int64  `json:"expires_seconds"`
		TaskUses       int64  `json:"task_uses"`
	}
	key, err := readCommand(w, r, &command)
	if err != nil {
		api.failure(w, err)
		return
	}
	if len(command.Digest) != 64 || command.ExpiresSeconds < 1 || command.ExpiresSeconds > 86400 || command.TaskUses < 1 || command.TaskUses > 64 {
		api.failure(w, invalid("approval", "requires an exact evidence digest, 1-86400 second lifetime and 1-64 task uses"))
		return
	}
	id := r.PathValue("evaluation_id")
	record, err := api.mutation(r.Context(), key, r.PathValue("system_id"), "learning.approve", struct {
		ID      string
		Command any
	}{id, command}, func(tx *sql.Tx, s systemRecord) (any, error) {
		var tool, state, digest, configurationDigest string
		var version, systemRevision int64
		var definition []byte
		err := tx.QueryRowContext(r.Context(), `SELECT tool_id,version,state,digest,definition,system_revision,configuration_digest FROM learning_evaluations WHERE system_id=? AND evaluation_id=?`, s.ID, id).Scan(&tool, &version, &state, &digest, &definition, &systemRevision, &configurationDigest)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errSystemNotFound
		}
		if err != nil {
			return nil, err
		}
		if state != "pending_approval" || digest != command.Digest {
			return nil, invalid("approval", "stale, replayed or mismatched evaluation approval")
		}
		s, err = api.cfg.inspect(s)
		if err != nil {
			return nil, err
		}
		if s.Revision != systemRevision || s.Grants.DefinitionsDigest != configurationDigest || s.BlockedReason != "" {
			return nil, invalid("approval", "evaluated configuration or baseline revision changed")
		}
		if _, err := readLearningDraft(r.Context(), tx, s.ID, tool, version); err != nil {
			return nil, err
		}
		var def toolConfig
		if err := decodeJSON(definition, &def); err != nil {
			return nil, err
		}
		if def.Kind == "executable" {
			if _, err := readGeneratedArtifact(api.cfg.DataDir, s.ID, def.BinaryDigest); err != nil {
				return nil, err
			}
		}
		pinDigest, _, err := jsonDigest(def)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(r.Context(), `INSERT INTO learning_approvals(system_id,tool_id,version,evaluation_id,definition,digest,expires_at,remaining_tasks) VALUES(?,?,?,?,?,?,?,?)`,
			s.ID, tool, version, id, definition, pinDigest, time.Now().Add(time.Duration(command.ExpiresSeconds)*time.Second).UnixMilli(), command.TaskUses)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(r.Context(), `UPDATE learning_evaluations SET state='active' WHERE system_id=? AND evaluation_id=?`, s.ID, id); err != nil {
			return nil, err
		}
		return toolPin{Name: tool, Version: version, Digest: pinDigest}, nil
	})
	api.systemResponse(w, 200, record, err)
}

func (api *controlAPI) learningView(w http.ResponseWriter, r *http.Request) {
	after, err := runtimeCursor(r)
	if err != nil {
		api.failure(w, err)
		return
	}
	tx, err := api.store.db.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		api.failure(w, err)
		return
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			api.logger.Printf("learning view rollback: %v", err)
		}
	}()
	systemID := r.PathValue("system_id")
	if _, err := readSystem(r.Context(), tx, localAdministrator, systemID); err != nil {
		api.failure(w, err)
		return
	}
	query := ""
	section := r.PathValue("section")
	switch section {
	case "":
		query = `SELECT evaluation_id,json_object('evaluation_id',evaluation_id,'tool_id',tool_id,'version',version,'check_id',check_id,'origin_task',origin_task,'state',state,'reason',reason,'digest',digest,'artifact_digest',artifact_digest,'profile_digest',profile_digest,'toolchain_digest',toolchain_digest,'baseline',json(baseline),'evidence',json(evidence)) FROM learning_evaluations WHERE system_id=? AND evaluation_id>? ORDER BY evaluation_id LIMIT 21`
	case "checks":
		query = `SELECT check_id,json_object('check_id',check_id,'digest',digest,'cases',json(cases),'created_at',created_at) FROM learning_checks WHERE system_id=? AND check_id>? ORDER BY check_id LIMIT 21`
	case "feedback":
		query = `SELECT feedback_id,json_object('feedback_id',feedback_id,'task_id',task_id,'rating',rating,'content',content,'created_at',created_at) FROM learning_feedback WHERE system_id=? AND feedback_id>? ORDER BY feedback_id LIMIT 21`
	default:
		api.failure(w, errSystemNotFound)
		return
	}
	rows, err := tx.QueryContext(r.Context(), query, systemID, after)
	if err != nil {
		api.failure(w, err)
		return
	}
	items := []json.RawMessage{}
	ids := []string{}
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			rows.Close()
			api.failure(w, err)
			return
		}
		ids = append(ids, id)
		items = append(items, json.RawMessage(data))
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		api.failure(w, err)
		return
	}
	next := ""
	if len(items) > 20 {
		items = items[:20]
		next = ids[19]
	}
	api.respond(w, 200, struct {
		Items []json.RawMessage `json:"items"`
		Next  string            `json:"next,omitempty"`
	}{items, next})
}
