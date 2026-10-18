# 请求报文归档 · 验证报告

基线：`3a6fd1c9`（v0.2.15）。验证日期：2026-10-14。已变基到上游 `b0a02a33`（v0.2.17）并复验，见第 8、9 节。

> **上游说明：本报告中的端到端验证使用本地模拟上游（SIMULATED）**，仿 Anthropic `/v1/messages` 与 OpenAI `/v1/chat/completions`、`/v1/responses` 的同步与 SSE 协议；每个响应带 `X-Mock-Upstream: simulated`，归档中可见。会话环境没有真实模型服务凭据，**未对真实上游进行验证**。网关、数据库（PostgreSQL 16）、Redis、管理台前端、Nginx 均为真实进程。

## 环境

| 项 | 值 |
|---|---|
| 机器 | 4 vCPU，ext4（沙箱） |
| Go | 1.27.2（`-race` 构建运行全部 E2E） |
| Node / pnpm | 22.22 / 10.28（CI 使用 Node 20 / pnpm 9） |
| 数据库 | PostgreSQL 16.15（本地实例，`AUTO_SETUP` 初始化） |
| Redis | 7.0.15 |
| 浏览器 | Chromium（Playwright 1.56） |
| 转发层 | Nginx（`docs/audit-archive/nginx.conf.example`） |

测试实例为一次性本地库；为访问管理接口，测试脚本通过 API 完成了内置的“管理员合规确认”门禁。生产部署须由运营者本人确认。

## 1. 端到端：两个模型 × 原生/交换协议 × 同步/SSE（8 项）

每项核对：HTTP 状态、正常终止、实际出站协议、四份归档完整性，以及四份正文的 SHA-256 与“客户端实际发送 / 客户端实际收到 / 模拟上游实际收到 / 模拟上游实际发送”逐一相等；鉴权头脱敏且真实上游收到原值；下载正文哈希一致。

| 模型 | 协议 | 客户端入口 | 模式 | 状态 | 正常终止 | 实际出站 | 客户端/上游传输 | 归档完整 | 四份哈希核对 | 脱敏 | 下载一致 | 结果 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| claude-sonnet-4-5 | 原生 | `/v1/messages` | 同步 | 200 | ✓ | `POST /v1/messages?beta=true` | http / http | ✓ | ✓ | ✓ | ✓ | PASS |
| claude-sonnet-4-5 | 原生 | `/v1/messages` | SSE | 200 | ✓ | `POST /v1/messages?beta=true` | sse / sse | ✓ | ✓ | ✓ | ✓ | PASS |
| claude-sonnet-4-5 | 交换 | `/v1/chat/completions` | 同步 | 200 | ✓ | `POST /v1/messages?beta=true` | http / **sse** | ✓ | ✓ | ✓ | ✓ | PASS |
| claude-sonnet-4-5 | 交换 | `/v1/chat/completions` | SSE | 200 | ✓ | `POST /v1/messages?beta=true` | sse / sse | ✓ | ✓ | ✓ | ✓ | PASS |
| gpt-5.1 | 原生 | `/v1/chat/completions` | 同步 | 200 | ✓ | `POST /v1/chat/completions` | http / http | ✓ | ✓ | ✓ | ✓ | PASS |
| gpt-5.1 | 原生 | `/v1/chat/completions` | SSE | 200 | ✓ | `POST /v1/chat/completions` | sse / sse | ✓ | ✓ | ✓ | ✓ | PASS |
| gpt-5.1 | 交换 | `/v1/messages` | 同步 | 200 | ✓ | `POST /v1/chat/completions` | http / http | ✓ | ✓ | ✓ | ✓ | PASS |
| gpt-5.1 | 交换 | `/v1/messages` | SSE | 200 | ✓ | `POST /v1/chat/completions` | sse / sse | ✓ | ✓ | ✓ | ✓ | PASS |

观察：Claude 交换协议的**同步**请求，网关实际以 SSE 向上游发起再聚合返回——归档如实记录了这一出站行为（上游段 transport=`sse`）。上游请求头中可见网关补充的 `anthropic-version` 等默认头（键名大小写按实际发送保留）。

权限：无 token、以 API Key 作为 Bearer 访问归档接口均返回 **401**。

