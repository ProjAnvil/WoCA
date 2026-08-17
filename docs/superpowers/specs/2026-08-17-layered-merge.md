woca 分层 Merge 设计稿

  日期：2026-08-17
  状态：设计已拍板，待实现
  范围：批次 ①（统一 merge 抽象 + skills/agents/MCP 接入）+ 批次 ②（CLAUDE.md user 级 + 向上遍历拼接）

  1. 背景与目标

  现状：五类配置（CLAUDE.md、skills、agents、MCP、commands）各自是独立的 "append 列表 + Find 首命中"，没有显式的层级优先级、同名去重和冲突可观测性；MCP 同名会重复连接。对照 Claude Code /
  Codex / Gemini CLI / OpenCode 四家，主流形态是 map[name] + 显式层优先级 + 冲突只留一个（资产类）和 多级全量拼接（指令文件类）。

  目标：把"来源层"建模为带优先级的合并，消除隐式行为，对齐业界。

  2. 决策记录（本次 brainstorming 已确认）

  ┌─────┬────────────────┬──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┐
  │  #  │      决策      │                                                                             内容                                                                             │
  ├─────┼────────────────┼──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
  │ D1  │ 范围           │ merge 抽象 + skills/agents/MCP + CLAUDE.md 分层；commands 文件加载不做（未来项）                                                                             │
  ├─────┼────────────────┼──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
  │ D2  │ 优先级         │ 统一 workspace > user，flag/会话注入层最高。⚠️ Breaking：skills/agents 现有"user 首命中胜出"语义翻转                                                         │
  ├─────┼────────────────┼──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
  │ D3  │ 向上遍历       │ 到 git root（.git 为目录或文件均认可，兼容 worktree）；非 git 仓库只看 workDir                                                                               │
  ├─────┼────────────────┼──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
  │ D4  │ CLAUDE.md 布局 │ user 级 ~/.woca/CLAUDE.md（fallback ~/.claude/CLAUDE.md）；每目录收集 CLAUDE.md→AGENTS.md→.claude/CLAUDE.md 全注入；拼接顺序 user 最前 →                     │
  │     │                │ root→workDir（越局部越后注入）；CLAUDE.local.md 不做                                                                                                         │
  ├─────┼────────────────┼──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
  │ D5  │ workspace 级   │ 本批新增 workdir/.woca/mcp.json；只看 workDir 一层，不向上遍历                                                                                               │
  │     │ MCP            │                                                                                                                                                              │
  ├─────┼────────────────┼──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
  │ D6  │ 可观测性       │ merge 返回冲突列表，调用方写 obslog 结构化日志；UI 面板 / /doctor 为未来项                                                                                   │
  └─────┴────────────────┴──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┘

  3. 架构：统一 merge 抽象

  新包 internal/merge（零依赖小包；若与现有代码风格更契合也可并入 internal/config）：

  package merge

  type Level int

  const (
      LevelFlag      Level = iota // 会话注入（--mcp 等），最高
      LevelWorkspace             // workDir 及其到 git root 的祖先目录
      LevelUser                  // ~/.woca
  )

  type Entry[T any] struct {
      Name   string
      Value  T
      Level  Level
      Order  int    // 同 Level 内次优先级，小者胜；workspace 用于"离 workDir 近者胜"
      Source string // 文件路径或 "flag"，用于日志
  }

  type Conflict struct {
      Name, Winner, Loser string // Winner/Loser 为 Source
  }

  // 按 Name 合并：Level 高者胜，同 Level 时 Order 小者胜；
  // 返回确定性顺序的结果列表 + 全部冲突记录。
  func Entries[T any](es []Entry[T]) ([]Entry[T], []Conflict)

  - 冲突日志由调用方负责（merge 包不依赖 obslog，保持可测）。
  - 确定性：遍历顺序固定（见下），同名决胜稳定。

  Order 赋值约定（skills/agents）：
  - workspace 收集从 workDir 向上到 git root（含），每目录先 .woca/skills 后 .claude/skills；Order = 目录序号*2 + (0:.woca, 1:.claude)，即近者胜、自家优先。
  - user 层 ~/.woca/skills 的 Order 无关紧要（Level 已分高下）。

  4. 各维度接入

  4.1 Skills（cmd/woca/main.go loadSkills）

  - roots 变为：workDir→gitRoot 每目录的 .woca/skills + .claude/skills，加 ~/.woca/skills（user）。
  - 构造 []Entry → merge.Entries → 得去重后的最终列表 + 冲突 → obslog 记录。
  - 插件 skill 维持现状：追加末尾、插件名-skill名 命名空间，不参与 map 冲突。

  4.2 Agents（loadAgents）

  - 同 skills；目录为 .woca/agents（不新增 .claude/agents 读取，未来项）。

  4.3 MCP（connectMCP / mcpSpecs）

  - 三层：flag（--mcp，LevelFlag）→ workDir/.woca/mcp.json（LevelWorkspace）→ ~/.woca/mcp.json（LevelUser）。
  - 只对 merge 结果遍历连接——修复同名重复启动 bug。
  - workspace 层不向上遍历（D5）。

  4.4 CLAUDE.md（internal/prompt/prompt.go，拼接语义，不走 merge map）

  collectMemoryFiles(workDir):
    1. ~/.woca/CLAUDE.md（不存在则 ~/.claude/CLAUDE.md）
    2. gitRoot→workDir 逐目录：CLAUDE.md → AGENTS.md → .claude/CLAUDE.md（存在即注入）
  - 需要辅助 findGitRoot(workDir)：向上找 .git（目录或文件，worktree 兼容）；找不到返回 workDir。

  5. Breaking Changes（需 release note 标注）

  1. skills/agents 同名时改为 workspace 胜出（原 user 隐式胜出）。
  2. CLAUDE.md 注入来源扩大（user 级 + 祖先目录）→ 上下文变长；serve 模式每个 session 的 workDir 都受影响。
  3. 新增 workdir/.woca/mcp.json 后 MCP server 集合可能变化（仅新增文件时生效）。

  6. 测试计划

  - internal/merge：Level/Order 覆盖、冲突列表完整性、输出确定性。
  - loadSkills/loadAgents：tmp 目录树（user + 嵌套 workspace 同名 + .woca vs .claude 同目录同名）断言胜者与日志调用。
  - MCP：三层同名只连一次且配置来自正确来源；flag 重名去重。
  - CLAUDE.md：顺序断言（user 最前、root→workDir）、CLAUDE.md/AGENTS.md 共存注入、无 git 仓库、worktree（.git 为文件）。
  - 回归：git status 显示大量现存测试引用这些入口，跑全量。

  7. 未来项（不在本批）

  commands 文件加载（.woca/commands，含与 skills 同名优先级规则）· CLAUDE.local.md · web 冲突面板 / /doctor 式诊断 · Codex 式 profiles · .claude/agents 兼容读取 · CLAUDE.md
  子目录按需加载与 @import。

  8. 实现注意事项

  - 本设计稿引用的接入点（cmd/woca/main.go:1664-1760、internal/prompt/prompt.go:131-135 等）来自先前分析，实现时需核对当前代码（工作树有大量未提交改动）。
  - gitRoot 解析在 serve 模式下按 session 的 workDir 进行，启动时算一次并缓存。
  - merge 包保持零依赖，冲突日志统一由 cmd/woca 接入层写入 obslog。