# browser（golang 插件）

让 woca 的 agent 拥有真实浏览器能力。实现是**编译进 woca 二进制的 Go 代码**
（`internal/pluginrt`），本目录只承载清单（plugin.json）与文档；运行时外部依赖
pinchtab server 由 woca 自动下载、校验、启动与复用。

## 工作方式

1. 在设置面板「插件」启用 browser（设置键 `builtin-plugins` 加入 `"browser"`）。
2. 点击「安装」：从 GitHub Releases 下载固定版本 pinchtab（当前 v0.15.1）到
   `~/.woca/plugins/runtimes/pinchtab/v0.15.1/pinchtab`，SHA-256 与源码内 pin 死的官方
   checksums.txt 比对。
3. 首次调用工具时懒启动：woca 生成随机 Bearer token 与空闲端口，写入
   `~/.woca/plugins/runtimes/pinchtab/config.json`，以 `PINCHTAB_CONFIG` 启动
   `pinchtab server`（127.0.0.1），健康检查通过后复用；woca 重启后同样复用存活实例。
4. agent 通过 4 个工具操作浏览器（导航/正文/快照/动作）。

## 前置条件

- 本机已安装 Chrome/Chromium（pinchtab 不内嵌浏览器）。找不到时可用
  `pinchtab config set browser.binary <path>` 指定（写进上面的 config.json）。
- **fake-IP 模式 VPN 不兼容**：Surge/Clash fake-IP 把域名解析到 198.18.0.0/15，
  pinchtab 的 SSRF 防护会拒绝导航（设计如此，无放行开关）。切到 real-IP/直连模式。

## 工具

| 工具 | 权限 | 说明 |
| --- | --- | --- |
| BrowserNavigate | web（只读） | 导航并返回正文 + 交互元素快照 |
| BrowserSnapshot | web（只读） | 当前页可交互元素（ref 引用，如 `e5`） |
| BrowserText | web（只读） | 当前页正文（readability 提取） |
| BrowserAction | write | click/fill/press（按 ref 操作） |

安全细节与调研结论见 `docs/plugin-research.md`。
