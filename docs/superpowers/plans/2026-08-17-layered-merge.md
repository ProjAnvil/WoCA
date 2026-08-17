# 分层 Merge（layered-merge）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 skills / agents / MCP / CLAUDE.md 的来源层建模为带优先级的合并（flag > workspace > user），消除隐式首命中行为，修复 MCP 同名重复连接，并为 CLAUDE.md 增加 user 级与祖先目录注入。

**Architecture:** 新增零依赖泛型包 `internal/merge`（`Entry[T]` + `Entries()` 按 Level/Order 决胜并返回冲突列表；`WorkspaceDirs()` 从 workDir 向上走到 git root，兼容 worktree 的 `.git` 文件，结果按 workDir 缓存）。`cmd/woca` 的 `loadSkills` / `loadAgents` / MCP spec 组装改为构造 Entry 后走 merge，冲突写 zerolog 结构化日志；`internal/prompt` 新增 `collectMemoryFiles` 实现 user 级（`~/.woca/CLAUDE.md`，fallback `~/.claude/CLAUDE.md`）+ gitRoot→workDir 逐目录（`CLAUDE.md` → `AGENTS.md` → `.claude/CLAUDE.md`）拼接。

**Tech Stack:** Go 1.26（泛型、`sync.Map` 缓存）、zerolog、现有 `internal/skills` / `internal/agents` / `internal/claudeenv` 加载器。

**设计稿：** `docs/superpowers/specs/2026-08-17-layered-merge.md`（决策 D1–D6）。注意 D2 为 **Breaking**：skills/agents 同名从"user 首命中胜出"翻转为"workspace 胜出"。

**Level 数值方向说明：** 设计稿里 `LevelFlag = iota` 在前但语义"最高"。为让"数值大者胜"直觉成立，本实现取 `LevelUser=0 < LevelWorkspace=1 < LevelFlag=2`，语义与设计稿一致（flag 最高）。

**对设计稿的已声明偏离（D6）：** 冲突日志写 **zerolog 进程结构化日志**（`config merge conflict resolved`），不写 obslog。理由：obslog 是默认关闭的落盘诊断日志（`~/.woca/logs`），而 merge 冲突属于用户应当即时看到的信息，主进程日志才是默认可见通道；obslog 内建 `obs` 在 `buildAgent` 中构造于 loadSkills 首次调用（约 1001 行）之后，接入需搬迁构造点，收益不成比例。obslog / UI 面板随设计稿"未来项"一并考虑。

---

## 文件结构总览

| 文件 | 动作 | 职责 |
| --- | --- | --- |
| `internal/merge/merge.go` | 新建 | Level / Entry[T] / Conflict / Entries[T] 泛型合并 |
| `internal/merge/workspace.go` | 新建 | WorkspaceDirs：向上遍历到 git root + 缓存 |
| `internal/merge/merge_test.go` | 新建 | 合并核心测试 |
| `internal/merge/workspace_test.go` | 新建 | 遍历与缓存测试 |
| `internal/skills/skills.go` | 修改 | Skill 增加 Source 字段（冲突日志需要真实路径） |
| `cmd/woca/main.go` | 修改 | loadSkills/loadAgents 分层重写、mergeMCPSpecs、MCP 组装、app 增加 log 字段、logMergeConflicts |
| `cmd/woca/merge_layers_test.go` | 新建 | 分层加载与 MCP 合并测试 |
| `internal/prompt/memoryfiles.go` | 新建 | memoryFile / collectMemoryFiles / fileExists |
| `internal/prompt/prompt.go` | 修改 | BuildWithStyle 改用 collectMemoryFiles |
| `internal/prompt/outputstyles.go` | 修改 | memoryFileSection → memorySection（按绝对路径） |
| `internal/prompt/*_test.go` | 修改 | 新测试 + 既有测试 HOME 隔离 |
| `README.md` | 修改 | Breaking changes 说明 |

---

### Task 1: internal/merge 核心泛型合并

**Files:**
- Create: `internal/merge/merge.go`
- Test: `internal/merge/merge_test.go`

- [x] **Step 1: 写失败测试**

创建 `internal/merge/merge_test.go`：

