package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
)

// The goal API: the web counterpart of the /goal console command. Commands
// execute without a model turn; set/resume additionally start a goal run
// (the assess → execute → evaluate loop) whose SSE stream carries the usual
// delta/tool events plus a "goal" event on every goal-state change. Goal
// state persists per session as a <id>.goal.json sidecar so a server restart
// can restore it.

// handleGetGoal returns the session's goal: the live goal when this process
// owns it, otherwise the persisted sidecar (post-restart display).
func (s *Server) handleGetGoal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	g := s.goalForSession(id)
	writeJSON(w, http.StatusOK, map[string]any{"goal": g, "running": s.agent.GoalRunning()})
}

// goalForSession resolves the goal a session should see.
func (s *Server) goalForSession(id string) *agent.Goal {
	if cur := s.agent.Goal(); cur != nil && (cur.SessionID == "" || cur.SessionID == id) {
		return cur
	}
	return s.sessions.loadGoal(id)
}

// handleGoalCommand applies a /goal command (identical syntax to the TUI)
// without a model turn and starts the goal loop when appropriate.
func (s *Server) handleGoalCommand(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Command   string `json:"command"`
		Workspace string `json:"workspace"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cmd, err := agent.ParseGoalCommand(strings.Fields(req.Command))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	output := ""
	var runID any
	switch cmd.Verb {
	case "help":
		output = agent.GoalHelpText

	case "status":
		output = agent.FormatGoal(s.goalForSession(id))

	case "set":
		if s.agent.GoalRunning() {
			writeError(w, http.StatusConflict, "goal loop is running (pause or clear it first)")
			return
		}
		if s.agent.Goal() != nil {
			s.agent.ClearGoal()
		}
		if _, err := s.agent.SetGoal(cmd.Objective, id, cmd.MaxCycles, cmd.MaxTokens); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		g := s.agent.Goal()
		output = fmt.Sprintf("goal set (max %d cycles · %s tokens): %s\nthe loop starts now — completion claims are verified by an independent evaluator.",
			g.MaxCycles, agent.FormatGoalTokens(g.MaxTokens), g.Objective)
		run, err := s.startGoalRun(id, req.Workspace)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		runID = run.ID()

	case "pause":
		if !s.agent.PauseGoal() {
			g := s.agent.Goal()
			if g == nil {
				writeError(w, http.StatusNotFound, "no goal set")
			} else {
				writeError(w, http.StatusConflict, fmt.Sprintf("goal is %s, nothing to pause", g.Status))
			}
			return
		}
		output = "goal paused (stops at the next cycle boundary)"

	case "resume":
		// Ensure semantics: also used by the frontend to continue an active
		// idle goal after an interjected user run — never a hard error.
		if s.agent.Goal() == nil {
			if persisted := s.sessions.loadGoal(id); persisted != nil {
				if err := s.agent.RestoreGoal(persisted); err == nil {
					s.agent.ResumeGoal()
				}
			}
		}
		g := s.agent.Goal()
		switch {
		case g == nil:
			output = "no goal set"
		case g.Status == agent.GoalComplete:
			output = "goal is already complete: " + agent.FormatGoal(g)
		case s.agent.GoalRunning():
			output = "goal loop is already running"
		default:
			if g.Status != agent.GoalActive {
				if !s.agent.ResumeGoal() {
					output = agent.FormatGoal(g)
					break
				}
			}
			run, err := s.startGoalRun(id, req.Workspace)
			if err != nil {
				output = "goal resumed but the loop could not start: " + err.Error()
				break
			}
			runID = run.ID()
			output = "goal resumed — the loop continues"
		}

	case "clear":
		if !s.agent.ClearGoal() {
			output = "no goal set"
		} else {
			output = "goal cleared"
		}

	case "budget":
		var cycles int
		var tokens int64
		if cmd.BudgetKey == "cycles" {
			cycles = int(cmd.BudgetVal)
		} else {
			tokens = cmd.BudgetVal
		}
		if !s.agent.GoalSetBudget(cycles, tokens) {
			writeError(w, http.StatusNotFound, "no goal set")
			return
		}
		g := s.agent.Goal()
		output = fmt.Sprintf("budget: cycle %d/%d · %s/%s tokens", g.Cycles, g.MaxCycles, agent.FormatGoalTokens(g.TokensUsed), agent.FormatGoalTokens(g.MaxTokens))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"output": output,
		"goal":   s.agent.Goal(),
		"run_id": runID,
	})
}

// startGoalRun launches the goal loop as a run over the session history.
// Goal-state changes stream into the run as "goal" events; the final
// transcript (all cycle messages) persists back to the session on completion.
func (s *Server) startGoalRun(sessionID, workspace string) (*Run, error) {
	a := s.agent
	if g := a.Goal(); g == nil || g.Status != agent.GoalActive {
		return nil, fmt.Errorf("server: goal is not active")
	}
	if a.GoalRunning() {
		return nil, fmt.Errorf("server: goal loop already running")
	}
	if workspace == "" {
		workspace = s.workdir
	}
	id := fmt.Sprintf("run-%d", atomic.AddInt64(&runSeq, 1))
	// Same run-scoped memory workspace as startRun: the ctx value feeds
	// RetainGoal (WithoutCancel preserves it) and the goal graph's prefetch.
	ctx, cancel := context.WithCancel(agent.WithMemoryWorkspace(context.Background(), workspace))
	run := newRun(id, a, ctx, cancel)

	s.mu.Lock()
	s.runs[id] = run
	s.mu.Unlock()

	s.log.Info().Str("run", id).Str("session", sessionID).Msg("goal run started")
	s.refreshDebugLog()
	if obs := a.Obs(); obs != nil {
		a.SetObsRun(workspace, sessionID, id)
		obs.Log("info", "goal_run_start", map[string]any{})
	}
	emitGoal := func(g *agent.Goal) {
		run.emit(Event{Type: "goal", Data: map[string]any{"goal": g, "running": a.GoalRunning()}})
	}
	unsub := a.SubscribeGoal(emitGoal)
	emitGoal(a.Goal())

	history := s.sessions.load(sessionID)
	run.startGoal(history, func(final []messages.Message) {
		unsub()
		_ = s.sessions.save(sessionID, stripSystem(final), workspace)
		if g := a.Goal(); g != nil {
			s.log.Info().Str("run", id).Str("session", sessionID).
				Str("status", string(g.Status)).Msg("goal run ended")
		}
		s.logObsRunEnd(sessionID, id, final)
	})
	return run, nil
}
