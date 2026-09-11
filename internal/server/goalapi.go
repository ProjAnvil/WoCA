package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/wocadb"
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

// startGoalRun launches the goal loop as a run over the session history,
// behind the session admission gate (same pattern as startRun; the
// agent-level goal claim below stays as defense in depth). Goal-state
// changes stream into the run as "goal" events; the final transcript (all
// cycle messages) persists back to the session on completion.
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
	id := newRunID()
	// Same run-scoped memory workspace as startRun: the ctx value feeds
	// RetainGoal (WithoutCancel preserves it) and the goal graph's prefetch.
	ctx, cancel := context.WithCancel(agent.WithRunWorkspace(context.Background(), workspace))
	run := newRun(id, a, ctx, cancel)
	s.registerRun(id, run)

	s.log.Info().Str("run", id).Str("session", sessionID).Msg("goal run started")
	s.refreshDebugLog()
	if obs := a.Obs(); obs != nil {
		a.SetObsRun(workspace, sessionID, id)
		obs.Log("info", "goal_run_start", map[string]any{})
	}
	emitGoal := func(g *agent.Goal) {
		run.emit(Event{Type: EventGoal, Data: map[string]any{"goal": g, "running": a.GoalRunning()}})
	}
	unsub := a.SubscribeGoal(emitGoal)
	emitGoal(a.Goal())
	// The subscription is tied to the run's terminal event, which fires on
	// success, error and cancellation alike — an error-exiting goal run must
	// not leave its subscriber (and this Run closure) on the agent forever.
	// The same hook clears the session run-state row (compare-and-clear).
	run.onFinish = func() { unsub(); s.clearSessionState(sessionID, id) }
	run.onPark = func() { s.markSessionState(sessionID, wocadb.SessionAwaiting, id) }
	run.onResume = func() { s.markSessionState(sessionID, wocadb.SessionRunning, id) }

	w, pos, ok := s.admission.enter(sessionID, func(p int) {
		run.emit(Event{Type: EventQueuePosition, Data: map[string]any{"position": p}})
	})
	if !ok {
		run.finish(EventError, map[string]any{"message": "session run queue is full"})
		return nil, errQueueFull
	}
	if pos > 0 {
		run.emit(Event{Type: EventRunQueued, Data: map[string]any{"position": pos}})
	}
	go func() {
		if w != nil && !s.admission.wait(run.ctx, sessionID, w) {
			// Cancelled while queued (cancelRun cancelled run.ctx) or shutdown.
			run.finish(EventDone, map[string]any{"cancelled": true})
			return
		}
		run.emit(Event{Type: EventRunStarted, Data: map[string]any{}})
		s.markSessionState(sessionID, wocadb.SessionRunning, id)
		history := s.sessions.load(run.ctx, sessionID)
		run.startGoal(history, func(final []messages.Message) {
			if final != nil {
				// Completion persistence must survive run cancellation:
				// keep the ctx values, drop the cancellation.
				_ = s.sessions.save(context.WithoutCancel(run.ctx), sessionID, stripSystem(final), workspace)
				if g := a.Goal(); g != nil {
					s.log.Info().Str("run", id).Str("session", sessionID).
						Str("status", string(g.Status)).Msg("goal run ended")
				}
				s.logObsRunEnd(sessionID, id, workspace, final)
			}
			// Correctness core: the session gate is released only after
			// onDone (including persistence) returns.
			s.admission.release(sessionID)
		})
	}()
	return run, nil
}
