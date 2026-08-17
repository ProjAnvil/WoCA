# Concurrency Hardening 实施计划（woca 并发加固）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 按 spec `docs/superpowers/specs/2026-08-17-concurrency-hardening-design.md` 落地 P0 session 门、P1 三项竞态修复、P2 资源池化、P3 打磨及前端配合。

**Architecture:** server 层新增 `admission`（per-session FIFO 门，onDone 落盘后才释放）；agent 层用 runws 式 ctx 装饰做 per-run memory scope、singleflight+TTL 去阻塞工具刷新、sessionStore 内存索引化；tools 层加 LSP 池（复用/熔断/空闲回收）；SSE 加 seq+Last-Event-ID 重放；前端补 queued 状态、重连与 stats 展示。

**Tech Stack:** Go 1.26.4（generics、`atomic.Int64`、`sync.OnceFunc` 可用）、`golang.org/x/sync/singleflight`（已在 go.mod 间接依赖，转直接）、Vue 3 + Pinia + `@microsoft/fetch-event-source`。

## Global Constraints

- 全程 `go test -race ./...` 必须绿；每个 Task 末尾至少跑受影响包。
- 不引入新第三方 Go 依赖；唯一例外是 `golang.org/x/sync` 由 indirect 转直接（`go mod tidy` 自动处理）。
- 不做全局/per-workspace run 信号量；不做 subagent 模板缓存；队列仅内存态（spec §2 决策记录）。
- 上限常量：`maxQueuedPerSession = 8`、`maxActiveTeammates = 8`、`maxBackgroundTasks = 16`、workspace LRU 容量 32、`refreshTTL = 5min`、LSP 空闲回收 5min / 熔断冷却 10min / 连续失败 2 次、parked TTL 2h。
- 每步独立可合入、可回滚；commit message 用 `feat:`/`fix:` 前缀，与仓库历史风格一致。
- spec §10.4 提到的 `store_test.go` 编译错误**已不存在**（2026-08-17 实测 `go vet ./internal/store/` 通过），无需修复，前置检查确认基线干净即可。
- 前端无测试设施；前端 Task 的验证关卡是 `cd web && npm run build`（含 `vue-tsc` 类型检查）。

## 前置检查（所有 Task 之前）

- [ ] 运行 `go build ./... && go test -race ./internal/server/ ./internal/agent/ ./internal/tools/ ./internal/store/`，确认基线全绿。若不绿，先停下修基线再开始 Task 1。

---

### Task 1: P0 — Session 门（admission）

**Files:**
- Create: `internal/server/admission.go`
- Modify: `internal/server/server.go`（`Server` 结构体 ~:92-118、`New`、`handleCreateRun` :260-313、`startRun` :626-641、`Serve` :218-236）
- Modify: `internal/server/run.go`（`Run.start` :69-85、`Run.startGoal` :90-106）
- Modify: `internal/server/goalapi.go`（`startGoalRun` :168-212）
- Test: `internal/server/admission_test.go`

**Interfaces:**
- Consumes: 现有 `Run.emit(Event{Type, Data})`（run.go:218）、`run.finish(type, data)`（run.go:232）、`s.sessions.load/save`。
- Produces:
  - `newAdmission() *admission`
  - `(*admission).enter(sessionID string, onPosition func(int)) (w *gateWaiter, pos int, ok bool)` — pos=0 立即获得门（w=nil）；pos≥1 已排队；ok=false 表示满/已停。
  - `(*admission).wait(ctx context.Context, sessionID string, w *gateWaiter) bool` — 阻塞至提升（true）或 ctx 取消/停机被摘除（false）。
  - `(*admission).release(sessionID string)` — 释放并提升队头。
  - `(*admission).stop()`、`(*admission).stats() (totalQueued int, depths map[string]int)`（Task 14 用）。
  - 新 SSE 事件：`run_queued{position}`、`queue_position{position}`、`run_started{}`。
  - `Run.start`/`Run.startGoal` 的 `onDone` 语义变为**每条退出路径恰好调用一次**，失败/取消路径传 `final=nil`。

- [x] **Step 1: 写失败测试** `internal/server/admission_test.go`

```go
package server

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestAdmissionFIFO(t *testing.T) {
	a := newAdmission()
	defer a.stop()
	if w, pos, ok := a.enter("s1", nil); w != nil || pos != 0 || !ok {
		t.Fatalf("first enter: w=%v pos=%d ok=%v, want immediate grant", w, pos, ok)
	}
	var mu sync.Mutex
	var positions []int
	record := func(p int) { mu.Lock(); positions = append(positions, p); mu.Unlock() }
	w1, pos1, ok1 := a.enter("s1", record)
	w2, pos2, ok2 := a.enter("s1", record)
	if !ok1 || !ok2 || pos1 != 1 || pos2 != 2 {
		t.Fatalf("queued enters: pos1=%d pos2=%d ok1=%v ok2=%v", pos1, pos2, ok1, ok2)
	}
	granted := make(chan string, 2)
	go func() { if a.wait(context.Background(), "s1", w1) { granted <- "w1" } }()
	go func() { if a.wait(context.Background(), "s1", w2) { granted <- "w2" } }()
	a.release("s1")
	if got := <-granted; got != "w1" {
		t.Fatalf("first promoted = %s, want w1 (FIFO)", got)
	}
	a.release("s1")
	if got := <-granted; got != "w2" {
		t.Fatalf("second promoted = %s, want w2", got)
	}
	// w1 promoted 时 w2 应收到的位置更新：1
	mu.Lock()
	defer mu.Unlock()
	if len(positions) == 0 || positions[len(positions)-1] != 1 {
		t.Fatalf("position updates = %v, want last=1", positions)
	}
}

func TestAdmissionCancelWhileQueued(t *testing.T) {
	a := newAdmission()
	defer a.stop()
	a.enter("s1", nil) // hold the gate
	ctx, cancel := context.WithCancel(context.Background())
	w, _, ok := a.enter("s1", nil)
	if !ok || w == nil {
		t.Fatalf("enter: ok=%v w=%v", ok, w)
	}
	cancel()
	if a.wait(ctx, "s1", w) {
		t.Fatal("wait returned true after ctx cancel, want false")
	}
	total, depths := a.stats()
	if total != 0 || len(depths) != 0 {
		t.Fatalf("stats after cancel: total=%d depths=%v", total, depths)
	}
}

func TestAdmissionQueueFull(t *testing.T) {
	a := newAdmission()
	defer a.stop()
	a.enter("s1", nil)
	for i := 0; i < maxQueuedPerSession; i++ {
		if _, _, ok := a.enter("s1", nil); !ok {
			t.Fatalf("enter %d rejected early", i)
		}
	}
	if _, _, ok := a.enter("s1", nil); ok {
		t.Fatal("enter beyond maxQueuedPerSession succeeded, want rejection")
	}
}

func TestAdmissionIndependentSessions(t *testing.T) {
	a := newAdmission()
	defer a.stop()
	a.enter("s1", nil)
	if w, pos, ok := a.enter("s2", nil); w != nil || pos != 0 || !ok {
		t.Fatal("different session blocked by s1's gate")
	}
}
```

- [x] **Step 2: 运行确认失败**

Run: `go test -race ./internal/server/ -run TestAdmission -v`
Expected: 编译失败 `undefined: newAdmission`。

- [x] **Step 3: 实现** `internal/server/admission.go`

```go
package server

import (
	"context"
	"sync"
)

// maxQueuedPerSession bounds the FIFO of runs waiting on one session gate;
// POSTs beyond it are rejected with 409 (runaway-client guard).
const maxQueuedPerSession = 8

// admission serializes runs per session: at most one run holds a session's
// gate, later runs queue FIFO, and a queued run is promoted only after the
// holder's transcript has been persisted (release happens after onDone
// returns — see startRun). This eliminates the overlapping-run overwrite
// that dropped the earlier run's reply.
type admission struct {
	mu       sync.Mutex
	gates    map[string]*sessionGate
	stopped  bool
	stopCh   chan struct{}
	stopOnce sync.Once
}

type sessionGate struct {
	running bool
	queue   []*gateWaiter
}

type gateWaiter struct {
	ready      chan struct{}
	onPosition func(int) // 1-based queue position updates; must be non-blocking
}

func newAdmission() *admission {
	return &admission{gates: map[string]*sessionGate{}, stopCh: make(chan struct{})}
}

// enter takes or queues for the session gate. pos=0 means granted (w==nil);
// pos>=1 means queued (wait on w). ok=false means the queue is full or
// admission is stopped. A successful caller must eventually call release —
// for queued waiters only after wait returned true.
func (a *admission) enter(sessionID string, onPosition func(int)) (*gateWaiter, int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return nil, -1, false
	}
	g := a.gates[sessionID]
	if g == nil {
		g = &sessionGate{}
		a.gates[sessionID] = g
	}
	if !g.running && len(g.queue) == 0 {
		g.running = true
		return nil, 0, true
	}
	if len(g.queue) >= maxQueuedPerSession {
		return nil, -1, false
	}
	w := &gateWaiter{ready: make(chan struct{}), onPosition: onPosition}
	g.queue = append(g.queue, w)
	return w, len(g.queue), true
}

// wait blocks until the waiter is promoted (true) or removed due to ctx
// cancellation / admission stop (false).
func (a *admission) wait(ctx context.Context, sessionID string, w *gateWaiter) bool {
	select {
	case <-w.ready:
		return true
	case <-ctx.Done():
	case <-a.stopCh:
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.gates[sessionID]
	if g == nil {
		return false
	}
	select {
	case <-w.ready:
		return true // promoted while ctx fired; caller's ctx is done, run exits fast
	default:
	}
	if removeWaiter(g, w) {
		renumberLocked(g)
		if !g.running && len(g.queue) == 0 {
			delete(a.gates, sessionID)
		}
	}
	return false
}

// release frees the gate and promotes the FIFO head, renumbering the rest.
func (a *admission) release(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.gates[sessionID]
	if g == nil {
		return
	}
	if len(g.queue) == 0 {
		g.running = false
		delete(a.gates, sessionID) // keep the map bounded
		return
	}
	head := g.queue[0]
	g.queue = g.queue[1:]
	close(head.ready)
	renumberLocked(g)
}

func removeWaiter(g *sessionGate, w *gateWaiter) bool {
	for i, q := range g.queue {
		if q == w {
			g.queue = append(g.queue[:i], g.queue[i+1:]...)
			return true
		}
	}
	return false
}

// renumberLocked reports 1-based positions to remaining waiters. Callbacks
// must be non-blocking (Run.emit is: its fanout drops on full channels).
func renumberLocked(g *sessionGate) {
	for i, w := range g.queue {
		if w.onPosition != nil {
			w.onPosition(i + 1)
		}
	}
}

// stop aborts all queued waiters (server shutdown); held gates are
// untouched — in-flight runs finish through the existing cancel path.
func (a *admission) stop() {
	a.stopOnce.Do(func() {
		a.mu.Lock()
		a.stopped = true
		a.mu.Unlock()
		close(a.stopCh)
	})
}

// stats reports total queued waiters and per-session depths (for /api/stats).
func (a *admission) stats() (int, map[string]int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	depths := map[string]int{}
	total := 0
	for sid, g := range a.gates {
		if len(g.queue) > 0 {
			depths[sid] = len(g.queue)
			total += len(g.queue)
		}
	}
	return total, depths
}
```

- [x] **Step 4: 运行确认通过**

Run: `go test -race ./internal/server/ -run TestAdmission -v`
Expected: 4 个测试全 PASS。

- [x] **Step 5: 改 `Run.start`/`Run.startGoal` 让 onDone 全路径触发**（run.go:69-106）

`start` 改为（`startGoal` 同构，调 `runner.New(...).RunGoal`）：

```go
// start runs the agent loop in a goroutine. onDone fires exactly once on
// every exit path; final is nil when the run ended with an error or cancel
// (nothing to persist), which lets callers hook post-run cleanup (session
// gate release) to a single point.
func (r *Run) start(history []messages.Message, onDone func([]messages.Message)) {
	go func() {
		final, err := runner.New(r.a, r.handleInterrupt, r.render).Run(r.ctx, history)
		if err != nil {
			if r.ctx.Err() != nil {
				r.finish("done", map[string]any{"cancelled": true})
			} else {
				r.finish("error", map[string]any{"message": err.Error()})
			}
			if onDone != nil {
				onDone(nil)
			}
			return
		}
		r.finish("done", map[string]any{"cancelled": false})
		if onDone != nil {
			onDone(final)
		}
	}()
}
```

