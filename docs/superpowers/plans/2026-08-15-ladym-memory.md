# LadyM 事实记忆库集成 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 LadyM（github.com/ProjAnvil/LadyM）作为 woca 的事实记忆库：每 turn 语义召回注入、run 完成自动沉淀、4 个记忆工具、settings 实时配置、web 记忆面板。

**Architecture:** in-process 单例 `*engine.Engine`（项目级，`~/.woca/ladym.db` 单库 + workspace 行级隔离）；新包 `internal/memory` 提供窄接口 `memory.Engine`，agent/console/server 只依赖接口；写入收口在 runner Done 单一 choke point；System 2 为机会主义 post-run 触发（无常驻 daemon）。

**Tech Stack:** Go 1.26.4、LadyM v0.3.1（含本计划 Task 1 新增的 `List`/`StatsFor` API）、projanvil/langchain-golang v0.5.0、Vue3 + Pinia（前端改动必须先 `npm run build` 再 `go build`）。

**Spec:** `docs/superpowers/specs/2026-08-15-ladym-memory-design.md`（§ 编号在任务中引用）

**两个仓库：** Task 1 在 `~/Documents/codebase/projanvil/ladyM`（LadyM 仓库）执行并打 tag；其余任务在 woca 仓库执行。

**执行注意：**
- 本机 Go 工具链已为 go1.26.4（`go version` 验证）。
- LadyM v0.3.1 tag 必须推到 GitHub 后 woca 才能 `go get`；若暂不推送，在 woca go.mod 加 `replace github.com/ProjAnvil/LadyM => ../ladyM`（**不提交该行**，发布前移除）。
- 依赖注入方向：`internal/agent`、`internal/console`、`internal/server` 只 import `internal/memory`，`internal/memory` 是唯一 import LadyM 的 woca 包。

## 任务总览

| # | 阶段 | 任务 | 产出 |
|---|---|---|---|
| 1 | P0 | LadyM `Engine.List` + `StatsFor` + tag v0.3.1 | LadyM 只读 API |
| 2 | P0 | woca go.mod：go 1.26.4 + 依赖 LadyM | 依赖就绪 |
| 3 | P0 | settings 层：10 个新键 + secret | 持久化配置 |
| 4 | P0 | internal/memory：接口 + LadyM 适配器 + Build | 记忆引擎包 |
| 5 | P0 | agent：prefetch 走 Recall + workspace 绑定 | ①读路径 |
| 6 | P0 | main.go：buildAgent 注入 + rebuildMemory + serve 接线 | 启动/热重建 |
| 7 | P0 | 冒烟验证 | P0 完成判据 |
| 8 | P1 | 4 个记忆工具 + subagent 只读过滤 | ②工具面 |
| 9 | P1 | runner Done：auto-retain（Remember+RecordEvent） | ③写路径 |
| 10 | P1 | console /memory 引擎子命令 + /settings 即时生效 | TUI 面 |
| 11 | P2 | server：MemoryEngine 注入 + 4 个 REST endpoint | ⑥面板 API |
| 12 | P2 | settingsapi：PUT 新键 + 实时 rebuild | ⑤实时生效 |
| 13 | P2 | 前端 types/client/memory store | 前端地基 |
| 14 | P2 | MemoryPanel + title bar 入口 | 记忆面板 |
| 15 | P2 | SettingsPanel 记忆区 + 构建 | 设置页 |
| 16 | P3 | System 2 机会主义触发 | ④后台整理 |
| 17 | P3 | /memory consolidate + subagent 事件沉淀 | P3 收尾 |

---

## Task 1（LadyM 仓库）：新增 `Engine.List` 与 `Engine.StatsFor`

**仓库：** `~/Documents/codebase/projanvil/ladyM`（当前 main = 4526369）

**Files:**
- Create: `engine/list.go`
- Create: `engine/list_test.go`
- Modify: `engine/engine.go`（`Stats` 拆出 `StatsFor`）

- [ ] **Step 1: 写失败测试** — 新建 `engine/list_test.go`：

```go
package engine

import (
	"testing"

	"github.com/ProjAnvil/LadyM/schema"
)

func TestListNewestFirstExcludesRetired(t *testing.T) {
	eng := newTestEngine(t) // engine_test.go 里现成的 *Engine 构造（hashing embedding + tmp db）
	defer eng.Close()
	ws := "list-test"
	if _, err := eng.Remember("older fact", schema.LayerSemantic, schema.TypeFact, nil, nil, "t", ""); err != nil {
		t.Fatal(err)
	}
	m2, err := eng.Remember("newer fact", schema.LayerSemantic, schema.TypeFact, nil, nil, "t", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := eng.List(ws, nil, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("List returned no memories")
	}
	if got[0].ID != m2.ID {
		t.Fatalf("newest first: got %s want %s", got[0].ID, m2.ID)
	}
	// layer filter
	sem := schema.LayerSemantic
	filtered, err := eng.List(ws, &sem, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range filtered {
		if m.Layer != schema.LayerSemantic {
			t.Fatalf("layer filter leaked %s", m.Layer)
		}
	}
	// pagination
	page2, err := eng.List(ws, nil, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) > 1 {
		t.Fatalf("limit=1 returned %d", len(page2))
	}
}

func TestStatsForScopesWorkspace(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()
	if _, err := eng.Remember("fact a", schema.LayerSemantic, schema.TypeFact, nil, nil, "t", ""); err != nil {
		t.Fatal(err)
	}
	st, err := eng.StatsFor("stats-other-ws")
	if err != nil {
		t.Fatal(err)
	}
	if st.TotalMemories != 0 {
		t.Fatalf("other workspace should be empty, got %d", st.TotalMemories)
	}
}
```

注意：若 `engine_test.go` 的测试引擎构造函数名不是 `newTestEngine`（先 `grep -n 'func newTestEngine\|func newEngine\|ForTesting' engine/engine_test.go` 确认），用实际存在的构造替换；`Remember` 的 workspace 参数默认取 `eng.Config.Workspace`，测试里先 `eng.SetWorkspace(ws)`（engine.go:103）再 Remember，让两条记忆落在同一 workspace。

- [ ] **Step 2: 跑测试确认失败** — `go test ./engine/ -run 'TestList|TestStatsFor' -v`，预期 `undefined: eng.List` / `undefined: eng.StatsFor` 编译失败。

- [ ] **Step 3: 实现** — 新建 `engine/list.go`：

```go
package engine

import (
	"sort"

	"github.com/ProjAnvil/LadyM/operations"
	"github.com/ProjAnvil/LadyM/schema"
)

// List returns a workspace's memories newest-first, excluding retired items
// (expired predictions, superseded models). limit<=0 means 20; offset pages.
// Read-only: it never calls the LLM.
func (e *Engine) List(workspace string, layer *schema.Layer, limit, offset int) ([]*schema.Memory, error) {
	if workspace == "" {
		workspace = e.Config.Workspace
	}
	layerStr := ""
	if layer != nil {
		layerStr = string(*layer)
	}
	all, err := e.Store.IterMemories(workspace, layerStr, "")
	if err != nil {
		return nil, err
	}
	live := make([]*schema.Memory, 0, len(all))
	for _, m := range all {
		if !operations.IsRetired(m) {
			live = append(live, m)
		}
	}
	sort.SliceStable(live, func(i, j int) bool { return live[i].UpdatedAt > live[j].UpdatedAt })
	if offset >= len(live) {
		return []*schema.Memory{}, nil
	}
	live = live[offset:]
	if limit <= 0 {
		limit = 20
	}
	if len(live) > limit {
		live = live[:limit]
	}
	return live, nil
}
```

在 `engine/engine.go` 的 `Stats()`（第 526 行起）处改为委托（`e.Store.Count` 的 workspace 实参从 `e.Config.Workspace` 提出来）：

```go
// Stats returns aggregate store statistics for the engine's default workspace.
func (e *Engine) Stats() (*schema.Stats, error) {
	return e.StatsFor(e.Config.Workspace)
}

// StatsFor returns aggregate store statistics for one workspace.
func (e *Engine) StatsFor(workspace string) (*schema.Stats, error) {
	counts, err := e.Store.Count(workspace)
	if err != nil {
		return nil, err
	}
	// ……保留原 Stats() 函数体中 counts 之后的全部逻辑，workspace 相关处用 workspace 变量
}
```

（原函数体从 `counts, err := e.Store.Count(...)` 之后的部分原样移入 `StatsFor`；只改 workspace 的来源。）

- [ ] **Step 4: 跑测试确认通过** — `go test ./engine/ -v`，预期全部 PASS（含原有用例）。

- [ ] **Step 5: 全量回归** — `go test ./...`，预期 PASS（hermetic，无网络）。

- [ ] **Step 6: 提交并打 tag**：

```bash
git add engine/list.go engine/list_test.go engine/engine.go
git commit -m "feat(engine): add read-only List and per-workspace StatsFor"
git tag v0.3.1
git push origin main v0.3.1
```

若 push 不可用：只打本地 tag，并在 woca 侧用 `replace` 指令（见执行注意）。

---

## Task 2（woca）：go 1.26.4 + 依赖 LadyM v0.3.1

**Files:**
- Modify: `go.mod`、`go.sum`

- [ ] **Step 1: 升 go directive** — 编辑 `go.mod` 第一段：`go 1.25.0` → `go 1.26.4`。

- [ ] **Step 2: 拉依赖**：

```bash
go get github.com/ProjAnvil/LadyM@v0.3.1
go mod tidy
```

若 tag 未推送：在 go.mod 加 `replace github.com/ProjAnvil/LadyM => ../ladyM` 后再 tidy（此行不提交）。

- [ ] **Step 3: 验证** — `go build ./...`，预期无错误；`go test ./...`，预期全 PASS（LadyM 依赖尚未被引用，纯基线）。

- [ ] **Step 4: Commit** — `git add go.mod go.sum && git commit -m "build: bump go 1.26.4 and depend on LadyM v0.3.1"`

---

## Task 3（woca）：settings 层 10 个新键 + 1 个 secret

spec §5.1。DB 存储键用下划线、显示键用连字符（沿用现有约定）。

**Files:**
- Modify: `internal/settings/settings.go`
- Modify: `internal/settings/keys.go`
- Create: `internal/settings/ladym_test.go`

- [ ] **Step 1: 写失败测试** — 新建 `internal/settings/ladym_test.go`：

```go
package settings

import "testing"

func TestLadyMDefaults(t *testing.T) {
	got, err := Default().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryEngine != MemoryEngineLadyM {
		t.Fatalf("memory engine default = %q, want ladym", got.MemoryEngine)
	}
	if got.LadyMEmbedding != LadyMEmbeddingInherit {
		t.Fatalf("embedding default = %q, want inherit", got.LadyMEmbedding)
	}
	if got.LadyMLLM != LadyMLLMInherit {
		t.Fatalf("llm default = %q, want inherit", got.LadyMLLM)
	}
	if !got.LadyMAutoRetain {
		t.Fatal("autoretain default should be true")
	}
	if got.LadyMTopK != 6 {
		t.Fatalf("topk default = %d, want 6", got.LadyMTopK)
	}
	if got.LadyMSystem2Interval != 300 {
		t.Fatalf("system2 interval default = %d, want 300", got.LadyMSystem2Interval)
	}
}

func TestApplyLadyMKeys(t *testing.T) {
	cfg := Default()
	cases := []struct {
		key, value string
		wantErr    bool
	}{
		{KeyMemoryEngine, "none", false},
		{KeyMemoryEngine, "ladym", false},
		{KeyMemoryEngine, "bogus", true},
		{KeyLadyMEmbedding, "hashing", false},
		{KeyLadyMEmbedding, "inherit", false},
		{KeyLadyMEmbedding, "nope", true},
		{KeyLadyMLLM, "none", false},
		{KeyLadyMLLM, "inherit", false},
		{KeyLadyMLLM, "weird", true},
		{KeyLadyMTopK, "20", false},
		{KeyLadyMTopK, "21", true},
		{KeyLadyMTopK, "0", true},
		{KeyLadyMSystem2Interval, "60", false},
		{KeyLadyMSystem2Interval, "59", true},
		{KeyLadyMAutoRetain, "false", false},
		{KeyLadyMDBPath, "/tmp/x.db", false},
		{KeyLadyMEmbeddingModel, "text-embedding-3-small", false},
		{KeyLadyMEmbeddingBaseURL, "https://x", false},
		{KeyLadyMEmbeddingAPIKey, "sk-x", true}, // secret: Apply must refuse
	}
	for _, c := range cases {
		err := Apply(&cfg, c.key, c.value)
		if c.wantErr && err == nil {
			t.Fatalf("Apply(%s=%s) should fail", c.key, c.value)
		}
		if !c.wantErr && err != nil {
			t.Fatalf("Apply(%s=%s): %v", c.key, c.value, err)
		}
	}
}

func TestLadyMPairsRoundTrip(t *testing.T) {
	base := Default()
	pairs := map[string]string{
		"memory_engine":           "none",
		"ladym_db_path":           "/tmp/mem.db",
		"ladym_embedding":         "hashing",
		"ladym_embedding_model":   "m1",
		"ladym_embedding_base_url": "https://e",
		"ladym_llm":               "none",
		"ladym_topk":              "12",
		"ladym_system2":           "true",
		"ladym_system2_interval":  "600",
	}
	applyPairs(&base, pairs)
	if base.MemoryEngine != "none" || base.LadyMDBPath != "/tmp/mem.db" || base.LadyMTopK != 12 ||
		!base.LadyMSystem2 || base.LadyMSystem2Interval != 600 {
		t.Fatalf("applyPairs mismatch: %+v", base)
	}
}
```

- [ ] **Step 2: 跑测试确认失败** — `go test ./internal/settings/ -run TestLadyM -v`，预期编译失败（字段/常量未定义）。

- [ ] **Step 3: settings.go 改动**：

(a) 常量区（`MemoryPostgres` 之后）追加：

```go
// Memory-engine selectors (the ladyM factual-memory subsystem).
const (
	MemoryEngineLadyM = "ladym"
	MemoryEngineNone  = "none"
)

// ladyM embedding providers.
const (
	LadyMEmbeddingInherit = "inherit"
	LadyMEmbeddingHashing = "hashing"
	LadyMEmbeddingOpenAI  = "openai"
	LadyMEmbeddingOllama  = "ollama"
	LadyMEmbeddingHTTP    = "http"
)

// ladyM LLM usage for offline consolidation.
const (
	LadyMLLMInherit = "inherit"
	LadyMLLMNone    = "none"
)
```

(b) secret 常量区追加：`SecretLadyMEmbeddingAPIKey = "ladym_embedding_api_key"`。

(c) `Settings` 结构体在 `DebugLog` 前追加字段：

```go
	MemoryEngine          string `json:"memory_engine"`
	LadyMDBPath           string `json:"ladym_db_path"`
	LadyMEmbedding        string `json:"ladym_embedding"`
	LadyMEmbeddingModel   string `json:"ladym_embedding_model"`
	LadyMEmbeddingBaseURL string `json:"ladym_embedding_base_url"`
	LadyMLLM              string `json:"ladym_llm"`
	LadyMAutoRetain       bool   `json:"ladym_autoretain"`
	LadyMTopK             int    `json:"ladym_topk"`
	LadyMSystem2          bool   `json:"ladym_system2"`
	LadyMSystem2Interval  int    `json:"ladym_system2_interval"`
```

(d) `Default()` 返回值追加：`MemoryEngine: MemoryEngineLadyM, LadyMAutoRetain: true,`（其余零值由 Normalize 补齐）。

(e) `Normalize()` 在 memoryBackend switch 之后追加：

```go
	switch out.MemoryEngine {
	case "":
		out.MemoryEngine = MemoryEngineLadyM
	case MemoryEngineLadyM, MemoryEngineNone:
	default:
		return out, fmt.Errorf("settings: unknown memory engine %q", out.MemoryEngine)
	}
	switch out.LadyMEmbedding {
	case "":
		out.LadyMEmbedding = LadyMEmbeddingInherit
	case LadyMEmbeddingInherit, LadyMEmbeddingHashing, LadyMEmbeddingOpenAI, LadyMEmbeddingOllama, LadyMEmbeddingHTTP:
	default:
		return out, fmt.Errorf("settings: unknown ladym embedding %q", out.LadyMEmbedding)
	}
	switch out.LadyMLLM {
	case "":
		out.LadyMLLM = LadyMLLMInherit
	case LadyMLLMInherit, LadyMLLMNone:
	default:
		return out, fmt.Errorf("settings: unknown ladym llm %q", out.LadyMLLM)
	}
	if out.LadyMTopK == 0 {
		out.LadyMTopK = 6
	}
	if out.LadyMSystem2Interval == 0 {
		out.LadyMSystem2Interval = 300
	}
```

(f) `applyPairs` 末尾追加：

