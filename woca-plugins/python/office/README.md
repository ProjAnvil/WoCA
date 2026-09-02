# office（python 插件）

在 uv 管理的独立 Python 环境里运行**随 woca 二进制内嵌**的 `scripts/office.py`
（python-docx 1.2.0 / openpyxl 3.1.5 / python-pptx 1.0.2，全 MIT），为 agent 提供
Word / Excel / PowerPoint 的读写工具。脚本与依赖锁都在本目录，经 `go:embed`
进入二进制；磁盘上的环境收在 `~/.woca/plugins/office/`（独立 venv；uv 与
CPython 共享于 `~/.woca/plugins/runtimes/python/`，与 pdf 插件复用同一套运行时）。

选型与市场调研见 `woca-internal-docs/builtin-skills-research.md`：办公文档三件套
是 Claude Code 阵营的开箱标配、自托管 harness 阵营的空白，且三库均为各自格式的
Python 事实标准。python-pptx 自 2024-08 起维护放缓，因此 requirements.in 锁死
精确版本，升级需人工评估。

## 工作方式

与 pdf 插件完全一致：

1. 设置面板「插件」启用 office（设置键 `builtin-plugins` 加入 `"office"`）。
2. 点击「安装」：下载固定版本 uv（SHA-256 校验）→ 托管 CPython 3.12 → 建 venv →
   按 `requirements.lock`（`--generate-hashes` + `--only-binary=:all:`）安装 wheel。
3. 工具调用时物化内嵌脚本，用 venv 的 python 直接执行，180s 超时。

**只运行内嵌脚本**；spec 经 argv 传入（不落临时文件，兼容沙箱执行器），lock 逐
wheel 哈希校验。

## 依赖维护

```
# 编辑 requirements.in 后重新锁定（在任意装有 uv 的机器上）
uv pip compile requirements.in --generate-hashes --no-strip-markers -o requirements.lock
```

升级依赖会改变 lock 哈希 → woca 检测到 ENV_OK stamp 不匹配，UI 提示重新安装。

## 工具

| 工具 | 权限 | 说明 |
| --- | --- | --- |
| OfficeRead | read | .docx/.xlsx/.pptx（含宏变体）→ Markdown：Word 标题/列表/表格、Excel 工作表表格（每表最多 1000 行 / 200 列展示）、PPT 逐页标题/要点/备注 |
| DocxWrite | write | 结构化 blocks（heading/paragraph/bullets/table/pagebreak）生成 Word |
| XlsxWrite | write | 多工作表生成 Excel；单元格字符串自动判型（数字样转数值、`=` 开头公式、`'` 前缀强制文本），可选表头行（加粗+冻结+筛选），自适应列宽 |
| PptxWrite | write | 封面 + 多页生成 PowerPoint（标题页 + 标题/要点/备注页） |