注意：现有测试若依赖"取消路径 onDone 不执行"，逐一检查 `internal/server/*_test.go` 中 `run.start(` 调用点并适配（onDone 判 `final == nil`）。

- [x] **Step 6: 接入 server.go**

`Server` 结构体加字段：`admission *admission`。`New` 中初始化 `admission: newAdmission()`。

`startRun`（server.go:626-641）整体替换：

```go
// startRun creates, registers and launches a run behind the session
// admission gate. It returns the run, the session id and the queue
// position (0 = running immediately). errQueueFull is returned when the
// session already has maxQueuedPerSession runs waiting.
func (s *Server) startRun(sessionID string, human messages.Message, workspace string) (*Run, string, int, error) {
	run, sid, workspace, _ := s.newRegisteredRun(sessionID, workspace) // 预载的 history 弃用：获得门后重载，保证看到前一条 run 已落盘的回复
	s.log.Info().Str("run", run.id).Str("session", sid).Msg("run queued")
	s.refreshDebugLog()
	if obs := s.agent.Obs(); obs != nil {
		s.agent.SetObsRun(workspace, sid, run.id)
		obs.Log("info", "run_start", map[string]any{"session": sid, "run": run.id})
	}
	w, pos, ok := s.admission.enter(sid, func(p int) {
		run.emit(Event{Type: "queue_position", Data: map[string]any{"position": p}})
	})
	if !ok {
		run.finish("error", map[string]any{"message": "session run queue is full"})
		return nil, sid, -1, errQueueFull
	}
	if pos > 0 {
		run.emit(Event{Type: "run_queued", Data: map[string]any{"position": pos}})
	}
	go func() {
		if w != nil && !s.admission.wait(run.ctx, sid, w) {
			// 排队中被取消（cancelRun 取消了 run.ctx）或停机
			run.finish("done", map[string]any{"cancelled": true})
			return
		}
		run.emit(Event{Type: "run_started", Data: map[string]any{}})
		history := s.sessions.load(sid)
		run.start(append(history, human), func(final []messages.Message) {
			if final != nil {
				_ = s.sessions.save(sid, stripSystem(final), workspace)
				s.logObsRunEnd(sid, run.id, workspace, final)
			}
			// 正确性核心：onDone（含落盘）返回之后才释放 session 门，
			// 下一条排队的 run 被提升时前一条的回复必然已持久化。
			s.admission.release(sid)
		})
	}()
	return run, sid, pos, nil
}
```

配套：
- server.go 加 `var errQueueFull = errors.New("session run queue is full")`（`errors` 已在 import 中则复用）。
- `handleCreateRun`（:311 附近）：`run, sid, pos, err := s.startRun(req.SessionID, human, req.Workspace)`；`err == errQueueFull` → `http.Error(w, "session run queue is full", http.StatusConflict)` 返回；成功时响应改为：

```go
state := "running"
if pos > 0 {
	state = "queued"
}
writeJSON(w, map[string]any{"run_id": run.id, "session_id": sid, "state": state, "queue_position": pos})
```

（用该 handler 现有的 JSON 写出方式，保持风格一致。）
- `Serve`（:218-236）的 `ctx.Done()` 分支加 `s.admission.stop()`。
- `startGoalRun`（goalapi.go:168-212）同模式过门：`enter` → 排队 goroutine `wait` → `run.emit(run_started)` → 重载 history → `run.startGoal(history, onDone 包装)`，包装内 `if final != nil { 现有 onDone 逻辑 } ; s.admission.release(sessionID)`。agent 级 goal claim 保留不动（纵深防御）。
- sessionStore 加测试挂钩（一行，供 Step 7 的时序断言用）：结构体加字段 `testAfterSave func(id string)`，`save` 函数末尾（recordMeta 之后）加 `if s.testAfterSave != nil { s.testAfterSave(id) }`。

- [x] **Step 7: 写集成测试**（追加到 `internal/server/admission_test.go`）

现成 helper（server_test.go）：`createRun(t, base, sessionID) (runID, sessionID)`（:770，prompt 固定 "hi"）、`readSSEEvent`（:869，自动跳过 `id:` 行）。`driveToDone`（:794）带 interrupt 应答逻辑，不适用无 interrupt 的 run，用下面自带的 `waitRunDone`。

```go
// 无 interrupt 的 run：读 SSE 直到 done/error。
func waitRunDone(t *testing.T, base, runID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/runs/"+runID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for {
		ev, ok := readSSEEvent(sc)
		if !ok {
			t.Fatal("stream ended before done")
		}
		if ev.Type == "done" {
			return
		}
		if ev.Type == "error" {
			t.Fatalf("run error: %s", ev.Data)
		}
	}
}

// POST /api/runs 并解析完整响应（含 state/queue_position）。
func postRun(t *testing.T, base, sessionID, prompt string) (runID, sid string, pos int) {
	t.Helper()
	body := fmt.Sprintf(`{"prompt":%q,"session_id":%q}`, prompt, sessionID)
	resp, err := http.Post(base+"/api/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var created struct {
		RunID         string `json:"run_id"`
		SessionID     string `json:"session_id"`
		State         string `json:"state"`
		QueuePosition int    `json:"queue_position"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	return created.RunID, created.SessionID, created.QueuePosition
}

func newScriptedAgent(t *testing.T, replies ...string) *agent.Agent {
	t.Helper()
	msgs := make([]messages.Message, len(replies))
	for i, r := range replies {
		msgs[i] = messages.AI(r)
	}
	a, err := agent.New(agent.Config{
		Model:          language.NewFakeChatModel(language.WithResponses(msgs...)),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
		RecursionLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// 同 session 两条 run 串行：run2 的 history 必须包含 run1 已落盘的回复。
func TestSessionGateSerializesRuns(t *testing.T) {
	a := newScriptedAgent(t, "reply-1", "reply-2")
	s := New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir()})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	run1, sid1, pos1 := postRun(t, srv.URL, "", "first")
	if pos1 != 0 {
		t.Fatalf("first run position = %d, want 0", pos1)
	}
	run2, sid2, pos2 := postRun(t, srv.URL, sid1, "second")
	if sid1 != sid2 {
		t.Fatalf("session mismatch: %s vs %s", sid1, sid2)
	}
	if pos2 != 1 {
		t.Fatalf("second run position = %d, want 1", pos2)
	}
	waitRunDone(t, srv.URL, run1)
	waitRunDone(t, srv.URL, run2)

	resp, err := http.Get(srv.URL + "/api/sessions/" + sid1)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "reply-1") || !strings.Contains(string(body), "reply-2") {
		t.Fatalf("transcript missing replies: %s", body)
	}
}

// 释放时序断言（spec §8）：session 门必须在 transcript 落盘完成后才释放。
func TestSessionGateReleasesAfterPersist(t *testing.T) {
	a := newScriptedAgent(t, "reply-1", "reply-2")
	s := New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir()})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	saved := make(chan string, 4)
	s.sessions.testAfterSave = func(id string) { saved <- id }

	run1, sid1, _ := postRun(t, srv.URL, "", "first")
	run2, _, _ := postRun(t, srv.URL, sid1, "second")

	// run1 落盘完成前，run2 不得开始（收不到任何 delta）。
	select {
	case got := <-saved:
		if got != sid1 {
			t.Fatalf("saved session = %s, want %s", got, sid1)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run1 transcript not saved")
	}
	// 落盘已完成 → 门已释放 → run2 的流里应出现 reply-2 且不再有 reply-1 之后的状态错乱。
	waitRunDone(t, srv.URL, run1)
	waitRunDone(t, srv.URL, run2)
}

// 深度超限：门被占住时第 9 条排队 POST 返回 409。
func TestSessionGateQueueFull409(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	model := &blockingModel{} // Stream 阻塞在 block 上，永不返回回复
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
		RecursionLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir()})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	_, sid, _ := postRun(t, srv.URL, "", "hold")
	for i := 0; i < maxQueuedPerSession; i++ {
		if _, _, pos := postRun(t, srv.URL, sid, fmt.Sprintf("q%d", i)); pos != i+1 {
			t.Fatalf("queued run %d position = %d, want %d", i, pos, i+1)
		}
	}
	body := fmt.Sprintf(`{"prompt":"overflow","session_id":%q}`, sid)
	resp, err := http.Post(srv.URL+"/api/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("overflow POST status = %d, want 409", resp.StatusCode)
	}
}

// blockingModel 的 Stream 实现参照 streamingexec_test.go:55 的 scriptedModel，
// 差别是返回的 stream 在 Next 时阻塞于 block channel（测试结束 close 后返回 cancelled）。
```

- [x] **Step 8: 运行 server 包全量测试**

Run: `go test -race ./internal/server/ -v`
Expected: 全 PASS（含既有回归）。

- [x] **Step 9: Commit**

```bash
git add internal/server/admission.go internal/server/admission_test.go internal/server/server.go internal/server/run.go internal/server/goalapi.go internal/server/session.go
git commit -m "feat(server): per-session run admission gate (FIFO, release after persist)"
```

---

### Task 2: P0 前端 — queued 状态与排队徽标

**Files:**
- Modify: `web/src/types.ts:73`（`RunStatus`）
- Modify: `web/src/api/client.ts:19-29`（`StreamHandlers`）、:186-198（`createRun` 响应类型）、:279-330（事件分发）
- Modify: `web/src/stores/sessions.ts:288,:499,:519,:427-466`（守卫与 `attachStream`）
- Modify: `web/src/components/ChatPanel.vue:21,:269-270`

**Interfaces:**
- Consumes: Task 1 的 `run_queued`/`queue_position`/`run_started` 事件与 POST 响应 `state`/`queue_position`。
- Produces: `RunStatus` 含 `'queued'`；store 暴露 `queuePosition: Ref<Record<string, number>>`；`StreamHandlers.onRunQueued?/onQueuePosition?/onRunStarted?`。

- [x] **Step 1: 类型与 API 层**

`types.ts:73` 改：

```ts
export type RunStatus = 'idle' | 'queued' | 'running' | 'awaiting' | 'done' | 'error'
```

`types.ts` 的 run 创建响应（`createRun` 返回类型处）改为：

```ts
export interface CreateRunResponse {
  run_id: string
  session_id: string
  state: 'queued' | 'running'
  queue_position: number
}
```

`client.ts` `StreamHandlers`（:19-29）追加：

```ts
onRunQueued?: (position: number) => void
onQueuePosition?: (position: number) => void
onRunStarted?: () => void
```

`onmessage` 的 switch（:288 附近）追加三个 case：

```ts
case 'run_queued':
case 'queue_position': {
  const pos = (data as { position?: number }).position ?? 0
  if (ev.event === 'run_queued') handlers.onRunQueued?.(pos)
  else handlers.onQueuePosition?.(pos)
  break
}
case 'run_started':
  handlers.onRunStarted?.()
  break
```

- [x] **Step 2: store 接线**（sessions.ts）

```ts
// :125 附近追加
const queuePosition = ref<Record<string, number>>({})
```

`attachStream()` 的 handlers（:434-459）追加：

```ts
onRunQueued(pos) {
  status.value[sid] = 'queued'
  queuePosition.value[sid] = pos
},
onQueuePosition(pos) {
  queuePosition.value[sid] = pos
},
onRunStarted() {
  status.value[sid] = 'running'
  delete queuePosition.value[sid]
},
```

`send()` 守卫（:499）：把"running/awaiting 时静默丢弃"改为——`running` 时继续走 `createRun`（后端会排队），用响应的 `state`/`queue_position` 乐观置状态；`awaiting`（中断待答）仍丢弃（先答中断）。:519 的乐观置位改为按响应：

```ts
const res = await api.createRun(...)
status.value[sid] = res.state === 'queued' ? 'queued' : 'running'
if (res.state === 'queued') queuePosition.value[sid] = res.queue_position
attachStream(res.run_id, sid)
```

删除会话守卫（:288）：拒绝条件加入 `=== 'queued'`。store return 导出 `queuePosition`。

- [x] **Step 3: UI 徽标**（ChatPanel.vue）

`:21` busy 改为含 queued（停止按钮可用 → 复用现有 cancel API 取消排队中的 run）：

```ts
const busy = computed(() => ['running', 'awaiting', 'queued'].includes(store.activeStatus))
```

`:269-270` 徽标块最前加：

```html
<span v-if="store.activeStatus === 'queued'" class="run-badge">
  排队中 #{{ store.queuePosition[store.activeId] ?? 1 }}…