```go
	if v, ok := pairs["memory_engine"]; ok {
		out.MemoryEngine = v
	}
	if v, ok := pairs["ladym_db_path"]; ok {
		out.LadyMDBPath = v
	}
	if v, ok := pairs["ladym_embedding"]; ok {
		out.LadyMEmbedding = v
	}
	if v, ok := pairs["ladym_embedding_model"]; ok {
		out.LadyMEmbeddingModel = v
	}
	if v, ok := pairs["ladym_embedding_base_url"]; ok {
		out.LadyMEmbeddingBaseURL = v
	}
	if v, ok := pairs["ladym_llm"]; ok {
		out.LadyMLLM = v
	}
	if v, ok := pairs["ladym_autoretain"]; ok {
		out.LadyMAutoRetain, _ = ParseBool(v)
	}
	if v, ok := pairs["ladym_topk"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			out.LadyMTopK = n
		}
	}
	if v, ok := pairs["ladym_system2"]; ok {
		out.LadyMSystem2, _ = ParseBool(v)
	}
	if v, ok := pairs["ladym_system2_interval"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			out.LadyMSystem2Interval = n
		}
	}
```

(g) `Save()` 的 `fields` 切片追加字符串字段：

```go
		{"memory_engine", cfg.MemoryEngine},
		{"ladym_db_path", cfg.LadyMDBPath},
		{"ladym_embedding", cfg.LadyMEmbedding},
		{"ladym_embedding_model", cfg.LadyMEmbeddingModel},
		{"ladym_embedding_base_url", cfg.LadyMEmbeddingBaseURL},
		{"ladym_llm", cfg.LadyMLLM},
```

在 `debug_log` 显式写入之后追加（bool 需要 false 也能持久化）：

```go
	fields = append(fields, field{"ladym_autoretain", strconv.FormatBool(cfg.LadyMAutoRetain)})
	fields = append(fields, field{"ladym_system2", strconv.FormatBool(cfg.LadyMSystem2)})
	if cfg.LadyMTopK != 0 {
		fields = append(fields, field{"ladym_topk", strconv.Itoa(cfg.LadyMTopK)})
	}
	if cfg.LadyMSystem2Interval != 0 {
		fields = append(fields, field{"ladym_system2_interval", strconv.Itoa(cfg.LadyMSystem2Interval)})
	}
```

注意：`fields` 追加发生在 `for _, f := range fields` 写库循环**之前**——把这两段插入位置放在 `debug_log` 那行之后、循环之前。

- [ ] **Step 4: keys.go 改动**：

(a) 常量区追加：

```go
	KeyMemoryEngine          = "memory-engine"
	KeyLadyMDBPath           = "ladym-db-path"
	KeyLadyMEmbedding        = "ladym-embedding"
	KeyLadyMEmbeddingModel   = "ladym-embedding-model"
	KeyLadyMEmbeddingBaseURL = "ladym-embedding-base-url"
	KeyLadyMEmbeddingAPIKey  = "ladym-embedding-api-key"
	KeyLadyMLLM              = "ladym-llm"
	KeyLadyMAutoRetain       = "ladym-autoretain"
	KeyLadyMTopK             = "ladym-topk"
	KeyLadyMSystem2          = "ladym-system2"
	KeyLadyMSystem2Interval  = "ladym-system2-interval"
```

(b) `KeyNames()` 在 `KeyDebugLog` 前插入全部新键（保持上面顺序）。

(c) `SecretFor` 追加：

```go
	case KeyLadyMEmbeddingAPIKey:
		return SecretLadyMEmbeddingAPIKey, true
```

(d) `KeyHelp` 追加：

```go
	case KeyMemoryEngine:
		return "factual memory engine: ladym | none (default ladym)"
	case KeyLadyMDBPath:
		return "ladym sqlite db path (default ~/.woca/ladym.db)"
	case KeyLadyMEmbedding:
		return "embedding: inherit | hashing | openai | ollama | http (default inherit)"
	case KeyLadyMEmbeddingModel:
		return "embedding model (openai default text-embedding-3-small)"
	case KeyLadyMEmbeddingBaseURL:
		return "embedding base URL (inherit uses openai-base-url)"
	case KeyLadyMEmbeddingAPIKey:
		return "embedding API key (stored encrypted; empty inherits openai-api-key)"
	case KeyLadyMLLM:
		return "offline consolidation LLM: inherit | none (default inherit)"
	case KeyLadyMAutoRetain:
		return "auto-remember each run's Q/A and record an episode: true | false"
	case KeyLadyMTopK:
		return "recall top-k per model turn (1-20, default 6)"
	case KeyLadyMSystem2:
		return "background consolidation after runs (uses LLM): true | false"
	case KeyLadyMSystem2Interval:
		return "min seconds between System 2 cycles per workspace (>=60, default 300)"
```

(e) `Apply` 在 `KeyDebugLog` case 之后追加（secret 拒绝放 switch 里已有的 secret case 列表）：

先修改现有 secret case 行为兼容：`case KeyAnthropicAPIKey, KeyOpenAIAPIKey, KeyPostgresURL:` 改为 `case KeyAnthropicAPIKey, KeyOpenAIAPIKey, KeyPostgresURL, KeyLadyMEmbeddingAPIKey:`。然后追加新 case：

```go
	case KeyMemoryEngine:
		switch value {
		case MemoryEngineLadyM, MemoryEngineNone:
		default:
			return fmt.Errorf("memory-engine must be ladym|none, got %q", value)
		}
		cfg.MemoryEngine = value
	case KeyLadyMDBPath:
		cfg.LadyMDBPath = value
	case KeyLadyMEmbedding:
		switch value {
		case LadyMEmbeddingInherit, LadyMEmbeddingHashing, LadyMEmbeddingOpenAI, LadyMEmbeddingOllama, LadyMEmbeddingHTTP:
		default:
			return fmt.Errorf("ladym-embedding must be inherit|hashing|openai|ollama|http, got %q", value)
		}
		cfg.LadyMEmbedding = value
	case KeyLadyMEmbeddingModel:
		cfg.LadyMEmbeddingModel = value
	case KeyLadyMEmbeddingBaseURL:
		cfg.LadyMEmbeddingBaseURL = value
	case KeyLadyMLLM:
		switch value {
		case LadyMLLMInherit, LadyMLLMNone:
		default:
			return fmt.Errorf("ladym-llm must be inherit|none, got %q", value)
		}
		cfg.LadyMLLM = value
	case KeyLadyMAutoRetain:
		b, err := ParseBool(value)
		if err != nil {
			return err
		}
		cfg.LadyMAutoRetain = b
	case KeyLadyMTopK:
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 20 {
			return fmt.Errorf("ladym-topk must be an integer in [1,20], got %q", value)
		}
		cfg.LadyMTopK = n
	case KeyLadyMSystem2:
		b, err := ParseBool(value)
		if err != nil {
			return err
		}
		cfg.LadyMSystem2 = b
	case KeyLadyMSystem2Interval:
		n, err := strconv.Atoi(value)
		if err != nil || n < 60 {
			return fmt.Errorf("ladym-system2-interval must be >=60 seconds, got %q", value)
		}
		cfg.LadyMSystem2Interval = n
```

(f) `Describe` 在 `debug-log` 行前追加：

```go
	fmt.Fprintf(&b, "memory-engine: %s\n", orDefault(cfg.MemoryEngine, MemoryEngineLadyM))
	fmt.Fprintf(&b, "ladym-db-path: %s\n", orDefault(cfg.LadyMDBPath, "(default ~/.woca/ladym.db)"))
	fmt.Fprintf(&b, "ladym-embedding: %s\n", orDefault(cfg.LadyMEmbedding, LadyMEmbeddingInherit))
	fmt.Fprintf(&b, "ladym-embedding-model: %s\n", orDefault(cfg.LadyMEmbeddingModel, "(default)"))
	fmt.Fprintf(&b, "ladym-embedding-base-url: %s\n", orDefault(cfg.LadyMEmbeddingBaseURL, "(inherit)"))
	fmt.Fprintf(&b, "ladym-embedding-api-key: %s\n", mask(SecretLadyMEmbeddingAPIKey))
	fmt.Fprintf(&b, "ladym-llm: %s\n", orDefault(cfg.LadyMLLM, LadyMLLMInherit))
	fmt.Fprintf(&b, "ladym-autoretain: %s\n", strconv.FormatBool(cfg.LadyMAutoRetain))
	fmt.Fprintf(&b, "ladym-topk: %s\n", orDefault(strconv.Itoa(cfg.LadyMTopK), "6"))
	fmt.Fprintf(&b, "ladym-system2: %s\n", strconv.FormatBool(cfg.LadyMSystem2))
	fmt.Fprintf(&b, "ladym-system2-interval: %s\n", orDefault(strconv.Itoa(cfg.LadyMSystem2Interval), "300"))
```

(g) `KeyValue` 追加对应 case（照字段直读，int 用 `strconv.Itoa`，bool 用 `FormatBool`，空 int 显示 ""）：

```go
	case KeyMemoryEngine:
		return cfg.MemoryEngine
	case KeyLadyMDBPath:
		return cfg.LadyMDBPath
	case KeyLadyMEmbedding:
		return cfg.LadyMEmbedding
	case KeyLadyMEmbeddingModel:
		return cfg.LadyMEmbeddingModel
	case KeyLadyMEmbeddingBaseURL:
		return cfg.LadyMEmbeddingBaseURL
	case KeyLadyMLLM:
		return cfg.LadyMLLM
	case KeyLadyMAutoRetain:
		return strconv.FormatBool(cfg.LadyMAutoRetain)
	case KeyLadyMTopK:
		return strconv.Itoa(cfg.LadyMTopK)
	case KeyLadyMSystem2:
		return strconv.FormatBool(cfg.LadyMSystem2)
	case KeyLadyMSystem2Interval:
		return strconv.Itoa(cfg.LadyMSystem2Interval)
```

- [ ] **Step 5: 跑测试** — `go test ./internal/settings/ -v`，预期全 PASS（含新测试）。

- [ ] **Step 6: Commit** — `git add internal/settings/ && git commit -m "feat(settings): add memory-engine and ladym_* settings keys"`

---

## Task 4（woca）：internal/memory 包（接口 + LadyM 适配器）

spec §2/§5.2/§5.3。

**Files:**
- Create: `internal/memory/memory.go`
- Create: `internal/memory/ladym.go`
- Create: `internal/memory/ladym_test.go`

- [ ] **Step 1: 接口** — 新建 `internal/memory/memory.go`：

```go
// Package memory adapts the ladyM memory engine into woca. The Engine
// interface is the narrow surface consumed by the agent loop, the console,
// and the web API; ladyM is the only implementation, and a nil Engine means
// the memory subsystem is disabled.
package memory

import "context"

// Hit is one memory item on the read path (recall, search, list).
type Hit struct {
	ID        string   `json:"id"`
	Layer     string   `json:"layer"`
	Type      string   `json:"type"`
	Summary   string   `json:"summary"`
	Content   string   `json:"content"`
	Source    string   `json:"source"`
	Tags      []string `json:"tags"`
	Score     float64  `json:"score,omitempty"`
	UpdatedAt float64  `json:"updated_at"`
}

// Stats summarizes one workspace's memory.
type Stats struct {
	Total  int            `json:"total"`
	Layers map[string]int `json:"layers"`
}

// Engine is the factual-memory subsystem. All methods take a workspace
// (row-level isolation inside one sqlite db). Implementations must be safe
// for concurrent use.
type Engine interface {
	// Recall runs the two-tier semantic retrieval (heuristic, no LLM).
	Recall(ctx context.Context, query, workspace string, topK int) ([]Hit, error)
	// SearchCode searches indexed code symbols.
	SearchCode(ctx context.Context, query, workspace string, topK int) ([]Hit, error)
	// Remember stores a semantic fact (subject to ladyM's attention gate).
	Remember(ctx context.Context, workspace, content string, tags []string, source string) (string, error)
	// RecordEvent stores an L1 episodic tuple (food for consolidation).
	RecordEvent(ctx context.Context, workspace, agentName, action, observation, outcome string) error
	// List returns recent memories newest-first (layer may be "" for all).
	List(ctx context.Context, workspace, layer string, limit, offset int) ([]Hit, error)
	// Forget deletes one memory by id.
	Forget(ctx context.Context, id string) error
	// Stats counts memories per layer for one workspace.
	Stats(ctx context.Context, workspace string) (*Stats, error)
	// IndexCode (re)indexes a source root into code memories; returns a report.
	IndexCode(ctx context.Context, root, workspace string, force bool) (string, error)
	// Consolidate runs one System 2 cycle synchronously; returns a report.
	Consolidate(ctx context.Context, workspace string) (string, error)
	// Description names the engine for /memory stats output.
	Description() string
	// Close releases the underlying store.
	Close() error
}
```

- [ ] **Step 2: 写失败测试** — 新建 `internal/memory/ladym_test.go`：

```go
package memory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func testInput(t *testing.T) BuildInput {
	t.Helper()
	return BuildInput{
		EngineEnabled:    true,
		DBPath:           filepath.Join(t.TempDir(), "mem.db"),
		DefaultWorkspace: "/ws/test",
		Embedding:        LadyMEmbeddingHashing,
		LLM:              LadyMLLMNone,
	}
}

func TestBuildDisabled(t *testing.T) {
	in := testInput(t)
	in.EngineEnabled = false
	if _, err := Build(in); err == nil {
		t.Fatal("Build should fail when disabled")
	}
}

func TestRememberRecallListStatsForget(t *testing.T) {
	eng, err := Build(testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	ctx := context.Background()

	id, err := eng.Remember(ctx, "/ws/test", "auth uses JWT with 24h expiry", []string{"auth"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("Remember returned empty id")
	}
	hits, err := eng.Recall(ctx, "how does auth work", "/ws/test", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("Recall found nothing after Remember")
	}
	if hits[0].Layer == "" || hits[0].Type == "" {
		t.Fatalf("hit missing layer/type: %+v", hits[0])
	}
	// workspace isolation
	other, err := eng.Recall(ctx, "how does auth work", "/ws/other", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("other workspace should be empty, got %d", len(other))
	}
	items, err := eng.List(ctx, "/ws/test", "", 10, 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("List = %v, %v", items, err)
	}
	st, err := eng.Stats(ctx, "/ws/test")
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 1 || st.Layers["L2_semantic"] != 1 {
		t.Fatalf("Stats = %+v", st)
	}
	if err := eng.RecordEvent(ctx, "/ws/test", "woca", "run", "test", "success"); err != nil {
		t.Fatal(err)
	}
	if err := eng.Forget(ctx, id); err != nil {
		t.Fatal(err)
	}
	st2, _ := eng.Stats(ctx, "/ws/test")
	if st2.Total != 1 { // the episode remains
		t.Fatalf("after Forget: %+v", st2)
	}
}

func TestResolveEmbeddingInherit(t *testing.T) {
	// inherit + openai config present -> openai provider with inherited values
	p, m, u, k := resolveEmbedding(BuildInput{
		Embedding: "inherit", OpenAIKey: "sk-1", OpenAIBaseURL: "https://api.example.com",
		EmbeddingModel: "", EmbeddingBaseURL: "", EmbeddingAPIKey: "",
	})
	if p != "openai" || m != "text-embedding-3-small" || u != "https://api.example.com" || k != "sk-1" {
		t.Fatalf("inherit/openai = %q %q %q %q", p, m, u, k)
	}
	// inherit + nothing configured -> hashing fallback
	p, _, _, _ = resolveEmbedding(BuildInput{Embedding: "inherit"})
	if p != "hashing" {
		t.Fatalf("inherit/none = %q, want hashing", p)
	}
	// explicit embedding key wins over inherit
	_, _, _, k = resolveEmbedding(BuildInput{
		Embedding: "openai", OpenAIKey: "sk-old", EmbeddingAPIKey: "sk-new",
	})
	if k != "sk-new" {
		t.Fatalf("explicit key should win, got %q", k)
	}
}

func TestDescriptionMentionsDB(t *testing.T) {
	in := testInput(t)
	eng, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if !strings.Contains(eng.Description(), in.DBPath) {
		t.Fatalf("Description = %q", eng.Description())
	}
}
```

- [ ] **Step 3: 跑测试确认失败** — `go test ./internal/memory/ -v`，预期编译失败（Build/BuildInput 未定义）。

- [ ] **Step 4: 适配器** — 新建 `internal/memory/ladym.go`：