## 2. 重试与账户切换（真实网关故障转移）

同组两个模拟账户：高优先级账户固定返回 503，另一个正常。

| 请求 | 客户端状态 | 归档尝试（账户 → 上游状态） | 用量记录账户 | 归档状态 |
|---|---|---|---|---|
| 第 1 次（同步） | 200 | #1 账户 17 → **503**；#2 账户 18 → 200 | 18 | complete |
| 第 2 次（SSE） | 200 | #1 账户 18 → 200 | 18 | complete |

第 2 次请求被网关直接路由到健康账户（会话粘性 / 故障冷却），归档只有一次尝试，与实际一致。

## 3. 存储与目录

| 场景 | 方法 | 结果 |
|---|---|---|
| 目录结构 | E2E 实际落盘 | `2026-10/14/{session}/{clientRequestId}-1-{kind}.zst`，每请求 4 个文件 |
| 无会话 | E2E / 单测 | 当天 `nosession/` |
| 跨日 | 单测（固定时钟 23:59 → 次日） | 同会话仍写入首次日期目录 |
| 跨日 + 乱序完成 | 单测 | 先开始的长请求晚结束，会话日期仍取最先开始的请求 |
| 重启 | 真实进程 SIGTERM 重启前后同会话各 1 次 | 同一目录；停机前请求已完整落盘 |
| 重复请求 ID | 单测 ×3 | `-1/-2/-3` 均保留，无覆盖；查看时以候选列表呈现歧义 |
| 非法会话 / 请求 ID | 单测（`../../evil session`） | 映射为 `h-<sha256>`，不逃逸根目录 |
| 缓存有界 | 单测（上限 16，200 会话） | LRU ≤ 16 |
| 解压完整 | 单测 6 MB 随机正文（流式模式） | 下载字节与原始字节完全相同 |
| 截断 / 空文件 | 单测 + 浏览器 | 显示“不完整 / 文件损坏 / 未落盘” |
| 鉴权脱敏 | 单测 + E2E | 四份文件解压后全文搜索不到任何原始密钥（含 `?key=`、Cookie、Set-Cookie） |
| WebSocket | 集成测试（coder/websocket，permessage-deflate 上下文接管） | 客户端原始帧解析还原 3 条压缩消息 + close；上游以消息粒度记录，常驻读循环（无归档 context）读到的消息仍归入绑定请求；握手 101 头与账户记录 |

## 4. 定位与写入性能

`TestManyFilesLocatePerformance`（`AUDIT_PERF_N=20000`，16 并发，2 KB 正文，200 会话 + 10% 无会话）：

| 指标 | 结果 |
|---|---|
| 文件总数 | 100,180（20,000 × 4 + 索引 / 会话标记） |
| 请求路径 `Begin`（含会话定位） | p50 **18 µs**，p95 84 µs，p99 6.4 ms（新会话首次建标记文件） |
| 持续落盘吞吐 | ~2,000 请求/s（受本沙箱文件创建开销限制，见下） |
| 按 ID 查找归档 | p50 **34 µs**，p99 232 µs（与历史文件数无关，不扫描目录） |
| 重启后冷会话定位 | 平均 101 µs（读 1 个标记文件） |

基准（`go test -bench`，`TMPDIR=/var/tmp`）：

| 基准 | 结果 |
|---|---|
| SSE 单块写入（~120 B/块） | **325–377 ns/块**，0 分配 |
| 网关请求路径开销（32 KB 请求 + 8 KB 响应，含 RoundTripper；落盘不计） | 关闭 0.63–0.65 ms → 开启 ~1.0–1.24 ms（+0.35–0.6 ms，主要为 Header JSON 编码与缓冲复制） |
| 大请求并行（1 MB + 64 KB，4 份） | ~280 MB/s 原始数据入库 |

说明：本沙箱磁盘上单次 `O_CREAT|O_EXCL` 约 0.3–1 ms（`/var/tmp` 实测 31 µs/次，而部分路径高达 668 µs/次），因此持续吞吐数字偏保守。实现上请求热路径只做内存操作；目录、四个文件、索引的创建与 zstd 压缩都在请求结束后的有界后台 worker 中完成（`flush_workers`），突发 20,000 请求不会耗尽文件句柄（修复前曾复现 `too many open files`）。