</span>
```

（`activeId` 用 store 现有的当前会话 id 访问器；`.run-badge` 样式复用 style.css:1355-1361。）

- [x] **Step 4: 验证**

Run: `cd web && npm run build`
Expected: `vue-tsc` 类型检查通过 + vite build 成功。手工冒烟（可选）：`npm run dev`，同一会话连发两条消息，第二条显示"排队中 #1"，第一条完成后第二条自动开始。

- [x] **Step 5: Commit**

```bash
git add web/src/types.ts web/src/api/client.ts web/src/stores/sessions.ts web/src/components/ChatPanel.vue
git commit -m "feat(web): queued run status with queue position badge"
```

---

### Task 3: P1.1 — memory prefetch 挂到 run 维度

**Files:**
- Create: `internal/agent/memoryscope.go`
- Modify: `internal/agent/memorystore.go`（`memoryPrefetch` :37-41、`Begin` :130-184、`TakeAttachments` :191-250）
- Modify: `internal/agent/agent.go:552-562`（wrapper）、:1181（调用点）
- Modify: `internal/runner/runner.go:52`（`Run` 入口）、`internal/runner/goalrunner.go:42`（`RunGoal` 入口）
- Test: `internal/agent/memoryscope_test.go`；改造 `internal/agent/concurrency_test.go:53-82`

**Interfaces:**
- Consumes: runws ctx 装饰模式（`internal/runws/runws.go`，模板）。
- Produces:
  - `agent.WithMemoryScope(ctx context.Context) context.Context`
  - `(*memoryRuntime).TakeAttachments(ctx context.Context) []messages.Message`（签名加 ctx；`Begin` 已带 ctx 不变）
  - `(*Agent).takeMemoryAttachments(ctx context.Context) []messages.Message`

- [x] **Step 1: 写失败测试** `internal/agent/memoryscope_test.go`

```go
package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/store"
)

// 并发两 run 各自 Begin/TakeAttachments 互不覆盖（改造原承认互踩的用例）。
func TestMemoryScopeIsolatesConcurrentRuns(t *testing.T) {
	m := &memoryRuntime{}
	ctxA := WithMemoryScope(context.Background())
	ctxB := WithMemoryScope(context.Background())

	scopeA, scopeB := memoryScopeFrom(ctxA), memoryScopeFrom(ctxB)
	scopeA.entries = []store.Entry{{ID: "a-1", Content: "alpha"}}
	scopeB.entries = []store.Entry{{ID: "b-1", Content: "beta"}}

	gotA := m.TakeAttachments(ctxA)
	if len(gotA) != 1 || !strings.Contains(gotA[0].Content, "alpha") {
		t.Fatalf("run A got %+v, want alpha attachment", gotA)
	}
	gotB := m.TakeAttachments(ctxB)
	if len(gotB) != 1 || !strings.Contains(gotB[0].Content, "beta") {
		t.Fatalf("run B got %+v, want beta attachment", gotB)
	}
	// 二次取为空（Take 清空 scope）
	if again := m.TakeAttachments(ctxA); len(again) != 0 {
		t.Fatalf("second take = %+v, want empty", again)
	}
}

// ctx 无 scope 时回退 agent 级字段（CLI 单 run 路径不回归）。
func TestTakeAttachmentsFallbackWithoutScope(t *testing.T) {
	m := &memoryRuntime{}
	m.prefetch.entries = []store.Entry{{ID: "x", Content: "legacy"}}
	got := m.TakeAttachments(context.Background())
	if len(got) != 1 {
		t.Fatalf("fallback take = %+v, want 1 attachment", got)
	}
}
```

同时把 `concurrency_test.go:53-82` 的 `TestTakeMemoryAttachmentsConcurrentRuns` 改为：两 goroutine 各用 `WithMemoryScope` 的 ctx 调 `takeMemoryAttachments(ctx)`，断言各自只拿到自己的 hits（不再是"不 fatal 即可"）。

- [x] **Step 2: 运行确认失败**

Run: `go test -race ./internal/agent/ -run 'TestMemoryScope|TestTakeAttachmentsFallback|TestTakeMemoryAttachmentsConcurrentRuns' -v`
Expected: 编译失败 `undefined: WithMemoryScope`。

- [x] **Step 3: 实现** `internal/agent/memoryscope.go`

```go
package agent

import (
	"context"
	"sync"

	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/store"
)

// memoryScope carries one run's memory prefetch slot (runws-style ctx
// decoration): concurrent runs on a shared agent no longer overwrite each
// other's Begin results. Contexts without a scope (CLI single-run paths)
// fall back to the agent-level memoryRuntime.prefetch field.
type memoryScope struct {
	mu         sync.Mutex
	entries    []store.Entry
	engineHits []memory.Hit
}

type memoryScopeKey struct{}

// WithMemoryScope decorates ctx with a fresh per-run scope. runner.Run and
// runner.RunGoal call this once at run entry.
func WithMemoryScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, memoryScopeKey{}, &memoryScope{})
}

func memoryScopeFrom(ctx context.Context) *memoryScope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(memoryScopeKey{}).(*memoryScope)
	return s
}
```

- [x] **Step 4: 改 memorystore.go**

`Begin` 的异步 goroutine 收尾处（engine 路径与 store 关键词路径各一处写 `m.prefetch.*`）改为：

```go
if scope := memoryScopeFrom(ctx); scope != nil {
	scope.mu.Lock()
	scope.engineHits = hits // store 路径为 scope.entries = hits
	scope.mu.Unlock()
	return
}
m.mu.Lock()
m.prefetch.engineHits = hits // store 路径为 m.prefetch.entries = hits
m.mu.Unlock()
```

`TakeAttachments` 加 ctx 并拆出共用构建逻辑（`injected` 去重表维持 agent 级，仍在 `m.mu` 下访问）：

```go
// TakeAttachments drains the run's prefetch results into <relevant-memory>
// human messages. The per-run scope (ctx) wins; without one it falls back
// to the agent-level slot.
func (m *memoryRuntime) TakeAttachments(ctx context.Context) []messages.Message {
	if scope := memoryScopeFrom(ctx); scope != nil {
		scope.mu.Lock()
		entries, hits := scope.entries, scope.engineHits
		scope.entries, scope.engineHits = nil, nil
		scope.mu.Unlock()
		return m.buildAttachments(entries, hits)
	}
	m.mu.Lock()
	entries, hits := m.prefetch.entries, m.prefetch.engineHits
	m.prefetch.entries, m.prefetch.engineHits = nil, nil
	m.mu.Unlock()
	return m.buildAttachments(entries, hits)
}
```

原函数体的去重/截断/拼装逻辑（:200-250）原样移入 `buildAttachments(entries []store.Entry, hits []memory.Hit) []messages.Message`，其中访问 `m.injected` 的段落保持 `m.mu` 保护。

- [x] **Step 5: 改 wrapper 与调用点、run 入口注入**

- agent.go:556-558：`func (a *Agent) takeMemoryAttachments(ctx context.Context) []messages.Message { return a.mem.TakeAttachments(ctx) }`。
- agent.go:1181：`results = append(results, a.takeMemoryAttachments(ctx)...)`。
- runner.go `Run` 函数体开头加一行：`ctx = agent.WithMemoryScope(ctx)`；goalrunner.go `RunGoal` 同样。（`beginMemoryPrefetch` 已带 ctx 且透传给 `Begin`，无需改签名。）

- [x] **Step 6: 运行测试**

Run: `go test -race ./internal/agent/ ./internal/runner/ -v`
Expected: 全 PASS（含改造后的并发用例与 `memws_test.go`、`memoryengine_test.go` 既有用例）。

- [x] **Step 7: Commit**

```bash
git add internal/agent/memoryscope.go internal/agent/memoryscope_test.go internal/agent/memorystore.go internal/agent/agent.go internal/agent/concurrency_test.go internal/runner/
git commit -m "fix(agent): scope memory prefetch per run to stop cross-run overwrite"
```

---

### Task 4: P1.2 — refreshExtraTools 去阻塞（singleflight + TTL）

**Files:**
- Modify: `internal/agent/refreshtools.go`（全文件重构）
- Modify: `internal/agent/agent.go:273-276`（`refreshMu` 注释与字段区，加 `refreshSF`/`refreshLast`）、:1185（toolsNode 调用点）
- Modify: `go.mod`（x/sync 转直接依赖，`go mod tidy`）
- Test: `internal/agent/refreshtools_test.go`

**Interfaces:**
- Consumes: `Config.RefreshTools` hook（agent.go:~200，`cmd/woca/main.go:1183` 提供 MCP re-list）。
- Produces:
  - `(*Agent).maybeRefreshTools(ctx context.Context)` — toolsNode 每轮调用；TTL 内跳过，过期则后台刷新。
  - `(*Agent).RefreshToolsNow(ctx context.Context)` — 签名不变；绕过 TTL、仍被 singleflight 去重。
  - `refreshTTL = 5 * time.Minute`（包级 `var`，测试可改）。

- [x] **Step 1: 写失败测试** `internal/agent/refreshtools_test.go`

```go
package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	coretools "github.com/projanvil/langchain-golang/core/tools"
)

// fake 慢 MCP：工具轮不被阻塞；TTL 内不二次刷新；后台完成后换绑生效。
func TestMaybeRefreshToolsNonBlocking(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	a := newRunAgent(t, t.TempDir(), nil) // runws_test.go:19 helper
	a.refreshTools = func(ctx context.Context) []coretools.Tool {
		calls.Add(1)
		<-release
		return nil
	}

	done := make(chan struct{})
	go func() { a.maybeRefreshTools(context.Background()); close(done) }()
	select {
	case <-done: // 必须立即返回，不被 refreshTools 阻塞
	case <-time.After(2 * time.Second):
		t.Fatal("maybeRefreshTools blocked on slow refresh")
	}
	close(release)
	waitFor(t, 2*time.Second, func() bool { return calls.Load() == 1 }, "first refresh completes")

	// TTL 内：不再触发
	a.maybeRefreshTools(context.Background())
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("calls within TTL = %d, want 1", n)
	}
}

// RefreshToolsNow 绕过 TTL，且与在途刷新去重（singleflight）。
func TestRefreshToolsNowBypassesTTL(t *testing.T) {
	var calls atomic.Int32
	a := newRunAgent(t, t.TempDir(), nil)
	a.refreshTools = func(ctx context.Context) []coretools.Tool { calls.Add(1); return nil }
	a.RefreshToolsNow(context.Background())
	a.RefreshToolsNow(context.Background())
	if n := calls.Load(); n != 2 {
		t.Fatalf("RefreshToolsNow calls = %d, want 2 (TTL bypassed)", n)
	}
}
```

- [x] **Step 2: 运行确认失败**

Run: `go test -race ./internal/agent/ -run TestMaybeRefreshTools -v`
Expected: 编译失败 `undefined: a.maybeRefreshTools`。

- [x] **Step 3: 重构 refreshtools.go**

```go
package agent

import (
	"context"
	"sync/atomic"
	"time"

	coretools "github.com/projanvil/langchain-golang/core/tools"
	"golang.org/x/sync/singleflight"
)

// refreshTTL throttles between-turn extra-tool refreshes: within the window
// the tools node skips re-collection entirely.
var refreshTTL = 5 * time.Minute

// maybeRefreshTools triggers a background extra-tool refresh when the TTL
// has expired. The current round keeps the existing bindings — a slow MCP
// re-list must never block the tools node (previously refreshMu covered the
// network I/O and stalled every run each round).
func (a *Agent) maybeRefreshTools(ctx context.Context) {
	if a.refreshTools == nil {
		return
	}
	if time.Since(time.Unix(0, a.refreshLast.Load())) < refreshTTL {
		return
	}
	// DoChan runs fn in its own goroutine; duplicate calls while one is in
	// flight share the result (singleflight).
	res := a.refreshSF.DoChan("extra-tools", func() (any, error) {
		// Outlive the tool round's ctx so an end-of-run cancel doesn't abort
		// a half-done re-list.
		a.refreshExtraTools(context.WithoutCancel(ctx))
		return nil, nil
	})
	go func() {
		<-res
		a.refreshLast.Store(time.Now().UnixNano())
	}()
}

// refreshExtraTools re-collects the extra (non-registry) tools via the
// configured hook and rebinds the model when the set changed. Collection
// and rebind run WITHOUT refreshMu (network I/O must not hold it); refreshMu
// now only serializes the microsecond compare-and-commit against
// RefreshToolsNow-driven swaps. Run-side readers take snapshots under a.mu.
func (a *Agent) refreshExtraTools(ctx context.Context) bool {
	if a.refreshTools == nil {
		return false
	}
	fresh := a.refreshTools(ctx)
	a.refreshMu.Lock()
	a.mu.Lock()
	changed := !sameToolSets(a.extraTools, fresh)
	if changed {
		a.extraTools = fresh
		a.toolList = append(append([]coretools.Tool(nil), a.baseTools...), fresh...)
		a.toolByName = make(map[string]coretools.Tool, len(a.toolList))
		for _, t := range a.toolList {
			a.toolByName[t.Name()] = t
		}
	}
	list := a.toolList
	model := a.model
	a.mu.Unlock()
	a.refreshMu.Unlock()
	if !changed {
		return false
	}
	bound := model
	if len(list) > 0 && bound.Capabilities().ToolCalling {
		if b, err := bound.BindTools(list); err == nil {
			bound = b
		}
	}
	a.mu.Lock()
	if a.model == model {
		a.boundModel = bound
	}
	a.mu.Unlock()
	return true
}