```go
package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ProjAnvil/LadyM/adapter"
	ladymcfg "github.com/ProjAnvil/LadyM/config"
	ladymfacade "github.com/ProjAnvil/LadyM/ladym"
	ladymengine "github.com/ProjAnvil/LadyM/engine"
	ladymops "github.com/ProjAnvil/LadyM/operations"
	ladymschema "github.com/ProjAnvil/LadyM/schema"

	"github.com/projanvil/langchain-golang/core/language"
)

// embedKeyEnv is the environment variable ladyM's built-in openai embedding
// provider reads its API key from. woca owns the process, so Build sets it
// from its own resolved secrets instead of ladyM's secret store.
const embedKeyEnv = "WOCA_LADYM_EMBED_KEY"

// BuildInput carries everything Build needs, resolved by the caller (main).
// Strings mirror the settings keys of the same names.
type BuildInput struct {
	EngineEnabled bool
	DBPath        string // absolute; "~" already expanded
	// DefaultWorkspace is the engine's fallback workspace (the workdir).
	DefaultWorkspace string
	// Embedding: inherit | hashing | openai | ollama | http.
	Embedding string
	// EmbeddingModel/BaseURL/APIKey override the inherit chain when set.
	EmbeddingModel   string
	EmbeddingBaseURL string
	EmbeddingAPIKey  string
	// LLM: inherit | none. ChatModel is woca's active model (inject for
	// consolidate/L5/L6 when LLM=inherit); nil means none.
	LLM       string
	ChatModel language.ChatModel
	// OpenAIKey/OpenAIBaseURL feed the "inherit" embedding resolution.
	OpenAIKey      string
	OpenAIBaseURL  string
}

// DefaultDBPath returns ~/.woca/ladym.db (empty string on home-dir failure).
func DefaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".woca", "ladym.db")
}

// ExpandHome resolves a leading ~ in a user-supplied db path.
func ExpandHome(p string) string {
	if p == "~" || len(p) >= 2 && p[0] == '~' && p[1] == filepath.Separator {
		home, err := os.UserHomeDir()
		if err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// resolveEmbedding maps the settings values to a concrete ladyM embedding
// provider config: inherit falls back to woca's OpenAI-compatible setup when
// present, else to the offline hashing provider.
func resolveEmbedding(in BuildInput) (provider, model, baseURL, key string) {
	orDefault := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	switch in.Embedding {
	case LadyMEmbeddingHashing, LadyMEmbeddingOllama, LadyMEmbeddingHTTP:
		return in.Embedding, in.EmbeddingModel, in.EmbeddingBaseURL, in.EmbeddingAPIKey
	case LadyMEmbeddingOpenAI:
		return "openai",
			orDefault(in.EmbeddingModel, "text-embedding-3-small"),
			orDefault(in.EmbeddingBaseURL, in.OpenAIBaseURL),
			orDefault(in.EmbeddingAPIKey, in.OpenAIKey)
	default: // inherit
		if in.OpenAIKey != "" || in.OpenAIBaseURL != "" || in.EmbeddingAPIKey != "" {
			return "openai",
				orDefault(in.EmbeddingModel, "text-embedding-3-small"),
				orDefault(in.EmbeddingBaseURL, in.OpenAIBaseURL),
				orDefault(in.EmbeddingAPIKey, in.OpenAIKey)
		}
		return "hashing", "", "", ""
	}
}

// Build constructs the in-process ladyM engine. The config is assembled in
// code (no TOML / LADYM_* env chain) so woca stays the single source of
// truth for db path, workspace, and providers.
func Build(in BuildInput) (*LadyM, error) {
	if !in.EngineEnabled {
		return nil, fmt.Errorf("memory: engine disabled")
	}
	if in.DBPath == "" {
		return nil, fmt.Errorf("memory: db path is required")
	}
	provider, model, baseURL, key := resolveEmbedding(in)

	cfg := ladymcfg.Default()
	cfg.DBPath = in.DBPath
	cfg.Workspace = in.DefaultWorkspace
	cfg.EnableWAL = true
	// woca-managed re-embedding: switching embedding providers rebuilds the
	// vector index in place at engine open (spec §5.3).
	cfg.EmbeddingAllowDimChange = true
	cfg.EmbeddingProvider = provider
	cfg.EmbeddingModel = model
	cfg.EmbeddingBaseURL = baseURL
	cfg.EmbeddingAPIKeyEnv = embedKeyEnv
	if key != "" {
		os.Setenv(embedKeyEnv, key)
	}
	// The LLM is injected per-op below (host model), never resolved by
	// ladyM's own provider factories.
	cfg.LLMProvider = "none"

	var (
		eng *ladymengine.Engine
		err error
	)
	if in.LLM == LadyMLLMInherit && in.ChatModel != nil {
		eng, err = ladymfacade.NewEngineWithModels(cfg, &adapter.ModelRouting{
			Consolidate: adapter.WrapChatModel(in.ChatModel, "function_calling"),
		})
	} else {
		eng, err = ladymfacade.NewEngine(cfg)
	}
	if err != nil {
		return nil, fmt.Errorf("memory: build ladyM engine: %w", err)
	}
	return &LadyM{eng: eng, path: in.DBPath}, nil
}

// LadyM implements Engine over an in-process ladyM engine.
type LadyM struct {
	eng  *ladymengine.Engine
	path string
}

func (l *LadyM) Recall(ctx context.Context, query, workspace string, topK int) ([]Hit, error) {
	if topK <= 0 {
		topK = 6
	}
	resp, err := l.eng.Recall(query, workspace, topK, nil, nil, 0)
	if err != nil {
		return nil, err
	}
	return hitsFromRecall(resp), nil
}

func (l *LadyM) SearchCode(ctx context.Context, query, workspace string, topK int) ([]Hit, error) {
	if topK <= 0 {
		topK = 8
	}
	resp, err := l.eng.SearchCode(query, topK, workspace)
	if err != nil {
		return nil, err
	}
	return hitsFromRecall(resp), nil
}

func (l *LadyM) Remember(ctx context.Context, workspace, content string, tags []string, source string) (string, error) {
	if workspace == "" {
		workspace = l.eng.Config.Workspace
	}
	m, err := l.eng.Remember(content, ladymschema.LayerSemantic, ladymschema.TypeFact, tags, map[string]any{"workspace": workspace}, source, "")
	if err != nil {
		return "", err
	}
	return m.ID, nil
}

func (l *LadyM) RecordEvent(ctx context.Context, workspace, agentName, action, observation, outcome string) error {
	if workspace == "" {
		workspace = l.eng.Config.Workspace
	}
	_, err := l.eng.RecordEvent(agentName, action, observation, outcome, nil, nil)
	return err
}

func (l *LadyM) List(ctx context.Context, workspace, layer string, limit, offset int) ([]Hit, error) {
	if limit <= 0 {
		limit = 20
	}
	var layerPtr *ladymschema.Layer
	if layer != "" {
		l := ladymschema.Layer(layer)
		layerPtr = &l
	}
	items, err := l.eng.List(workspace, layerPtr, limit, offset)
	if err != nil {
		return nil, err
	}
	out := make([]Hit, 0, len(items))
	for _, m := range items {
		out = append(out, hitFromMemory(m, 0))
	}
	return out, nil
}

func (l *LadyM) Forget(ctx context.Context, id string) error {
	return l.eng.Forget(id)
}

func (l *LadyM) Stats(ctx context.Context, workspace string) (*Stats, error) {
	st, err := l.eng.StatsFor(workspace)
	if err != nil {
		return nil, err
	}
	return &Stats{Total: st.TotalMemories, Layers: st.ByLayer}, nil
}

func (l *LadyM) IndexCode(ctx context.Context, root, workspace string, force bool) (string, error) {
	rep, err := l.eng.IndexCode(root, force, workspace, nil)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("index report: %+v", rep), nil
}

func (l *LadyM) Consolidate(ctx context.Context, workspace string) (string, error) {
	rep, err := ladymops.RunSystem2Cycle(l.eng, workspace)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("consolidate=%+v proceduralize=%+v skipped_llm_steps=%v decay=%+v",
		rep.Consolidate, rep.Proceduralize, rep.SkippedLLMSteps, rep.Decay), nil
}

func (l *LadyM) Description() string {
	return "ladym " + l.path
}

func (l *LadyM) Close() error { return l.eng.Close() }

func hitFromMemory(m *ladymschema.Memory, score float64) Hit {
	summary := m.Summary
	if summary == "" {
		summary = m.Content
	}
	return Hit{
		ID:        m.ID,
		Layer:     string(m.Layer),
		Type:      string(m.Type),
		Summary:   summary,
		Content:   m.Content,
		Source:    m.Source,
		Tags:      m.Tags,
		Score:     score,
		UpdatedAt: m.UpdatedAt,
	}
}

func hitsFromRecall(resp *ladymschema.RecallResponse) []Hit {
	out := make([]Hit, 0, len(resp.Results))
	for _, r := range resp.Results {
		h := hitFromMemory(&r.Memory, r.Score)
		out = append(out, h)
	}
	return out
}
```

编译注意：`ladymcfg`/`adapter`/`ladymfacade` 若有未用 import（如 context 在 ladym.go 未用）以编译器提示为准修剪——`ctx` 参数在部分方法里刻意不用（接口对齐），方法签名保留。

- [ ] **Step 5: 跑测试** — `go test ./internal/memory/ -v`，预期全 PASS（hermetic：hashing embedding、tmp db、LLM none）。

- [ ] **Step 6: Commit** — `git add internal/memory/ && git commit -m "feat(memory): internal/memory package with ladyM engine adapter"`

---

## Task 5（woca）：agent 读路径 —— prefetch 走 Recall + workspace 绑定

spec §4.1/§4.2。

**Files:**
- Modify: `internal/agent/agent.go`
- Modify: `internal/agent/memorystore.go`
- Create: `internal/agent/memoryengine_test.go`

- [ ] **Step 1: 写失败测试** — 新建 `internal/agent/memoryengine_test.go`（package agent，可用裸 `&Agent{}` 构造，参考同包其他测试）：

```go
package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/settings"
)

// fakeEngine is a minimal memory.Engine for agent-side tests.
type fakeEngine struct {
	mu        sync.Mutex
	recalls   []string
	remembered []string
	events    []string
	hits      []memory.Hit
}

func (f *fakeEngine) Recall(ctx context.Context, query, workspace string, topK int) ([]memory.Hit, error) {
	f.mu.Lock()
	f.recalls = append(f.recalls, workspace+"|"+query)
	f.mu.Unlock()
	return f.hits, nil
}
func (f *fakeEngine) SearchCode(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	return nil, nil
}
func (f *fakeEngine) Remember(ctx context.Context, ws, content string, tags []string, src string) (string, error) {
	f.mu.Lock()
	f.remembered = append(f.remembered, content)
	f.mu.Unlock()
	return "id-1", nil
}
func (f *fakeEngine) RecordEvent(ctx context.Context, ws, agentName, action, observation, outcome string) error {
	f.mu.Lock()
	f.events = append(f.events, agentName+"/"+action+"/"+outcome)
	f.mu.Unlock()
	return nil
}
func (f *fakeEngine) List(ctx context.Context, ws, layer string, limit, offset int) ([]memory.Hit, error) {
	return nil, nil
}
func (f *fakeEngine) Forget(ctx context.Context, id string) error { return nil }
func (f *fakeEngine) Stats(ctx context.Context, ws string) (*memory.Stats, error) {
	return &memory.Stats{Total: 0, Layers: map[string]int{}}, nil
}
func (f *fakeEngine) IndexCode(ctx context.Context, root, ws string, force bool) (string, error) {
	return "", nil
}
func (f *fakeEngine) Consolidate(ctx context.Context, ws string) (string, error) {
	return "", nil
}
func (f *fakeEngine) Description() string { return "fake" }
func (f *fakeEngine) Close() error        { return nil }

func TestPrefetchUsesEngineRecall(t *testing.T) {
	f := &fakeEngine{hits: []memory.Hit{{
		ID: "h1", Layer: "L2_semantic", Type: "fact",
		Summary: "auth uses JWT", Content: "auth uses JWT with 24h expiry",
		Score: 0.83, UpdatedAt: 1,
	}}}
	a := &Agent{memEngine: f, workdir: "/ws"}
	a.SetMemoryWorkspace("/ws/custom")
	a.beginMemoryPrefetch(context.Background(), []messages.Message{messages.Human("how does auth work?")})
	// prefetch is async: poll until the attachment is available
	var got []messages.Message
	for i := 0; i < 50 && got == nil; i++ {
		got = a.takeMemoryAttachments()
		if got == nil {
			timeSleepMs(10)
		}
	}
	if len(got) != 1 {
		t.Fatalf("attachments = %d, want 1", len(got))
	}
	body := got[0].Content
	if !strings.Contains(body, "<relevant-memory>") || !strings.Contains(body, "[L2_semantic|fact|0.83]") {
		t.Fatalf("attachment format: %q", body)
	}
	if len(f.recalls) != 1 || f.recalls[0] != "/ws/custom|how does auth work?" {
		t.Fatalf("recalls = %v", f.recalls)
	}
	// second consume: dedup by id
	if more := a.takeMemoryAttachments(); len(more) != 0 {
		t.Fatalf("dedup failed: %v", more)
	}
}

func TestMemoryWorkspaceDefaultsToWorkdir(t *testing.T) {
	a := &Agent{workdir: "/ws/default"}
	if ws := a.memoryWorkspace(); ws != "/ws/default" {
		t.Fatalf("workspace = %q", ws)
	}
	a.SetMemoryWorkspace("/ws/override")
	if ws := a.memoryWorkspace(); ws != "/ws/override" {
		t.Fatalf("workspace = %q", ws)
	}
}

func timeSleepMs(ms int) { timeSleep(timeDuration(ms)) }
```

`timeSleepMs` 两个辅助写成文件底部的小函数（避免每个测试文件重复 import time）：

```go
func timeSleep(d int64)          { /* time.Sleep(time.Duration(d) * time.Millisecond) */ }
func timeDuration(ms int) int64  { return int64(ms) }
```

（更直接的做法：在测试文件顶部 `import "time"`，把 `timeSleepMs(10)` 写成 `time.Sleep(10 * time.Millisecond)` 并删掉辅助函数——执行者二选一，保持文件能编译即可。）

- [ ] **Step 2: 跑测试确认失败** — `go test ./internal/agent/ -run TestPrefetchUsesEngine -v`，预期编译失败（memEngine 字段未定义）。

- [ ] **Step 3: agent.go 改动**：

(a) import 区追加 `"github.com/projanvil/woca/internal/memory"` 与 `"github.com/projanvil/woca/internal/settings"`。

(b) `Config` 在 `Memory` 字段之后追加：

```go
	// MemoryEngine is the factual-memory engine (ladyM). When set, the
	// per-turn prefetch runs semantic Recall instead of the KV keyword
	// match, and memory tools are registered. Nil disables the subsystem.
	MemoryEngine memory.Engine
	// LoadSettings re-reads persisted settings per run (nil-safe to omit in
	// tests); the memory paths read ladym-topk / ladym-autoretain /
	// ladym-system2 live.
	LoadSettings func() settings.Settings
```

(c) `Agent` 结构体在 `memPrefetch` 字段附近追加：

```go
	// memEngine is the factual-memory engine (nil = disabled); swapped
	// whole-pointer on settings rebuild (SetMemoryEngine).
	memEngine memory.Engine
	// memWorkspace is the current run's memory workspace (defaults to
	// workdir); set per run by the harnesses alongside SetObsRun.
	memWorkspace string
	// loadSettings re-reads settings for per-run knobs (nil = defaults).
	loadSettings func() settings.Settings
```

(d) `New()` 的 `a := &Agent{...}` 字面量追加：`memEngine: cfg.MemoryEngine, loadSettings: cfg.LoadSettings,`。

(e) `SetObsRun` 之后追加方法：

```go
// SetMemoryWorkspace scopes subsequent memory-engine calls to one workspace.
// Harnesses call it at run start (web startRun; the TUI defaults to workdir).
func (a *Agent) SetMemoryWorkspace(ws string) {
	if ws == "" {
		return
	}
	a.mu.Lock()
	a.memWorkspace = ws
	a.mu.Unlock()
}

func (a *Agent) memoryWorkspace() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.memWorkspace != "" {
		return a.memWorkspace
	}
	return a.workdir
}

// MemoryEngine returns the current memory engine (nil = disabled).
func (a *Agent) MemoryEngine() memory.Engine {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.memEngine
}

// SetMemoryEngine swaps the memory engine whole-pointer (settings rebuild).
// In-flight calls keep the old engine; the caller delays Close (spec §5.4).
func (a *Agent) SetMemoryEngine(m memory.Engine) {
	a.mu.Lock()
	a.memEngine = m
	a.mu.Unlock()
}
```

- [ ] **Step 4: memorystore.go 改动**：

(a) import 区追加 `"github.com/projanvil/woca/internal/memory"`。

(b) `memoryPrefetch` 结构体追加字段：

```go
	engineHits []memory.Hit
```

(c) `beginMemoryPrefetch` 在 `if a.memory == nil` 块**之前**插入引擎分支：

```go
	if eng := a.MemoryEngine(); eng != nil {
		query := lastHumanQuery(msgs)
		if query == "" {
			return
		}
		topK := 6
		if a.loadSettings != nil {
			if k := a.loadSettings().LadyMTopK; k > 0 {
				topK = k
			}
		}
		ws := a.memoryWorkspace()
		go func() {
			hits, err := eng.Recall(context.WithoutCancel(ctx), query, ws, topK)
			if err != nil {
				return
			}
			a.memPrefetch.mu.Lock()
			a.memPrefetch.engineHits = hits
			a.memPrefetch.mu.Unlock()
		}()
		return
	}
```