```go
package merge

import "testing"

func TestEntriesLevelWins(t *testing.T) {
	out, conflicts := Entries([]Entry[string]{
		{Name: "a", Value: "user", Level: LevelUser, Source: "~/.woca"},
		{Name: "a", Value: "ws", Level: LevelWorkspace, Source: "work/.woca"},
		{Name: "a", Value: "flag", Level: LevelFlag, Source: "flag"},
	})
	if len(out) != 1 || out[0].Value != "flag" {
		t.Fatalf("flag layer must win, got %+v", out)
	}
	if len(conflicts) != 2 {
		t.Fatalf("expected 2 conflicts, got %+v", conflicts)
	}
	for _, c := range conflicts {
		if c.Name != "a" || c.Winner == c.Loser {
			t.Fatalf("bad conflict record: %+v", c)
		}
	}
}

func TestEntriesOrderBreaksTies(t *testing.T) {
	out, conflicts := Entries([]Entry[string]{
		{Name: "a", Value: "far", Level: LevelWorkspace, Order: 4, Source: "root"},
		{Name: "a", Value: "near", Level: LevelWorkspace, Order: 0, Source: "workdir"},
	})
	if len(out) != 1 || out[0].Value != "near" {
		t.Fatalf("smaller Order must win, got %+v", out)
	}
	if len(conflicts) != 1 || conflicts[0].Winner != "workdir" || conflicts[0].Loser != "root" {
		t.Fatalf("bad conflict: %+v", conflicts)
	}
}

func TestEntriesDeterministicOutput(t *testing.T) {
	out, _ := Entries([]Entry[int]{
		{Name: "b", Value: 1, Level: LevelUser, Order: 0, Source: "u"},
		{Name: "a", Value: 2, Level: LevelWorkspace, Order: 3, Source: "w"},
		{Name: "c", Value: 3, Level: LevelWorkspace, Order: 1, Source: "w"},
	})
	var got []string
	for _, e := range out {
		got = append(got, e.Name)
	}
	// 高 Level 在前；同 Level 内 Order 小者在前
	if len(got) != 3 || got[0] != "c" || got[1] != "a" || got[2] != "b" {
		t.Fatalf("unexpected order: %v", got)
	}
}

func TestEntriesSkipsEmptyNames(t *testing.T) {
	out, conflicts := Entries([]Entry[string]{
		{Name: "", Value: "x", Level: LevelUser, Source: "s"},
		{Name: "a", Value: "y", Level: LevelUser, Source: "s"},
	})
	if len(out) != 1 || out[0].Name != "a" {
		t.Fatalf("empty names must be dropped: %+v", out)
	}
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %+v", conflicts)
	}
}

func TestLevelString(t *testing.T) {
	for _, tc := range []struct {
		l    Level
		want string
	}{{LevelUser, "user"}, {LevelWorkspace, "workspace"}, {LevelFlag, "flag"}} {
		if got := tc.l.String(); got != tc.want {
			t.Fatalf("Level(%d).String() = %q, want %q", tc.l, got, tc.want)
		}
	}
}
```

- [x] **Step 2: 跑测试确认失败**

Run: `go test ./internal/merge/ -v`
Expected: 编译失败 `undefined: Entries` / `undefined: Level`（包不存在或符号缺失）。

- [x] **Step 3: 实现**

创建 `internal/merge/merge.go`：

```go
// Package merge implements layered configuration merging for skills,
// agents and MCP servers: entries from higher-priority levels (flag >
// workspace > user) win, same-name conflicts are recorded for observability,
// and the merged output order is deterministic. The package stays free of
// woca-internal imports so it stays trivially testable.
package merge

import "sort"

// Level is a configuration source layer. Higher values win over lower ones.
type Level int

const (
	LevelUser      Level = iota // ~/.woca (user level)
	LevelWorkspace              // workDir and its ancestors up to the git root
	LevelFlag                   // session injection (--mcp etc.), highest
)

// String returns the log-friendly layer name.
func (l Level) String() string {
	switch l {
	case LevelFlag:
		return "flag"
	case LevelWorkspace:
		return "workspace"
	default:
		return "user"
	}
}

// Entry is one named candidate from a source layer.
type Entry[T any] struct {
	Name   string
	Value  T
	Level  Level
	Order  int    // tiebreaker within a Level; smaller wins
	Source string // file path or "flag", for conflict logs
}

// Conflict records one same-name resolution. Winner and Loser are Sources.
type Conflict struct {
	Name   string
	Winner string
	Loser  string
}

// Entries merges es by Name: higher Level wins, then smaller Order, then
// lexicographic Source (total determinism). The result is sorted by
// (Level desc, Order asc, Name asc); every same-name collision appends one
// Conflict. Entries with an empty Name are dropped.
func Entries[T any](es []Entry[T]) ([]Entry[T], []Conflict) {
	byName := make(map[string]Entry[T], len(es))
	var conflicts []Conflict
	for _, e := range es {
		if e.Name == "" {
			continue
		}
		prev, ok := byName[e.Name]
		if !ok {
			byName[e.Name] = e
			continue
		}
		winner, loser := pick(prev, e)
		byName[e.Name] = winner
		conflicts = append(conflicts, Conflict{Name: e.Name, Winner: winner.Source, Loser: loser.Source})
	}
	out := make([]Entry[T], 0, len(byName))
	for _, e := range byName {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Level != out[j].Level {
			return out[i].Level > out[j].Level
		}
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Name < out[j].Name
	})
	return out, conflicts
}

// pick returns the winning and losing entry (winner first).
func pick[T any](a, b Entry[T]) (Entry[T], Entry[T]) {
	if a.Level != b.Level {
		if a.Level > b.Level {
			return a, b
		}
		return b, a
	}
	if a.Order != b.Order {
		if a.Order < b.Order {
			return a, b
		}
		return b, a
	}
	if a.Source <= b.Source {
		return a, b
	}
	return b, a
}
```

- [x] **Step 4: 跑测试确认通过**

Run: `go test ./internal/merge/ -v`
Expected: 全部 PASS。

- [x] **Step 5: Commit**

```bash
git add internal/merge/
git commit -m "feat(merge): layered merge core with conflict recording"
```

---

### Task 2: merge.WorkspaceDirs（向上遍历到 git root + 缓存）

**Files:**
- Create: `internal/merge/workspace.go`
- Test: `internal/merge/workspace_test.go`

- [x] **Step 1: 写失败测试**

创建 `internal/merge/workspace_test.go`：

