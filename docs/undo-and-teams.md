# woca 文件级 /undo 与 Agent Teams（调研 + 设计）

日期：2026-08-16。对应市面对标差距分析中的第 2 项（文件级回滚）与第 4 项（多代理协作）。

## 1. 文件级 /undo /redo

### 业界机制（调研结论）

| Harness | 机制 | 覆盖范围 |
| --- | --- | --- |
| opencode | 影子 git 对象库（每步 `write-tree` 树哈希），`/undo` 按 step 边界回退，redo=恢复 pre-undo 基线；消息隐藏不删除，下一条 prompt 才硬删 | **含 Bash 改动**（worktree 级 step 前后 diff）；跳过 gitignore/2MB+ 未跟踪文件 |
| Claude Code | `~/.claude/file-history/<session>/` 存**每次文件工具编辑前的内容副本**（版本化）；`/rewind` 菜单可选恢复代码/对话/两者 | 仅 Write/Edit/NotebookEdit；Bash、外部编辑不跟踪 |
| Crush | SQLite `files` 表全量内容版本（含用户手工中间版本）；PR #2563 加 revert 标记 + 定向恢复 | 文件工具；bash 删除无法恢复 |
| aider | git 自动提交 + 带**守卫的 `reset --soft`**（脏文件/已推送/merge 拒绝） | 仅 aider 自己的提交 |

共识要点：编辑前内容快照是最简单确定的方案（无 git 依赖）；防呆包括 assert-not-busy、恢复路径 contained、恢复清单定向（避免 mtime 风暴）、跳过符号链接并报告。

### woca 实现（`internal/tools/filehistory.go`）

- Write/Edit/NotebookEdit 成功后 `Record(path, before, after)`：**编辑前字节 + 编辑后字节**；nil 表示不存在/被删除。
- **连续同路径合并为一步**（一次"Write+多次 Edit 同一文件"= 一次 /undo），不同文件各成一步，LIFO 回退。
- `Undo()` 恢复 before（文件不存在则删除，保留当前 mode），压入 redo；任何新变更清空 redo。
- 暴露：TUI `/undo` `/redo`（console）；web 直接把 `/undo` `/redo` 作为 prompt 发送——`handleCreateRun` 拦截，不进模型，经 `startCommandRun` 以 delta+done 回流并持久化到会话。
- **不跟踪 Bash 造成的文件变更**（与 Claude Code 相同；opencode 的 worktree 级影子 git 是唯一覆盖 Bash 的方案，留作后续）。会话内存态，不跨重启。

## 2. Agent Teams

### Claude Code 机制（v2.1.178 后的现行设计）

- **teammate = 带名字的持久完整会话**（自有上下文，不继承 lead 历史）；团队隐式形成（Task 工具带 name 即 teammate），无 TeamCreate 工具；lead 固定为交互会话。
- **共享任务列表**：就是 TaskCreate/Get/List/Update 工具族——团队生效后变为团队作用域；pending/in_progress/completed + 依赖门控（前置未完成不可认领）；认领靠**文件锁**防竞态；teammate 自领或 lead 指派。
- **通信**：SendMessage 点对点（无广播），写入收件人 mailbox 文件即送达，**推送**——空闲 teammate 被消息唤醒；给 lead 的消息以中断进入会话；关闭团队也是消息协议。
- **隔离**：默认**不做** worktree 隔离（isolation 参数对 team agent 失效是已知问题）——靠任务所有权划分避免冲突。
- **生命周期**：teammate 空闲自终止或消息关闭；`TeammateIdle` hook exit 2 可续命；实验开关 env 控制；成本≈线性（plan 模式约 7x token）。

### woca 实现（`internal/agent/team.go`）

- `TeamManager`：共享 `TaskList`（扩展 `Owner` + `DependsOn` + 原子 `Claim`/`Release`）+ `bus.Bus`（存储）+ poke 通道（推送唤醒）。
- `TeamSpawn {name, prompt, subagent_type?}`：teammate = `newSubagent` + 绑定共享任务工具与 SendMessage（`adoptTeamTools`），own context、共享 workdir。
- **自主循环**：先处理收件箱消息 → 否则自领任务（pending+无主+依赖完成）→ 否则 idle 等 poke；**每轮 10min 硬超时**、错误轮释放任务回池；空闲 10min 自动停止。
- 工具族：`TeamSpawn` / `TeamStatus`（成员、任务、lead 收件箱——读取即确认）/ `TeamStop`；`SendMessage` 在团队内路由并 poke 收件人。团队生效后 lead 的 Task* 工具切到共享列表（Claude Code 语义）；`TaskUpdate` 认领即占用，非 owner 不可改（lead 可覆盖）。
- 隔离：同 Claude Code——共享 worktree、按任务所有权划分（文档明示）。TUI `/team` 命令查看状态；进程退出统一 `CloseTeams()`。
- v1 限制：进程内存态（不跨重启）；teammate 无独立 UI 行（经 TeamStatus 观察）；无按消息回滚 teammate 会话。

## 3. 后续候选

- opencode 式影子 git（覆盖 Bash 改动、跨会话恢复）；
- teammate 可选 worktree 隔离（EnterWorktree 复用）；
- teammate→lead 消息经 inputs 队列在 run 中途注入；
- TeamIdle hook（exit 2 续命协议）。