(d) `takeMemoryAttachments` 在读取 `entries` 之后、KV 循环之前插入引擎消费（把函数开头取 entries 的模式扩展为同时取 engineHits）：

```go
	a.memPrefetch.mu.Lock()
	entries := a.memPrefetch.entries
	a.memPrefetch.entries = nil
	hits := a.memPrefetch.engineHits
	a.memPrefetch.engineHits = nil
	injected := a.memPrefetch.injected
	a.memPrefetch.mu.Unlock()
```

然后在 `var out []messages.Message` 之后、KV 循环之前插入：

```go
	for _, h := range hits {
		if len(out) >= maxMemoryAttachments {
			break
		}
		if injected[h.ID] {
			continue
		}
		injected[h.ID] = true
		summary := strings.TrimSpace(h.Summary)
		content := strings.TrimSpace(h.Content)
		if summary == "" && content == "" {
			continue
		}
		if len(content) > 2000 {
			content = content[:2000] + "…"
		}
		line := fmt.Sprintf("[%s|%s|%.2f] %s", h.Layer, h.Type, h.Score, summary)
		if content != "" && content != summary {
			line += "\n" + content
		}
		out = append(out, messages.Human(
			"<relevant-memory>\n"+line+"\n</relevant-memory>"))
	}
```

import 需补 `"fmt"`。函数尾部把 `injected` 写回的逻辑对 engine 路径同样生效（共用同一 map、同一 `injectedMemoryLimit`）。

(e) 文件底部追加辅助函数：

```go
// lastHumanQuery returns the last human message text (the recall query),
// clamped to 2000 characters. Unlike memoryTerms it keeps the natural
// language: semantic recall wants the sentence, not the keywords.
func lastHumanQuery(msgs []messages.Message) string {
	last := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == messages.RoleHuman {
			last = msgs[i].Content
			break
		}
	}
	if len(last) > 2000 {
		last = last[:2000]
	}
	return strings.TrimSpace(last)
}
```

- [ ] **Step 5: 跑测试** — `go test ./internal/agent/ -v`，预期全 PASS（新增 2 个 + 原有）。

- [ ] **Step 6: Commit** — `git add internal/agent/ && git commit -m "feat(agent): route memory prefetch through the memory engine with per-run workspace"`

---

## Task 6（woca）：main.go 启动注入 + 引擎热重建 + serve 接线

spec §2/§5.4。`buildAgent` 在进程启动构建引擎（失败降级 nil），`rebuildMemory` 供 settings 变更换引擎。

**Files:**
- Modify: `cmd/woca/main.go`

- [ ] **Step 1: app 结构体与引擎构建** — `app` 结构体追加字段：

```go
	memEngine memory.Engine
```

import 区补 `"github.com/projanvil/woca/internal/memory"`。

- [ ] **Step 2: buildMemoryEngine helper** — 放在 `buildAgent` 之后：

```go
// buildMemoryEngine constructs the memory engine from current settings.
// engine=none returns (nil, nil); build failures degrade to nil (logged by
// the caller) so woca always starts (spec §10).
func buildMemoryEngine(ap *app, dbSettings settings.Settings, chatModel language.ChatModel) (memory.Engine, error) {
	if dbSettings.MemoryEngine == settings.MemoryEngineNone {
		return nil, nil
	}
	dbPath := memory.ExpandHome(dbSettings.LadyMDBPath)
	if dbPath == "" {
		dbPath = memory.DefaultDBPath()
	}
	in := memory.BuildInput{
		EngineEnabled:    true,
		DBPath:           dbPath,
		DefaultWorkspace: ap.cfg.WorkDir,
		Embedding:        dbSettings.LadyMEmbedding,
		EmbeddingModel:   dbSettings.LadyMEmbeddingModel,
		EmbeddingBaseURL: dbSettings.LadyMEmbeddingBaseURL,
		EmbeddingAPIKey:  ap.settings.GetSecret(settings.SecretLadyMEmbeddingAPIKey),
		LLM:              dbSettings.LadyMLLM,
		ChatModel:        chatModel,
		OpenAIKey:        ap.settings.GetSecret(settings.SecretOpenAIAPIKey),
		OpenAIBaseURL:    dbSettings.OpenAIBaseURL,
	}
	return memory.Build(in)
}
```

注意：`ap.settings` 为 nil（settings 库禁用）时 `GetSecret` 在 nil receiver 上安全（settings.Store 方法均有 nil 检查——若 `GetSecret` 没有 nil 检查，先在 settings.go 给它加上 `if s == nil { return "" }`，与 `HasSecret` 同款）。启动路径传入 buildAgent 局部变量 `model`（主模型）；`rebuildMemory` 传入 `ap.a.CurrentModel()`。

- [ ] **Step 3: rebuildMemory** — 放在 `rebuildModel` 之后：

```go
// rebuildMemory reconstructs the memory engine from live settings and swaps
// it into the agent (spec §5.4). The old engine closes after a grace period
// so in-flight prefetch/tool calls on it degrade to a logged error instead
// of racing a Close.
func (ap *app) rebuildMemory() error {
	dbSettings := ap.settings.Load()
	// rebuildMemory 的引擎重建调用处改为：
	eng, err := buildMemoryEngine(ap, dbSettings, ap.a.CurrentModel())
	if err != nil {
		return err
	}
	old := ap.memEngine
	ap.memEngine = eng
	ap.a.SetMemoryEngine(eng)
	if old != nil {
		go func() {
			time.Sleep(60 * time.Second)
			_ = old.Close()
		}()
	}
	return nil
}
```

import 补 `"time"`。

- [ ] **Step 4: buildAgent 接线** — 在 KV memory switch（`var memory *store.Store` 块）之后插入：

```go
	// Factual-memory engine (ladyM): startup build degrades to nil on
	// failure so the harness always comes up; settings changes rebuild it
	// live via app.rebuildMemory.
	memEngine, memErr := buildMemoryEngine(&app{cfg: cfg, settings: st}, dbSettings, model)
	if memErr != nil {
		log.Warn().Err(memErr).Msg("memory engine unavailable; continuing without memory")
		memEngine = nil
	}
```

`agent.Config{...}` 字面量追加：

```go
		MemoryEngine: memEngine,
		LoadSettings: st.Load,
```

（`st` 为 nil 时 `st.Load` method value 仍然安全：`Load` 有 nil-receiver 分支返回 Default。若编译器拒绝 nil 指针 method value，改为 `func() settings.Settings { return st.Load() }`。）

agent 构建后追加默认 workspace 绑定：

```go
	a.SetMemoryWorkspace(cfg.WorkDir)
```

`app` 返回值追加 `memEngine: memEngine`。

- [ ] **Step 5: serve() 接线** — `server.New(server.Config{...})` 追加两个字段（`Memory` KV 字段保留不动，P2 再处理）：

```go
		MemoryEngine: ap.memEngine,
		RebuildMemory: func() error { return ap.rebuildMemory() },
```

（`server.Config` 尚无这两个字段——本 Step 先在 `internal/server/server.go` 的 `Config` 加上字段声明与 `Server` 字段镜像，**路由与 handler 留到 Task 11**：）

```go
	// MemoryEngine is the factual-memory engine backing /api/memory (nil
	// disables the memory API).
	MemoryEngine memory.Engine
	// RebuildMemory rebuilds the memory engine after memory settings changes
	// submitted through /api/settings (optional).
	RebuildMemory func() error
```

`Server` 结构体追加 `memEngine memory.Engine`、`rebuildMemory func() error`；`New()` 装配。

同时检查 `internal/webgui`：`grep -rn 'Memory' internal/webgui/`——若 webgui.Config 也有 Memory 透传字段，镜像加 `MemoryEngine`（webgui.New 在 main.go `/web` 命令路径构建 server.Config，两处调用点都要传 `MemoryEngine: ap.memEngine`）。

- [ ] **Step 6: 构建与测试** — `go build ./... && go test ./...`，预期全 PASS。

- [ ] **Step 7: Commit** — `git add cmd/ internal/server/ internal/webgui/ && git commit -m "feat(main): wire the memory engine at startup with live rebuild"`

---

## Task 7（woca）：P0 冒烟验证

- [ ] **Step 1: 构建冒烟**：

```bash
go build -o /tmp/woca-p0 ./cmd/woca
WOCA_DB= /tmp/woca-p0 --prompt "say hi" 2>&1 | tail -5
```

预期：正常输出 hi（无 memory 相关报错）；`ls ~/.woca/ladym.db` 存在（引擎默认 ladym，首次启动即建库）。

- [ ] **Step 2: 降级冒烟**：

```bash
WOCA_DB= /tmp/woca-p0 --prompt "say hi" --db none 2>&1 | tail -3
```

预期：正常输出（settings 禁用时 memEngine=nil 路径不崩）。

- [ ] **Step 3: 无 commit**（验证型任务）。

---

## Task 8（woca）：4 个记忆工具 + subagent 只读过滤

spec §4.4/§6。工具在 agent 层注册（非 registry），调用时经 `a.MemoryEngine()` 取当前引擎（热重建后仍指向新引擎）。

**Files:**
- Create: `internal/agent/memorytools.go`
- Create: `internal/agent/memorytools_test.go`
- Modify: `internal/agent/agent.go`（New 装配 + newSubagent 继承）
- Modify: `internal/tools/registry.go`（categoryByName）

- [ ] **Step 1: 写失败测试** — 新建 `internal/agent/memorytools_test.go`：

```go
package agent

import (
	"context"
	"strings"
	"testing"

	coretools "github.com/projanvil/langchain-golang/core/tools"
)

func toolsByName(list []coretools.Tool) map[string]coretools.Tool {
	out := map[string]coretools.Tool{}
	for _, t := range list {
		out[t.Name()] = t
	}
	return out
}

func TestMemoryToolsMainIncludesWrite(t *testing.T) {
	a := &Agent{memEngine: &fakeEngine{}, workdir: "/ws"}
	got := toolsByName(a.memoryTools())
	for _, name := range []string{"recall_memory", "search_code", "remember_fact", "index_code"} {
		if got[name] == nil {
			t.Fatalf("missing tool %s", name)
		}
	}
}

func TestMemoryToolsSubagentReadOnly(t *testing.T) {
	a := &Agent{memEngine: &fakeEngine{}, workdir: "/ws", subagent: true}
	got := toolsByName(a.memoryTools())
	if got["recall_memory"] == nil || got["search_code"] == nil {
		t.Fatal("subagent should keep read tools")
	}
	if got["remember_fact"] != nil || got["index_code"] != nil {
		t.Fatal("subagent must not get write tools")
	}
}

func TestMemoryToolsNilEngine(t *testing.T) {
	a := &Agent{workdir: "/ws"}
	if list := a.memoryTools(); len(list) != 0 {
		t.Fatalf("nil engine should yield no tools, got %d", len(list))
	}
}

func TestRecallMemoryToolInvokesEngine(t *testing.T) {
	f := &fakeEngine{hits: []memory.Hit{{ID: "h1", Layer: "L2_semantic", Type: "fact", Summary: "JWT", Score: 0.9}}}
	a := &Agent{memEngine: f, workdir: "/ws"}
	tool := toolsByName(a.memoryTools())["recall_memory"]
	res, err := tool.Invoke(context.Background(), map[string]any{"query": "auth"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "[L2_semantic|fact|0.90] JWT") {
		t.Fatalf("result = %q", res.Content)
	}
	if len(f.recalls) == 0 {
		t.Fatal("engine not called")
	}
}
```

顶部补 import `"github.com/projanvil/woca/internal/memory"`（fakeEngine 复用 Task 5 的 memoryengine_test.go）。

- [ ] **Step 2: 跑测试确认失败** — `go test ./internal/agent/ -run TestMemoryTool -v`，预期编译失败。

- [ ] **Step 3: 实现 memorytools.go**：

```go
package agent

import (
	"context"
	"fmt"
	"strings"

	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/memory"
)

// memoryTools builds the memory-facing tool set. Subagents get the read-only
// subset (spec §6: reads shared, writes governed by the main agent).
func (a *Agent) memoryTools() []coretools.Tool {
	eng := a.MemoryEngine()
	if eng == nil {
		return nil
	}
	readonly := a.subagent
	ws := func() string { return a.memoryWorkspace() }

	type recallArgs struct {
		Query string `json:"query"`
		TopK  int    `json:"top_k,omitempty"`
	}
	type rememberArgs struct {
		Content string   `json:"content"`
		Tags    []string `json:"tags,omitempty"`
	}
	type indexArgs struct {
		Root  string `json:"root,omitempty"`
		Force bool   `json:"force,omitempty"`
	}

	var out []coretools.Tool

	recall, err := coretools.FromFunc("recall_memory",
		"Retrieve relevant long-term memories (facts, decisions, insights, predictions, playbooks).\n\n"+
			"Call BEFORE answering when prior context, past decisions, or how-to knowledge may be relevant. "+
			"One line per hit, or \"(no hits)\".",
		func(ctx context.Context, in recallArgs) (coretools.Result, error) {
			e := a.MemoryEngine()
			if e == nil {
				return coretools.Result{Content: "memory disabled"}, nil
			}
			hits, err := e.Recall(ctx, in.Query, ws(), in.TopK)
			if err != nil {
				return coretools.Result{}, err
			}
			return coretools.Result{Content: formatHits(hits)}, nil
		})
	if err != nil {
		return nil
	}
	out = append(out, recall)

	search, err := coretools.FromFunc("search_code",
		"Search indexed source symbols/files. Use for 'how does X work in the codebase' "+
			"against previously indexed source (index_code).",
		func(ctx context.Context, in recallArgs) (coretools.Result, error) {
			e := a.MemoryEngine()
			if e == nil {
				return coretools.Result{Content: "memory disabled"}, nil
			}
			hits, err := e.SearchCode(ctx, in.Query, ws(), in.TopK)
			if err != nil {
				return coretools.Result{}, err
			}
			return coretools.Result{Content: formatHits(hits)}, nil
		})
	if err != nil {
		return nil
	}
	out = append(out, search)

	if !readonly {
		remember, err := coretools.FromFunc("remember_fact",
			"Persist a durable fact worth recalling later (user preference, key decision, verified answer). "+
				"Do NOT store ephemeral/transactional state.\n\n"+
				"Subject to the attention gate: low-value or duplicate content may be dropped automatically.",
			func(ctx context.Context, in rememberArgs) (coretools.Result, error) {
				e := a.MemoryEngine()
				if e == nil {
					return coretools.Result{Content: "memory disabled"}, nil
				}
				id, err := e.Remember(ctx, ws(), in.Content, in.Tags, "woca:tool")
				if err != nil {
					return coretools.Result{}, err
				}
				return coretools.Result{Content: fmt.Sprintf("stored id=%s", id)}, nil
			})
		if err != nil {
			return nil
		}
		out = append(out, remember)

		index, err := coretools.FromFunc("index_code",
			"Index a source root into memory (tree-sitter symbols + cross-refs) so search_code can find them. "+
				"force=true reindexes unchanged files too. Incremental by default.",
			func(ctx context.Context, in indexArgs) (coretools.Result, error) {
				e := a.MemoryEngine()
				if e == nil {
					return coretools.Result{Content: "memory disabled"}, nil
				}
				root := in.Root
				if root == "" {
					root = a.workdir
				}
				report, err := e.IndexCode(ctx, root, ws(), in.Force)
				if err != nil {
					return coretools.Result{}, err
				}
				return coretools.Result{Content: report}, nil
			})
		if err != nil {
			return nil
		}
		out = append(out, index)
	}
	return out, nil
}

func formatHits(hits []memory.Hit) string {
	if len(hits) == 0 {
		return "(no hits)"
	}
	lines := make([]string, 0, len(hits))
	for _, h := range hits {
		lines = append(lines, fmt.Sprintf("[%s|%s|%.2f] %s", h.Layer, h.Type, h.Score, strings.TrimSpace(h.Summary)))
	}
	return strings.Join(lines, "\n")
}
```

- [ ] **Step 4: agent.go 装配** — `New()` 工具组装：

非 subagent 分支（`a.toolList = append(a.toolList, a.goalTools()...)` 之后、`cfg.ExtraTools` 之前）：

```go
		a.toolList = append(a.toolList, a.memoryTools()...)
```

subagent 分支（`a.toolList = filterTools(base, ...)` 之后）：

```go
		a.toolList = append(a.toolList, a.memoryTools()...)
```

（两处同句：`memoryTools` 内部按 `a.subagent` 裁剪；`a.subagent` 在结构体字面量赋值时已就位。）

`newSubagent` 的 `Config{...}` 追加：