```go
package merge

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWorkspaceDirsStopsAtGitRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	ResetWorkspaceCache()
	got := WorkspaceDirs(deep)
	want := []string{deep, filepath.Join(root, "a"), root}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestWorkspaceDirsWorktreeGitFile(t *testing.T) {
	main := t.TempDir()
	if err := os.MkdirAll(filepath.Join(main, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(main, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: ../.git/worktrees/wt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(wt, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ResetWorkspaceCache()
	got := WorkspaceDirs(sub)
	want := []string{sub, wt}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("worktree .git file must be recognized, got %v, want %v", got, want)
	}
}

func TestWorkspaceDirsNonGitOnlyWorkDir(t *testing.T) {
	dir := t.TempDir()
	ResetWorkspaceCache()
	got := WorkspaceDirs(dir)
	if len(got) != 1 || got[0] != dir {
		t.Fatalf("non-git dir must yield itself only, got %v", got)
	}
}

func TestWorkspaceDirsReturnsCopy(t *testing.T) {
	dir := t.TempDir()
	ResetWorkspaceCache()
	WorkspaceDirs(dir)
	got := WorkspaceDirs(dir)
	got[0] = "/mutated"
	if again := WorkspaceDirs(dir); again[0] != dir {
		t.Fatalf("cache was mutated: %v", again)
	}
}
```

- [x] **Step 2: 跑测试确认失败**

Run: `go test ./internal/merge/ -v`
Expected: 编译失败 `undefined: WorkspaceDirs` / `undefined: ResetWorkspaceCache`。

- [x] **Step 3: 实现**

创建 `internal/merge/workspace.go`：

```go
package merge

import (
	"os"
	"path/filepath"
	"sync"
)

// workspaceCache memoizes WorkspaceDirs per workDir: serve mode resolves a
// chain per session workspace and rebuilds prompts often, so the walk (and
// its stats) must not repeat. Entries never expire within the process
// lifetime — a workspace that runs `git init` later keeps its old chain
// until restart (accepted per design §8 "启动时算一次并缓存").
var workspaceCache sync.Map // workDir -> []string

// WorkspaceDirs returns the workspace directory chain for workDir: workDir
// first, then each ancestor up to and including the git root. A .git
// directory or the file git worktrees use both mark the root; outside a
// repository the chain is workDir alone (D3).
func WorkspaceDirs(workDir string) []string {
	if v, ok := workspaceCache.Load(workDir); ok {
		return append([]string(nil), v.([]string)...)
	}
	chain := workspaceDirs(workDir)
	workspaceCache.Store(workDir, chain)
	return append([]string(nil), chain...)
}

// ResetWorkspaceCache drops cached WorkspaceDirs results (tests).
func ResetWorkspaceCache() {
	workspaceCache = sync.Map{}
}

func workspaceDirs(workDir string) []string {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		abs = workDir
	}
	root := findGitRoot(abs)
	var chain []string
	for dir := abs; ; dir = filepath.Dir(dir) {
		chain = append(chain, dir)
		if root == "" || dir == root || dir == filepath.Dir(dir) {
			break
		}
	}
	return chain
}

// findGitRoot walks up from dir looking for .git (directory, or the file
// git worktrees use). It returns "" when no repository is found.
func findGitRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
```

- [x] **Step 4: 跑测试确认通过**

Run: `go test ./internal/merge/ -v`
Expected: 全部 PASS。

- [x] **Step 5: Commit**

```bash
git add internal/merge/
git commit -m "feat(merge): workspace dir chain walk up to git root"
```

---

### Task 3: skills.Skill 增加 Source 字段

冲突日志需要每个 skill 的真实文件路径（目录名 ≠ frontmatter 覆盖后的 skill 名）。

**Files:**
- Modify: `internal/skills/skills.go`（Skill 结构体约 15 行、Load 内 append 约 56 行）
- Test: `internal/skills/skills_test.go`（追加测试）

- [x] **Step 1: 写失败测试**

在 `internal/skills/skills_test.go` 末尾追加：

```go
func TestLoadSetsSource(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "my-skill")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := Load(root)
	if err != nil || len(list) != 1 {
		t.Fatalf("load: %v %+v", err, list)
	}
	if want := filepath.Join(dir, "SKILL.md"); list[0].Source != want {
		t.Fatalf("Source = %q, want %q", list[0].Source, want)
	}
}
```

（若文件缺少 `os` / `path/filepath` / `testing` import 则补上。）

- [x] **Step 2: 跑测试确认失败**

Run: `go test ./internal/skills/ -run TestLoadSetsSource -v`
Expected: 编译失败 `list[0].Source undefined`。

- [x] **Step 3: 实现**

`internal/skills/skills.go` 中 Skill 结构体改为：

```go
// Skill is a loaded skill: a name, a one-line description, and the markdown
// body that is expanded into the prompt when the skill is invoked.
type Skill struct {
	Name        string
	Description string
	Body        string
	Args        []SkillArg
	// Source is the absolute path of the SKILL.md file (layered-merge
	// conflict logs; empty for hand-constructed values).
	Source string
}
```

Load 内（原 `out = append(out, parseSkill(filepath.Base(dir), data))`）改为：

```go
			sk := parseSkill(filepath.Base(dir), data)
			sk.Source = path
			out = append(out, sk)
```

- [x] **Step 4: 跑测试确认通过**

Run: `go test ./internal/skills/ -v`
Expected: 全部 PASS（含既有测试）。

- [x] **Step 5: Commit**

```bash
git add internal/skills/
git commit -m "feat(skills): record SKILL.md source path on Skill"
```

---

### Task 4: loadSkills 分层重写

**Files:**
- Modify: `cmd/woca/main.go`（loadSkills 约 1664-1681 行；调用点约 984、1001、1165、1375 行；app 结构体约 821 行；buildAgent 返回约 1200 行）
- Test: `cmd/woca/merge_layers_test.go`（新建）

- [x] **Step 1: 写失败测试**

创建 `cmd/woca/merge_layers_test.go`：