## 5. 压缩传输与读取

| 检查 | 结果 |
|---|---|
| 归档 JSON 经 Nginx，`Accept-Encoding: gzip` | `Content-Encoding: gzip`、`Vary: Accept-Encoding`，12,014 B → 2,440 B，解码后与未压缩响应逐字节相同 |
| 正文下载经 Nginx gzip 传输 | 解码后 SHA-256 = `X-Archive-Body-SHA256` = 客户端实际收到的 SHA-256 |
| 浏览器下载 | Playwright 实际下载文件 SHA-256 与页面内校验一致（“SHA-256 与归档一致”） |
| 模型 SSE 经同一 Nginx（带 `Accept-Encoding: gzip`） | 未压缩（无 Content-Encoding），`text/event-stream`，正常以 `message_stop` 结束 |
| 无解压临时文件 | 对服务进程 `strace -e openat,creat,rename,unlink,memfd_create`：读取 / 下载期间归档文件仅 `O_RDONLY` 打开（12 次）；唯一的 `O_CREAT` 是本次 SSE 探测请求自身的新归档与索引 |
| 上游 Content-Encoding | 单测：上游 gzip 正文按原始字节归档；展示时解码；`decode=0` 下载与上游发送字节一致，`decode=1` 为解码内容 |

## 6. 页面（真实浏览器，嵌入构建补丁的产物）

18 项全部通过（截图见交付页面）：

- 服务返回的 `index.html` 含补丁标记；桌面每行都有入口（20/20）；
- 打开后弹窗用量 ID 与所点行一致；状态“完整”；四个标签页均有内容；
- 下载原始正文，页面内 SHA-256 校验通过；
- 第 2 页、按时间排序后、路由跳转后均不串日志（跳转自动关闭弹窗）；
- 权限错误（拦截返回 403）、不完整（截断文件）、关联歧义（重复 ref）均有明确提示；
- 移动端（iPhone 13）卡片布局每行有入口，弹窗全屏且不超出 390 px 宽。

真实浏览器发现并修复了一个 jsdom 测不到的问题：标签栏在纵向 flex 容器中被压缩为 1 px。

## 7. 编译、静态检查与回归

| 项 | 结果 |
|---|---|
| `go build ./...`、`go vet` | 通过 |
| `golangci-lint v2.14.0`（审计包 + 所有改动包） | 0 issues |
| `go test -race ./internal/audit/...` | 通过（多次 `-count`） |
| 改动包单测（`-tags=unit`：server、routes、middleware、repository、handler/admin、cmd/server） | 通过 |
| `internal/service` 单测 | 仅 `TestValidateCreateParams_CheckModeMatrix` 2 个子用例失败——在**未修改的基线**上同样失败（沙箱无法解析 DNS），与本改动无关 |
| `-race` 二进制 E2E | 运行期间仅有 1 条竞争报告：`timezone.Init` 与 `lumberjack` 日志轮转 goroutine——在**未修改的基线 race 构建**上同样出现，不涉及 `internal/audit`。本改动曾因初始化日志过早额外触发同类竞争，已将 `audit.Init()` 移到应用装配（时区初始化）之后修复 |
| 前端：补丁 vue-tsc、10 个 vitest、原项目 `check:i18n` + `vue-tsc -b` + `vite build` | 通过 |
| 原始启动方式 / 页面 | 同一二进制与配置启动；关闭归档（`AUDIT_ENABLED=false`）时所有钩子透传（单测覆盖） |

## 8. 变基到上游 v0.2.17（`b0a02a33`，2026-10-18）

基线之后上游新增 127 个提交，变基无冲突。上游改动中涉及本功能接入点的只有 `routes/gateway.go`，新增的是 Gemini `cachedContents` 路由，注册在已接入归档的 `/v1beta` 分组里；新增的上游调用都走已包装的 `httpUpstream.Do`。用量写入仓储、前端 `UsageTable.vue` 锚点、`Dockerfile`、`docker-compose.yml`、entrypoint 均无改动。