```go
		MemoryEngine: a.memEngine,
		LoadSettings: a.loadSettings,
```

并在 `New(...)` 返回后追加 workspace 继承：

```go
	sub.SetMemoryWorkspace(a.memoryWorkspace())
```

- [ ] **Step 5: registry.go 权限分类** — `categoryByName` 追加：

```go
	"recall_memory": CatRead,
	"search_code":   CatRead,
	"remember_fact": CatWrite,
	"index_code":    CatWrite,
```

- [ ] **Step 6: 跑测试** — `go test ./internal/agent/ ./internal/tools/ -v`，预期全 PASS。

- [ ] **Step 7: Commit** — `git add internal/agent/ internal/tools/ && git commit -m "feat(agent): memory tools (recall/search/remember/index) with read-only subagent subset"`

---

## Task 9（woca）：runner Done —— auto-retain 写路径

spec §4.3。单一 choke point：`runner.Run` 的 defer + named return（覆盖所有 return 路径）；subagent 不经 runner 天然不触发。

**Files:**
- Create: `internal/agent/retain.go`
- Create: `internal/agent/retain_test.go`
- Modify: `internal/runner/runner.go`

- [ ] **Step 1: 写失败测试** — 新建 `internal/agent/retain_test.go`：

```go
package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/settings"
)

func TestRetainRunStoresFactAndEvent(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{
		memEngine: f,
		workdir:   "/ws",
		loadSettings: func() settings.Settings {
			s := settings.Default()
			s.LadyMAutoRetain = true
			return s
		},
	}
	history := []messages.Message{messages.Human("fix the login bug")}
	final := []messages.Message{messages.AI("fixed by rotating the JWT secret")}
	a.RetainRun(context.Background(), history, final, nil)

	if len(f.remembered) != 1 {
		t.Fatalf("remembered = %v", f.remembered)
	}
	if !strings.Contains(f.remembered[0], "Q: fix the login bug") ||
		!strings.Contains(f.remembered[0], "A: fixed by rotating") {
		t.Fatalf("content = %q", f.remembered[0])
	}
	if len(f.events) != 1 || !strings.Contains(f.events[0], "woca/run/success") {
		t.Fatalf("events = %v", f.events)
	}
}

func TestRetainRunDisabledOrEmpty(t *testing.T) {
	f := &fakeEngine{}
	off := settings.Default()
	off.LadyMAutoRetain = false
	a := &Agent{
		memEngine:    f,
		workdir:      "/ws",
		loadSettings: func() settings.Settings { return off },
	}
	a.RetainRun(context.Background(), []messages.Message{messages.Human("q")},
		[]messages.Message{messages.AI("a")}, nil)
	if len(f.remembered) != 0 || len(f.events) != 0 {
		t.Fatal("autoretain off should write nothing")
	}
	// empty final answer: remember skipped, event still recorded? -> spec:
	// fact needs a non-empty answer; the episode records the run outcome.
	on := settings.Default()
	on.LadyMAutoRetain = true
	a2 := &Agent{memEngine: f, workdir: "/ws", loadSettings: func() settings.Settings { return on }}
	a2.RetainRun(context.Background(), []messages.Message{messages.Human("q")}, nil, nil)
	if len(f.remembered) != 0 {
		t.Fatal("empty answer should not Remember")
	}
	if len(f.events) != 1 {
		t.Fatal("episode should still record")
	}
}
```

- [ ] **Step 2: 跑测试确认失败** — `go test ./internal/agent/ -run TestRetainRun -v`，预期编译失败。

- [ ] **Step 3: 实现 retain.go**：

```go
package agent

import (
	"context"
	"fmt"

	"github.com/projanvil/langchain-golang/core/messages"
)

// RetainRun stores a completed run into long-term memory: the Q/A pair as an
// L2 fact (ladyM's attention gate dedups noise) and the run outcome as an L1
// episode (food for System 2). It is the single write choke point, called by
// the runner when a run finishes; subagents never pass through the runner.
// P3 extends this with the opportunistic System 2 trigger (spec §4.5).
func (a *Agent) RetainRun(ctx context.Context, history, final []messages.Message, runErr error) {
	eng := a.MemoryEngine()
	if eng == nil || a.subagent {
		return
	}
	auto := true
	if a.loadSettings != nil {
		auto = a.loadSettings().LadyMAutoRetain
	}
	if !auto {
		return
	}
	ws := a.memoryWorkspace()
	question := firstHumanText(history)
	outcome := "success"
	if runErr != nil {
		outcome = "error"
	}
	if question != "" {
		obs := question
		if len(obs) > 500 {
			obs = obs[:500]
		}
		_ = eng.RecordEvent(ctx, ws, "woca", "run", obs, outcome)
		answer := lastAIText(final)
		if answer != "" {
			q, ans := question, answer
			if len(q) > 2000 {
				q = q[:2000]
			}
			if len(ans) > 4000 {
				ans = ans[:4000]
			}
			_, _ = eng.Remember(ctx, ws, fmt.Sprintf("Q: %s\nA: %s", q, ans),
				[]string{"woca", "auto"}, "woca:auto")
		}
	}
}

// firstHumanText returns the first human message of the run (the request).
func firstHumanText(msgs []messages.Message) string {
	for _, m := range msgs {
		if m.Role == messages.RoleHuman {
			return m.Content
		}
	}
	return ""
}

// lastAIText returns the last non-empty AI message of the run (the answer).
func lastAIText(msgs []messages.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == messages.RoleAI && msgs[i].Content != "" {
			return msgs[i].Content
		}
	}
	return ""
}
```

- [ ] **Step 4: runner.go 挂 defer** — `Run` 签名改为 named return，函数体开头（`r.agent.ResetTurnCounter()` 之前）插入：

```go
	defer func() {
		r.agent.RetainRun(context.WithoutCancel(ctx), history, final, err)
	}()
```

签名：`func (r *Runner) Run(ctx context.Context, history []messages.Message) (final []messages.Message, err error) {`。

函数体内原有的 `return nil, err` / `return msgs, nil` / `return agent.MessagesFrom(step.Values), nil` 全部保持不变（named return 自动捕获）；唯一必须改的是 `return nil, fmt.Errorf(...)` 这类字面量——它们同样兼容，无需改动。

- [ ] **Step 5: 跑测试** — `go test ./internal/agent/ ./internal/runner/ -v`，预期全 PASS。

- [ ] **Step 6: Commit** — `git add internal/agent/ internal/runner/ && git commit -m "feat(runner): auto-retain run Q/A and outcome into memory at run end"`

---

## Task 10（woca）：console /memory 引擎子命令 + /settings 即时生效

spec §8。引擎在时 `/memory` 以引擎为主，KV 子命令收纳到 `kv` 前缀之下（向后保留）。

**Files:**
- Modify: `internal/console/console.go`（Deps）
- Modify: `internal/console/settings_cmd.go`（memoryCmd + settingsSet）
- Modify: `internal/slashcmds/slashcmds.go`（描述）
- Create: `internal/console/memory_cmd_test.go`

- [ ] **Step 1: 写失败测试** — 新建 `internal/console/memory_cmd_test.go`：

```go
package console

import (
	"context"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/memory"
)

type fakeMemEngine struct{ hits []memory.Hit }

func (f *fakeMemEngine) Recall(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	return f.hits, nil
}
func (f *fakeMemEngine) SearchCode(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	return nil, nil
}
func (f *fakeMemEngine) Remember(ctx context.Context, ws, c string, tags []string, src string) (string, error) {
	return "m1", nil
}
func (f *fakeMemEngine) RecordEvent(ctx context.Context, ws, a, act, obs, o string) error { return nil }
func (f *fakeMemEngine) List(ctx context.Context, ws, layer string, limit, off int) ([]memory.Hit, error) {
	return f.hits, nil
}
func (f *fakeMemEngine) Forget(ctx context.Context, id string) error { return nil }
func (f *fakeMemEngine) Stats(ctx context.Context, ws string) (*memory.Stats, error) {
	return &memory.Stats{Total: 3, Layers: map[string]int{"L2_semantic": 2, "L1_episodic": 1}}, nil
}
func (f *fakeMemEngine) IndexCode(ctx context.Context, root, ws string, force bool) (string, error) {
	return "indexed", nil
}
func (f *fakeMemEngine) Consolidate(ctx context.Context, ws string) (string, error) {
	return "cycle done", nil
}
func (f *fakeMemEngine) Description() string { return "ladym fake.db" }
func (f *fakeMemEngine) Close() error        { return nil }

func TestMemoryCmdEngineSubcommands(t *testing.T) {
	c := New(Deps{
		MemoryEngine: &fakeMemEngine{hits: []memory.Hit{{ID: "m1", Layer: "L2_semantic", Type: "fact", Summary: "JWT", Score: 0.8}}},
		MemoryWorkspace: func() string { return "/ws" },
	})
	res, err := c.Execute(context.Background(), []string{"memory", "recall", "auth"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "[L2_semantic|fact|0.80] JWT") {
		t.Fatalf("recall output = %q", res.Output)
	}
	res, err = c.Execute(context.Background(), []string{"memory", "stats"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "ladym fake.db") || !strings.Contains(res.Output, "L2_semantic") {
		t.Fatalf("stats output = %q", res.Output)
	}
	res, err = c.Execute(context.Background(), []string{"memory", "remember", "prefers tabs"})
	if err != nil || !strings.Contains(res.Output, "stored") {
		t.Fatalf("remember = %q, %v", res.Output, err)
	}
}
```

（`Console.Execute` 的调用方式以 `internal/console/console.go` 现有签名为准——若第一个参数不是 ctx，按现有测试的调用方式调整；关键是 Deps 新字段可注入。）

- [ ] **Step 2: 跑测试确认失败** — `go test ./internal/console/ -run TestMemoryCmd -v`，预期编译失败。

- [ ] **Step 3: console.go Deps** — `Memory` 字段之后追加：

```go
	// MemoryEngine is the factual-memory engine (/memory engine subcommands).
	// Nil falls /memory back to the KV memoryStore.
	MemoryEngine memory.Engine
	// MemoryWorkspace resolves the current memory workspace for /memory.
	MemoryWorkspace func() string
	// RebuildMemory rebuilds the memory engine after ladym settings changes
	// made via /settings (optional).
	RebuildMemory func() error
```

import 补 `"github.com/projanvil/woca/internal/memory"`。

- [ ] **Step 4: memoryCmd 重写（settings_cmd.go）** — 替换整个 `memoryCmd` 函数：

```go
// memoryCmd implements /memory. With the memory engine wired, the engine
// surface is primary; the legacy KV memoryStore lives under `kv`:
//
//	/memory                       recent engine memories (workspace-scoped)
//	/memory recall <query...>     semantic recall
//	/memory remember <text...>    store a fact (attention-gated)
//	/memory stats                 layer counts + db path
//	/memory forget <id>           delete one memory
//	/memory consolidate           run one System 2 cycle now
//	/memory index [path] [--force] index source into memory
//	/memory kv list|get|put|del … legacy KV memoryStore
func (c *Console) memoryCmd(ctx context.Context, args []string) (Result, error) {
	if eng := c.deps.MemoryEngine; eng != nil {
		return c.memoryEngineCmd(ctx, eng, args)
	}
	m := c.deps.Memory
	if m == nil {
		return Result{}, fmt.Errorf("memory is disabled (/settings set memory-engine ladym, or /settings set memory sqlite|memory|postgres and restart)")
	}
	if len(args) > 0 && args[0] == "kv" {
		args = args[1:]
	}
	return c.memoryKVCmd(ctx, m, args)
}

func (c *Console) memoryEngineCmd(ctx context.Context, eng memory.Engine, args []string) (Result, error) {
	if len(args) == 0 {
		args = []string{"list"}
	}
	ws := ""
	if c.deps.MemoryWorkspace != nil {
		ws = c.deps.MemoryWorkspace()
	}
	switch args[0] {
	case "list":
		hits, err := eng.List(ctx, ws, "", 20, 0)
		if err != nil {
			return Result{}, err
		}
		if len(hits) == 0 {
			return Result{Output: "no memories yet — they accumulate as you work\n"}, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "memory engine: %s (workspace %s)\n", eng.Description(), ws)
		for _, h := range hits {
			line := h.Summary
			if len(line) > 80 {
				line = line[:80] + "…"
			}
			fmt.Fprintf(&b, "  [%s|%s] %s\n", h.Layer, h.Type, line)
		}
		return Result{Output: b.String()}, nil
	case "recall":
		if len(args) < 2 {
			return Result{}, fmt.Errorf("usage: /memory recall <query...>")
		}
		hits, err := eng.Recall(ctx, strings.Join(args[1:], " "), ws, 8)
		if err != nil {
			return Result{}, err
		}
		if len(hits) == 0 {
			return Result{Output: "(no hits)\n"}, nil
		}
		var b strings.Builder
		for _, h := range hits {
			fmt.Fprintf(&b, "[%.2f|%s|%s] %s\n", h.Score, h.Layer, h.Type, h.Summary)
		}
		return Result{Output: b.String()}, nil
	case "remember":
		if len(args) < 2 {
			return Result{}, fmt.Errorf("usage: /memory remember <content...>")
		}
		id, err := eng.Remember(ctx, ws, strings.Join(args[1:], " "), []string{"manual"}, "woca:cmd")
		if err != nil {
			return Result{}, err
		}
		return Result{Output: fmt.Sprintf("stored id=%s\n", id)}, nil
	case "stats":
		st, err := eng.Stats(ctx, ws)
		if err != nil {
			return Result{}, err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "engine: %s\nworkspace: %s\ntotal: %d\n", eng.Description(), ws, st.Total)
		for layer, n := range st.Layers {
			fmt.Fprintf(&b, "  %s: %d\n", layer, n)
		}
		return Result{Output: b.String()}, nil
	case "forget":
		if len(args) != 2 {
			return Result{}, fmt.Errorf("usage: /memory forget <id>")
		}
		if err := eng.Forget(ctx, args[1]); err != nil {
			return Result{}, err
		}
		return Result{Output: "forgotten\n"}, nil
	case "consolidate":
		report, err := eng.Consolidate(ctx, ws)
		if err != nil {
			return Result{}, err
		}
		return Result{Output: report + "\n"}, nil
	case "index":
		root := ""
		force := false
		for _, a := range args[1:] {
			if a == "--force" || a == "-f" {
				force = true
			} else if root == "" {
				root = a
			}
		}
		if root == "" && c.deps.MemoryWorkspace != nil {
			root = c.deps.MemoryWorkspace()
		}
		report, err := eng.IndexCode(ctx, root, ws, force)
		if err != nil {
			return Result{}, err
		}
		return Result{Output: report + "\n"}, nil
	case "kv":
		if c.deps.Memory == nil {
			return Result{}, fmt.Errorf("memoryStore is disabled")
		}
		return c.memoryKVCmd(ctx, c.deps.Memory, args[1:])
	default:
		return Result{}, fmt.Errorf("unknown /memory subcommand %q (use list/recall/remember/stats/forget/consolidate/index/kv)", args[0])
	}
}
```

原 `memoryCmd` 的 KV 主体改名为 `memoryKVCmd(ctx context.Context, m *store.Store, args []string) (Result, error)`，函数体原样保留（`list` 默认行为不变）。

- [ ] **Step 5: settingsSet 即时生效** — `settingsSet` 的 switch 追加（放在 `KeyCheckpoint...` restart case 之后）：

```go
	case settings.KeyMemoryEngine, settings.KeyLadyMDBPath, settings.KeyLadyMEmbedding,
		settings.KeyLadyMEmbeddingModel, settings.KeyLadyMEmbeddingBaseURL,
		settings.KeyLadyMLLM:
		if err := c.applyMemoryChange(key); err != nil {
			return Result{Output: fmt.Sprintf("%s saved; memory engine rebuild failed: %v\n", key, err)}, nil
		}
		return Result{Output: fmt.Sprintf("%s saved; takes effect immediately\n", key)}, nil
	case settings.KeyLadyMAutoRetain, settings.KeyLadyMTopK,
		settings.KeyLadyMSystem2, settings.KeyLadyMSystem2Interval:
		return Result{Output: fmt.Sprintf("%s saved; takes effect next run\n", key)}, nil
```

secret 分支：`settingsSet` 开头的 `if name, ok := settings.SecretFor(key); ok` 块在 `applyModelChange` 之后追加 memory secret 的 rebuild：

```go
		if name == settings.SecretLadyMEmbeddingAPIKey {
			// engine rebuild picks up the new key (model rebuild irrelevant)
		}
```

（更简单：在该 secret 分支里同时调用 `c.applyMemoryChange(key)`——把 `applyModelChange(key)` 的调用改为对 `KeyLadyMEmbeddingAPIKey` 调 `applyMemoryChange`、其余 secret 调 `applyModelChange`。）并新增方法：

```go
// applyMemoryChange rebuilds the memory engine when the changed key affects it.
func (c *Console) applyMemoryChange(key string) error {
	if c.deps.RebuildMemory == nil {
		return nil
	}
	return c.deps.RebuildMemory()
}
```