```go
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/merge"
	"github.com/projanvil/woca/internal/skills"
)

// writeFileT creates path's parent dirs and writes content.
func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSkillsLayered(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b")
	writeFileT(t, filepath.Join(root, ".git", ".keep"), "")
	writeFileT(t, filepath.Join(home, ".woca", "skills", "dup", "SKILL.md"), "USER SKILL")
	writeFileT(t, filepath.Join(root, ".claude", "skills", "dup", "SKILL.md"), "ROOT CLAUDE")
	writeFileT(t, filepath.Join(deep, ".woca", "skills", "dup", "SKILL.md"), "DEEP WOCA")
	writeFileT(t, filepath.Join(deep, ".claude", "skills", "dup", "SKILL.md"), "DEEP CLAUDE")
	writeFileT(t, filepath.Join(home, ".woca", "skills", "only-user", "SKILL.md"), "USER ONLY")
	merge.ResetWorkspaceCache()
	t.Cleanup(merge.ResetWorkspaceCache)

	var buf bytes.Buffer
	list := loadSkills(deep, zerolog.New(&buf))

	byName := map[string]skills.Skill{}
	for _, s := range list {
		byName[s.Name] = s
	}
	if got := byName["dup"]; len(got.Body) == 0 || got.Body != "DEEP WOCA" {
		t.Fatalf("dup winner must be workDir .woca/skills (D2 breaking flip), got %q", got.Body)
	}
	if got := byName["only-user"]; got.Body != "USER ONLY" {
		t.Fatalf("user-only skill missing: %+v", list)
	}
	logs := buf.String()
	if !strings.Contains(logs, "config merge conflict resolved") {
		t.Fatalf("conflicts not logged: %q", logs)
	}
	if !strings.Contains(logs, filepath.Join(deep, ".woca", "skills", "dup", "SKILL.md")) {
		t.Fatalf("winner source not in logs: %q", logs)
	}
}

func TestLoadSkillsSameDirWocaBeatsClaude(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir() // 无 .git：只看本目录
	writeFileT(t, filepath.Join(dir, ".woca", "skills", "dup", "SKILL.md"), "WOCA")
	writeFileT(t, filepath.Join(dir, ".claude", "skills", "dup", "SKILL.md"), "CLAUDE")
	merge.ResetWorkspaceCache()
	t.Cleanup(merge.ResetWorkspaceCache)

	list := loadSkills(dir, zerolog.Nop())
	if len(list) != 1 || list[0].Body != "WOCA" {
		t.Fatalf("same-dir .woca must beat .claude, got %+v", list)
	}
}
```

- [x] **Step 2: 跑测试确认失败**

Run: `go test ./cmd/woca/ -run TestLoadSkills -v`
Expected: 编译失败 `loadSkills(deep, zerolog.New(&buf))` 参数不匹配（现在只接受 1 个参数）。

- [x] **Step 3: 实现**

`cmd/woca/main.go`：

1. import 增加 `"github.com/projanvil/woca/internal/merge"`。

2. `loadSkills` 整体替换为：

```go
// loadSkills loads skills from layered directories: workspace roots (workDir
// up to the git root, .woca/skills before .claude/skills in each directory)
// win over the user root ~/.woca/skills. Same-name skills resolve to the
// higher layer — workspace beats user, which reverses the old first-hit
// semantics where the user root (loaded first) silently won.
func loadSkills(workdir string, log zerolog.Logger) []skills.Skill {
	var es []merge.Entry[skills.Skill]
	for i, dir := range merge.WorkspaceDirs(workdir) {
		for j, sub := range []string{".woca", ".claude"} {
			root := filepath.Join(dir, sub, "skills")
			list, err := skills.Load(root)
			if err != nil || len(list) == 0 {
				continue
			}
			for _, s := range list {
				es = append(es, merge.Entry[skills.Skill]{
					Name: s.Name, Value: s, Level: merge.LevelWorkspace,
					Order: i*2 + j, Source: s.Source,
				})
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if list, err := skills.Load(filepath.Join(home, ".woca", "skills")); err == nil {
			for _, s := range list {
				es = append(es, merge.Entry[skills.Skill]{
					Name: s.Name, Value: s, Level: merge.LevelUser,
					Order: 0, Source: s.Source,
				})
			}
		}
	}
	merged, conflicts := merge.Entries(es)
	logMergeConflicts(log, "skill", conflicts)
	out := make([]skills.Skill, 0, len(merged))
	for _, e := range merged {
		out = append(out, e.Value)
	}
	return out
}

// logMergeConflicts emits one structured log line per layered-merge
// conflict (D6: observability without a UI panel).
func logMergeConflicts(log zerolog.Logger, kind string, conflicts []merge.Conflict) {
	for _, c := range conflicts {
		log.Info().Str("kind", kind).Str("name", c.Name).
			Str("winner", c.Winner).Str("loser", c.Loser).
			Msg("config merge conflict resolved")
	}
}
```

3. 更新全部调用点（`log` 为 buildAgent 的 zerolog 参数）：
   - `sysFor` 内（约 984 行）：`if list := loadSkills(ws, log); len(list) > 0 {`
   - 约 1001 行：`skillList := loadSkills(workdir, log)`
   - 约 1165 行：`SkillsFor: func(ws string) []skills.Skill { return loadSkills(ws, log) },`
   - `applyClaudeEnv` 内（约 1375 行）：`list := loadSkills(ap.workdir, ap.log)`

4. app 结构体（约 821 行）增加字段，buildAgent 返回值（约 1200 行）带上：

```go
	// log is the process logger, used by post-build reloads
	// (applyClaudeEnv) for merge-conflict lines.
	log zerolog.Logger
```

```go
	return &app{a: a, cfg: cfg, opts: opts, settings: st, memory: memory, memEngine: memEngine, workdir: workdir, pluginMgr: pluginMgr, log: log}, nil
```

