# woca 前端端到端测试（Playwright）

覆盖五块前端形态的浏览器级 E2E，替代"人工打开页面点一遍"的验证方式。
选型依据 2026 年现状：Playwright 是新项目默认（市占、速度、auto-wait、
trace/报告体系），配合仓库已有的 Vitest 单测层构成「单测 + E2E」分层。

## 五形态矩阵与反混淆断言

一套 Vue SPA 分发到五个形态；**形态判定全部是运行时探针**，E2E 的核心
职责就是锁死每个形态的门面契约——尤其是登录/连接卡片绝不串形：

| 形态 | 载体 | 登录/连接面 | 必须出现 | 必须不出现 |
|---|---|---|---|---|
| 1. 个人内嵌 web | 真实 `woca serve` 二进制（隔离 WOCA_HOME） | 无（环回信任） | 应用外壳 | LoginGate、原生向导卡片、状态灯 |
| 2. 企业 bridge web | lab：真实 bridge（apikey，已注册私有核心） | LoginGate（API key + QR） | 外壳、状态灯、设置「组织配置」「远端连接」 | 原生向导卡片、「接入点」 |
| 4. 企业 web browser（edge） | lab：真实 edge 入口，SPA/API 全走隧道 | LoginGate（edge 应答、核心验证） | 外壳、状态灯、设置「接入点」bridge 清单 | 原生向导卡片、「远端连接」 |
| 3. 企业 wails | shim 版（默认）：真 webdist + window.go shim 驱动真向导；真机版（opt-in）：`wails dev` | NativeGate 向导 P0-P5 | P0「本机模式/远程模式」双卡、P1「连接私有云/直连 bridge」、P2A/P2B 表单、P3 选桥、P4 三输入注册、P5 单密钥 | LoginGate（原生形态永不出现 web 登录门） |
| 5. 个人远程 relay | lab：真实 woca-relay（密钥/认证/隧道/吊销扫描）+ 真实个人 serve（`relay connect` 同款持久化接入） | LoginGate（wcr_ 会合密钥；这是个人版唯一出现登录门的形态） | 吊销密钥 → revoked-notice；正确密钥 → 隧道往返进外壳 | 状态灯、设置「组织配置」「接入点」、原生向导卡片 |

形态间的互斥断言（金丝雀）双向都测：个人版无卡片、bridge/edge 无向导、
原生无登录门。金丝雀的有效性经突变验证（反转期望必红）。

## 架构

```
playwright.config.ts     # 四个常驻 webServer（第五个 opt-in）+ 端口约定
fixtures/                # lab 事实（lab.json / relay.json）、登录/设置 helpers、wails shim
tests/{personal,bridge,edge,native,relay}/
lab/                     # Go：一个进程一个拓扑
  enterprise             # 假核心（真 entrygw/tenant/authcore/nativeauth 存储）
                         # + 真 edge handler + 真 bridge（经真 /gw/enroll 注册、
                         #   apikey、隧道开启、登录在核心验证）
  static                 # 纯静态 webdist（原生向导 shim 的宿主页面）
  relay                  # ci/e2e-relay.sh 的进程内孪生：真 woca-relay（密钥/
                         #   认证/隧道/吊销扫描）+ 真个人 serve（relayclient 四钩子
                         #   与 cmd/woca 同构）；预铸一把已吊销密钥供 revoked-notice 用例
```

要点：

- **真实优先**：个人版跑真二进制；企业链路用与 `cmd/woca-bridge` 完全相同
  的 servefront Edition 装配（已提取为 `bridge.ServeState.Edition()` 共享，
  cmd 与 lab 不会漂移），注册/心跳/隧道/登录全部走真实协议路径。
- **隔离**：每次运行 `e2e/.runtime/` 下独立 HOME/WOCA_HOME，端口固定在
  127.0.0.1:13710-13714，企业链路就绪标准是"真实登录成功"而非端口可连。
