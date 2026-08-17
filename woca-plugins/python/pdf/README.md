# pdf（python 插件）

在 uv 管理的独立 Python 环境里运行**随 woca 二进制内嵌**的 `scripts/pdf.py`
（pypdf），为 agent 提供 PDF 处理工具。脚本与依赖锁都在本目录，经 `go:embed`
进入二进制；磁盘上的环境收在 `~/.woca/plugins/pdf/`（独立 venv；uv 与 CPython 共享于 `~/.woca/plugins/runtimes/python/`）。

## 工作方式

1. 设置面板「插件」启用 pdf（设置键 `builtin-plugins` 加入 `"pdf"`）。
2. 点击「安装」：woca 下载固定版本的 uv（SHA-256 校验）→ 安装托管 CPython 3.12
   （`--no-bin`，不污染 `~/.local`）→ 建 venv → 按 `requirements.lock`
   （`--generate-hashes` + `--only-binary=:all:`）安装 wheel。
3. 工具调用时把内嵌脚本物化到 `~/.woca/plugins/pdf/scripts/`（内容
   比对，仅在变化时覆写），用 venv 的 python 直接执行（不走 `uv run`），120s 超时。

**只运行内嵌脚本**：工具层不接受任意脚本路径；`--only-binary` 禁止源码构建，
lock 逐 wheel 哈希校验。安全分析见 `docs/plugin-research.md`。

## 依赖维护

```
# 编辑 requirements.in 后重新锁定（在任意装有 uv 的机器上）
uv pip compile requirements.in --generate-hashes --no-strip-markers -o requirements.lock
```

升级依赖会改变 lock 哈希 → woca 检测到 ENV_OK stamp 不匹配，UI 提示重新安装。

## 工具

| 工具 | 权限 | 说明 |
| --- | --- | --- |
| PdfRead | read | text（可按页）与 metadata 提取，支持 `--password` |
| PdfWrite | write | merge / split / rotate / encrypt / decrypt |