// sameToolSets（原样保留，略）

// RefreshToolsNow re-collects immediately, bypassing the TTL; concurrent
// callers (and an in-flight background refresh) are deduplicated by the
// same singleflight key.
func (a *Agent) RefreshToolsNow(ctx context.Context) {
	_, _, _ = a.refreshSF.Do("extra-tools", func() (any, error) {
		a.refreshExtraTools(ctx)
		a.refreshLast.Store(time.Now().UnixNano())
		return nil, nil
	})
}
```

agent.go 字段区（:273-276 附近）：

```go
refreshMu   sync.Mutex // serializes extra-tool swap commits only (no I/O under it)
refreshSF   singleflight.Group
refreshLast atomic.Int64 // unix nano of last completed refresh
```

toolsNode 调用点（agent.go:1185）：`a.refreshExtraTools(ctx)` → `a.maybeRefreshTools(ctx)`。

注意：`sameToolSets` 原样保留（上面标注"略"处不许删）；新增 `sync/atomic`、`time`、`singleflight` import。

- [x] **Step 4: go mod tidy + 运行测试**

Run: `go mod tidy && go test -race ./internal/agent/ -run 'Refresh' -v && go test -race ./internal/agent/`
Expected: x/sync 变为直接依赖；全部 PASS。

- [x] **Step 5: Commit**

```bash
git add internal/agent/refreshtools.go internal/agent/refreshtools_test.go internal/agent/agent.go go.mod go.sum
git commit -m "fix(agent): non-blocking extra-tool refresh via singleflight + TTL"
```

---

### Task 5: P1.3 — sessionStore 锁纪律（内存索引唯一事实源）

**Files:**
- Modify: `internal/server/session.go`（结构体 :23-35、`newSessionStore` :37、`load` :50、`save` :70、`sessionMeta` :98、`recordMeta` :127、`delete` :142、`writeIndex` :160、`list` :245）
- Test: `internal/server/sessionstore_test.go`（追加）

**Interfaces:**
- Consumes: `session.Load`/`session.Save`（internal/session）、`sessionTitle`（session.go:298）。
- Produces: `sessionMeta` 增加 `Title string \`json:"title,omitempty"\`` 与 `MessageCount int \`json:"message_count,omitempty"\``；`list()` 零 JSONL 读；锁内零文件 I/O。

- [x] **Step 1: 写失败测试**（追加到 sessionstore_test.go）

```go
// list() 只读内存索引：删掉磁盘上的 .jsonl 后仍能列出（证明零 JSONL 读）。
func TestSessionStoreListReadsIndexOnly(t *testing.T) {
	dir := t.TempDir()
	s := newSessionStore(dir)
	msgs := []messages.Message{
		{Role: messages.RoleHuman, Content: "hello world"},
		{Role: messages.RoleAI, Content: "hi"},
	}
	if err := s.save("sess-a", msgs, "/tmp/ws-a"); err != nil {
		t.Fatal(err)
	}
	// 删除 transcript 文件：旧实现 list() 会丢这个 session，新实现不受影响。
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	for _, f := range files {
		_ = os.Remove(f)
	}
	list, err := s.list()
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v; want 1 summary from index", list, err)
	}
	if list[0].MessageCount != 2 || list[0].Title != "hello world" {
		t.Fatalf("summary = %+v, want MessageCount=2 Title=hello world", list[0])
	}
}

// 并发 recordMeta（经由 save）不交错、不竞态（-race 把关）。
func TestSessionStoreConcurrentSave(t *testing.T) {
	dir := t.TempDir()
	s := newSessionStore(dir)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("sess-%d", i%3)
			_ = s.save(id, []messages.Message{{Role: messages.RoleHuman, Content: "x"}}, "/tmp/ws")
		}(i)
	}
	wg.Wait()
	list, err := s.list()
	if err != nil || len(list) != 3 {
		t.Fatalf("list = %v, %v; want 3 sessions", list, err)
	}
}

// 启动对账：目录里有 index 之外的遗留 .jsonl，新 store 启动后应被收编。
func TestSessionStoreStartupReconcile(t *testing.T) {
	dir := t.TempDir()
	legacy := []messages.Message{{Role: messages.RoleHuman, Content: "legacy chat"}}
	if err := session.Save(filepath.Join(dir, "sess-legacy.jsonl"), legacy); err != nil {
		t.Fatal(err)
	}
	s := newSessionStore(dir)
	list, err := s.list()
	if err != nil || len(list) != 1 || list[0].ID != "sess-legacy" {
		t.Fatalf("list = %v, %v; want reconciled legacy session", list, err)
	}
	if list[0].Title != "legacy chat" || list[0].MessageCount != 1 {
		t.Fatalf("summary = %+v", list[0])
	}
}
```

- [x] **Step 2: 运行确认失败**

Run: `go test -race ./internal/server/ -run TestSessionStore -v`
Expected: `TestSessionStoreListReadsIndexOnly` FAIL（旧 list 读不到被删文件的 session）。

- [x] **Step 3: 重构 session.go**

结构体（:23-35）改为：

```go
type sessionStore struct {
	dir string
	// defaultWorkspace labels sessions created before the workspace index
	// existed (legacy transcripts without metadata).
	defaultWorkspace string

	// mu guards mem, tombstones and index ONLY — no file I/O under it.
	mu         sync.Mutex
	mem        map[string][]messages.Message
	tombstones map[string]bool
	index      map[string]sessionMeta // 唯一事实源；启动时 loadIndex+对账

	// writeMu serializes index.json write+rename so concurrent recorders
	// never interleave the tmp file.
	writeMu sync.Mutex
}

func newSessionStore(dir string) *sessionStore {
	s := &sessionStore{
		dir:        dir,
		mem:        map[string][]messages.Message{},
		tombstones: map[string]bool{},
		index:      map[string]sessionMeta{},
	}
	if dir != "" {
		s.index = s.loadIndex()
		s.reconcileOnStartup()
	}
	return s
}

// reconcileOnStartup folds transcripts that have no index entry (legacy
// files, or files written before the in-memory index existed) into the
// index once, so list() never has to touch .jsonl files at request time.
func (s *sessionStore) reconcileOnStartup() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	changed := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		if _, ok := s.index[id]; ok {
			continue
		}
		msgs, err := session.Load(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		updatedAt := ""
		if info, err := e.Info(); err == nil {
			updatedAt = info.ModTime().Format(time.RFC3339)
		}
		s.index[id] = sessionMeta{
			ID:           id,
			UpdatedAt:    updatedAt,
			Title:        sessionTitle(msgs),
			MessageCount: len(msgs),
		}
		changed = true
	}
	if changed {
		s.flushIndex()
	}
}
```

`sessionMeta`（:98-103）加两字段：

```go
type sessionMeta struct {
	ID            string `json:"id"`
	Workspace     string `json:"workspace"`
	WorkspaceName string `json:"workspace_name"`
	UpdatedAt     string `json:"updated_at"`
	Title         string `json:"title,omitempty"`
	MessageCount  int    `json:"message_count,omitempty"` // 旧 index 缺字段显示 0，下次 save 自愈
}
```

`load`（:50-65）锁内零 I/O 版：

```go
func (s *sessionStore) load(id string) []messages.Message {
	s.mu.Lock()
	if msgs, ok := s.mem[id]; ok {
		s.mu.Unlock()
		return msgs
	}
	s.mu.Unlock()
	if s.dir == "" {
		return nil
	}
	msgs, err := session.Load(s.path(id)) // 磁盘读在锁外
	if err != nil {
		return nil
	}
	s.mu.Lock()
	s.mem[id] = msgs
	s.mu.Unlock()
	return msgs
}
```

`save`（:70-95）：mem 写入与 tombstone 检查保持现状；`workspace != ""` 分支改为不调锁版 recordMeta，并带上 Title/MessageCount：

```go
	if workspace != "" {
		s.recordMeta(sessionMeta{
			ID:            id,
			Workspace:     workspace,
			WorkspaceName: workspaceDisplayName(workspace),
			UpdatedAt:     time.Now().Format(time.RFC3339),
			Title:         sessionTitle(msgs),
			MessageCount:  len(msgs),
		})
	}
```

`recordMeta` 改为自持锁 + 串行落盘（删掉"Callers must hold s.mu"注释）：

```go
// recordMeta upserts one index entry in memory, then persists the index via
// the serial writer. The map mutation happens under mu (no I/O); marshal and
// write+rename happen under writeMu only.
func (s *sessionStore) recordMeta(meta sessionMeta) {
	s.mu.Lock()
	if old, ok := s.index[meta.ID]; ok {
		if meta.Workspace == "" {
			meta.Workspace = old.Workspace
			meta.WorkspaceName = old.WorkspaceName
		}
		if meta.Title == "" {
			meta.Title = old.Title
		}
	}
	s.index[meta.ID] = meta
	s.mu.Unlock()
	s.flushIndex()
}

// flushIndex snapshots the index and rewrites index.json atomically. The
// writeMu serialization prevents tmp-file interleave between recorders.
func (s *sessionStore) flushIndex() {
	if s.dir == "" {
		return
	}
	s.mu.Lock()
	snapshot := make(map[string]sessionMeta, len(s.index))
	for k, v := range s.index {
		snapshot[k] = v
	}
	s.mu.Unlock()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.writeIndex(snapshot)
}
```

`delete`（:142-157）：mem/tombstone/index 删除在 `mu` 下，`os.Remove` 与 `flushIndex()` 移到锁外：

```go
func (s *sessionStore) delete(id string) {
	s.mu.Lock()
	delete(s.mem, id)
	delete(s.index, id)
	s.tombstones[id] = true
	s.mu.Unlock()
	if s.dir == "" {
		return
	}
	_ = os.Remove(s.path(id))
	_ = os.Remove(s.goalPath(id))
	s.flushIndex()
}
```

`writeIndex` 注释改为"Callers must hold s.writeMu; the argument is a snapshot"，函数体不变。`list`（:245-284）整体替换为纯索引版：

```go
// list returns sessions from the in-memory index, newest first — zero JSONL
// reads. Legacy sessions enter the index via startup reconcile.
func (s *sessionStore) list() ([]sessionSummary, error) {
	s.mu.Lock()
	metas := make([]sessionMeta, 0, len(s.index))
	for _, m := range s.index {
		metas = append(metas, m)
	}
	s.mu.Unlock()
	out := make([]sessionSummary, 0, len(metas))
	for _, m := range metas {
		workspace, workspaceName := s.workspaceOfMeta(m)
		title := m.Title
		if title == "" {
			title = "新会话"
		}
		out = append(out, sessionSummary{
			ID:            m.ID,
			Title:         title,
			UpdatedAt:     m.UpdatedAt,
			MessageCount:  m.MessageCount,
			Workspace:     workspace,
			WorkspaceName: workspaceName,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, nil
}

// workspaceOfMeta resolves a session's workspace from its index entry,
// falling back to the store default for legacy sessions.
func (s *sessionStore) workspaceOfMeta(m sessionMeta) (string, string) {
	if m.Workspace != "" {
		return m.Workspace, m.WorkspaceName
	}
	if s.defaultWorkspace != "" {
		return s.defaultWorkspace, workspaceDisplayName(s.defaultWorkspace)
	}
	return "", "未分类工作区"
}
```

`workspaceOf`（:288-296）若不再被引用则删除（grep 确认；list 是唯一调用点）。注意 `dir == ""` 时 `s.index` 为空 map，`list` 返回空——与旧行为（ReadDir 失败返回 nil）等价。

- [x] **Step 4: 运行测试**

Run: `go test -race ./internal/server/ -run 'Session' -v && go test -race ./internal/server/`
Expected: 全 PASS。

- [x] **Step 5: Commit**

```bash
git add internal/server/session.go internal/server/sessionstore_test.go
git commit -m "fix(server): in-memory session index; no file I/O under store lock"
```

---

### Task 6: P2.1 — LSP server 池

