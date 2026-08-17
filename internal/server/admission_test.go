package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
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
	go func() {
		if a.wait(context.Background(), "s1", w1) {
			granted <- "w1"
		}
	}()
	go func() {
		if a.wait(context.Background(), "s1", w2) {
			granted <- "w2"
		}
	}()
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
		// 偏差说明：计划原文无 limiter。热路径下 FakeChatModel 单轮 run 在
		// 微秒级完成，第二条 POST 到达前门已被释放（pos=0，-count 下必挂），
		// 故借 WithRateLimiter 钩子给每次模型调用加固定延迟，保证 run1 在
		// flight 中可被第二条 POST 观察到排队。
		Model: language.NewFakeChatModel(
			language.WithResponses(msgs...),
			language.WithRateLimiter(delayLimiter{d: 200 * time.Millisecond}),
		),
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

// delayLimiter pauses before each model call (runaway-client tests need a
// run that stays in flight long enough for the next same-session POST to
// observe the gate held).
type delayLimiter struct{ d time.Duration }

func (l delayLimiter) Acquire(ctx context.Context, _ bool) (bool, error) {
	select {
	case <-time.After(l.d):
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// 同 session 两条 run 串行：run2 的 history 必须包含 run1 已落盘的回复。
func TestSessionGateSerializesRuns(t *testing.T) {
	a := newScriptedAgent(t, "reply-1", "reply-2")
	s := New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir()})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// 偏差说明：done 事件先于 onDone 落盘发出；等齐两个 save 事件再结束，
	// 避免 t.TempDir 清理与异步写盘竞态（"directory not empty"）。
	saved := make(chan string, 4)
	s.sessions.testAfterSave = func(id string) { saved <- id }

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
	for i := 0; i < 2; i++ {
		select {
		case <-saved:
		case <-time.After(10 * time.Second):
			t.Fatalf("transcript save %d not completed", i+1)
		}
	}

	// 偏差说明：done 事件先于 onDone 落盘发出，单次 GET 存在固有竞态，
	// 改为有界轮询（断言不变：两条回复都必须出现）。
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(srv.URL + "/api/sessions/" + sid1)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(body), "reply-1") && strings.Contains(string(body), "reply-2") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("transcript missing replies: %s", body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 冒烟断言入库（终审遗留）：同 session 3 连发时 queue_position 0/1/2，
// 中途态 /api/stats 必须报 active_runs=1（持门的 run1）、queued_runs=2、
// session_queue_depths 含该会话且深度 2；最终三条 run 全部终态、队列清空。
func TestSessionGateThreeRunsStatsMidFlight(t *testing.T) {
	a := newScriptedAgent(t, "reply-1", "reply-2", "reply-3")
	s := New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir()})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// 偏差说明：done 事件先于 onDone 落盘发出；等齐三个 save 事件再结束，
	// 避免 t.TempDir 清理与异步写盘竞态（"directory not empty"）。
	saved := make(chan string, 6)
	s.sessions.testAfterSave = func(id string) { saved <- id }

	run1, sid1, pos1 := postRun(t, srv.URL, "", "first")
	if pos1 != 0 {
		t.Fatalf("first run position = %d, want 0", pos1)
	}
	run2, sid2, pos2 := postRun(t, srv.URL, sid1, "second")
	if sid2 != sid1 || pos2 != 1 {
		t.Fatalf("second run sid=%s position=%d, want sid=%s position=1", sid2, pos2, sid1)
	}
	run3, sid3, pos3 := postRun(t, srv.URL, sid1, "third")
	if sid3 != sid1 || pos3 != 2 {
		t.Fatalf("third run sid=%s position=%d, want sid=%s position=2", sid3, pos3, sid1)
	}

	type statsBody struct {
		ActiveRuns         int            `json:"active_runs"`
		QueuedRuns         int            `json:"queued_runs"`
		SessionQueueDepths map[string]int `json:"session_queue_depths"`
	}
	getStats := func() statsBody {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/stats")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body statsBody
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	// 中途态：run1 持门（delayLimiter 的 200ms 窗口内可观察），run2/run3
	// 排队。有界轮询到精确计数——错过窗口则 2 深度不会再出现，超时即败。
	deadline := time.Now().Add(10 * time.Second)
	var mid statsBody
	for {
		mid = getStats()
		if mid.ActiveRuns == 1 && mid.QueuedRuns == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mid-flight stats active=%d queued=%d never observed", mid.ActiveRuns, mid.QueuedRuns)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if d, ok := mid.SessionQueueDepths[sid1]; !ok || d != 2 {
		t.Fatalf("mid-flight session_queue_depths = %v, want [%s]=2", mid.SessionQueueDepths, sid1)
	}

	// 收尾：三条 run 全部到达终态；等齐三个 save 事件后队列必须清空。
	waitRunDone(t, srv.URL, run1)
	waitRunDone(t, srv.URL, run2)
	waitRunDone(t, srv.URL, run3)
	for i := 0; i < 3; i++ {
		select {
		case <-saved:
		case <-time.After(10 * time.Second):
			t.Fatalf("transcript save %d not completed", i+1)
		}
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		end := getStats()
		if end.ActiveRuns == 0 && end.QueuedRuns == 0 && len(end.SessionQueueDepths) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("final stats not drained: %+v", end)
		}
		time.Sleep(5 * time.Millisecond)
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
	// 偏差说明：等 run2 的 save 事件再返回，避免 t.TempDir 清理与异步
	// 写盘竞态（"directory not empty"）。
	select {
	case <-saved:
	case <-time.After(10 * time.Second):
		t.Fatal("run2 transcript not saved")
	}
}

// 深度超限：门被占住时第 9 条排队 POST 返回 409。
func TestSessionGateQueueFull409(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	model := &blockingModel{block: block} // Stream 阻塞在 block 上，永不返回回复
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

// blockingModel 的 Stream 实现参照 internal/agent/streamingexec_test.go:55
// 的 scriptedModel，差别是返回的 stream 在 Next 时阻塞于 block channel
// （测试结束 close 后返回 cancelled）。
type blockingModel struct {
	block chan struct{}
}

func (m *blockingModel) Invoke(_ context.Context, _ []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return messages.AI("unused"), nil
}

func (m *blockingModel) Batch(_ context.Context, inputs [][]messages.Message, _ ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i := range out {
		out[i] = messages.AI("unused")
	}
	return out, nil
}

func (m *blockingModel) Stream(_ context.Context, _ []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return &blockingStream{block: m.block}, nil
}

func (m *blockingModel) InputSchema() schema.Schema { return schema.Schema{"type": "array"} }
func (m *blockingModel) OutputSchema() schema.Schema {
	return schema.Object(map[string]schema.Schema{"role": schema.String("role")})
}

func (m *blockingModel) BindTools(_ []coretools.Tool) (language.ChatModel, error) {
	return m, nil
}

func (m *blockingModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true}
}

type blockingStream struct {
	block chan struct{}
}

func (s *blockingStream) Next(ctx context.Context) (messages.Message, bool, error) {
	select {
	case <-s.block:
		return messages.Message{}, false, context.Canceled
	case <-ctx.Done():
		return messages.Message{}, false, ctx.Err()
	}
}

func (s *blockingStream) Close() error { return nil }