5. 全仓修正其余 `loadSkills(` 编译错误（grep 确认）：`grep -n "loadSkills(" cmd/woca/*.go internal/ -r`。serve 侧 `internal/runws` 如透传的是 agent.Config 的 SkillsFor 闭包则无需改动。测试文件同步两件事（已知调用点：`cmd/woca/main_test.go:93` 的 `TestLoadSkills`、`cmd/woca/coverage2_test.go:406/409`、Task 5 涉及 `cmd/woca/helpers_test.go:489` 的 `TestLoadAgents`）：
   - 签名：旧调用改为传 `zerolog.Nop()`；
   - **HOME 隔离（必须）**：`TestLoadSkills`（`main_test.go:85` 起）当前依赖真实 `~/.woca/skills` 为空——本机该目录非空时测试**确定性失败**（不是 flaky）。给该测试（以及防御性地给 `TestLoadAgents`）加 `t.Setenv("HOME", t.TempDir())`，使其只看到 fixture 目录。

- [x] **Step 4: 跑测试确认通过**

Run: `go test ./cmd/woca/ -run TestLoadSkills -v && go build ./...`
Expected: 新测试 PASS，构建成功。

- [x] **Step 5: Commit**

```bash
git add cmd/woca/ internal/
git commit -m "feat(skills): layered workspace-over-user loading with conflict logs"
```

---

### Task 5: loadAgents 分层重写

**Files:**
- Modify: `cmd/woca/main.go`（loadAgents 约 1694-1708 行；调用点约 1002、1171 行）
- Test: `cmd/woca/merge_layers_test.go`（追加）

- [x] **Step 1: 写失败测试**

在 `cmd/woca/merge_layers_test.go` 追加：

```go
func TestLoadAgentsLayered(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	deep := filepath.Join(root, "a")
	writeFileT(t, filepath.Join(root, ".git", ".keep"), "")
	writeFileT(t, filepath.Join(home, ".woca", "agents", "dup.md"), "---\nname: dup\ndescription: user\n---\nUSER AGENT")
	writeFileT(t, filepath.Join(deep, ".woca", "agents", "dup.md"), "---\nname: dup\ndescription: ws\n---\nWS AGENT")
	writeFileT(t, filepath.Join(home, ".woca", "agents", "only-user.md"), "---\nname: only-user\ndescription: u\n---\nONLY USER")
	merge.ResetWorkspaceCache()
	t.Cleanup(merge.ResetWorkspaceCache)

	var buf bytes.Buffer
	list := loadAgents(deep, zerolog.New(&buf))

	byName := map[string]string{}
	for _, d := range list {
		byName[d.Name] = d.Prompt
	}
	if byName["dup"] != "WS AGENT" {
		t.Fatalf("dup winner must be workspace, got %q", byName["dup"])
	}
	if byName["only-user"] != "ONLY USER" {
		t.Fatalf("user-only agent missing: %+v", list)
	}
	if !strings.Contains(buf.String(), "config merge conflict resolved") {
		t.Fatalf("conflicts not logged: %q", buf.String())
	}
}
```

（本测试只用 `d.Name` / `d.Prompt` 字段，无需额外 import `internal/agents`。）

- [x] **Step 2: 跑测试确认失败**

Run: `go test ./cmd/woca/ -run TestLoadAgents -v`
Expected: 编译失败 `loadAgents` 参数不匹配。

- [x] **Step 3: 实现**

`loadAgents` 整体替换为（agents.Definition 已有 Source 字段；按 D1 只读 `.woca/agents`，不新增 `.claude/agents`）：

```go
// loadAgents loads custom subagent definitions from layered directories:
// workDir/.woca/agents plus every ancestor's up to the git root, winning
// over the user root ~/.woca/agents on name conflicts.
func loadAgents(workdir string, log zerolog.Logger) []agents.Definition {
	var es []merge.Entry[agents.Definition]
	for i, dir := range merge.WorkspaceDirs(workdir) {
		list, err := agents.Load(filepath.Join(dir, ".woca", "agents"))
		if err != nil || len(list) == 0 {
			continue
		}
		for _, d := range list {
			es = append(es, merge.Entry[agents.Definition]{
				Name: d.Name, Value: d, Level: merge.LevelWorkspace,
				Order: i, Source: d.Source,
			})
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if list, err := agents.Load(filepath.Join(home, ".woca", "agents")); err == nil {
			for _, d := range list {
				es = append(es, merge.Entry[agents.Definition]{
					Name: d.Name, Value: d, Level: merge.LevelUser,
					Order: 0, Source: d.Source,
				})
			}
		}
	}
	merged, conflicts := merge.Entries(es)
	logMergeConflicts(log, "agent", conflicts)
	out := make([]agents.Definition, 0, len(merged))
	for _, e := range merged {
		out = append(out, e.Value)
	}
	return out
}
```

调用点更新：
- 约 1002 行：`agentDefs := loadAgents(workdir, log)`
- 约 1171 行：`AgentsFor: func(ws string) []agents.Definition { return loadAgents(ws, log) },`
- `helpers_test.go` 的 `TestLoadAgents`（约 489 行）：改传 `zerolog.Nop()` 并加 `t.Setenv("HOME", t.TempDir())`（同 Task 4 Step 3.5 的隔离要求）；其余引用 grep `loadAgents(` 后同方式修正。

- [x] **Step 4: 跑测试确认通过**

Run: `go test ./cmd/woca/ -run TestLoadAgents -v && go build ./...`
Expected: PASS，构建成功。

- [x] **Step 5: Commit**

```bash
git add cmd/woca/
git commit -m "feat(agents): layered workspace-over-user loading with conflict logs"
```

---