- [ ] **Step 6: slashcmds 描述** — `internal/slashcmds/slashcmds.go` 的 memory 条目改为：

```go
	{Name: "memory", Description: "factual memory: recall/remember/stats/forget/consolidate/index + legacy kv store"},
```

- [ ] **Step 7: 接线 + 测试** — REPL/TUI 构造 Console 的 Deps 处（`grep -n 'console.New\|Deps{' cmd/woca/main.go internal/repl/ internal/tui/`）追加：

```go
		MemoryEngine:    ap.memEngine,
		MemoryWorkspace: func() string { return ap.a.MemoryWorkspace() },
		RebuildMemory:   func() error { return ap.rebuildMemory() },
```

（`Agent.MemoryWorkspace()` 导出版本：把 Task 5 的 `memoryWorkspace` 加一个导出包装 `func (a *Agent) MemoryWorkspace() string { return a.memoryWorkspace() }`。）

`go test ./... ` 全 PASS。

- [ ] **Step 8: Commit** — `git add internal/ cmd/ && git commit -m "feat(console): /memory engine subcommands and immediate ladym settings effect"`

---

## Task 11（woca）：server 记忆 API（4 个 endpoint）

spec §7。Task 6 已加好 `Config.MemoryEngine` 字段；本任务补路由与 handler。

**Files:**
- Create: `internal/server/memoryapi.go`
- Create: `internal/server/memoryapi_test.go`
- Modify: `internal/server/server.go`（Handler 路由）

- [ ] **Step 1: 写失败测试** — 新建 `internal/server/memoryapi_test.go`（复用 Task 10 console 测试的 fakeMemEngine 形状，另加 nil 场景）：

```go
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/projanvil/woca/internal/memory"
)

type fakeMemEngine struct{ hits []memory.Hit }

func (f *fakeMemEngine) Recall(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	return f.hits, nil
}
func (f *fakeMemEngine) SearchCode(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	return nil, nil
}
func (f *fakeMemEngine) Remember(ctx context.Context, ws, c string, tags []string, src string) (string, error) {
	return "srv-1", nil
}
func (f *fakeMemEngine) RecordEvent(ctx context.Context, ws, ag, act, obs, o string) error { return nil }
func (f *fakeMemEngine) List(ctx context.Context, ws, layer string, limit, off int) ([]memory.Hit, error) {
	return f.hits, nil
}
func (f *fakeMemEngine) Forget(ctx context.Context, id string) error { return nil }
func (f *fakeMemEngine) Stats(ctx context.Context, ws string) (*memory.Stats, error) {
	return &memory.Stats{Total: 2, Layers: map[string]int{"L2_semantic": 2}}, nil
}
func (f *fakeMemEngine) IndexCode(ctx context.Context, root, ws string, force bool) (string, error) {
	return "ok", nil
}
func (f *fakeMemEngine) Consolidate(ctx context.Context, ws string) (string, error) { return "ok", nil }
func (f *fakeMemEngine) Description() string { return "ladym test.db" }
func (f *fakeMemEngine) Close() error        { return nil }

func newMemoryTestServer(t *testing.T, eng memory.Engine) *Server {
	t.Helper()
	return New(Config{MemoryEngine: eng, Logger: zerolog.Nop()})
}

func TestMemoryListAndSearch(t *testing.T) {
	s := newMemoryTestServer(t, &fakeMemEngine{hits: []memory.Hit{{
		ID: "m1", Layer: "L2_semantic", Type: "fact", Summary: "JWT", UpdatedAt: 1,
	}}})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// no q -> List
	resp, err := http.Get(srv.URL + "/api/memory?workspace=/ws")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Engine string       `json:"engine"`
		Items  []memory.Hit `json:"items"`
		Total  int          `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Engine != "ladym" || len(body.Items) != 1 || body.Items[0].ID != "m1" {
		t.Fatalf("list body = %+v", body)
	}
	// with q -> Recall
	resp2, err := http.Get(srv.URL + "/api/memory?workspace=/ws&q=auth")
	if err != nil {
		t.Fatal(err)
	}
	var b2 struct {
		Items []memory.Hit `json:"items"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&b2); err != nil {
		t.Fatal(err)
	}
	if len(b2.Items) != 1 {
		t.Fatalf("search body = %+v", b2)
	}
}

func TestMemoryStatsPostDelete(t *testing.T) {
	s := newMemoryTestServer(t, &fakeMemEngine{})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/api/memory/stats?workspace=/ws")
	var st struct {
		Engine string       `json:"engine"`
		Stats  *memory.Stats `json:"stats"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Engine != "ladym" || st.Stats.Total != 2 {
		t.Fatalf("stats = %+v", st)
	}

	postResp, err := http.Post(srv.URL+"/api/memory", "application/json",
		stringsReader(`{"workspace":"/ws","content":"prefers tabs","tags":["manual"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(postResp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.ID != "srv-1" {
		t.Fatalf("created = %+v", created)
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/memory/srv-1", nil)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", delResp.StatusCode)
	}
}

func TestMemoryDisabledDegrades(t *testing.T) {
	s := newMemoryTestServer(t, nil)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/api/memory?workspace=/ws")
	var body struct {
		Engine string `json:"engine"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Engine != "none" {
		t.Fatalf("engine = %q", body.Engine)
	}
}

func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }
```

顶部 import 补 `"strings"` 与 `"github.com/rs/zerolog"`（若 server 包测试已用别的 logger 构造方式，从现有 `*_test.go` 抄）。

- [ ] **Step 2: 跑测试确认失败** — `go test ./internal/server/ -run TestMemory -v`，预期编译失败（路由不存在返回 404 使断言失败）。

- [ ] **Step 3: 实现 memoryapi.go**：

```go
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/projanvil/woca/internal/memory"
)

// memoryListResponse is the GET /api/memory payload. Engine is "none" when
// the memory subsystem is disabled (graceful degradation, spec §7).
type memoryListResponse struct {
	Engine string       `json:"engine"`
	Items  []memory.Hit `json:"items"`
	Total  int          `json:"total"`
}

func (s *Server) handleListMemory(w http.ResponseWriter, r *http.Request) {
	eng := s.memEngine
	if eng == nil {
		writeJSON(w, http.StatusOK, memoryListResponse{Engine: "none", Items: []memory.Hit{}})
		return
	}
	q := r.URL.Query()
	ws := q.Get("workspace")
	if ws == "" {
		ws = s.workdir
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	var (
		items []memory.Hit
		err   error
	)
	if query := q.Get("q"); query != "" {
		items, err = eng.Recall(r.Context(), query, ws, limit)
	} else {
		items, err = eng.List(r.Context(), ws, q.Get("layer"), limit, offset)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if items == nil {
		items = []memory.Hit{}
	}
	writeJSON(w, http.StatusOK, memoryListResponse{Engine: "ladym", Items: items, Total: len(items)})
}

func (s *Server) handleMemoryStats(w http.ResponseWriter, r *http.Request) {
	eng := s.memEngine
	if eng == nil {
		writeJSON(w, http.StatusOK, map[string]any{"engine": "none", "stats": nil})
		return
	}
	ws := r.URL.Query().Get("workspace")
	if ws == "" {
		ws = s.workdir
	}
	st, err := eng.Stats(r.Context(), ws)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"engine": "ladym", "stats": st, "workspace": ws})
}