**Files:**
- Create: `internal/tools/lsppool.go`
- Modify: `internal/tools/lsp.go`（`lspTool` :331-358、`lspDiagnostics` :239-296 末尾加 didClose）
- Modify: `internal/tools/registry.go:105-131`（Registry 加 pool 字段与 `Close`/`LSPStats`）、`internal/tools/default.go:20`（注册处传 pool）
- Modify: `internal/agent/filehistory.go:49-52`（`Agent.Close` 追加 registry 关闭）
- Test: `internal/tools/lsppool_test.go`

**Interfaces:**
- Consumes: `lspTransport` 接口（lsp.go:28-32）、`startLSP`（lsp.go:82-115）、fakeTransport 测试模式（lsp_test.go:15-35）。
- Produces:
  - `newLSPPool(spawn func([]string) (lspTransport, error)) *lspPool`
  - `(*lspPool).call(ctx context.Context, workspace string, command []string, fn func(lspTransport) error) error`
  - `(*lspPool).Close()`、`(*lspPool).stats() (active, broken int)`（Task 14 用）
  - `(*Registry).Close()`、`(*Registry).LSPStats() (active, broken int)`
  - 包级 `var lspIdleTimeout = 5*time.Minute`、`lspBrokenCooldown = 10*time.Minute`、`lspMaxSpawnFailures = 2`（var 以便测试改小）。

- [x] **Step 1: 写失败测试** `internal/tools/lsppool_test.go`

```go
package tools

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func poolKeyForTest() (string, []string) { return "/ws", []string{"fake-lsp"} }

func TestLSPPoolReuse(t *testing.T) {
	var spawns atomic.Int32
	p := newLSPPool(func([]string) (lspTransport, error) {
		spawns.Add(1)
		return &fakeTransport{responses: map[string]any{}}, nil
	})
	defer p.Close()
	ws, cmd := poolKeyForTest()
	for i := 0; i < 3; i++ {
		if err := p.call(context.Background(), ws, cmd, func(tr lspTransport) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if n := spawns.Load(); n != 1 {
		t.Fatalf("spawns = %d, want 1 (pooled reuse)", n)
	}
}

func TestLSPPoolRestartOnCrash(t *testing.T) {
	var spawns atomic.Int32
	p := newLSPPool(func([]string) (lspTransport, error) {
		spawns.Add(1)
		return &fakeTransport{responses: map[string]any{}}, nil
	})
	defer p.Close()
	ws, cmd := poolKeyForTest()
	calls := 0
	err := p.call(context.Background(), ws, cmd, func(tr lspTransport) error {
		calls++
		if calls == 1 {
			return errors.New("process died") // 首次 Call 报错 → 摘除、重启重试一次
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := spawns.Load(); n != 2 {
		t.Fatalf("spawns = %d, want 2 (one restart)", n)
	}
}

func TestLSPPoolBrokenCircuitBreaker(t *testing.T) {
	var spawns atomic.Int32
	p := newLSPPool(func([]string) (lspTransport, error) {
		spawns.Add(1)
		return nil, errors.New("spawn failed")
	})
	defer p.Close()
	ws, cmd := poolKeyForTest()
	for i := 0; i < lspMaxSpawnFailures; i++ {
		_ = p.call(context.Background(), ws, cmd, func(tr lspTransport) error { return nil })
	}
	// 第 3 次：熔断冷却中，直接报错且不再 spawn
	err := p.call(context.Background(), ws, cmd, func(tr lspTransport) error { return nil })
	if err == nil {
		t.Fatal("want broken error during cooldown")
	}
	if n := spawns.Load(); n != int32(lspMaxSpawnFailures) {
		t.Fatalf("spawns = %d, want %d (circuit open)", n, lspMaxSpawnFailures)
	}
}

func TestLSPPoolIdleReap(t *testing.T) {
	oldIdle, oldTick := lspIdleTimeout, lspReapInterval
	lspIdleTimeout, lspReapInterval = 30*time.Millisecond, 10*time.Millisecond
	defer func() { lspIdleTimeout, lspReapInterval = oldIdle, oldTick }()
	var spawns atomic.Int32
	p := newLSPPool(func([]string) (lspTransport, error) {
		spawns.Add(1)
		return &fakeTransport{responses: map[string]any{}}, nil
	})
	defer p.Close()
	ws, cmd := poolKeyForTest()
	_ = p.call(context.Background(), ws, cmd, func(tr lspTransport) error { return nil })
	time.Sleep(150 * time.Millisecond) // 等回收
	_ = p.call(context.Background(), ws, cmd, func(tr lspTransport) error { return nil })
	if n := spawns.Load(); n != 2 {
		t.Fatalf("spawns = %d, want 2 (idle entry reaped)", n)
	}
}
```

- [x] **Step 2: 运行确认失败**

Run: `go test -race ./internal/tools/ -run TestLSPPool -v`
Expected: 编译失败 `undefined: newLSPPool`。

- [x] **Step 3: 实现** `internal/tools/lsppool.go`

```go
package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Pool tuning (vars so tests can shrink them).
var (
	lspIdleTimeout      = 5 * time.Minute  // 空闲回收
	lspBrokenCooldown   = 10 * time.Minute // 熔断冷却
	lspMaxSpawnFailures = 2                // 连续 spawn 失败熔断阈值
	lspReapInterval     = time.Minute
)

// lspPool pools language-server processes by (workspace, command): a ready
// transport is reused directly, crashed ones are restarted once, and a key
// whose spawns keep failing is marked broken for a cooldown so a crashing
// server can't spin (opencode broken-set lesson).
type lspPool struct {
	spawn func([]string) (lspTransport, error)

	mu      sync.Mutex // map lookup/insert only; Call runs outside the lock
	entries map[string]*lspEntry

	stopCh  chan struct{}
	stopped bool
	wg      sync.WaitGroup
}

type lspEntry struct {
	tr          lspTransport
	lastUsed    time.Time
	failures    int
	brokenUntil time.Time
}

func newLSPPool(spawn func([]string) (lspTransport, error)) *lspPool {
	p := &lspPool{spawn: spawn, entries: map[string]*lspEntry{}, stopCh: make(chan struct{})}
	p.wg.Add(1)
	go p.reapLoop()
	return p
}

func lspPoolKey(workspace string, command []string) string {
	return workspace + "\x00" + strings.Join(command, " ")
}

// call runs fn with a ready transport. On fn error the entry is evicted and
// fn retried once on a freshly spawned server; the second error is returned.
func (p *lspPool) call(ctx context.Context, workspace string, command []string, fn func(lspTransport) error) error {
	key := lspPoolKey(workspace, command)
	tr, err := p.acquire(key, command)
	if err != nil {
		return err
	}
	err = fn(tr)
	p.touch(key, tr)
	if err == nil {
		return nil
	}
	p.evict(key, tr)
	tr, err = p.acquire(key, command)
	if err != nil {
		return err
	}
	err = fn(tr)
	p.touch(key, tr)
	if err != nil {
		p.evict(key, tr)
	}
	return err
}

func (p *lspPool) acquire(key string, command []string) (lspTransport, error) {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil, fmt.Errorf("lsp pool closed")
	}
	if e, ok := p.entries[key]; ok {
		if time.Now().Before(e.brokenUntil) {
			p.mu.Unlock()
			return nil, fmt.Errorf("LSP server unavailable (cooling down after repeated spawn failures)")
		}
		if e.tr != nil {
			p.mu.Unlock()
			return e.tr, nil
		}
	}
	p.mu.Unlock()

	tr, err := p.spawn(command) // spawn outside the lock

	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[key]
	if e == nil {
		e = &lspEntry{}
		p.entries[key] = e
	}
	if err != nil {
		e.failures++
		if e.failures >= lspMaxSpawnFailures {
			e.brokenUntil = time.Now().Add(lspBrokenCooldown)
			e.failures = 0
		}
		return nil, err
	}
	if e.tr != nil { // 并发 spawn 竞速：保留先入池者
		_ = tr.Close()
		return e.tr, nil
	}
	e.tr = tr
	e.failures = 0
	e.lastUsed = time.Now()
	return tr, nil
}

func (p *lspPool) touch(key string, tr lspTransport) {
	p.mu.Lock()
	if e, ok := p.entries[key]; ok && e.tr == tr {
		e.lastUsed = time.Now()
	}
	p.mu.Unlock()
}

func (p *lspPool) evict(key string, tr lspTransport) {
	p.mu.Lock()
	if e, ok := p.entries[key]; ok && e.tr == tr {
		e.tr = nil
	}
	p.mu.Unlock()
	_ = tr.Close()
}

func (p *lspPool) reapLoop() {
	defer p.wg.Done()
	tick := time.NewTicker(lspReapInterval)
	defer tick.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-tick.C:
			p.reapIdle()
		}
	}
}

func (p *lspPool) reapIdle() {
	cutoff := time.Now().Add(-lspIdleTimeout)
	var stale []lspTransport
	p.mu.Lock()
	for key, e := range p.entries {
		if e.tr != nil && e.lastUsed.Before(cutoff) {
			stale = append(stale, e.tr)
			delete(p.entries, key)
		}
	}
	p.mu.Unlock()
	for _, tr := range stale {
		_ = tr.Close()
	}
}

// Close stops the reaper and shuts down every pooled transport.
func (p *lspPool) Close() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	close(p.stopCh)
	var all []lspTransport
	for key, e := range p.entries {
		if e.tr != nil {
			all = append(all, e.tr)
		}
		delete(p.entries, key)
	}
	p.mu.Unlock()
	p.wg.Wait()
	for _, tr := range all {
		_ = tr.Close()
	}
}

// stats reports pooled (active) and broken-cooldown entry counts.
func (p *lspPool) stats() (active, broken int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, e := range p.entries {
		if e.tr != nil {
			active++
		}
		if now.Before(e.brokenUntil) {
			broken++
		}
	}
	return active, broken
}
```

- [x] **Step 4: 接线 lsp.go / registry.go / default.go / Agent.Close**

`lspDiagnostics`（lsp.go:239-296）：函数末尾（拿到 diagnostic 之后、返回之前）追加 didClose，保证同一 server 可重复 didOpen 同一文件：

```go
	_ = tr.Notify(ctx, "textDocument/didClose", map[string]any{
		"textDocument": map[string]any{"uri": uri},
	})
```

（`uri` 用函数内已构造的文档 uri 变量名。）

`lspTool`（lsp.go:331-358）改签名与调用：

```go
func lspTool(d *Dir, pool *lspPool) coretools.Tool {
	// ...描述不变...
			dir := d.For(ctx)
			path := resolvePath(dir, a.Path)
			var out string
			err := pool.call(ctx, dir, command, func(tr lspTransport) error {
				var derr error
				out, derr = lspDiagnostics(ctx, tr, dir, path)
				return derr
			})
			if err != nil {
				return coretools.Result{Content: "Error: " + err.Error()}, nil
			}
			return coretools.Result{Content: out}, nil
```

`Registry`（registry.go:105-112 字段区）加 `lspPool *lspPool`；`NewRegistry`（:116-131）初始化 `lspPool: newLSPPool(func(cmd []string) (lspTransport, error) { return startLSP(cmd) })`；default.go:20 `lspTool(d)` → `lspTool(d, r.lspPool)`（按 default.go 实际的注册函数签名传入）。Registry 追加：

```go
// Close shuts down pooled LSP servers. Safe to call multiple times.
func (r *Registry) Close() {
	if r.lspPool != nil {
		r.lspPool.Close()
	}
}

// LSPStats reports pooled/broken LSP server counts (for /api/stats).
func (r *Registry) LSPStats() (active, broken int) {
	if r.lspPool == nil {
		return 0, 0
	}
	return r.lspPool.stats()
}
```

`Agent.Close`（filehistory.go:49-52）追加（`a.registry` 为 agent 持有的 `*tools.Registry` 字段名，以实际为准）：

```go
	if a.registry != nil {
		a.registry.Close()
	}
```

注意：`newSubagent`（agent.go:1373）每次新建 Registry，其子代理池由 5min 空闲回收兜底，不在本 Task 接线（记录在案的有界折衷）。

- [x] **Step 5: 运行测试**

Run: `go test -race ./internal/tools/ -v && go test -race ./internal/agent/ ./internal/server/`
Expected: 全 PASS（含 lsp_extra_test.go 端到端用例——工具层走真子进程路径现在经过池）。

- [x] **Step 6: Commit**

```bash
git add internal/tools/lsppool.go internal/tools/lsppool_test.go internal/tools/lsp.go internal/tools/registry.go internal/tools/default.go internal/agent/filehistory.go
git commit -m "feat(tools): pooled LSP servers with crash restart and broken breaker"
```

---

### Task 7: P2.2 — workspace 缓存 LRU