### Task 6: MCP 三层合并（修同名重复连接）

**Files:**
- Modify: `cmd/woca/main.go`（buildAgent 内 MCP 组装约 1013-1020 行；新增 mergeMCPSpecs）
- Test: `cmd/woca/merge_layers_test.go`（追加）

- [x] **Step 1: 写失败测试**

在 `cmd/woca/merge_layers_test.go` 追加：

```go
func TestMergeMCPSpecs(t *testing.T) {
	var buf bytes.Buffer
	got := mergeMCPSpecs(
		[]string{"dup=flag-cmd", "flagonly=flag-cmd", "dup2=flag-a", "dup2=flag-b", "bad-no-eq"},
		[]string{"dup=ws-cmd", "wsonly=ws-cmd"},
		[]string{"dup=user-cmd", "useronly=user-cmd"},
		zerolog.New(&buf),
	)
	sort.Strings(got)
	want := []string{"dup=flag-cmd", "dup2=flag-a", "flagonly=flag-cmd", "useronly=user-cmd", "wsonly=ws-cmd"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	logs := buf.String()
	if !strings.Contains(logs, "config merge conflict resolved") {
		t.Fatalf("conflicts not logged: %q", logs)
	}
	if !strings.Contains(logs, "invalid mcp spec") {
		t.Fatalf("invalid spec not warned: %q", logs)
	}
}
```

（需补 import `"reflect"` 与 `"sort"`。）

- [x] **Step 2: 跑测试确认失败**

Run: `go test ./cmd/woca/ -run TestMergeMCPSpecs -v`
Expected: 编译失败 `undefined: mergeMCPSpecs`。

- [x] **Step 3: 实现**

`cmd/woca/main.go` 新增（放在 connectMCP 附近）：

```go
// mergeMCPSpecs deduplicates name=command MCP specs across the three layers:
// --mcp flags beat workDir/.woca/mcp.json beat ~/.woca/mcp.json (D5/D6).
// Same-layer duplicates keep the first occurrence; invalid specs are warned
// and dropped. Connecting the merged list fixes the same-name
// double-connect bug.
func mergeMCPSpecs(flag, workspace, user []string, log zerolog.Logger) []string {
	layer := func(specs []string, lvl merge.Level) []merge.Entry[string] {
		var es []merge.Entry[string]
		for i, spec := range specs {
			name, _, ok := strings.Cut(spec, "=")
			if !ok || strings.TrimSpace(name) == "" {
				log.Warn().Str("spec", spec).Str("layer", lvl.String()).Msg("invalid mcp spec, expected name=command")
				continue
			}
			es = append(es, merge.Entry[string]{
				Name: name, Value: spec, Level: lvl, Order: i,
				Source: lvl.String(),
			})
		}
		return es
	}
	es := append(append(layer(flag, merge.LevelFlag), layer(workspace, merge.LevelWorkspace)...), layer(user, merge.LevelUser)...)
	merged, conflicts := merge.Entries(es)
	logMergeConflicts(log, "mcp", conflicts)
	out := make([]string, 0, len(merged))
	for _, e := range merged {
		out = append(out, e.Value)
	}
	return out
}
```

buildAgent 内（原 1013-1020 行）替换为：

```go
	// MCP servers resolve as three layers: --mcp flags (highest), the
	// workspace file workDir/.woca/mcp.json (no upward walk, D5), and the
	// user ~/.woca/mcp.json kept in sync with the Claude Code import
	// selection. Same-name servers connect once, from the winning layer.
	var userSpecs []string
	if specs, err := claudeMCPStartupSpecs(wocaRoot(), dbSettings.ClaudeMCP); err != nil {
		log.Warn().Err(err).Msg("claude mcp import failed")
	} else if len(specs) > 0 {
		log.Info().Strs("servers", specs).Msg("imported claude mcp servers via ~/.woca/mcp.json")
		userSpecs = specs
	}
	wsSpecs, err := claudeenv.LoadMCPConfig(filepath.Join(workdir, ".woca"))
	if err != nil {
		log.Warn().Err(err).Msg("workspace mcp.json unreadable")
	}
	extraTools, mcpClients := connectMCP(ctx, log, mergeMCPSpecs(opts.mcpServers, wsSpecs, userSpecs, log), st)
```

- [x] **Step 4: 跑测试确认通过**

Run: `go test ./cmd/woca/ -run TestMergeMCPSpecs -v && go build ./...`
Expected: PASS，构建成功。

- [x] **Step 5: Commit**

```bash
git add cmd/woca/
git commit -m "fix(mcp): three-layer spec merge, connect same-name server once"
```

---

### Task 7: CLAUDE.md user 级 + 向上遍历拼接

**Files:**
- Create: `internal/prompt/memoryfiles.go`
- Modify: `internal/prompt/prompt.go`（BuildWithStyle 内存文件循环约 131-135 行）
- Modify: `internal/prompt/outputstyles.go`（memoryFileSection 约 103-123 行）
- Test: `internal/prompt/memoryfiles_test.go`（新建）；`internal/prompt/prompt_test.go`、`internal/prompt/coverage_test.go`、`internal/prompt/outputstyles_test.go`（HOME 隔离与签名更新）

- [x] **Step 1: 写失败测试**

创建 `internal/prompt/memoryfiles_test.go`：