func (s *Server) handleRememberMemory(w http.ResponseWriter, r *http.Request) {
	eng := s.memEngine
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "memory engine is disabled")
		return
	}
	var req struct {
		Workspace string   `json:"workspace"`
		Content   string   `json:"content"`
		Tags      []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Content == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}
	if req.Workspace == "" {
		req.Workspace = s.workdir
	}
	id, err := eng.Remember(r.Context(), req.Workspace, req.Content, append(req.Tags, "panel"), "woca:panel")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (s *Server) handleForgetMemory(w http.ResponseWriter, r *http.Request) {
	eng := s.memEngine
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "memory engine is disabled")
		return
	}
	id := r.PathValue("id")
	if err := eng.Forget(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
```

- [ ] **Step 4: 路由** — `Handler()` 在 `GET/PUT /api/settings` 之后追加：

```go
	mux.HandleFunc("GET /api/memory", s.handleListMemory)
	mux.HandleFunc("GET /api/memory/stats", s.handleMemoryStats)
	mux.HandleFunc("POST /api/memory", s.handleRememberMemory)
	mux.HandleFunc("DELETE /api/memory/{id}", s.handleForgetMemory)
```

- [ ] **Step 5: 跑测试** — `go test ./internal/server/ -v`，预期全 PASS。

- [ ] **Step 6: Commit** — `git add internal/server/ && git commit -m "feat(server): memory REST API (list/search/remember/forget/stats)"`

---

## Task 12（woca）：settingsapi PUT 新键 + 实时 rebuild

spec §5.4 web 侧。

**Files:**
- Modify: `internal/server/settingsapi.go`
- Modify: `cmd/woca/main.go`（serve 已在 Task 6 传 RebuildMemory——确认无遗漏即可）

- [ ] **Step 1: appSettings GET 增字段** — `appSettings` 结构体追加：

```go
	MemoryEngine             string `json:"memory_engine"`
	LadyMDBPath              string `json:"ladym_db_path"`
	LadyMEmbedding           string `json:"ladym_embedding"`
	LadyMEmbeddingModel      string `json:"ladym_embedding_model"`
	LadyMEmbeddingBaseURL    string `json:"ladym_embedding_base_url"`
	LadyMEmbeddingAPIKeySet  bool   `json:"ladym_embedding_api_key_set"`
	LadyMLLM                 string `json:"ladym_llm"`
	LadyMAutoRetain          bool   `json:"ladym_autoretain"`
	LadyMTopK                int    `json:"ladym_topk"`
	LadyMSystem2             bool   `json:"ladym_system2"`
	LadyMSystem2Interval     int    `json:"ladym_system2_interval"`
```

`handleGetSettings` 的返回值对应补齐（`MemoryEngine: cfg.MemoryEngine, ...`，secret 用 `st.HasSecret(settings.SecretLadyMEmbeddingAPIKey)`）。

- [ ] **Step 2: PUT 增字段** — `appSettingsUpdate` 追加指针字段：

```go
	MemoryEngine          *string   `json:"memory_engine"`
	LadyMDBPath           *string   `json:"ladym_db_path"`
	LadyMEmbedding        *string   `json:"ladym_embedding"`
	LadyMEmbeddingModel   *string   `json:"ladym_embedding_model"`
	LadyMEmbeddingBaseURL *string   `json:"ladym_embedding_base_url"`
	LadyMEmbeddingAPIKey  *string   `json:"ladym_embedding_api_key"`
	LadyMLLM              *string   `json:"ladym_llm"`
	LadyMAutoRetain       *bool     `json:"ladym_autoretain"`
	LadyMTopK             *int      `json:"ladym_topk"`
	LadyMSystem2          *bool     `json:"ladym_system2"`
	LadyMSystem2Interval  *int      `json:"ladym_system2_interval"`
```

`handleUpdateSettings` 在 `req.CheckpointDir` 块之后追加处理分支（引擎构造类置 `memoryChanged = true`，per-run 类不重建）：

```go
	memoryChanged := false
	if req.MemoryEngine != nil {
		if err := settings.Apply(&cfg, settings.KeyMemoryEngine, *req.MemoryEngine); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		memoryChanged = true
	}
	if req.LadyMDBPath != nil {
		if err := settings.Apply(&cfg, settings.KeyLadyMDBPath, *req.LadyMDBPath); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		memoryChanged = true
	}
	if req.LadyMEmbedding != nil {
		if err := settings.Apply(&cfg, settings.KeyLadyMEmbedding, *req.LadyMEmbedding); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		memoryChanged = true
	}
	if req.LadyMEmbeddingModel != nil {
		if err := settings.Apply(&cfg, settings.KeyLadyMEmbeddingModel, *req.LadyMEmbeddingModel); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		memoryChanged = true
	}
	if req.LadyMEmbeddingBaseURL != nil {
		if err := settings.Apply(&cfg, settings.KeyLadyMEmbeddingBaseURL, *req.LadyMEmbeddingBaseURL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		memoryChanged = true
	}
	if req.LadyMLLM != nil {
		if err := settings.Apply(&cfg, settings.KeyLadyMLLM, *req.LadyMLLM); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		memoryChanged = true
	}
	if req.LadyMAutoRetain != nil {
		if err := settings.Apply(&cfg, settings.KeyLadyMAutoRetain, strconv.FormatBool(*req.LadyMAutoRetain)); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.LadyMTopK != nil {
		if err := settings.Apply(&cfg, settings.KeyLadyMTopK, itoa(*req.LadyMTopK)); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.LadyMSystem2 != nil {
		if err := settings.Apply(&cfg, settings.KeyLadyMSystem2, strconv.FormatBool(*req.LadyMSystem2)); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.LadyMSystem2Interval != nil {
		if err := settings.Apply(&cfg, settings.KeyLadyMSystem2Interval, itoa(*req.LadyMSystem2Interval)); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
```

secret 段（`req.PostgresURL` 之后）追加：

```go
	if req.LadyMEmbeddingAPIKey != nil {
		if err := st.SetSecret(settings.SecretLadyMEmbeddingAPIKey, *req.LadyMEmbeddingAPIKey); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		memoryChanged = true
	}
```

应用段（`modelChanged` 重建之后）追加：

```go
	if memoryChanged && s.rebuildMemory != nil {
		if err := s.rebuildMemory(); err != nil {
			applyErr = err.Error()
		}
	}
```

（`applyErr` 变量在原代码中已存在；若 modelChanged 分支先赋值会覆盖——把两个 rebuild 的错误拼接或 memory 优先，取后者覆盖语义即可，保持响应体字段不变。）

- [ ] **Step 3: 构建 + 全测** — `go build ./... && go test ./...`，预期全 PASS。

- [ ] **Step 4: 冒烟（serve + curl）**：

```bash
go build -o /tmp/woca-p2 ./cmd/woca
/tmp/woca-p2 serve --no-browser --addr 127.0.0.1:39871 &
sleep 2
curl -s 127.0.0.1:39871/api/settings | python3 -m json.tool | grep -E 'memory_engine|ladym'
curl -s -X PUT 127.0.0.1:39871/api/settings -d '{"ladym-topk":8}' -H 'Content-Type: application/json'
curl -s 127.0.0.1:39871/api/memory | python3 -m json.tool | head -8
kill %1
```

预期：GET 显示 `"memory_engine": "ladym"` 与 ladym_* 字段；PUT 返回 `{"ok":true,"restart_required":false,"apply_error":""}`；/api/memory 返回 `{"engine":"ladym","items":[],"total":0}`。

（PUT body 的 json tag 是下划线：`{"ladym_topk": 8}`——以 Step 2 的 json tag 为准。）

- [ ] **Step 5: Commit** — `git add internal/server/ && git commit -m "feat(server): live-apply memory settings via PUT /api/settings"`

---

## Task 13（前端）：types + client + memory store

**Files:**
- Modify: `web/src/types.ts`
- Modify: `web/src/api/client.ts`
- Create: `web/src/stores/memory.ts`

- [ ] **Step 1: types.ts** — `AppSettingsUpdate` 之后追加：

```ts
// --- memory (mirror /api/memory) ---

export type MemoryLayer =
  | 'L1_episodic'
  | 'L2_semantic'
  | 'L3_procedural'
  | 'L4_associative'
  | 'L5_mental'
  | 'L6_predictive'

export interface MemoryItem {
  id: string
  layer: MemoryLayer | string
  type: string
  summary: string
  content: string
  source: string
  tags: string[] | null
  score?: number
  updated_at: number
}

export interface MemoryStats {
  total: number
  layers: Record<string, number>
}
```

`AppSettings` 追加字段（与 Task 12 的 GET JSON 一一对应）：

```ts
  memory_engine: string
  ladym_db_path: string
  ladym_embedding: string
  ladym_embedding_model: string
  ladym_embedding_base_url: string
  ladym_embedding_api_key_set: boolean
  ladym_llm: string
  ladym_autoretain: boolean
  ladym_topk: number
  ladym_system2: boolean
  ladym_system2_interval: number
```

`AppSettingsUpdate` 追加可选字段（同名，可选）。

- [ ] **Step 2: client.ts** — 文件末尾追加：

```ts
/** Lists (or searches with q) a workspace's memories. */
export async function listMemory(opts: {
  workspace?: string
  q?: string
  layer?: string
  limit?: number
  offset?: number
}): Promise<{ engine: string; items: MemoryItem[]; total: number }> {
  const params = new URLSearchParams()
  if (opts.workspace) params.set('workspace', opts.workspace)
  if (opts.q) params.set('q', opts.q)
  if (opts.layer) params.set('layer', opts.layer)
  if (opts.limit) params.set('limit', String(opts.limit))
  if (opts.offset) params.set('offset', String(opts.offset))
  const resp = await fetch(`/api/memory?${params}`)
  if (!resp.ok) throw new Error(`加载记忆失败：${resp.status}`)
  return resp.json()
}

export async function memoryStats(workspace?: string): Promise<{
  engine: string
  stats: MemoryStats | null
}> {
  const params = workspace ? `?workspace=${encodeURIComponent(workspace)}` : ''
  const resp = await fetch(`/api/memory/stats${params}`)
  if (!resp.ok) throw new Error(`加载记忆统计失败：${resp.status}`)
  return resp.json()
}

export async function rememberMemory(body: {
  workspace?: string
  content: string
  tags?: string[]
}): Promise<{ id: string }> {
  return postJSON('/api/memory', body)
}

export async function forgetMemory(id: string): Promise<void> {
  const resp = await fetch(`/api/memory/${encodeURIComponent(id)}`, { method: 'DELETE' })
  if (!resp.ok) throw new Error(`删除失败：${resp.status}`)
}
```

import type 行补 `MemoryItem, MemoryStats`。

- [ ] **Step 3: memory store** — 新建 `web/src/stores/memory.ts`：

```ts
import { defineStore } from 'pinia'
import { ref } from 'vue'
import { forgetMemory, listMemory, memoryStats, rememberMemory } from '../api/client'
import type { MemoryItem, MemoryStats } from '../types'

// Workspace-scoped memory panel state. open(ws) fixes the workspace for the
// panel's lifetime; reopening refreshes it (spec §9.2).
export const useMemoryStore = defineStore('memory', () => {
  const panelOpen = ref(false)
  const workspace = ref('')
  const workspaceName = ref('')
  const items = ref<MemoryItem[]>([])
  const stats = ref<MemoryStats | null>(null)
  const query = ref('')
  const layerFilter = ref('')
  const loading = ref(false)
  const engine = ref('ladym')
  const error = ref('')
  let offset = 0
  let searchTimer: ReturnType<typeof setTimeout> | null = null

  function open(ws: string, name?: string) {
    workspace.value = ws ?? ''
    workspaceName.value = name ?? ws ?? ''
    panelOpen.value = true
    query.value = ''
    layerFilter.value = ''
    void reload()
  }

  function close() {
    panelOpen.value = false
  }

  async function reload() {
    loading.value = true
    error.value = ''
    offset = 0
    try {
      const [list, st] = await Promise.all([
        listMemory({
          workspace: workspace.value,
          q: query.value || undefined,
          layer: layerFilter.value || undefined,
          limit: 20,
        }),
        memoryStats(workspace.value),
      ])
      engine.value = list.engine
      items.value = list.items
      offset = list.items.length
      stats.value = st.stats
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }

  function searchDebounced() {
    if (searchTimer) clearTimeout(searchTimer)
    searchTimer = setTimeout(() => void reload(), 300)
  }

  async function loadMore() {
    if (loading.value) return
    loading.value = true
    try {
      const list = await listMemory({
        workspace: workspace.value,
        q: query.value || undefined,
        layer: layerFilter.value || undefined,
        limit: 20,
        offset,
      })
      items.value = [...items.value, ...list.items]
      offset += list.items.length
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }

  async function add(content: string, tags: string[] = []) {
    await rememberMemory({ workspace: workspace.value, content, tags })
    await reload()
  }

  async function remove(id: string) {
    await forgetMemory(id)
    items.value = items.value.filter((m) => m.id !== id)
    void reload()
  }

  return {
    panelOpen, workspace, workspaceName, items, stats, query, layerFilter,
    loading, engine, error,
    open, close, reload, searchDebounced, loadMore, add, remove,
  }
})
```

- [ ] **Step 4: 前端类型检查 + 构建** — `cd web && npm run build`，预期成功。

- [ ] **Step 5: Commit** — `git add web/src/ && git commit -m "feat(web): memory API client and pinia store"`

---

## Task 14（前端）：MemoryPanel + title bar 入口

spec §9.1/§9.2/§9.4。

**Files:**
- Create: `web/src/components/MemoryPanel.vue`
- Modify: `web/src/components/ChatPanel.vue`
- Modify: `web/src/App.vue`
- Modify: `web/src/style.css`

- [ ] **Step 1: MemoryPanel.vue**：

```vue
<script setup lang="ts">
import { computed, ref } from 'vue'
import { Brain, Plus, Search, Trash2, X } from 'lucide-vue-next'
import { useMemoryStore } from '../stores/memory'
import type { MemoryItem } from '../types'

const memory = useMemoryStore()
const newContent = ref<string>('')
const newTags = ref<string>('')

// Product names for layers (spec §9.4): no L1-L6 jargon in the UI.
const layerLabels: Record<string, string> = {
  L1_episodic: '经历',
  L2_semantic: '事实',
  L3_procedural: '剧本',
  L4_associative: '关联',
  L5_mental: '洞察',
  L6_predictive: '预判',
}

const layerOrder = ['L5_mental', 'L6_predictive', 'L2_semantic', 'L3_procedural', 'L1_episodic']

function label(m: MemoryItem): string {
  if (m.layer === 'L2_semantic' && (m.type === 'code_symbol' || m.type === 'code_file')) return '代码'
  return layerLabels[m.layer] ?? m.layer
}

const statsChips = computed(() => {
  const s = memory.stats
  if (!s) return []
  return layerOrder
    .filter((l) => (s.layers[l] ?? 0) > 0)
    .map((l) => ({ layer: l, label: layerLabels[l] ?? l, count: s.layers[l] }))
})

// Insights and predictions render as their own group at the top (spec §9.4).
const insightItems = computed(() =>
  memory.items.filter((m) => m.layer === 'L5_mental' || m.layer === 'L6_predictive'),
)
const normalItems = computed(() =>
  memory.items.filter((m) => m.layer !== 'L5_mental' && m.layer !== 'L6_predictive'),
)

function fmtTime(ts: number): string {
  if (!ts) return ''
  return new Date(ts * 1000).toLocaleString('zh-CN', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' })
}

function ttlLeft(m: MemoryItem): string {
  const horizon = Number((m as unknown as { horizon_s?: number }).horizon_s ?? 0)
  if (!horizon) return ''
  const days = Math.max(1, Math.round(horizon / 86400))
  return `约剩 ${days} 天`
}

function confidence(m: MemoryItem): number {
  return Math.round((m.score ?? 0) * 100)
}

async function removeItem(m: MemoryItem) {
  if (!window.confirm('忘记这条记忆？')) return
  await memory.remove(m.id)
}

async function addMemory() {
  const content = newContent.value.trim()
  if (!content) return
  const tags = newTags.value.split(/[,，\s]+/).filter(Boolean)
  await memory.add(content, tags)
  newContent.value = ''
  newTags.value = ''
}
</script>

<template>
  <div class="overlay" @click.self="memory.close()">
    <div class="card memory-card">
      <div class="card-head">
        <span class="card-title">
          <Brain :size="15" />
          工作区记忆
          <span class="mem-ws">{{ memory.workspaceName || '默认工作区' }}</span>
        </span>
        <button class="icon-btn" @click="memory.close()">
          <X :size="16" />
        </button>
      </div>

      <div class="card-body memory-body">
        <p class="mem-tagline">
          woca 会记住这个工作区的决策、偏好与代码知识——越用越懂你的项目。
        </p>

        <div class="mem-toolbar">
          <div class="mem-search">
            <Search :size="13" />
            <input
              v-model="memory.query"
              placeholder="搜索记忆…"
              @input="memory.searchDebounced()"
            />
          </div>
          <div v-if="statsChips.length" class="mem-chips">
            <button
              v-for="c in statsChips"
              :key="c.layer"
              class="mem-chip"
              :class="{ active: memory.layerFilter === c.layer }"
              @click="memory.layerFilter = memory.layerFilter === c.layer ? '' : c.layer; memory.reload()"
            >
              {{ c.label }} {{ c.count }}
            </button>
          </div>
        </div>

        <p v-if="memory.error" class="mem-error">{{ memory.error }}</p>
        <p v-if="memory.engine === 'none'" class="mem-empty">
          记忆引擎未启用（设置 → 记忆 → 记忆引擎）。
        </p>
        <p v-else-if="!memory.loading && memory.items.length === 0" class="mem-empty">
          还没有记忆。和 woca 聊几句，它会自动记住重要的东西。
        </p>
        <p v-if="memory.loading" class="mem-empty">加载中…</p>

        <!-- Insights & predictions group -->
        <div v-if="insightItems.length" class="mem-group">
          <div class="mem-group-title">洞察与预判</div>
          <div v-for="m in insightItems" :key="m.id" class="mem-item insight" :class="m.layer">
            <div class="mem-item-head">
              <span class="mem-badge" :class="m.layer">{{ label(m) }}</span>
              <span v-if="m.layer === 'L6_predictive' && confidence(m)" class="mem-conf">
                {{ confidence(m) }}%
              </span>
              <span v-if="m.layer === 'L6_predictive' && ttlLeft(m)" class="mem-ttl">{{ ttlLeft(m) }}</span>
              <span class="mem-time">{{ fmtTime(m.updated_at) }}</span>
              <button class="mem-del" title="忘记" @click="removeItem(m)">
                <Trash2 :size="12" />
              </button>
            </div>
            <div class="mem-summary">{{ m.summary }}</div>
            <div v-if="m.content && m.content !== m.summary" class="mem-content">{{ m.content }}</div>
          </div>
        </div>

        <!-- Everything else -->
        <div v-if="normalItems.length" class="mem-group">
          <div class="mem-group-title">记忆</div>
          <div v-for="m in normalItems" :key="m.id" class="mem-item">
            <div class="mem-item-head">
              <span class="mem-badge" :class="m.layer">{{ label(m) }}</span>
              <span class="mem-time">{{ fmtTime(m.updated_at) }}</span>
              <button class="mem-del" title="忘记" @click="removeItem(m)">
                <Trash2 :size="12" />
              </button>
            </div>
            <div class="mem-summary">{{ m.summary }}</div>
            <div v-if="m.content && m.content !== m.summary" class="mem-content">{{ m.content }}</div>
          </div>
        </div>

        <button v-if="memory.items.length >= 20" class="mem-more" @click="memory.loadMore()">
          加载更多
        </button>

        <div class="mem-add">
          <textarea
            v-model="newContent"
            rows="2"
            placeholder="手动记住一件事，例如：构建统一走 make build"
          ></textarea>
          <div class="mem-add-row">
            <input v-model="newTags" placeholder="标签（可选，逗号分隔）" class="mem-tags-input" />
            <button class="mem-add-btn" :disabled="!newContent.trim()" @click="addMemory()">
              <Plus :size="13" /> 记住
            </button>
          </div>
        </div>

        <p class="mem-privacy">所有记忆只保存在本地（~/.woca/ladym.db），随时查看、编辑或删除。</p>
      </div>
    </div>
  </div>
</template>
```

- [ ] **Step 2: ChatPanel.vue 入口** — script setup 区：lucide import 行加 `Brain`；stores import 区加 `import { useMemoryStore } from '../stores/memory'` 与 `const memory = useMemoryStore()`；sessions store 已有 `activeWorkspace` computed（`{path, name}`）。template 中 `.panel-toggle` 按钮**之前**插入：

```vue
      <button
        class="icon-btn"
        title="工作区记忆"
        @click="memory.open(store.activeWorkspace?.path ?? '', store.activeWorkspace?.name)"
      >
        <Brain :size="16" />
      </button>
```

（`store` 是 ChatPanel 里 useSessionStore 的实例名；若实际名不同以现文件为准。若 sessions store 的 return 未导出 `activeWorkspace`，先把它加进 `web/src/stores/sessions.ts` 的 return——它已存在为 computed（第 143 行），只是可能未导出。）

- [ ] **Step 3: App.vue 挂载** — import 区加 `import MemoryPanel from './components/MemoryPanel.vue'`；template 中 `<GoalPanel ...>` 之后加：

```vue
    <MemoryPanel v-if="memory.panelOpen" />
```

script setup 加 `import { useMemoryStore } from './stores/memory'` 与 `const memory = useMemoryStore()`。

- [ ] **Step 4: style.css 样式** — 文件末尾追加：

```css
/* --- memory panel --- */
.memory-card { width: min(720px, 94vw); max-height: 84vh; display: flex; flex-direction: column; overflow: hidden; }
.memory-body { overflow-y: auto; padding: 14px 16px; display: flex; flex-direction: column; gap: 10px; }
.mem-ws { font-weight: 400; opacity: 0.7; margin-left: 6px; font-size: 12px; }
.mem-tagline { margin: 0; font-size: 12.5px; opacity: 0.75; }
.mem-toolbar { display: flex; flex-direction: column; gap: 8px; }
.mem-search { display: flex; align-items: center; gap: 6px; border: 1px solid var(--border); border-radius: 8px; padding: 5px 10px; }
.mem-search input { flex: 1; border: none; background: transparent; outline: none; font-size: 13px; color: inherit; }
.mem-chips { display: flex; gap: 6px; flex-wrap: wrap; }
.mem-chip { border: 1px solid var(--border); background: transparent; border-radius: 999px; padding: 2px 10px; font-size: 12px; cursor: pointer; color: inherit; }
.mem-chip.active { background: var(--accent, #4f8ef7); color: #fff; border-color: transparent; }
.mem-error { color: #e5484d; font-size: 12.5px; margin: 0; }
.mem-empty { opacity: 0.6; font-size: 13px; margin: 8px 0; text-align: center; }
.mem-group { display: flex; flex-direction: column; gap: 6px; }
.mem-group-title { font-size: 11.5px; text-transform: uppercase; letter-spacing: 0.06em; opacity: 0.55; }
.mem-item { border: 1px solid var(--border); border-radius: 10px; padding: 8px 10px; display: flex; flex-direction: column; gap: 4px; }
.mem-item.insight { border-left: 3px solid #8b5cf6; }
.mem-item.L6_predictive { border-left-color: #f59e0b; }
.mem-item-head { display: flex; align-items: center; gap: 8px; }
.mem-badge { font-size: 11px; border-radius: 4px; padding: 1px 6px; background: rgba(127, 127, 127, 0.18); }
.mem-badge.L5_mental { background: rgba(139, 92, 246, 0.2); color: #8b5cf6; }
.mem-badge.L6_predictive { background: rgba(245, 158, 11, 0.2); color: #f59e0b; }
.mem-conf { font-size: 11px; opacity: 0.8; }
.mem-ttl { font-size: 11px; opacity: 0.6; }
.mem-time { margin-left: auto; font-size: 11px; opacity: 0.5; }
.mem-del { border: none; background: transparent; color: inherit; opacity: 0.45; cursor: pointer; padding: 2px; }
.mem-del:hover { opacity: 1; color: #e5484d; }
.mem-summary { font-size: 13px; font-weight: 600; }
.mem-content { font-size: 12.5px; opacity: 0.8; display: -webkit-box; -webkit-line-clamp: 3; -webkit-box-orient: vertical; overflow: hidden; white-space: pre-wrap; }
.mem-more { align-self: center; border: 1px solid var(--border); background: transparent; border-radius: 8px; padding: 4px 14px; font-size: 12.5px; cursor: pointer; color: inherit; }
.mem-add { border-top: 1px solid var(--border); padding-top: 10px; display: flex; flex-direction: column; gap: 6px; }
.mem-add textarea { width: 100%; border: 1px solid var(--border); border-radius: 8px; padding: 8px 10px; font-size: 13px; background: transparent; color: inherit; resize: vertical; }
.mem-add-row { display: flex; gap: 8px; }
.mem-tags-input { flex: 1; border: 1px solid var(--border); border-radius: 8px; padding: 6px 10px; font-size: 12.5px; background: transparent; color: inherit; }
.mem-add-btn { display: inline-flex; align-items: center; gap: 4px; border: none; border-radius: 8px; padding: 6px 14px; font-size: 13px; cursor: pointer; background: var(--accent, #4f8ef7); color: #fff; }
.mem-add-btn:disabled { opacity: 0.5; cursor: default; }
.mem-privacy { font-size: 11.5px; opacity: 0.5; margin: 4px 0 0; text-align: center; }
```

- [ ] **Step 5: 构建** — `cd web && npm run build`，预期成功；`cd .. && go build ./...`。

- [ ] **Step 6: Commit** — `git add web/ && git commit -m "feat(web): workspace memory panel with title-bar entry"`

---

## Task 15（前端）：SettingsPanel 记忆区 + settings store 扩展

spec §5.1/§9.4 文案。

**Files:**
- Modify: `web/src/stores/settings.ts`
- Modify: `web/src/components/SettingsPanel.vue`

- [ ] **Step 1: settings.ts** — 本地可编辑副本区追加 refs：

```ts
  const memoryEngine = ref('ladym')
  const ladymEmbedding = ref('inherit')
  const ladymEmbeddingModel = ref('')
  const ladymEmbeddingBaseURL = ref('')
  const ladymEmbeddingAPIKey = ref('')
  const ladymLLM = ref('inherit')
  const ladymAutoRetain = ref(true)
  const ladymTopK = ref(6)
  const ladymDBPath = ref('')
  const ladymSystem2 = ref(false)
```

`loadServer` 的 try 块内追加：

```ts
      memoryEngine.value = s.memory_engine || 'ladym'
      ladymEmbedding.value = s.ladym_embedding || 'inherit'
      ladymEmbeddingModel.value = s.ladym_embedding_model ?? ''
      ladymEmbeddingBaseURL.value = s.ladym_embedding_base_url ?? ''
      ladymLLM.value = s.ladym_llm || 'inherit'
      ladymAutoRetain.value = s.ladym_autoretain !== false
      ladymTopK.value = s.ladym_topk || 6
      ladymDBPath.value = s.ladym_db_path ?? ''
      ladymSystem2.value = Boolean(s.ladym_system2)
      ladymEmbeddingAPIKey.value = ''
```

`saveServer` 的 patch 对象追加：

```ts
      memory_engine: memoryEngine.value,
      ladym_embedding: ladymEmbedding.value,
      ladym_embedding_model: ladymEmbeddingModel.value,
      ladym_embedding_base_url: ladymEmbeddingBaseURL.value,
      ladym_llm: ladymLLM.value,
      ladym_autoretain: ladymAutoRetain.value,
      ladym_topk: ladymTopK.value,
      ladym_db_path: ladymDBPath.value,
      ladym_system2: ladymSystem2.value,
```

secret 段追加（在 postgresURL 条件旁）：

```ts
    if (ladymEmbeddingAPIKey.value) patch.ladym_embedding_api_key = ladymEmbeddingAPIKey.value
```

return 对象追加全部新 ref（`memoryEngine, ladymEmbedding, ladymEmbeddingModel, ladymEmbeddingBaseURL, ladymEmbeddingAPIKey, ladymLLM, ladymAutoRetain, ladymTopK, ladymDBPath, ladymSystem2`）与 computed：

```ts
    ladymEmbeddingKeySet: computed(() => Boolean(server.value?.ladym_embedding_api_key_set)),
```

- [ ] **Step 2: SettingsPanel.vue** — lucide import 加 `Brain`；`sections` 数组在 `storage` 之后插入：

```ts
  { id: 'memory', label: '记忆', icon: Brain },
```

template 在 storage section 之后追加（沿用 setting-row/field/switch 类；文案按 spec §9.4）：

```vue
          <!-- Memory (ladyM factual memory engine) -->
          <section v-if="active === 'memory'" class="setting-section">
            <div class="setting-section-title"><Brain :size="13" style="vertical-align: -2px" /> 记忆</div>
            <p class="mem-intro">
              woca 会记住这个工作区的决策、偏好与代码知识——越用越懂你的项目。
            </p>

            <div class="setting-row">
              <div>
                <div class="row-label">记忆引擎</div>
                <div class="row-hint">关闭后不再读写记忆（实时生效）</div>
              </div>
              <select v-model="settings.memoryEngine" class="field" style="max-width: 200px">
                <option value="ladym">启用（默认）</option>
                <option value="none">关闭</option>
              </select>
            </div>

            <template v-if="settings.memoryEngine === 'ladym'">
              <div class="setting-row">
                <div>
                  <div class="row-label">自动记忆</div>
                  <div class="row-hint">对话结束后自动沉淀值得记住的决策与偏好</div>
                </div>
                <button
                  class="switch"
                  :class="{ on: settings.ladymAutoRetain }"
                  @click="settings.ladymAutoRetain = !settings.ladymAutoRetain"
                ></button>
              </div>

              <div class="setting-row">
                <div>
                  <div class="row-label">后台整理</div>
                  <div class="row-hint">定期把经历提炼为事实与洞察、预判下一步（消耗少量模型调用）</div>
                </div>
                <button
                  class="switch"
                  :class="{ on: settings.ladymSystem2 }"
                  @click="settings.ladymSystem2 = !settings.ladymSystem2"
                ></button>
              </div>

              <div class="setting-row">
                <div>
                  <div class="row-label">召回条数</div>
                  <div class="row-hint">每轮对话注入的相关记忆条数（1–20）</div>
                </div>
                <input
                  v-model.number="settings.ladymTopK"
                  type="number" min="1" max="20"
                  class="field" style="max-width: 90px"
                />
              </div>

              <div class="setting-subgroup">
                <div class="row-label">模型</div>
                <div class="row-hint">默认继承上方模型配置；也可单独指定（如用便宜的 embedding 模型）</div>

                <div class="setting-row">
                  <div><div class="row-label">Embedding</div></div>
                  <select v-model="settings.ladymEmbedding" class="field" style="max-width: 200px">
                    <option value="inherit">继承 woca</option>
                    <option value="hashing">hashing（离线）</option>
                    <option value="openai">openai</option>
                    <option value="ollama">ollama</option>
                    <option value="http">http</option>
                  </select>
                </div>

                <div class="setting-row">
                  <div><div class="row-label">Embedding 模型</div></div>
                  <input
                    v-model="settings.ladymEmbeddingModel"
                    class="field" style="max-width: 260px"
                    placeholder="text-embedding-3-small"
                  />
                </div>

                <div class="setting-row">
                  <div><div class="row-label">Embedding Base URL</div></div>
                  <input
                    v-model="settings.ladymEmbeddingBaseURL"
                    class="field" style="max-width: 260px"
                    placeholder="继承 OpenAI Base URL"
                  />
                </div>

                <div class="setting-row">
                  <div>
                    <div class="row-label">Embedding API Key</div>
                    <div class="row-hint">{{ settings.ladymEmbeddingKeySet ? '已设置（加密存储）' : '未设置（继承 OpenAI Key）' }}</div>
                  </div>
                  <input
                    v-model="settings.ladymEmbeddingAPIKey"
                    class="field" style="max-width: 260px"
                    type="password" autocomplete="off"
                    placeholder="输入新值以更新"
                  />
                </div>

                <div class="setting-row">
                  <div>
                    <div class="row-label">整理模型（LLM）</div>
                    <div class="row-hint">继承 = 用当前模型做离线整理；none = 纯本地启发式（洞察/预判停用）</div>
                  </div>
                  <select v-model="settings.ladymLLM" class="field" style="max-width: 200px">
                    <option value="inherit">继承 woca</option>
                    <option value="none">none</option>
                  </select>
                </div>
              </div>

              <div class="setting-row">
                <div>
                  <div class="row-label">数据库路径</div>
                  <div class="row-hint">留空使用 ~/.woca/ladym.db（实时生效）</div>
                </div>
                <input
                  v-model="settings.ladymDBPath"
                  class="field" style="max-width: 260px"
                  placeholder="~/.woca/ladym.db"
                />
              </div>
            </template>
          </section>
```

style.css 追加两个小类：

```css
.mem-intro { margin: 0 0 10px; font-size: 12.5px; opacity: 0.75; }
.setting-subgroup { border: 1px solid var(--border); border-radius: 10px; padding: 10px 12px; margin: 8px 0; display: flex; flex-direction: column; gap: 8px; }
```

- [ ] **Step 3: 构建 + 冒烟**：

```bash
cd web && npm run build && cd ..
go build ./...
```

起 serve 后手测：设置 → 记忆 → 切换引擎保存（loading 数秒内返回"已保存"）；title bar Brain 按钮打开面板；空态文案正确；手动"记住"一条后列表出现。

- [ ] **Step 4: Commit** — `git add web/src/ && git commit -m "feat(web): memory settings section with inherit-by-default model config"`

---

## Task 16（woca）：System 2 机会主义触发

spec §4.5/§4.6。挂点与 auto-retain 同处（runner 的 RetainRun），per-workspace 节流，goroutine fire-and-forget。

**Files:**
- Modify: `internal/agent/agent.go`（throttle 字段）
- Modify: `internal/agent/retain.go`
- Modify: `internal/agent/retain_test.go`

- [ ] **Step 1: 写失败测试** — `retain_test.go` 追加：

```go
func TestSystem2ThrottledPerWorkspace(t *testing.T) {
	f := &fakeEngine{}
	// fakeEngine.Consolidate needs a counter for this test:
	// add `consolidates int` to fakeEngine and count in Consolidate.
	on := settings.Default()
	on.LadyMSystem2 = true
	a := &Agent{
		memEngine:    f,
		workdir:      "/ws",
		loadSettings: func() settings.Settings { return on },
	}
	hist := []messages.Message{messages.Human("q")}
	fin := []messages.Message{messages.AI("a")}
	a.RetainRun(context.Background(), hist, fin, nil)
	// fire-and-forget goroutine: wait for it to land
	for i := 0; i < 50 && f.consolidates == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if f.consolidates != 1 {
		t.Fatalf("consolidates = %d, want 1", f.consolidates)
	}
	// second run within the interval: throttled
	a.RetainRun(context.Background(), hist, fin, nil)
	time.Sleep(200 * time.Millisecond)
	if f.consolidates != 1 {
		t.Fatalf("throttle failed: %d", f.consolidates)
	}
	// different workspace: not throttled by the first
	a.SetMemoryWorkspace("/ws2")
	a.RetainRun(context.Background(), hist, fin, nil)
	for i := 0; i < 50 && f.consolidates < 2; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if f.consolidates != 2 {
		t.Fatalf("per-workspace throttle wrong: %d", f.consolidates)
	}
}
```

同步改 fakeEngine（memoryengine_test.go）：加字段 `consolidates int`（mutex 保护），`Consolidate` 实现改为计数并返回 `"cycle"`；文件顶部 import 补 `"time"`。

- [ ] **Step 2: 跑测试确认失败** — `go test ./internal/agent/ -run TestSystem2 -v`，预期失败（consolidates 恒 0）。

- [ ] **Step 3: 实现** — `agent.go` 的 `Agent` 结构体追加：

```go
	// lastSystem2 remembers when each workspace last ran a System 2 cycle
	// (opportunistic post-run throttling, spec §4.5).
	lastSystem2 map[string]int64 // unix seconds
```

`retain.go` 的 `RetainRun` 在 fact/episode 写入之后追加：

```go
	// System 2 (opportunistic, per-workspace throttle): consolidate the
	// fresh episodes into facts/playbooks/insights in the background. The
	// cycle itself gates on ladyM's episode threshold, so a no-op trigger
	// is a cheap count query (spec §4.5).
	if a.loadSettings != nil {
		s := a.loadSettings()
		if s.LadyMSystem2 {
			interval := int64(300)
			if s.LadyMSystem2Interval >= 60 {
				interval = int64(s.LadyMSystem2Interval)
			}
			if a.system2Due(ws, interval) {
				engC := eng
				go func() {
					_, _ = engC.Consolidate(context.WithoutCancel(ctx), ws)
				}()
			}
		}
	}
```

`retain.go` 追加方法：

```go
// system2Due applies the per-workspace minimum interval between System 2
// cycles and records the trigger time.
func (a *Agent) system2Due(ws string, intervalS int64) bool {
	now := time.Now().Unix()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastSystem2 == nil {
		a.lastSystem2 = map[string]int64{}
	}
	if last, ok := a.lastSystem2[ws]; ok && now-last < intervalS {
		return false
	}
	a.lastSystem2[ws] = now
	return true
}
```

import 补 `"time"`。注意 `ctx` 在 RetainRun 的参数里，goroutine 用 `context.WithoutCancel(ctx)`。

- [ ] **Step 4: 跑测试** — `go test ./internal/agent/ -v`，预期全 PASS（含竞态：`go test -race ./internal/agent/ -run 'TestSystem2|TestRetain'`）。

- [ ] **Step 5: Commit** — `git add internal/agent/ && git commit -m "feat(agent): opportunistic System 2 consolidation with per-workspace throttle"`

---

## Task 17（woca）：subagent 任务事件沉淀（收尾）

spec §6 表格最后一行：runTask 完成时记录 L1 事件（`agent=subagent 名`），喂 consolidate/L5/L6。`/memory consolidate` 子命令已在 Task 10 实现，此处只需 task.go 事件。

**Files:**
- Modify: `internal/agent/task.go`
- Modify: `internal/agent/retain_test.go`（或新文件 `task_event_test.go`）

- [ ] **Step 1: 写失败测试** — 追加到 `internal/agent/task_event_test.go`：

```go
package agent

import (
	"context"
	"strings"
	"testing"
)

func TestRecordTaskEvent(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{memEngine: f, workdir: "/ws"}
	a.SetMemoryWorkspace("/ws/task")
	a.recordTaskEvent("explorer", "map the auth flow", "done: 3 entry points found", nil)
	if len(f.events) != 1 {
		t.Fatalf("events = %v", f.events)
	}
	e := f.events[0]
	if !strings.Contains(e, "subagent:explorer") || !strings.Contains(e, "success") {
		t.Fatalf("event = %q", e)
	}
	a.recordTaskEvent("explorer", "x", "", context.Canceled)
	if len(f.events) != 2 || !strings.Contains(f.events[1], "error") {
		t.Fatalf("error event missing: %v", f.events)
	}
}
```

- [ ] **Step 2: 跑测试确认失败** — `go test ./internal/agent/ -run TestRecordTaskEvent -v`，预期编译失败。

- [ ] **Step 3: 实现** — `retain.go` 追加：

```go
// recordTaskEvent logs a completed subagent task as an L1 episode (spec §6:
// reads shared, writes governed — subagents report events, not facts).
func (a *Agent) recordTaskEvent(name, description, report string, taskErr error) {
	eng := a.MemoryEngine()
	if eng == nil || a.subagent {
		return
	}
	outcome := "success"
	if taskErr != nil {
		outcome = "error"
	}
	action := "task:" + description
	if len(action) > 200 {
		action = action[:200]
	}
	obs := report
	if len(obs) > 500 {
		obs = obs[:500]
	}
	_ = eng.RecordEvent(context.Background(), a.memoryWorkspace(), "subagent:"+name, action, obs, outcome)
}
```

`task.go` 两处调用：

(a) 后台路径 goroutine 内（`t.setFinal(TaskCompleted, report)` 之前）：

```go
			a.recordTaskEvent(subagentName(def, args), args.Description, report, err)
```

(b) 同步路径（`return coretools.Result{Content: report}, nil` 之前）：

```go
	a.recordTaskEvent(subagentName(def, args), args.Description, report, nil)
```

(c) `task.go` 追加辅助：

```go
// subagentName resolves the display name for task events.
func subagentName(def *agents.Definition, args taskArgs) string {
	if def != nil && def.Name != "" {
		return def.Name
	}
	if args.SubagentType != "" {
		return args.SubagentType
	}
	return "general"
}
```

（`agents.Definition` 的字段名以 `internal/agents/agents.go` 实际定义为准——先 `grep -n 'type Definition' internal/agents/agents.go` 核对 Name 字段；`def = &d` 处可见字段用法。）

- [ ] **Step 4: 跑测试 + 全量回归** — `go test ./... ` 全 PASS；`go vet ./...` 干净。

- [ ] **Step 5: Commit** — `git add internal/agent/ && git commit -m "feat(agent): record subagent task completion events into memory"`

---

## 最终验收清单（全部任务完成后）

- [ ] `go test ./...` 全绿；`cd web && npm run build` 成功；`go build ./...` 成功
- [ ] 端到端（TUI）：

```bash
go build -o /tmp/woca-final ./cmd/woca
cd /tmp && mkdir -p woca-e2e && cd woca-e2e
/tmp/woca-final   # REPL
# ① 输入：记住一件事：这个项目用 make build 构建（观察模型自动调 remember_fact 或退出后 auto-retain）
# ② /memory stats      -> 至少 1 条 L2 + 1 条 L1
# ③ /memory recall 构建 -> 命中
# ④ /settings set memory-engine none; /memory stats -> 提示 disabled
# ⑤ /settings set memory-engine ladym; /memory stats -> 恢复（实时生效）
```

- [ ] 端到端（web）：serve 后同一 workspace 两个 session —— session A 教一件事 → 新 session B 首轮提问收到 `<relevant-memory>`（可在 debug-log 里核对注入）；title bar Brain 面板可搜索/手动记住/忘记；设置页改 ladym-topk 保存后立即生效。
- [ ] subagent：`@general 帮我看看这个目录结构` 跑完后 `/memory stats` 的 L1 增长（task 事件）。
- [ ] 更新 `docs/agent-loop-comparison.md` §6 的 extractMemories 缺口标注为已对齐（一句话 + 指向本 spec）。

## 已知边界（不修，记录在案）

- 换 embedding provider 时 PUT 同步 re-embed 大库会慢（loading 态覆盖，spec §13.2）。
- 60s 延迟关旧引擎窗口内飞行中 run 丢一轮记忆注入（不 crash，spec §13.3）。
- `ladyM-main-fix` 目录与本计划无关——所有 LadyM 改动只进 `ladyM` 主 checkout。



