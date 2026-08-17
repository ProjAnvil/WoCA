# woca 插件能力调研与设计（pinchtab 浏览器 + uv/Python）

日期：2026-08-16。本文记录 woca 内置「能力插件」（capability plugins）的选型调研、
安全分析与实现设计。目标是让 agent 获得两类可插拔能力：

1. **browser（golang 插件）**：通过 [pinchtab](https://github.com/pinchtab/pinchtab) 访问网页；
2. **pdf（python 插件）**：下载 uv 并在 `~/.woca/plugins/` 下安装独立 Python
   环境，仅运行随二进制内嵌的 Python 脚本（如 PDF 处理）。

插件源码在仓库 `woca-plugins/` 下管理（`woca-plugins/golang/browser`、
`woca-plugins/python/pdf`），经 `go:embed` 打进 woca 二进制。

---

## 1. pinchtab 调研结论

pinchtab（MIT，v0.15.1，2026-08-03 发布）**不是可 import 的 Go 库**，而是一个独立的
Go 编写的 HTTP 服务：通过 chromedp/CDP 控制真实 Chrome，默认监听 `127.0.0.1:9867`，
为 AI agent 提供省 token 的网页读写 API。唯一公开的 Go 包是 `pkg/pinchtabaudit`。

| 项 | 结论 |
| --- | --- |
| 形态 | 独立二进制（`pinchtab server`），自带 dashboard；CLI/MCP 同源 |
| 发布物 | GitHub Releases 原始二进制（非压缩包），darwin/arm64 等 6 平台 + `checksums.txt`（sha256sum 格式，已核验） |
| Chrome | 不内嵌，要求本机已装 Chrome/Chromium（找不到时 `browser.binary` 指定路径） |
| 鉴权 | 配置文件 `server.token`，所有请求带 `Authorization: Bearer <token>`；不支持关闭鉴权（v0.8.3 后已移除 query 传 token 的方式，CVE-2026-33620） |
| 隔离配置 | `PINCHTAB_CONFIG` 环境变量指向自定义 config.json（woca 用它避免污染用户 `~/.pinchtab`） |
| 默认实例 | server 启动即拉起 default profile 的 headless 实例（实测 2s 内 ready） |
| 安全默认 | bind 127.0.0.1；`security.allowEvaluate` 默认 false（不启用任意 JS 执行）；IDPI 防注入；SSRF 防护 |

### woca 用到的 HTTP API（均已实测 v0.15.1）

- `GET /health` → `{status, version, authRequired, defaultInstance:{id,status}}`
- `POST /navigate` `{url, tabId?, newTab?}` → `{tabId, title, url}`；失败 `{code:"error", error:"..."}`
- `GET /text?maxChars=N` → `{url, title, text, textLength, truncated, extraction:"readability"}`
- `GET /snapshot?filter=interactive&format=compact` → 纯文本 `[e5] link "..."` 列表
- `POST /action` `{kind:click|fill|press, ref, text?, key?}`（fill 的字段名是 `text`）

### 已知问题与风险

- **GO-2026-4824**：`/wait` 端点曾可绕过安全策略执行任意 JS；使用最新版并保持不启用
  `allowEvaluate`/`--yolo`。woca 固定版本 v0.15.1 并校验 sha256。
- **fake-IP VPN 不兼容（实测本机）**： Surge/Clash 的 fake-IP 模式把所有域名解析到
  198.18.0.0/15（RFC 保留网段），pinchtab 的 SSRF 防护会拒绝导航（"navigation target
  resolves to blocked private/internal IP"），且没有配置项放行该网段（`security.allowedDomains`
  无效，config schema 中无相关开关）。**解决办法：把 VPN 切到 real-IP/直连模式**。
  错误会原样返回给模型，便于诊断。
- 历史漏洞 CVE-2026-33620（query 传 token）已在 v0.8.3 后修复。

---

## 2. uv / Python 调研结论

uv（Astral，MIT OR Apache-2.0，当前 0.12.5）是 Rust 实现的 Python 包/环境管理器。
**关键事实（已实测）**：

- GitHub Releases 提供 `uv-<triple>.tar.gz`（内含顶层目录 `uv-<triple>/uv,uvx`）与
  `sha256.sum` 校验清单；直接下载解压即可，无需 install.sh（脚本会改 shell rc、且无
  shasum 时静默跳过校验）。
- `uv python install 3.12 --no-bin`：下载 python-build-standalone 的 CPython 3.12 到
  `UV_PYTHON_INSTALL_DIR`，**`--no-bin` 避免往 `~/.local/bin` 链接可执行文件**（不加会
  污染用户目录）。
- `uv venv <dir> --python 3.12`：无需系统 Python。
- `uv pip install --python <venv> -r requirements.lock --only-binary=:all:`：
  `--only-binary=:all:` 禁止源码构建，**消除 sdist 构建期任意代码执行**这一 uv 文档
  明示的风险；lock 文件用 `--generate-hashes` 生成，逐 wheel 校验哈希。
- venv 默认不含 pip（uv 自己实现 pip 接口），减少攻击面。
- uv **没有任何执行期沙箱**（0.12.x 无 --sandbox）；进程级隔离需自行实现。

### 本机安装流程实测（全部成功）

```
curl uv-aarch64-apple-darwin.tar.gz → shasum 校验（与 sha256.sum 一致）→ 解压
UV_PYTHON_INSTALL_DIR=$ROOT/pythons UV_CACHE_DIR=$ROOT/cache uv python install 3.12 --no-bin   # CPython 3.12.14
uv venv $ROOT/venv --python 3.12
uv pip install --python $ROOT/venv -r requirements.lock --only-binary=:all:                    # pypdf 6.16.1 + cryptography 50.0.0
$ROOT/venv/bin/python -c "import pypdf, cryptography"  # OK
```

### 安全分析（python 插件）

威胁模型与缓解：

| 威胁 | 缓解 |
| --- | --- |
| 下载被篡改（MITM） | 固定版本 + Go 源码内嵌 pin 死的 SHA-256（uv/pinchtab 双向核对官方清单）；TLS |
| uv 供应链（恶意版本） | 版本+哈希 pin 死在 woca 源码里，升级需改代码重编译 |
| 安装期代码执行（sdist build） | `--only-binary=:all:` 只装预编译 wheel；`--require-hashes` 语义由 `--generate-hashes` lock 提供 |
| 恶意/偷换脚本 | **只运行 embed.FS 内嵌脚本**：脚本在构建时进二进制，运行前物化到 `~/.woca/plugins/<插件名>/scripts/`（内容不一致才覆写）；工具层不接收任意脚本路径 |
| 环境污染 | 全部状态收在 `~/.woca/plugins/`（共享 runtimes + 各插件独立环境），`--no-bin` 不碰 `~/.local` |
| 运行期任意行为 | 脚本仅接受文件路径/页码/密码等数据参数；执行有超时与输出上限；无 OS 级沙箱（残留风险，见下） |
| 加密 PDF 解析崩溃/内存 | pypdf/cryptography 为成熟库；输入仍是不可信数据，进程内解析（残留风险） |

**残留风险（明示）**：uv/CPython/pip 生态无执行沙箱，内嵌脚本以当前用户权限运行；
若将来加入处理不可信来源文件的高风险插件，应考虑独立用户/容器/macOS Seatbelt。当前
PDF 处理（pypdf，纯 Python 解析）风险可控。网络面：安装期访问 github.com（uv、
pinchtab、python-build-standalone）与 pypi.org（wheel），运行期 Python 脚本不联网。

---

## 3. 设计

### 3.1 仓库布局

```
woca-plugins/                     # go:embed 进二进制（根包 pluginsfs.go）
├── golang/browser/               # golang 插件：实现编译进 woca，此处是清单+文档
│   ├── plugin.json               # name/browser, kind/golang, runtime/pinchtab, tools 列表
│   └── README.md
└── python/pdf/                   # python 插件：脚本+依赖锁随二进制分发
    ├── plugin.json               # name/pdf, kind/python, runtime/python
    ├── scripts/pdf.py            # 唯一可执行入口（pypdf：text/metadata/merge/split/rotate/encrypt/decrypt）
    ├── requirements.in           # 人工维护的顶层依赖
    ├── requirements.lock         # uv pip compile --generate-hashes 生成（wheel 哈希锁定）
    └── README.md
```

manifest 字段：`name`、`kind`（golang|python）、`version`、`description`、`tools`、
`runtime`（pinchtab|python）。golang 插件的工具实现位于 `internal/pluginrt`（Go 不做
动态加载，二进制内置实现 + 清单外置描述）；python 插件由通用 python 运行器执行内嵌脚本。

### 3.2 运行时目录（~/.woca/plugins/）

插件根目录与市场插件（skills/agents，`internal/plugins`）**共用** `~/.woca/plugins/`：
加载器只认带 `plugin.json` 的目录，无 manifest 的目录（`runtimes/`、各插件环境）静默跳过——
内置插件由此与市场插件在同一个根下共存，同一套 manifest 语义认证（能力字段
`kind/tools/runtime/scripts/requirements` 是超集，市场加载器忽略未知字段）。

```
runtimes/                    # 重资产，跨插件共享（uv/pipx 同款布局）
├── python/bin/uv, uvx       # uv 0.12.5（固定版本，~20MB，只此一份）
├── python/pythons/          # 托管 CPython 3.12（UV_PYTHON_INSTALL_DIR，~100MB，只此一份）
├── python/cache/            # UV_CACHE_DIR（硬链接，跨插件去重）
└── pinchtab/                # browser 插件运行时
    ├── v0.15.1/pinchtab     # 固定版本二进制（sha256 校验后落盘）
    ├── config.json          # PINCHTAB_CONFIG 指向：随机 token + 空闲端口（重启复用）
    └── server.log
pdf/                         # 每个 python 插件一套独立环境（删目录即卸载）
├── venv/                    # 只装该插件 lock 的 wheel（依赖互不冲突）
├── scripts/scripts/pdf.py   # 从 embed.FS 物化的脚本
├── requirements.lock        # 该插件自己的 lock（参与就绪判定）
└── ENV_OK                   # 就绪 stamp（uv 版本、python 版本、lock sha256）
```

注意：市场插件与内置插件环境同名（如安装名为 `pdf` 的市场插件）会目录冲突，
`woca plugin install` 覆盖前请避开内置插件名（browser/pdf）。

### 3.3 设置与 API

- 设置键 `builtin-plugins`（JSON 名单，同 claude-skills 模式）：启用的插件名列表。
- `GET /api/plugins`：目录（manifest）+ enabled + 运行时状态（not_installed/installing/
  ready/error + 安装日志尾部）。
- `PUT /api/plugins {enabled:[...]}`：保存设置并立即刷新 agent 工具绑定（复用 MCP 的
  between-turn refresh 通道；新增 `Agent.RefreshToolsNow` 供 HTTP 侧主动触发）。
- `POST /api/plugins/{name}/install`：异步启动安装（下载 uv/pinchtab → 建环境），UI 轮询
  GET 获取进度。
- 权限分类：BrowserNavigate/BrowserSnapshot/BrowserText → CatWeb（只读）；BrowserAction
  → CatWrite；PdfRead → CatRead；PdfWrite → CatWrite。

### 3.4 agent 工具

browser（pinchtab，懒启动 server，Bearer 鉴权，127.0.0.1）：

- `BrowserNavigate{url, max_chars}`：导航 + 返回正文（readability）与交互元素快照
- `BrowserSnapshot{}`：当前页交互元素（ref 列表，如 `[e5] link "..."`）
- `BrowserText{max_chars, raw}`：当前页正文
- `BrowserAction{kind: click|fill|press, ref, text, key}`：点击/填写/按键

pdf（物化 pdf.py → venv python 执行，stdout JSON，120s 超时）：

- `PdfRead{file, pages, max_chars}`：文本/元数据
- `PdfWrite{op: merge|split|rotate|encrypt|decrypt, files, output, pages, angle, password, out_dir}`

### 3.5 升级与运维

- woca 二进制更新后：脚本物化按内容比对自动更新；requirements.lock 变化 → ENV_OK
  stamp 哈希不匹配 → UI 显示需重新安装（点击即重装，uv cache 复用，很快）。
- uv/pinchtab 版本升级：改 `internal/pluginrt` 中的版本常量与校验和映射（对照官方
  sha256.sum / checksums.txt）后重新编译。
- 布局升级（2026-08-16 第二版）：从「共享 venv + 合并 lock」改为 **pipx/uv tool 同款**
  「每插件独立 venv + 共享解释器与 cache」（`~/.woca/plugins/runtimes/` 共享重资产、
  `~/.woca/plugins/<name>/` 独立环境），消除多插件的依赖冲突与卸载残留问题。

## 4. 版本与校验和 pin（当前）

- uv **0.12.5**（sha256 摘自官方 sha256.sum，darwin-arm64 已本机复验）：
  `5bb0e5fe008a773c3dbcb97ff79cd89e1241464fe9d2f986d52ad8f1b037bd62`
- pinchtab **v0.15.1**（sha256 摘自官方 checksums.txt，darwin-arm64 已本机复验）：
  `0d4df1655e1115c1d45b86d2c3c3ef9212b496195219e3415551c57dffb3e4d9`
- Python：`3.12`（uv 0.12.5 冻结元数据解析为 3.12.14）
- PDF 依赖：`pypdf==6.16.1` + `cryptography==50.0.0`（+cffi/pycparser，全 wheel，带哈希）