```go
package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/merge"
)

func writeMem(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	merge.ResetWorkspaceCache()
	t.Cleanup(merge.ResetWorkspaceCache)
	return home
}

func pathsOf(files []memoryFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

func TestCollectMemoryFilesOrder(t *testing.T) {
	home := isolatedHome(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mid := filepath.Join(root, "pkg")
	if err := os.MkdirAll(mid, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMem(t, filepath.Join(home, ".woca", "CLAUDE.md"), "user rules")
	writeMem(t, filepath.Join(root, "CLAUDE.md"), "root rules")
	writeMem(t, filepath.Join(root, "AGENTS.md"), "root agents")
	writeMem(t, filepath.Join(mid, "CLAUDE.md"), "mid rules")
	writeMem(t, filepath.Join(mid, ".claude", "CLAUDE.md"), "mid claude")

	got := pathsOf(collectMemoryFiles(mid))
	want := []string{
		filepath.Join(home, ".woca", "CLAUDE.md"),
		filepath.Join(root, "CLAUDE.md"),
		filepath.Join(root, "AGENTS.md"),
		filepath.Join(mid, "CLAUDE.md"),
		filepath.Join(mid, ".claude", "CLAUDE.md"),
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order mismatch:\ngot  %v\nwant %v", got, want)
		}
	}
}

func TestCollectMemoryFilesUserFallback(t *testing.T) {
	home := isolatedHome(t)
	writeMem(t, filepath.Join(home, ".claude", "CLAUDE.md"), "claude user rules")
	got := pathsOf(collectMemoryFiles(t.TempDir()))
	if len(got) != 1 || got[0] != filepath.Join(home, ".claude", "CLAUDE.md") {
		t.Fatalf("fallback to ~/.claude/CLAUDE.md failed: %v", got)
	}
}

func TestCollectMemoryFilesNoGit(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	writeMem(t, filepath.Join(dir, "CLAUDE.md"), "local rules")
	writeMem(t, filepath.Join(filepath.Dir(dir), "CLAUDE.md"), "parent must be ignored")
	got := pathsOf(collectMemoryFiles(dir))
	if len(got) != 1 || got[0] != filepath.Join(dir, "CLAUDE.md") {
		t.Fatalf("non-git dir must only read itself: %v", got)
	}
}

func TestBuildInjectsAllLayersInOrder(t *testing.T) {
	home := isolatedHome(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMem(t, filepath.Join(home, ".woca", "CLAUDE.md"), "ZZ_USER")
	writeMem(t, filepath.Join(root, "CLAUDE.md"), "ZZ_ROOT")
	writeMem(t, filepath.Join(root, "AGENTS.md"), "ZZ_ROOT_AGENTS")
	got := Build(filepath.Join(root), "", "")
	iu := strings.Index(got, "ZZ_USER")
	ir := strings.Index(got, "ZZ_ROOT")
	ira := strings.Index(got, "ZZ_ROOT_AGENTS")
	if iu < 0 || ir < 0 || ira < 0 {
		t.Fatalf("missing layers in prompt:\n%s", got)
	}
	if !(iu < ir && ir < ira) {
		t.Fatalf("user must precede root; CLAUDE.md precede AGENTS.md (iu=%d ir=%d ira=%d)", iu, ir, ira)
	}
	if !strings.Contains(got, "# User instructions (CLAUDE.md)") {
		t.Fatalf("user header missing:\n%s", got)
	}
}
```

- [x] **Step 2: 跑测试确认失败**

Run: `go test ./internal/prompt/ -run 'TestCollectMemoryFiles|TestBuildInjects' -v`
Expected: 编译失败 `undefined: collectMemoryFiles` / `undefined: memoryFile`。

- [x] **Step 3: 实现**

创建 `internal/prompt/memoryfiles.go`：

```go
package prompt

import (
	"os"
	"path/filepath"

	"github.com/projanvil/woca/internal/merge"
)

// memoryFile is one instruction file to inject into the system prompt.
type memoryFile struct {
	Path  string // absolute path on disk
	Kind  string // header kind: "User" or "Project"
	Label string // header label, e.g. "CLAUDE.md" or ".claude/CLAUDE.md"
}

// collectMemoryFiles gathers layered instruction files (D4): the user-level
// ~/.woca/CLAUDE.md first (falling back to ~/.claude/CLAUDE.md), then every
// CLAUDE.md / AGENTS.md / .claude/CLAUDE.md found walking from the git root
// down to workDir — the more local a file, the later it is injected.
func collectMemoryFiles(workDir string) []memoryFile {
	var out []memoryFile
	// seen 按绝对路径去重：git root == $HOME 时 ~/.claude/CLAUDE.md 会同时
	// 命中 user 级 fallback 与 project 级遍历，只注入一次。
	seen := map[string]bool{}
	if home, err := os.UserHomeDir(); err == nil {
		for _, p := range []string{
			filepath.Join(home, ".woca", "CLAUDE.md"),
			filepath.Join(home, ".claude", "CLAUDE.md"),
		} {
			if fileExists(p) {
				seen[p] = true
				out = append(out, memoryFile{Path: p, Kind: "User", Label: "CLAUDE.md"})
				break
			}
		}
	}
	dirs := merge.WorkspaceDirs(workDir)
	for i := len(dirs) - 1; i >= 0; i-- { // git root first, workDir last
		for _, name := range []string{"CLAUDE.md", "AGENTS.md", ".claude/CLAUDE.md"} {
			p := filepath.Join(dirs[i], filepath.FromSlash(name))
			if fileExists(p) && !seen[p] {
				seen[p] = true
				out = append(out, memoryFile{Path: p, Kind: "Project", Label: name})
			}
		}
	}
	return out
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
```

`internal/prompt/outputstyles.go`：把 `memoryFileSection` 替换为按绝对路径的 `memorySection`：

