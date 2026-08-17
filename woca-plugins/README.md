# woca-plugins

woca 的**内置能力插件**（capability plugins）源码树，`go:embed` 进二进制
（根包 `pluginsfs.go`）。与 `~/.woca/plugins` 下的 skills/agents 插件
（`internal/plugins`，Claude Code 式市场分发）是两回事：这里的插件为 agent
提供**工具**，随 woca 版本一起发布。

```
golang/<name>/   # Go 插件：实现编译进 internal/pluginrt，此处是 plugin.json + 文档
python/<name>/   # Python 插件：scripts/ 内嵌脚本 + requirements 依赖锁
```

## plugin.json 字段

| 字段 | 说明 |
| --- | --- |
| name | 插件名（设置键 `builtin-plugins` 引用它） |
| kind | `golang` \| `python` |
| version | 插件自身版本 |
| description | 展示在设置页 |
| tools | 贡献的 agent 工具名（同时登记权限分类） |
| runtime | `pinchtab` \| `python` —— 安装按钮拉起的运行时 |
| scripts | （python）可物化执行的脚本，相对插件目录 |
| requirements | （python）依赖锁文件，相对插件目录 |

新增插件：建目录 + manifest；golang 插件还需在 `internal/pluginrt` 实现工具并在
`tools.go` / `registry.go`（权限分类）登记；python 插件按 `runtime/python` 自动可用。

磁盘布局：共享重资产在 `~/.woca/plugins/runtimes/`（uv/CPython/pinchtab，跨插件共享），每个 python 插件一套独立环境在 `~/.woca/plugins/<name>/`（venv/scripts/ENV_OK，删目录即卸载）——与市场插件（skills/agents）共用 `~/.woca/plugins` 根，加载器只认带 `plugin.json` 的目录。

设计与安全调研：`docs/plugin-research.md`。