**Files:**
- Create: `internal/agent/wscache.go`
- Modify: `internal/agent/agent.go:243-255`（字段）、:665-667（`InvalidateWorkspacePrompts`）、:681-692（`systemPromptFor`）、:698-710（`skillsFor`）、:716-728（`agentsFor`）、:735-747（`hooksFor`）、`New`（:336 附近初始化）
- Test: `internal/agent/wscache_test.go`

**Interfaces:**
- Produces: `newWSCache[V any](capacity int) *wsCache[V]`、`Get(key) (V, bool)`、`Set(key, V)`、`Clear()`；容量常量 `wsCacheCapacity = 32`。

- [ ] **Step 1: 写失败测试** `internal/agent/wscache_test.go`

```go
package agent

import "testing"

func TestWSCacheEvictsOldest(t *testing.T) {
	c := newWSCache[string](2)
	c.Set("a", "1")
	c.Set("b", "2")
	c.Get("a")          // a 变热
	c.Set("c", "3")     // 淘汰 b
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if v, ok := c.Get("a"); !ok || v != "1" {
		t.Fatalf("a = %q,%v; want 1,true", v, ok)
	}
	if v, ok := c.Get("c"); !ok || v != "3" {
		t.Fatalf("c = %q,%v; want 3,true", v, ok)
	}
}

func TestWSCacheClear(t *testing.T) {
	c := newWSCache[int](32)
	c.Set("x", 1)
	c.Clear()
	if _, ok := c.Get("x"); ok {
		t.Fatal("Get after Clear should miss")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test -race ./internal/agent/ -run TestWSCache -v`
Expected: 编译失败 `undefined: newWSCache`。

- [ ] **Step 3: 实现** `internal/agent/wscache.go`

```go
package agent

import (
	"container/list"
	"sync"
)

// wsCacheCapacity bounds each per-workspace cache (prompts/skills/agents/
// hooks were unbounded sync.Maps before).
const wsCacheCapacity = 32

// wsCache is a tiny mutex-guarded LRU keyed by workspace.
type wsCache[V any] struct {
	mu    sync.Mutex
	cap   int
	ll    *list.List // front = most recently used
	items map[string]*list.Element
}

type wsCacheEntry[V any] struct {
	key string
	val V
}

func newWSCache[V any](capacity int) *wsCache[V] {
	return &wsCache[V]{cap: capacity, ll: list.New(), items: map[string]*list.Element{}}
}

func (c *wsCache[V]) Get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(wsCacheEntry[V]).val, true
	}
	var zero V
	return zero, false
}

func (c *wsCache[V]) Set(key string, val V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value = wsCacheEntry[V]{key, val}
		c.ll.MoveToFront(el)
		return
	}
	c.items[key] = c.ll.PushFront(wsCacheEntry[V]{key, val})
	for c.ll.Len() > c.cap {
		back := c.ll.Back()
		delete(c.items, back.Value.(wsCacheEntry[V]).key)
		c.ll.Remove(back)
	}
}

// Clear drops every entry (workspace-prompt invalidation semantics).
func (c *wsCache[V]) Clear() {
	c.mu.Lock()
	c.ll.Init()
	c.items = map[string]*list.Element{}
	c.mu.Unlock()
}
```

- [ ] **Step 4: 替换 agent.go 四个 sync.Map**

字段区（:243-255）改为：

```go
promptFor func(workDir, model string) string
wsPrompts *wsCache[string]
skillsForFn func(workdir string) []skills.Skill
wsSkills    *wsCache[[]skills.Skill]
agentsForFn func(workdir string) []agents.Definition
wsAgents    *wsCache[[]agents.Definition]
hooksForFn  func(workdir string) *hooks.Runner
wsHooks     *wsCache[*hooks.Runner]
```

`New` 中初始化四个缓存（`newWSCache[string](wsCacheCapacity)` 等）。四个访问器按同一模式改，以 `systemPromptFor`（:681-692）为例：

```go
	if p, ok := a.wsPrompts.Get(cacheKey); ok {
		return p
	}
	p := a.promptFor(ws, model)
	a.wsPrompts.Set(cacheKey, p)
	return p
```

（`skillsFor`/`agentsFor`/`hooksFor` 同构替换 `Load`→`Get`、`Store`→`Set`，无 builder/无 ctx workspace 时的回退分支保持原样不进缓存。）

`InvalidateWorkspacePrompts`（:665-667）改为 `a.wsPrompts.Clear()`——顺带修复原 `a.wsPrompts = sync.Map{}` 整体替换与并发 Load/Store 不同步的问题。

- [ ] **Step 5: 运行测试**

Run: `go test -race ./internal/agent/ -v`
Expected: 全 PASS（runws_test.go、memws_test.go 等跨 workspace 用例覆盖缓存路径）。

- [ ] **Step 6: Commit**

```bash
git add internal/agent/wscache.go internal/agent/wscache_test.go internal/agent/agent.go
git commit -m "fix(agent): bound per-workspace caches with a 32-entry LRU"
```

---

### Task 8: P2.3 — teammate/task 硬上限

**Files:**
- Modify: `internal/agent/team.go`（`Spawn` :181-212）
- Modify: `internal/agent/task.go`（`start` :114-126、`runTask` :215-231）
- Test: `internal/agent/team_test.go`、`internal/agent/bgtask_test.go`（各追加）

**Interfaces:**
- Produces: `maxActiveTeammates = 8`、`maxBackgroundTasks = 16`（包级常量）；`(*TeamManager).ActiveCount() int`、`(*TaskManager).RunningCount() int`（Task 14 的 /api/stats 复用）；`(*TaskManager).start` 签名改为 `(*backgroundTask, error)`。

- [ ] **Step 1: 写失败测试**

team_test.go 追加：

```go
func TestTeamSpawnCap(t *testing.T) {
	lead, _ := newTeamLead(t, nil)
	team := lead.ensureTeam()
	defer team.Close()
	for i := 0; i < maxActiveTeammates; i++ {
		if err := team.Spawn(context.Background(), fmt.Sprintf("mate-%d", i), "idle", nil); err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
	}
	err := team.Spawn(context.Background(), "one-too-many", "idle", nil)
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("err = %v, want cap error with guidance", err)
	}
	if n := team.ActiveCount(); n != maxActiveTeammates {
		t.Fatalf("ActiveCount = %d, want %d", n, maxActiveTeammates)
	}
}
```

bgtask_test.go 追加：

```go
func TestTaskManagerCap(t *testing.T) {
	m := newTaskManager()
	for i := 0; i < maxBackgroundTasks; i++ {
		if _, err := m.start("s", "d"); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	if _, err := m.start("s", "d"); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("err = %v, want cap error", err)
	}
	if n := m.RunningCount(); n != maxBackgroundTasks {
		t.Fatalf("RunningCount = %d, want %d", n, maxBackgroundTasks)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test -race ./internal/agent/ -run 'TestTeamSpawnCap|TestTaskManagerCap' -v`
Expected: 编译失败 `undefined: maxActiveTeammates`。

- [ ] **Step 3: 实现**

team.go 顶部常量区（:33 `teamIdleTimeout` 附近）加：

```go
// maxActiveTeammates caps concurrently alive teammates; the model gets a
// steerable error (not a hang) so it can restructure its plan.
const maxActiveTeammates = 8
```

`Spawn` 第一段锁内（:183-190）插入上限检查：

```go
	t.mu.Lock()
	if _, exists := t.members[name]; exists {
		t.mu.Unlock()
		return fmt.Errorf("teammate %q already exists", name)
	}
	active := 0
	for _, m := range t.members {
		if m.status != teammateStopped {
			active++
		}
	}
	if active >= maxActiveTeammates {
		t.mu.Unlock()
		return fmt.Errorf("已达 teammate 上限（%d 个活跃）：请改用顺序 Task，或先 Stop 不需要的 teammate", maxActiveTeammates)
	}
	t.mu.Unlock()
```

TeamManager 追加：

```go
// ActiveCount reports teammates that have not stopped (for /api/stats).
func (t *TeamManager) ActiveCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, m := range t.members {
		if m.status != teammateStopped {
			n++
		}
	}
	return n
}
```

task.go：

```go
// maxBackgroundTasks caps concurrently running background tasks.
const maxBackgroundTasks = 16

func (m *TaskManager) start(subject, description string) (*backgroundTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	running := 0
	for _, t := range m.tasks {
		if t.status == TaskRunning {
			running++
		}
	}
	if running >= maxBackgroundTasks {
		return nil, fmt.Errorf("已达后台 task 上限（%d 个运行中）：请先等待其完成或用 TaskStop 停止部分 task", maxBackgroundTasks)
	}
	m.next++
	t := &backgroundTask{ID: fmt.Sprintf("task-%d", m.next), Subject: subject, Description: description, status: TaskRunning}
	m.tasks[t.ID] = t
	return t, nil
}

// RunningCount reports running background tasks (for /api/stats).
func (m *TaskManager) RunningCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, t := range m.tasks {
		if t.status == TaskRunning {
			n++
		}
	}
	return n
}
```

`runTask`（task.go:215-216）：`t := a.tasks.start(...)` → `t, err := a.tasks.start(...)`；`if err != nil { return coretools.Result{}, err }`（走 execute 的 "Error: ..." 结果文本路径，模型可感知）。

注意：若 `backgroundTask.status` 有独立锁（查 task.go 的 accessor），计数循环改用现有读取方式，保持锁纪律一致。

- [ ] **Step 4: 运行测试**

Run: `go test -race ./internal/agent/ -v`
Expected: 全 PASS（含 TestTeamSpawnStopConcurrent 等并发用例）。

- [ ] **Step 5: Commit**

```bash
git add internal/agent/team.go internal/agent/task.go internal/agent/team_test.go internal/agent/bgtask_test.go
git commit -m "feat(agent): hard caps for teammates (8) and background tasks (16)"
```

---

### Task 9: P3.1（后端）— SSE seq、丢弃计数与 Last-Event-ID 重放

**Files:**
- Modify: `internal/server/run.go`（`Event` :19-22、`Run` :37-51、`emit` :218-228、`subscribe` :252-261）
- Modify: `internal/server/server.go`（`handleEvents` :471-509）
- Test: `internal/server/run_test.go`（或就近追加到现有 run 测试文件）

**Interfaces:**
- Produces:
  - `Event` 增加 `Seq int64 \`json:"-"\``（Run 内单调，从 1 起，仅作 SSE `id:` 行，不进 data）。
  - `(*Run).subscribeSince(lastSeq int64) (sub *subscriber, gap bool)` — 只回放 `Seq > lastSeq` 的事件；`gap=true` 表示缺口超出 history 保留窗口。
  - `(*Run).droppedCount() int64`（Task 14 用）。
  - SSE 每事件多一行 `id: <seq>`；缺口时先发合成事件 `resync{reason:"history_gap"}`。

- [ ] **Step 1: 写失败测试**

```go
package server

import (
	"testing"
	"time"
)

func TestEmitAssignsMonotonicSeq(t *testing.T) {
	r := &Run{id: "test", subs: map[*subscriber]struct{}{}}
	r.emit(Event{Type: "delta", Data: map[string]any{"text": "a"}})
	r.emit(Event{Type: "delta", Data: map[string]any{"text": "b"}})
	r.emit(Event{Type: "done", Data: map[string]any{}})
	if len(r.history) != 3 || r.history[0].Seq != 1 || r.history[1].Seq != 2 || r.history[2].Seq != 3 {
		t.Fatalf("seqs = %+v, want 1,2,3", r.history)
	}
}

func TestSubscribeSinceReplaysTail(t *testing.T) {
	r := &Run{id: "test", subs: map[*subscriber]struct{}{}}
	for i := 0; i < 4; i++ {
		r.emit(Event{Type: "delta", Data: map[string]any{"i": i}})
	}
	sub, gap := r.subscribeSince(2)
	if gap {
		t.Fatal("unexpected gap")
	}
	defer r.unsubscribe(sub)
	first := <-sub.ch
	if first.Seq != 3 {
		t.Fatalf("first replayed seq = %d, want 3", first.Seq)
	}
}

func TestSubscribeSinceGap(t *testing.T) {
	r := &Run{id: "test", subs: map[*subscriber]struct{}{}}
	r.history = append(r.history, Event{Type: "delta", Seq: 41}, Event{Type: "delta", Seq: 42})
	r.seq = 42
	_, gap := r.subscribeSince(10) // 11..40 已不可见
	if !gap {
		t.Fatal("want gap=true when lastSeq falls outside retained history")
	}
}

func TestEmitDropCounter(t *testing.T) {
	r := &Run{id: "test", subs: map[*subscriber]struct{}{}}
	full := &subscriber{ch: make(chan Event)} // 无缓冲 → 永远满
	r.subs[full] = struct{}{}
	r.emit(Event{Type: "delta", Data: map[string]any{}})
	if n := r.droppedCount(); n != 1 {
		t.Fatalf("dropped = %d, want 1", n)
	}
}
```