```go
// memorySection renders one instruction-file section, cached by
// path/size/modtime/label so unchanged files are never re-read. Kind and
// Label participate in the key: the same file can render under different
// headers in the git-root-equals-$HOME edge case.
func memorySection(mf memoryFile) (string, bool) {
	st, err := os.Stat(mf.Path)
	if err != nil || st.IsDir() {
		return "", false
	}
	section := cachedSegment(fmt.Sprintf("%s|%d|%d|%s|%s", mf.Path, st.Size(), st.ModTime().UnixNano(), mf.Kind, mf.Label), func() string {
		data, err := os.ReadFile(mf.Path)
		if err != nil {
			return ""
		}
		content := strings.TrimSpace(string(data))
		if content == "" {
			return ""
		}
		return "\n\n# " + mf.Kind + " instructions (" + mf.Label + ")\n\n" + content
	})
	return section, section != ""
}
```

`internal/prompt/prompt.go` BuildWithStyle 中原循环（约 131-135 行）替换为：

```go
	for _, mf := range collectMemoryFiles(workDir) {
		if section, ok := memorySection(mf); ok {
			b.WriteString(section)
		}
	}
```

既有测试更新（HOME 隔离，防止开发机 ~/.woca/CLAUDE.md 干扰）：
- `prompt_test.go` 的 `TestBuildLoadsMemory` / `TestBuildSkipsMissingMemory`：函数开头加 `isolatedHome(t)`。
- `outputstyles_test.go` 中直接写 CLAUDE.md 再调 Build/BuildWithStyle 的测试：同样加 `isolatedHome(t)`（该 helper 在同包 memoryfiles_test.go 中定义，直接可用；如 helper 名冲突则改用内联 `t.Setenv("HOME", t.TempDir())`）。
- `coverage_test.go` 中 `memoryFileSection(dir, "CLAUDE.md")` 调用改为先构造 `memoryFile{Path: filepath.Join(dir, "CLAUDE.md"), Kind: "Project", Label: "CLAUDE.md"}` 再调 `memorySection`。

- [x] **Step 4: 跑测试确认通过**

Run: `go test ./internal/prompt/ -v && go build ./...`
Expected: 全部 PASS（含更新后的既有测试），构建成功。

- [x] **Step 5: Commit**

```bash
git add internal/prompt/
git commit -m "feat(prompt): user-level and ancestor-dir CLAUDE.md injection"
```

---

### Task 8: Breaking changes 说明 + 全量回归

**Files:**
- Modify: `README.md`（`## Notes` 节，约 419 行后）

- [x] **Step 1: README 增加 Breaking changes 小节**

在 `## Notes` 下追加：

```markdown
### Breaking changes (2026-08-17, layered merge)

1. Skills and subagent definitions with the same name now resolve to the
   **workspace** copy (`.woca/skills`, `.claude/skills`, `.woca/agents` from
   the working directory up to the git root); previously the user-level
   `~/.woca` copy silently won. Conflicts are logged (`config merge conflict
   resolved`).
2. `CLAUDE.md` injection widened: the user-level `~/.woca/CLAUDE.md`
   (fallback `~/.claude/CLAUDE.md`) is now injected first, plus every
   `CLAUDE.md` / `AGENTS.md` / `.claude/CLAUDE.md` from the git root down to
   the working directory. Prompts get longer when those files exist.
3. New workspace MCP file `<workdir>/.woca/mcp.json` (same
   `{"mcpServers": {...}}` shape as `~/.woca/mcp.json`). Same-name servers
   connect once: `--mcp` flag > workspace file > user file.
```

- [x] **Step 2: 全量回归**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: 构建与 vet 无输出，测试全部 PASS。注意：本机 `~/.woca/skills` 非空曾使旧 `TestLoadSkills` 确定性失败（环境依赖，非 flaky）——Task 4/5 已加 HOME 隔离修复；若 `cmd/woca` 仍有失败，先核对是否为未隔离的真实 HOME 泄漏，而不是重跑。

- [x] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: breaking changes for the layered merge"
```

---

## Self-Review 记录（writing-plans 步骤 + 2026-08-17 subagent 审计修订）

- **Spec 覆盖：** D1 范围（commands 不做）✓；D2 优先级翻转 → Task 4/5 ✓；D3 向上遍历+worktree → Task 2 ✓；D4 CLAUDE.md 布局 → Task 7 ✓；D5 workspace MCP 只看一层 → Task 6 ✓；D6 冲突日志 → logMergeConflicts，落点为 **zerolog（已声明偏离，见开头）** ✓；§6 测试计划逐条映射到各 Task 测试 ✓；§5 breaking changes → Task 8 ✓；§8 注意事项（接入点已按当前代码核对；serve 模式缓存 → Task 2 缓存；merge 零内部依赖）✓。
- **占位符扫描：** 无 TBD/“适当处理”类步骤；所有代码步骤含完整代码。
- **类型一致性：** `merge.Entry[T]`/`Conflict`/`WorkspaceDirs` 在 Task 4/5/6/7 中用法一致；`loadSkills(workdir, log)`/`loadAgents(workdir, log)` 签名在调用点更新中一致；`memoryFile{Path, Kind, Label}` 与 `memorySection(mf)` 一致。
- **审计修订（subagent 审计报告落实）：** ① D6 偏离已在文档头显式声明；② Task 4/5 增加对既有 `TestLoadSkills`/`TestLoadAgents` 的 HOME 隔离（本机 `~/.woca/skills` 非空导致基线确定性失败），Task 8 措辞同步修正；③ `memorySection` 缓存 key 加入 Kind/Label、`collectMemoryFiles` 按 path 去重（git root == $HOME 边缘场景）；④ workspaceCache 不失效的取舍写入注释；⑤ Task 5 import 说明矛盾已清理。