- **wails 边界**：Wails v2 无 headless 模式。默认用 shim 忠实实现
  `NativeBridge` 契约（web/src/native/bridge.ts）驱动真实构建产物里的真
  实向导组件树——被测的正是用户担心的卡片 UI；wails 进程内的 Go 逻辑
  （DPoP/TOFU/注册）已由 native-client 的 Go 测试覆盖。
  真机层 `WOCA_E2E_NATIVE_DEV=1`（`make e2e-native-dev`）经 `wails dev`
  的浏览器通道（:34115，含 Go 绑定）驱动真 app + 真内嵌 bridge，因需
  wails CLI 且开桌面窗口，不入默认套件。

## 运行

```sh
make e2e-install     # 一次：npm install + playwright install chromium
make e2e             # 五形态 31 例，其中 2 例 wails 真机默认跳过
                     # （含先 make web；relay 形态默认在内）
make e2e-native-dev  # 加上那 2 例真 wails 进程（opt-in，会开窗口）
make e2e-personal    # 只跑个人形态的 8 例（改单个分区时省掉其余四形态）
```

调试：`cd e2e && npx playwright test --headed`、失败自动留
`test-results/`（ARIA 快照 + 上下文）、`npx playwright show-report`。

### 两个会浪费半天的坑

1. **改完 Vue 必须走 make 目标**（`make e2e*` 都依赖 `web`）。Go 二进制嵌的是
   `web/dist`，直接 `bun run build` 之后不重新构建，浏览器里跑的是上一版产物，
   新加的选择器全部落空——症状是所有用例一起超时找不到元素。
2. **环境里设了 HTTP_PROXY 会假报「端口已被占用」**。Playwright 的 webServer
   就绪探测继承环境变量，探测 `127.0.0.1:13711/api/health` 时请求被代理吃掉，
   拿到 502 就被判定成端口冲突，而实际上端口是空的。make 目标里的 `E2E_ENV`
   已经把回环排除在代理之外，绕开 make 直接跑 `npx playwright test` 时会遇到。

## 已锁定（并修正过测试预期）的产品行为

- LoginGate 是**覆盖层**设计：apikey 姿态下应用外壳在门下渲染属设计，
  断言的是 overlay 铺满视口，而非外壳缺席。
- 内嵌 bridge 就是 woca-bridge：`/api/status` 为真 JSON → 原生「本机模
  式」下状态灯存在；个人版 `woca serve` 的 status 探测落 SPA HTML → 状态
  入口隐藏。两者恰成形态对照。
- 未上云 + 有内嵌 bridge 时，向导 P1 提供「注册上云」入口（enrolled 后
  换成注销两步确认）。
- relay 形态是个人版唯一出现登录门的形态，但门后能力面仍是个人版：
  无状态灯、无组织配置；吊销密钥登录展示 revoked-notice（该 UI 路径此前
  无任何测试覆盖）。

## 断言纪律

- 异步动作后的**负向断言有竞态**：`toHaveCount(0)` 会在元素尚未渲染时
  即刻通过。凡"动作后不应出现 X"的断言，必须先锚定动作的响应已回来
  （`waitForResponse`）或先等一个正向锚点（如外壳/登录门可见）。本套件
  的金丝雀均在正向锚点之后评估；吊销用例显式等待登录往返。
- **正向锚点也必须足够强**：登录门是覆盖层，未登录页面上外壳本就渲染
  在门下——"sidebar 可见"不是登录完成的信号。`loginViaGate` 的完成判
  据是登录响应 200 + 登录门在自我 reload 之后的新页面上消失（跨过
  380ms 交接窗），否则后续点击会撞上导航被清掉（edge 用例曾因此偶发）。

## 边界与后续

- CI 接线（Playwright 浏览器安装 + `make e2e` 进 CI job）留作后续拍板。
- 真 wails 层 `reuseExistingServer: true`：若开发者自留 `wails dev` 在跑，
  用例会复用其状态（可能直接 ready 而无向导），属 opt-in 已知行为。
- relay 的运行中吊销级联（登录成功后再吊销 → 会话 401 回落登录门）由
  `ci/e2e-relay.sh` 进程级覆盖；浏览器级如需可加 lab 控制端点后续拍板。