| 检查 | 结果 |
|---|---|
| `go build ./...`、`go vet`（改动包） | 通过 |
| `go test -race ./internal/audit/...` | 通过 |
| 改动包单测（server、repository、handler/admin、cmd/server） | 通过 |
| `internal/service` 中 WebSocket 相关单测 | 通过 |

## 9. Docker Compose（原版镜像 `weishaw/sub2api:0.2.17`，启动时编译）

环境：PostgreSQL 18 / Redis 8 以独立容器运行，模拟外部中间件。

沙箱专用的差异只放在测试覆盖文件里，未提交：
- 主机网络，并挂载出站代理 CA；
- `nofile` 上限为 20000；
- 沙箱代理禁止从 GitHub 下载源码压缩包（403），改为本地 `git archive` 生成同一提交的源码包，通过 `AUDIT_SOURCE_URL` 提供。因此 GitHub 下载这一步未实测。

| 场景 | 结果 |
|---|---|
| 首次启动 | 305 秒（依赖 9 秒、前端 75 秒、后端 200 秒，`AUDIT_BUILD_JOBS=1`）后替换为 `0.2.17-audit`，healthy；系统内存峰值 4.0 GB（空闲 0.6 GB） |
| 编译后清理 | 容器内只剩 PID 1 `/app/sub2api`；go / nodejs / git 已卸载；源码、node_modules、Go/pnpm 缓存已删除；编译卷只剩 119 MB 产物；容器内存 260 MiB |
| E2E（模拟上游） | 8/8 PASS；鉴权 401；32 个归档文件；页面含补丁标记 |
| `docker restart` / `--force-recreate` | 各约 2 秒，直接使用缓存产物，归档文件保留 |
| 换镜像版本（0.2.17 → 0.2.16） | 按 0.2.16 的提交自动重新编译（303 秒）并替换；旧产物删除 |
| 补丁无法应用（篡改锚点行） | `git apply` 失败，13 秒内以原版 0.2.16 启动，healthy；编译依赖已卸载；再次重启不重复编译，提示 `AUDIT_REBUILD=true` |

补丁改为明文 `audit.patch` 文件挂载（不再内嵌在 compose 中）后复验：首次启动编译约 5 分钟（前端 77 秒、后端 200 秒）后替换为 `0.2.17-audit`，healthy；容器内只剩 `/app/sub2api` 进程；E2E 8/8 PASS，32 个归档文件；`docker restart` 后直接使用缓存产物。

## 10. 正文展示改用 Monaco Editor（2026-10-18）

选型：以原始文本为输入，对比了 7 个开源方案的功能和性能。只有 Monaco 同时自带 JSON 排版、高亮、对象 / 数组折叠和换行切换；复制按钮用原项目已有的 `@vueuse/core`。1 MB / 4 MB 正文实测：挂载加排版 541 ms / 1.1 s，内存 16 / 41 MB；只渲染可见行，DOM 节点约 470 个。

真实管理台 + 真实后端（PostgreSQL / Redis），上游为模拟服务（SIMULATED），浏览器为 Chromium，15/15 通过：

| 检查 | 结果 |
|---|---|
| 打开报文前不加载 Monaco；打开后按需加载 | 打开前 0 个请求；约 0.75 s 至编辑器就绪 |
| 单行 JSON 自动排版 + 语法高亮 | 排为 28 行可见；`1.0` 原样保留 |
| 行号旁箭头折叠 / 展开；全部折叠 / 全部展开 | 通过（28→26 行；2 / 28 行） |
| 默认不换行；切换自动换行 | 不换行时为横向滚动；开启后超长行折行 |
| 复制 | 剪贴板内容与客户端实际发送的原文逐字节一致（3522 字符） |
| 暗色主题、关闭弹窗释放编辑器、无页面错误、移动端不超出屏幕 | 通过 |
| 补丁单测（vitest，含 Monaco 替身） | 14/14 |

构建与体积：
- 前端产物从 6.5 MB 增至 12 MB（嵌入二进制）。
- 原 `vendor-misc` 包大小不变（279 KB），首页预加载列表与原版一致。
- 只跑 Vite 构建（启动时编译走的路径）：61 秒，node 进程峰值约 2.0 GB；完整 `build.sh`（含类型检查和测试）：166 秒。