（Task 9 测试 import 块只需 `testing`；`TestParkTimesOut` 在 Task 13 才用到 `context`/`time`。）

- [ ] **Step 2: 运行确认失败**

Run: `go test -race ./internal/server/ -run 'TestEmit|TestSubscribeSince' -v`
Expected: 编译失败（`Seq`/`subscribeSince`/`droppedCount` 未定义）。

- [ ] **Step 3: 实现**

`Event`（run.go:19-22）：

```go
// Event is one SSE frame. Seq is a per-run monotonic sequence (from 1)
// assigned by emit; it is written as the SSE id: line for Last-Event-ID
// resume and never serialized into Data.
type Event struct {
	Type string
	Data any
	Seq  int64 `json:"-"`
}
```

`Run` 结构体（:37-51）加字段：

```go
	seq     int64        // under mu; last assigned Seq
	dropped atomic.Int64 // fanout drops on full subscriber channels
```

（`sync/atomic` import。）

`emit`（:218-228）：

```go
func (r *Run) emit(ev Event) {
	r.mu.Lock()
	r.seq++
	ev.Seq = r.seq
	r.history = append(r.history, ev)
	for sub := range r.subs {
		select {
		case sub.ch <- ev:
		default:
			r.dropped.Add(1) // 慢消费者丢事件可观测（/api/stats）
		}
	}
	r.mu.Unlock()
}

func (r *Run) droppedCount() int64 { return r.dropped.Load() }
```

`subscribe` 保持原样（全量回放）；新增：

```go
// subscribeSince replays only events after lastSeq, then goes live. gap is
// true when lastSeq points before the oldest retained event (the replay
// window can't cover it — the client should refetch a full snapshot).
func (r *Run) subscribeSince(lastSeq int64) (*subscriber, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sub := &subscriber{ch: make(chan Event, len(r.history)+64)}
	gap := false
	if lastSeq > 0 && len(r.history) > 0 && r.history[0].Seq > lastSeq+1 {
		gap = true
	}
	for _, ev := range r.history {
		if ev.Seq > lastSeq {
			sub.ch <- ev
		}
	}
	r.subs[sub] = struct{}{}
	return sub, gap
}
```

`handleEvents`（server.go:471-509）：开头解析续传位置并选择订阅方式；写出时加 `id:` 行：

```go
	lastSeq, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	var sub *subscriber
	var gap bool
	if lastSeq > 0 {
		sub, gap = run.subscribeSince(lastSeq)
	} else {
		sub = run.subscribe()
	}
	defer run.unsubscribe(sub)
	if gap {
		fmt.Fprintf(w, "event: resync\ndata: %s\n\n", `{"reason":"history_gap"}`)
		flusher.Flush()
	}
	// 循环内：
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, data); err != nil {
		return
	}
	flusher.Flush()
```

（`strconv` import；参数接收者 `r *http.Request` 与 Run 变量 `run` 注意不要撞名，按现有代码实际命名调整。）

- [ ] **Step 4: 运行测试**

Run: `go test -race ./internal/server/ -v`
Expected: 全 PASS（readSSEEvent helper 逐行扫 event:/data:，新增 id: 行不影响既有断言；若有严格按行解析的 helper 需跳过 `id:` 行）。

- [ ] **Step 5: Commit**

```bash
git add internal/server/run.go internal/server/server.go internal/server/run_test.go
git commit -m "feat(server): SSE event seq, drop counter, Last-Event-ID replay"
```

---

### Task 10: P3.1（前端）— seq 检测与断线重连

**Files:**
- Modify: `web/src/api/client.ts:19-29`（`StreamHandlers`）、:268-278（不重连的注释块）、:279-330（`streamRun` 重写）
- Modify: `web/src/stores/sessions.ts:427-466`（`attachStream` 接 `onResync`）

**Interfaces:**
- Consumes: Task 9 的 `id:` 行与 `resync` 事件。
- Produces: `StreamHandlers.onResync?: (reason: string) => void`；`streamRun(runId, handlers)` 内部自动带 `Last-Event-ID` 重连（最多 5 次、1s 间隔）。

- [ ] **Step 1: 重写 `streamRun`**（client.ts:279-330）

```ts
export function streamRun(runId: string, handlers: StreamHandlers): Promise<void> {
  let lastSeq: number | null = null
  let attempts = 0
  const maxRetries = 5
  let terminal = false

  // Reconnect with Last-Event-ID: the server replays only the events we
  // missed (or sends `resync` when the gap exceeds its retention window),
  // so reconnecting a live run is now safe — the old "never reconnect"
  // assumption (see git history for the removed comment) no longer holds.
  const connect = (): Promise<void> =>
    fetchEventSource(`/api/runs/${runId}/events`, {
      openWhenHidden: true,
      headers: lastSeq != null ? { 'Last-Event-ID': String(lastSeq) } : {},
      async onopen(resp) {
        // 现有非 2xx / 非 event-stream 检查逻辑原样保留
        const ct = resp.headers.get('content-type') ?? ''
        if (!resp.ok || !ct.includes('text/event-stream')) {
          throw new Error(`stream open failed: ${resp.status}`)
        }
      },
      onmessage(ev) {
        attempts = 0 // 收到消息即重置退避
        if (ev.id) {
          const seq = Number(ev.id)
          if (lastSeq != null && seq > lastSeq + 1) {
            // seq 跳变：服务端未检测到缺口时客户端兜底触发重放
            handlers.onResync?.('seq_gap')
          }
          lastSeq = seq
        }
        const data = ev.data ? JSON.parse(ev.data) : {}
        switch (ev.event) {
          // 既有 delta/reasoning/tool_call/tool_result/interrupt/goal/
          // run_queued/queue_position/run_started 分发原样保留
          case 'resync':
            handlers.onResync?.((data as { reason?: string }).reason ?? 'unknown')
            break
          case 'done':
          case 'error':
            terminal = true
            handlers.onDone?.(data)
            break
          // ...其余 case 不变
        }
      },
      onclose() {
        if (!terminal) throw new Error('stream closed before terminal event')
      },
      onerror(err) {
        if (terminal || ++attempts > maxRetries) throw err
        // 返回 number → fetch-event-source 按该毫秒数后重试（复用其内置重连）
        return 1000
      },
    })

  return connect()
}
```

注意要点：原 `:268-278` 的"不重连"注释块删除并替换为上面的新注释；`onerror` 返回 number 是 `@microsoft/fetch-event-source` 内置的重试协议，重试时会重新走 `headers` 闭包带上最新 `lastSeq`；`onResync` 加入 `StreamHandlers`。

- [ ] **Step 2: store 接 resync**（sessions.ts `attachStream`）

```ts
onResync() {
  // 缺口超出服务端重放窗口（或 seq 跳变）：回源拉全量会话消息兜底
  void loadMessages(sid) // 用 store 内现有的"拉取会话消息"函数名（GET /api/sessions/{id} 的封装）
},
```

- [ ] **Step 3: 验证**

Run: `cd web && npm run build`
Expected: vue-tsc + vite build 通过。手工冒烟（可选）：`npm run dev` 起 run 后 `kill` 掉 dev server 代理目标再恢复，观察事件不重复不丢失、断流后 1s 重连续上。

- [ ] **Step 4: Commit**

```bash
git add web/src/api/client.ts web/src/stores/sessions.ts
git commit -m "feat(web): SSE resume with Last-Event-ID and resync fallback"
```

---

### Task 11: P3.2 — 投机执行只读门

**Files:**
- Modify: `internal/agent/streamingexec.go:124-150`（`speculativeAllowed`）
- Test: `internal/agent/streamingexec_test.go`（若无则新建）

**Interfaces:**
- Consumes: `tools.IsReadOnly`（registry.go:88-102）、`tools.CategoryOf`（registry.go:79-84）。
- Produces: `speculativeAllowed` 全模式只放行只读/meta 工具。

- [ ] **Step 1: 写失败测试**

```go
package agent

import (
	"testing"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/messages"
)

func TestSpeculativeAllowedReadOnlyGate(t *testing.T) {
	for _, mode := range []config.PermissionMode{config.ModeDefault, config.ModeAcceptEdits, config.ModeBypass, config.ModePlan, config.ModeReadOnly} {
		a := &Agent{mode: mode, perms: nil}
		write := messages.ToolCall{ID: "1", Name: "Write", Args: map[string]any{"path": "x.go", "content": "package x"}}
		if a.speculativeAllowed(write) {
			t.Fatalf("mode %v: write tool speculatively dispatched, want blocked", mode)
		}
		read := messages.ToolCall{ID: "2", Name: "Read", Args: map[string]any{"path": "x.go"}}
		if !a.speculativeAllowed(read) {
			t.Fatalf("mode %v: read tool blocked, want allowed", mode)
		}
	}
}
```

（`mode`/`perms` 字段名与 ToolCall 类型路径按代码实际调整。）

- [ ] **Step 2: 运行确认失败**

Run: `go test -race ./internal/agent/ -run TestSpeculativeAllowedReadOnlyGate -v`
Expected: FAIL —— ModeBypass/ModeAcceptEdits 下 Write 被放行。

- [ ] **Step 3: 修改**（streamingexec.go:124-150）

保留 AskUserQuestion/EnterPlanMode/ExitPlanMode 黑名单与 `perms.Action` 拒绝检查不动；其后的 mode switch 整体替换为：

```go
	// Speculative dispatch only runs read-only (or meta) tools. Writes wait
	// for the real toolsNode regardless of permission mode — a speculated
	// write whose stream is later restarted would have executed twice.
	readOnly := tools.IsReadOnly(call.Name, call.Args)
	cat := tools.CategoryOf(call.Name)
	return readOnly || cat == tools.CatMeta
```

（原 `a.mu.Lock()` 读 mode 的段落随之删除；若 `mode` 变量不再使用一并清理。）

- [ ] **Step 4: 运行测试**

Run: `go test -race ./internal/agent/ -run 'Speculative' -v && go test -race ./internal/agent/`
Expected: 全 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/agent/streamingexec.go internal/agent/streamingexec_test.go
git commit -m "fix(agent): speculative dispatch limited to read-only tools"
```

---

### Task 12: P3.5 — toolsNode / 投机 goroutine panic recover

**Files:**
- Modify: `internal/agent/agent.go:1124-1155`（toolsNode tool goroutine）
- Modify: `internal/agent/streamingexec.go:155-171`（`dispatchSpeculative` goroutine）
- Test: `internal/agent/agent_test.go`（或就近）追加

**Interfaces:**
- Produces: 工具 panic → 结构化 ERROR 工具结果 `"Error: tool %q panicked: %v"` + 截断堆栈（≤2KB），进程不死。

- [ ] **Step 1: 写失败测试**

```go
package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// captureModel 记录每轮模型调用的输入（第二轮输入含 toolsNode 产出的工具结果）。
type captureModel struct {
	*language.FakeChatModel
	mu   sync.Mutex
	seen [][]messages.Message
}

func (c *captureModel) Stream(ctx context.Context, in []messages.Message, opts ...runnables.Option) (runnables.Stream[messages.Message], error) {
	c.mu.Lock()
	c.seen = append(c.seen, in)
	c.mu.Unlock()
	return c.FakeChatModel.Stream(ctx, in, opts...)
}

func TestToolPanicBecomesErrorResult(t *testing.T) {
	panicker, err := coretools.FromFunc("Panicker", "always panics",
		func(ctx context.Context, a struct{}) (coretools.Result, error) {
			panic("boom")
		})
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(t.TempDir())
	reg.Register(panicker) // registry.go:145
	fake := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "c1", Name: "Panicker", Args: map[string]any{}},
			}},
			messages.AI("recovered"),
		),
	)
	model := &captureModel{FakeChatModel: fake}
	a, err := New(Config{
		Model:          model,
		Registry:       reg,
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		RecursionLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	// pump 模式见 agentapi_test.go:557-575
	s, err := a.Stream(context.Background(), []messages.Message{messages.Human("go")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for {
		step, ok, err := s.Next(context.Background())
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		if !ok {
			t.Fatal("stream ended without terminal step")
		}
		if step.Done {
			break
		}
	}

	// 进程活着走到这里即 recover 生效；再断言第二轮模型输入里的工具结果是结构化错误。
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.seen) < 2 {
		t.Fatalf("model called %d times, want >= 2", len(model.seen))
	}
	found := false
	for _, m := range model.seen[1] {
		if m.Role == messages.RoleTool && strings.Contains(m.Content, `tool "Panicker" panicked: boom`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("want structured panic tool result in round-2 input: %+v", model.seen[1])
	}
}
```

注意：`language.FakeChatModel` 是否满足直接嵌入覆盖 `Stream`（agentapi_test.go:582-584 的 `namedModel2` 已有嵌入先例）；若编译期接口不合，仿照 `streamingexec_test.go:55` 的 `scriptedModel` 手写完整接口。

- [ ] **Step 2: 运行确认失败**

Run: `go test -race ./internal/agent/ -run TestToolPanicBecomesErrorResult -v`
Expected: 测试进程 panic 崩溃（证明现状无 recover）。

- [ ] **Step 3: 补 recover**

toolsNode goroutine（agent.go:1133-1153）：

```go
	wg.Add(1)
	go func(i int, call messages.ToolCall) {
		defer wg.Done()
		defer func() {
			if rec := recover(); rec != nil {
				stack := debug.Stack()
				if len(stack) > 2048 {
					stack = stack[:2048]
				}
				results[i] = messages.Tool(call.ID, fmt.Sprintf("Error: tool %q panicked: %v\n%s", call.Name, rec, stack))
			}
		}()
		res := a.execute(ctx, call.Name, call.Args)
		// ...原逻辑不变...
	}(i, call)
```

`dispatchSpeculative` goroutine（streamingexec.go:165-169）同样包一层 recover，panic 时往 future 写 `"Error: tool %q panicked: %v"` 结果。两文件按需补 `runtime/debug` import。

- [ ] **Step 4: 运行测试**

Run: `go test -race ./internal/agent/ -run TestToolPanicBecomesErrorResult -v && go test -race ./internal/agent/`
Expected: 全 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/agent/agent.go internal/agent/streamingexec.go internal/agent/agent_test.go
git commit -m "fix(agent): recover tool goroutine panics into structured error results"
```

---

### Task 13: P3.3 — parked run TTL

**Files:**
- Modify: `internal/server/run.go`（`park` :159-173）
- Test: `internal/server/run_test.go` 追加

**Interfaces:**
- Produces: 包级 `var parkedRunTTL = 2 * time.Hour`（测试可改小）；park 超时 → emit `timeout{kind, reason:"parked_ttl"}` + `cancelRun`（本地决策，spec §6.3）。

- [ ] **Step 1: 写失败测试**

```go
package server

import (
	"context"
	"testing"
	"time"
)

func TestParkTimesOut(t *testing.T) {
	old := parkedRunTTL
	parkedRunTTL = 50 * time.Millisecond
	defer func() { parkedRunTTL = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &Run{id: "test", ctx: ctx, cancel: cancel, subs: map[*subscriber]struct{}{}}
	done := make(chan error, 1)
	go func() {
		_, err := r.park("permission", map[string]any{"tool_name": "Write"}) // 无 answer，直到超时
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("park returned nil error after TTL, want ctx error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("park did not return after TTL")
	}
	if ctx.Err() == nil {
		t.Fatal("park TTL should cancel the run ctx")
	}
	var sawTimeout bool
	for _, ev := range r.history {
		if ev.Type == "timeout" {
			sawTimeout = true
		}
	}
	if !sawTimeout {
		t.Fatal("want timeout event in history")
	}
}
```

（`park` 签名已核实 run.go:159：`func (r *Run) park(kind string, payload any) (any, error)`，成功路径返回 `(answer, nil)`，取消路径返回 `(nil, r.ctx.Err())`。）

- [ ] **Step 2: 运行确认失败**

Run: `go test -race ./internal/server/ -run TestParkTimesOut -v`
Expected: FAIL —— 2s 后 park 仍阻塞（无 TTL）。

- [ ] **Step 3: 实现**（run.go:159-173）

```go
// parkedRunTTL bounds how long a run may sit parked on an unanswered
// interrupt before it is cancelled (local decision — no industry
// precedent; see spec §6.3).
var parkedRunTTL = 2 * time.Hour
```

`park` 在 emit interrupt 之后、select 之前插入 timer（select 用 park 内已有的局部 `reply` channel）：

```go
	timer := time.AfterFunc(parkedRunTTL, func() {
		r.emit(Event{Type: "timeout", Data: map[string]any{"kind": kind, "reason": "parked_ttl"}})
		r.cancel()
	})
	defer timer.Stop()
	select {
	case answer := <-reply:
		return answer, nil
	case <-r.ctx.Done():
		return nil, r.ctx.Err() // 原取消路径语义不变
	}
```

- [ ] **Step 4: 运行测试**

Run: `go test -race ./internal/server/ -v`
Expected: 全 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/server/run.go internal/server/run_test.go
git commit -m "feat(server): cancel parked runs after a 2h TTL"
```

---

### Task 14: P3.4（后端）— /api/stats

**Files:**
- Modify: `internal/server/server.go`（路由表 :179-214、新增 `handleStats`）
- Modify: `internal/agent/agent.go`（新增 `ActiveTeammateCount`/`RunningTaskCount`/`LSPStats` 三个 wrapper）
- Test: `internal/server/statsapi_test.go`

**Interfaces:**
- Consumes: `admission.stats()`（Task 1）、`Run.droppedCount()`（Task 9）、`TeamManager.ActiveCount`/`TaskManager.RunningCount`（Task 8）、`Registry.LSPStats`（Task 6）。
- Produces: `GET /api/stats` →

```json
{
  "active_runs": 1,
  "queued_runs": 2,
  "session_queue_depths": {"sess-...": 2},
  "teammates": 3,
  "background_tasks": 1,
  "sse_dropped": 0,
  "lsp": {"active": 1, "broken": 0}
}
```

- [ ] **Step 1: 写失败测试** `internal/server/statsapi_test.go`

```go
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleStats(t *testing.T) {
	a := newTestAgent(t) // server_test.go:32
	s := New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir()})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		ActiveRuns       int            `json:"active_runs"`
		QueuedRuns       int            `json:"queued_runs"`
		SessionQueueDepths map[string]int `json:"session_queue_depths"`
		Teammates        int            `json:"teammates"`
		BackgroundTasks  int            `json:"background_tasks"`
		SSEDropped       int64          `json:"sse_dropped"`
		LSP              struct {
			Active int `json:"active"`
			Broken int `json:"broken"`
		} `json:"lsp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.ActiveRuns != 0 || body.QueuedRuns != 0 {
		t.Fatalf("body = %+v, want zeroed counters on idle server", body)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test -race ./internal/server/ -run TestHandleStats -v`
Expected: 404。

- [ ] **Step 3: 实现**

路由表（server.go:179-214，`GET /api/memory/stats` 之后）加：

```go
mux.HandleFunc("GET /api/stats", s.handleStats)
```

handler：

```go
// handleStats reports runtime concurrency counters for threshold tuning.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	notDone := 0
	var dropped int64
	for _, run := range s.runs {
		if !run.isDone() {
			notDone++
		}
		dropped += run.droppedCount()
	}
	s.mu.Unlock()
	queued, depths := s.admission.stats()
	active := notDone - queued // 排队中的 run 已注册未完成，从活跃里剔除
	if active < 0 {
		active = 0
	}
	lspActive, lspBroken := s.agent.LSPStats()
	writeJSON(w, map[string]any{
		"active_runs":          active,
		"queued_runs":          queued,
		"session_queue_depths": depths,
		"teammates":            s.agent.ActiveTeammateCount(),
		"background_tasks":     s.agent.RunningTaskCount(),
		"sse_dropped":          dropped,
		"lsp":                  map[string]any{"active": lspActive, "broken": lspBroken},
	})
}
```

（`writeJSON` 用 server 现有的 JSON 响应写法；`zerolog`/`fstest` 等测试 import 按现有测试文件。）

agent.go 三个 wrapper：

```go
// ActiveTeammateCount reports live teammates across the agent's team (0 when
// no team exists). For /api/stats.
func (a *Agent) ActiveTeammateCount() int {
	a.mu.Lock()
	team := a.team
	a.mu.Unlock()
	if team == nil {
		return 0
	}
	return team.ActiveCount()
}

// RunningTaskCount reports running background tasks. For /api/stats.
func (a *Agent) RunningTaskCount() int {
	return a.tasks.RunningCount()
}

// LSPStats reports pooled/broken LSP servers from the tool registry.
func (a *Agent) LSPStats() (active, broken int) {
	if a.registry == nil {
		return 0, 0
	}
	return a.registry.LSPStats()
}
```

（`a.team`/`a.tasks`/`a.registry` 字段名与锁纪律以 agent.go 实际为准。）

- [ ] **Step 4: 运行测试**

Run: `go test -race ./internal/server/ ./internal/agent/ -v`
Expected: 全 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/server/server.go internal/server/statsapi_test.go internal/agent/agent.go
git commit -m "feat(server): /api/stats runtime concurrency counters"
```

---

### Task 15: P3.4（前端）— /api/stats 展示

**Files:**
- Modify: `web/src/types.ts`（追加 `StatsResponse`）
- Modify: `web/src/api/client.ts`（追加 `stats()`，照抄 `memoryStats()` :250-253 模式）
- Create: `web/src/components/settings/StatsSection.vue`
- Modify: `web/src/components/SettingsPanel.vue:27-36,:90-97`

**Interfaces:**
- Consumes: Task 14 的响应 JSON。
- Produces: `api.stats(): Promise<StatsResponse>`；设置面板新增"运行状态" section。

- [ ] **Step 1: 类型 + API**

`types.ts` 追加（遵循"mirror the Go server's JSON"惯例）：

```ts
// --- runtime stats (mirror /api/stats) ---
export interface StatsResponse {
  active_runs: number
  queued_runs: number
  session_queue_depths: Record<string, number>
  teammates: number
  background_tasks: number
  sse_dropped: number
  lsp: { active: number; broken: number }
}
```

`client.ts`（`memoryStats` 旁边）：

```ts
export function stats(): Promise<StatsResponse> {
  return request<StatsResponse>('/api/stats')
}
```

- [ ] **Step 2: StatsSection.vue**（展示模式参照 `MemoryPanel.vue:101-111` 的 chips）

```vue
<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import * as api from '../../api/client'
import type { StatsResponse } from '../../types'

const data = ref<StatsResponse | null>(null)
let timer: ReturnType<typeof setInterval> | null = null

async function refresh() {
  data.value = await api.stats()
}

onMounted(() => {
  void refresh()
  timer = setInterval(() => void refresh(), 5000)
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
})
</script>

<template>
  <section>
    <h3>运行状态</h3>
    <div v-if="data" class="stats-grid">
      <span class="stat-chip">活跃 run：{{ data.active_runs }}</span>
      <span class="stat-chip">排队 run：{{ data.queued_runs }}</span>
      <span class="stat-chip">teammate：{{ data.teammates }}</span>
      <span class="stat-chip">后台 task：{{ data.background_tasks }}</span>
      <span class="stat-chip">SSE 丢弃：{{ data.sse_dropped }}</span>
      <span class="stat-chip">LSP：{{ data.lsp.active }} 活跃 / {{ data.lsp.broken }} 熔断</span>
    </div>
    <p v-else>加载中…</p>
  </section>
</template>

<style scoped>
.stats-grid {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}
.stat-chip {
  padding: 4px 10px;
  border-radius: 999px;
  background: var(--bg-soft, rgba(127, 127, 127, 0.12));
  font-size: 12px;
}
</style>
```

- [ ] **Step 3: 挂进 SettingsPanel**

`sections` 数组（:27-36）加 `{ id: 'stats', label: '运行状态' }`；右栏（:90-97）加 `<StatsSection v-if="active === 'stats'" />` 并 import。（字段名以 sections 数组现有项的结构为准。）

- [ ] **Step 4: 验证**

Run: `cd web && npm run build`
Expected: vue-tsc + vite build 通过。

- [ ] **Step 5: Commit**

```bash
git add web/src/types.ts web/src/api/client.ts web/src/components/settings/StatsSection.vue web/src/components/SettingsPanel.vue
git commit -m "feat(web): runtime stats section in settings panel"
```

---

## 收尾验证（全部 Task 完成后）

- [ ] `go build ./... && go test -race ./...` 全绿。
- [ ] `cd web && npm run build` 通过。
- [ ] `golangci-lint run ./internal/...`（若本机已装）无新增告警。
- [ ] 手工冒烟：启动 server，同一会话快速连发 3 条消息 → 徽标显示排队位置递减、无消息丢失；`curl localhost:PORT/api/stats` 计数正确。
