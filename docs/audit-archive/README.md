# 请求报文归档（Request Archive）

对经过网关的每个客户端请求，完整采集 **客户端请求 / 实际上游请求 / 上游响应 / 客户端响应** 四个阶段的 Header + Body（HTTP、SSE、WebSocket），以 zstd 压缩写入本地磁盘，并在管理台「使用记录」每一行提供查看入口。

存储分两层：请求时只写 **active**（建议高速 SSD，每请求一个文件、四阶段共用压缩流）；每天凌晨把不再活跃的会话合并为 **archive**（可放大容量 HDD，每会话一个文件、整条会话共用压缩流，跨请求去重）。

- 后端：独立包 `backend/internal/audit`，通过 4 个钩子接入（Gin、RoundTripper、OnUsage、OTel trace）。
- 前端：独立构建补丁 `frontend-audit/`，构建期注入，不修改 `frontend/` 下任何 Vue/TS 源文件。
- 部署：仍是同一个服务、同一个进程，单实例运行；无扩展、无包装服务、无新增进程。

---

## 1. 配置

归档读取全局配置的 `audit:` 段（`config.yaml`），或同名环境变量（`.` → `_`，大写）。**全部有默认值，零配置即可开启。**

| 键 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `audit.enabled` | `AUDIT_ENABLED` | `true` | `false` 时所有钩子直接透传，不产生文件 |
| `audit.root_dir` | `AUDIT_ROOT_DIR` | `./data/audit` | 归档根目录（相对进程工作目录；Docker 镜像中即 `/app/data/audit`，随数据卷持久化）；下面两个目录未配置时位于其下 |
| `audit.active_dir` | `AUDIT_ACTIVE_DIR` | `{root_dir}/active` | 新请求写入目录（建议 SSD）；会话映射 `.sessions` 与请求索引 `.index` 也在这里 |
| `audit.archive_dir` | `AUDIT_ARCHIVE_DIR` | `{root_dir}/archive` | 会话归档目录（可为 HDD / 其它挂载点）；不得与 active 重叠 |
| `audit.archive_after` | `AUDIT_ARCHIVE_AFTER` | `72h` | 会话最后一次写入超过该时长后才归档 |
| `audit.archive_at` | `AUDIT_ARCHIVE_AT` | `03:00` | 每天执行归档的本地时间；`off` 关闭定时归档（仍可调接口手动执行） |
| `audit.archive_encoder_level` | `AUDIT_ARCHIVE_ENCODER_LEVEL` | `better` | 会话归档的 zstd 级别（夜间执行，偏重压缩率） |
| `audit.archive_window_mb` | `AUDIT_ARCHIVE_WINDOW_MB` | `32` | 会话归档的匹配窗口（上限 128）；越大跨请求去重越好，读取 / 合并时内存约为 2 倍窗口 |
| `audit.active_window_mb` | `AUDIT_ACTIVE_WINDOW_MB` | `4` | 单请求文件的匹配窗口；需覆盖客户端请求体到上游请求体的距离 |
| `audit.session_headers` | `AUDIT_SESSION_HEADERS` | 见下 | 会话 ID 请求头，按顺序取第一个非空值；环境变量用逗号分隔 |
| `audit.redact_headers` | `AUDIT_REDACT_HEADERS` | 见下 | 仅鉴权类 Header，落盘前脱敏 |
| `audit.redact_query_params` | `AUDIT_REDACT_QUERY_PARAMS` | `key,api_key,access_token` | URL 中的鉴权参数（如 Gemini `?key=`） |
| `audit.session_cache_size` | `AUDIT_SESSION_CACHE_SIZE` | `10000` | 会话→日期目录 LRU 缓存上限（有界） |
| `audit.memory_buffer_bytes` | `AUDIT_MEMORY_BUFFER_BYTES` | `524288` | 单请求内存累积阈值；超过后切换后台流式压缩 |
| `audit.encoder_level` | `AUDIT_ENCODER_LEVEL` | `default` | active 文件的 zstd 级别：`fastest` / `default` / `better` / `best`（单请求实测 `default` 与 `fastest` 速度相同，体积约小一半） |
| `audit.display_body_limit` | `AUDIT_DISPLAY_BODY_LIMIT` | `4194304` | 管理台单段正文展示上限；**只影响展示**，存储与下载永远完整 |
| `audit.finalize_timeout` | `AUDIT_FINALIZE_TIMEOUT` | `30s` | 停机时等待归档落盘的最长时间 |
| `audit.flush_workers` | `AUDIT_FLUSH_WORKERS` | `max(4, 2×CPU)` | 后台落盘并发上限（限制同时打开的文件数） |

默认会话头（兼容常见客户端）：

| 客户端 | Header |
|---|---|
| Claude Code | `X-Claude-Code-Session-Id` |
| Codex CLI | `session_id`、`conversation_id` |
| OpenRouter / 通用 | `Session-Id`、`X-Session-Id` |
| OpenCode | `X-OpenCode-Session` |
| Gemini 类客户端 | `X-Gemini-Session-Id`、`X-Goog-Session-Id` |

默认脱敏头：`Authorization`、`Proxy-Authorization`、`X-Api-Key`、`Api-Key`、`X-Goog-Api-Key`、`Cookie`、`Set-Cookie`、`X-Auth-Token`。
脱敏规则：保留前后各 4 位，中间以 `*` 替换（`Bearer ` 等方案前缀保留；长度 ≤ 8 时全部遮蔽）。**只作用于归档副本，真实请求不受影响**；正文与其它 Header 一字节不改。

```yaml
# config.yaml 示例
audit:
  enabled: true
  active_dir: /ssd/sub2api/audit-active     # 高速盘
  archive_dir: /hdd/sub2api/audit-archive   # 大容量盘
  archive_at: "03:00"
  archive_after: 72h
```

---

## 2. 目录与文件格式

```
{active_dir}/                                   # 高速盘：请求时只写这里
  {yyyy-MM}/{dd}/{sessionId}/{clientRequestId}-{n}.zst   # 每请求一个文件，四个阶段共用一条压缩流
  {yyyy-MM}/{dd}/nosession/...                  # 无会话请求：当天共用
  .sessions/{sha256[:2]}/{sha256}               # 会话首个日期目录（原子 link 创建）
  .index/{h[:2]}/{h[2:4]}/{h}.idx               # 请求 ID / 用量 request_id → 归档引用（追加写）

{archive_dir}/                                  # 大容量盘：夜间任务写入，平时只读
  {yyyy-MM}/{dd}/{sessionId}.zst                # 每会话一个文件，会话内全部请求共用一条压缩流
```

归档引用（ref）始终是 `{yyyy-MM}/{dd}/{sessionId}/{clientRequestId}-{n}`，与请求当前在 active 还是 archive 无关，因此会话映射、请求索引、LRU 缓存在归档前后都不变。

### 写入（请求热路径）

- **只做内存操作**：目录 / 文件创建、压缩、索引写入都在请求结束后的后台 worker 中完成（有界并发）；原始内容超过 `memory_buffer_bytes` 时提前切换为后台流式压缩落盘。
- **每请求一个文件，O_EXCL 创建，永不覆盖**；同一请求 ID 重复出现时 `n` 递增（`-1`、`-2`…），并跳过索引中已存在的引用（含已归档的），查看时以“候选列表”呈现关联歧义。
- **同会话固定首次日期目录**：跨天、重启后仍写入首次出现的日期目录；定位只读一个标记文件，**不扫描历史目录**；内存缓存为有界 LRU。
- **会话已归档后又有新请求（重激活）**：照常写入 active 中的同一相对路径，不读取、不解压 archive；由下一次夜间任务合并。
- 日期按服务配置的时区（`timezone` / `TZ`）计算。

### 夜间归档（默认每天 03:00）

1. 只遍历 active 的三层目录（最多约 `archive_after` 时长的数据），**不扫描 archive**；active 中的会话目录本身就是待归档 / 重激活记录。
2. 会话最后活跃时间 = 会话目录与其中文件的最大 mtime；晚于 `now - archive_after` 的跳过。正在写入（持有会话写入锁）的会话跳过，留到下一轮。
3. 没有旧归档：按完成时间顺序把会话内所有请求写入一条 zstd 流（`archive_window_mb` 大窗口，相邻请求的对话前缀被去重）。
   已有旧归档（重激活）：流式解压旧归档、逐帧接到新流前部，再追加 active 中的新请求，整体重新压缩，保持完整的跨请求去重；内存只占压缩窗口，不在 active 落地解压。
4. 写同目录临时文件 → fsync → rename 覆盖 → fsync 目录 → 删除 active 中的请求文件与空目录。
   - 中途崩溃：临时文件残留不影响读取；若已 rename 未删除 active，下次合并按引用去重，不会重复写入。
   - 旧归档损坏：放弃该会话（active 与旧归档都保持原样），在结果中报告。
5. 单个 zstd frame 原始数据超过 1 GB 时另起 frame（同一文件内仍是可顺序解压的一条流），只是兜底。

### 读取

- 先查 active（命中即一次解压得到四个阶段），否则打开 archive 中的会话文件，顺序解压到目标请求为止，其余请求的负载直接跳过、不复制；内存只占解压窗口（≤ `archive_window_mb`），**不产生任何临时文件**。
- 合并期间被替换的归档文件对已打开的读取无影响（POSIX 语义）；rename 后、删除 active 前两边都有数据，内容一致。

### 文件内容

- 请求文件：一个完整 zstd 流（带 CRC），解压后为 `S2AAUD02` 魔数 + 帧序列。帧头 8 字节：类型、阶段、Seq、长度。每段报文以 `H`（JSON 头，已脱敏）开始，`B` 为正文原始字节分块，`E`（JSON 尾：字节数、xxhash64、是否正常结束、错误）结束；缺少 `E` 即“不完整”。上游两个阶段中每个调度尝试（attempt）一段，重试 / 账户切换都可逐段查看。
- 会话文件：`S2AARC01` 魔数 + 若干条目；条目 = `Q`（JSON：ref、归档前大小、mtime）+ 该请求的全部帧 + `Z`（JSON：读取状态；截断的请求保留中断前的完整帧）。
- 写入路径只算廉价的 xxhash64，面向使用者的 SHA-256 在读取 / 下载时计算（下载响应头 `X-Archive-Body-SHA256`，并以 `X-Archive-Intact` 标示与写入记录是否一致）。
- WebSocket：客户端侧在劫持连接上记录原始帧字节（展示时解析掩码、分片与 permessage-deflate 上下文接管）；上游侧以消息粒度记录（连接池复用时按请求切换归属并标注 `ws_reused`）。
- 上游 `Content-Encoding` 原样归档（如 gzip 字节），展示阶段才解码；存储压缩（zstd）、上游内容编码、传输压缩三者互不混用。

### 压缩效果与性能

合成对话（每轮请求包含此前全部消息，见 `TestArchiveCompressionRatio`，`AUDIT_RATIO_TURNS` 可调），2 vCPU 沙箱：

| 会话 | 原始请求体（客户端 + 上游） | active：逐请求文件合计 | archive：会话文件 | 归档耗时 | 从归档读取最后一个请求 |
|---|---|---|---|---|---|
| 60 轮 | 31.4 MB | 4.8 MB | 0.17 MB（小 28 倍） | 0.4 s | 74 ms |
| 200 轮 | 345.8 MB | 52.8 MB | 2.1 MB（小 25 倍） | 2.6 s | 334 ms |

- 读取归档的耗时与会话**原始**大小成正比（顺序解压约 1 GB/s），与压缩后大小无关；内存约为 2 倍 `archive_window_mb`，同时最多 4 个归档解压。
- 窗口实测（200 轮，`better`）：16 MB → 4.3 MB，32 MB → 2.3 MB，64 MB → 1.3 MB；解压内存分别约 32 / 64 / 128 MB。
- 会话归档的压缩与解压关闭了 zstd 低内存模式：32 MB 窗口下低内存模式压缩慢约 40 倍、解压慢约 10 倍。

手工查看（无需服务）：

```bash
zstd -dc active/2026-10/14/sess/abc-1.zst | less     # 二进制帧，含 JSON 头
zstd -dc archive/2026-10/14/sess.zst | less
```

---

## 3. 链路关联

| 字段 | 来源 |
|---|---|
| 请求 ID | 预置的 `client_request_id`（与 `X-Client-Request-ID` 响应头、ops 日志一致） |
| 用量记录 | `usage_logs.request_id`（`client:<请求ID>` 或强制前缀 ID），由 OnUsage 钩子追加索引 |
| API Key / 用户 / 分组 / 账户 | 请求结束时从 Gin 上下文读取；每个上游尝试单独记录账户 ID |
| trace / span | 进程内 OTel span → W3C `traceparent` → 自动生成；每次上游尝试一个子 span。只读取，不向上游注入头 |
| 调度尝试 | RoundTripper 每次 RoundTrip 一个 attempt（含 WS 握手） |

**用量回调不冒充事务提交成功**：OnUsage 在仓储层写入返回后触发，状态为 `write_returned` / `duplicate_ignored` / `pending_tx_not_proven`（处于外部事务中）/ `write_failed`；页面展示的用量记录始终来自数据库查询。

管理接口（管理员鉴权之后）：

| 接口 | 说明 |
|---|---|
| `GET /api/v1/admin/audit-archive/status` | 是否启用、active / archive 目录、写入统计、最近一次归档结果 |
| `GET /api/v1/admin/audit-archive/usage/:id[?ref=]` | 按用量记录查找并解析归档；`resolution` = `unique` / `best_match_with_alternatives` / `ambiguous` / `not_found` / `chosen` |
| `GET /api/v1/admin/audit-archive/lookup?request_id=` | 按任意请求 ID 查候选 |
| `GET /api/v1/admin/audit-archive/archive?ref=` | 解析指定请求的四个阶段（`storage.location` = `active` / `archive`） |
| `GET /api/v1/admin/audit-archive/body?ref=&kind=&index=&decode=0\|1` | 流式下载某段正文；`decode=0` 为原始字节（响应头 `X-Archive-Body-SHA256` 可校验），`decode=1` 按 Content-Encoding 解码 |

| `GET /api/v1/admin/audit-archive/bundle?ref=` | 下载一个请求的全部阶段（单个 `.zst`，格式同 active 请求文件，四阶段共用压缩）；在 active 中时直接发送文件字节 |
| `POST /api/v1/admin/audit-archive/archive/run` | 立即执行一次归档（同步返回结果；已在执行时 409） |

读取全程在内存中流式解压，**不产生任何临时文件**。

---

## 4. 前端增强（构建补丁）

`frontend-audit/` 用 Vite 插件在构建期把查看入口挂到「使用记录」表格的“时间”单元格（始终可见列；桌面表格与移动端卡片共用同一插槽），入口直接绑定行对象，分页 / 排序 / 跳转后不会串日志。磁盘上的原始 `frontend/` 源文件保持不变；不使用该补丁构建时，产物与上游完全一致。

```bash
cd frontend && pnpm install --frozen-lockfile && cd ..
./frontend-audit/build.sh                 # i18n 检查 + vue-tsc + 补丁测试 + vite build → backend/internal/web/dist
cd backend && go build -tags embed -o sub2api ./cmd/server
```

基线变化导致锚点失效时，`build.sh` / 插件会直接失败，而不是静默产出没有入口的产物。

---

### 正文展示（Monaco Editor）

正文的原始文本直接交给 Monaco Editor（VS Code 编辑器内核，MIT）。以下功能均为编辑器自带，补丁没有自行解析 JSON：

| 功能 | 说明 |
|---|---|
| JSON 排版与高亮 | 压缩成一行的 JSON 用编辑器内置格式化排版，按文本处理，`1.0`、大整数原样保留 |
| 折叠 | 行号旁箭头折叠 / 展开对象、数组；另有「全部折叠 / 全部展开」 |
| 换行 | 默认不换行，超长行横向滚动；「自动换行」切换折行显示 |
| 复制 | 「复制」写入原始文本，与归档一致，不受排版和折叠影响；HTTP 部署下自动回退复制方式 |
| 其他 | 只读；跟随站点明暗主题；非 JSON 内容（SSE 原文、Base64）按纯文本展示 |

依赖与加载：
- Monaco 放在 `frontend-audit/deps/`，有独立的 `package.json` 和 lockfile，`build.sh` 会自动安装。原 `frontend/package.json` 不修改。
- 构建时单独成包（`audit-monaco-*.js`，约 1 MB gzip，外加两个 Worker 约 200 KB）。只在打开归档正文时按需加载，首页预加载内容与原版一致。
- 兼容默认 CSP：`worker-src 'self' blob:`、内联样式、`data:` 字体均已允许。

## 5. 传输压缩（Nginx）

传输压缩交给前置 Nginx 按 `Accept-Encoding` 协商（浏览器自动解码，前端不手动解压）；模型网关的 SSE / WebSocket 路径不压缩、不缓冲。示例见 [`nginx.conf.example`](./nginx.conf.example)。

---

## 6. Fork + CI 打包

`.github/workflows/audit-archive.yml`：

1. `backend`：vet、`go test -race ./internal/audit/...`、相关包单测、写入基准冒烟；
2. `package`（amd64/arm64）：`frontend-audit/build.sh` → `go build -tags embed` → 校验二进制内含补丁标记 → 上传产物；amd64 额外用 `Dockerfile.goreleaser` 构建镜像（`workflow_dispatch` 勾选 `push_image` 时推送到 `ghcr.io/<owner>/sub2api-audit`）。

产物与原服务启动方式完全相同（同一二进制、同一 `config.yaml` / 环境变量、同一端口）。

---

## 6.1 Docker Compose（原版镜像，启动时编译）

`deploy/audit-standalone/` 使用原版镜像 `weishaw/sub2api:<版本>`，不修改上游源码，也不构建镜像。PostgreSQL / Redis 按独立部署连接（变量与上游 `docker-compose.standalone.yml` 相同）。

| 文件 | 作用 |
|---|---|
| `docker-compose.yml` | 服务定义；把下面三个文件只读挂载到 `/opt/audit/` |
| `entrypoint.sh` | 包装启动脚本（替换镜像 entrypoint） |
| `build.sh` | 容器内编译脚本 |
| `audit.patch` | 明文补丁（`backend/` + `frontend-audit/`，不含测试），由 `make-patch.sh` 生成 |

```bash
# 服务器上只需要这个目录（4 个文件）和 .env
#   .env：SUB2API_VERSION=0.2.17、DATABASE_HOST、DATABASE_PASSWORD、REDIS_HOST ...
docker compose up -d
docker compose logs -f sub2api      # 首次启动会看到 [audit] / 编译日志
```

启动流程（`entrypoint.sh` → `build.sh`）：

1. 读取镜像内原版程序的版本与源码提交（`/app/sub2api --version`），因此源码一定与镜像对应；
2. 缓存卷中没有「提交 + 补丁」对应的产物时，在容器内编译一次：
   - 临时 `apk add go nodejs npm git`；
   - 下载同一提交的源码，`git apply` 补丁；
   - 构建前端，补丁锚点和标记都会检查；
   - `go build -tags embed`；
3. 编译在独立进程组中运行，结束后整组清理，并卸载编译依赖、删除源码与缓存。运行期只剩 sub2api 一个进程；
4. 用产物替换 `/app/sub2api`（原版保留为 `/app/sub2api.orig`），调用原版 `/app/docker-entrypoint.sh`。

| 场景 | 行为 |
|---|---|
| 同版本重启 / 重建容器 | 直接用缓存产物（`sub2api_audit_build` 卷，约 120 MB），约 2 秒 |
| 升级（改 `SUB2API_VERSION`） | 自动按新版本重新编译，旧产物删除 |
| 补丁打不上 / 编译失败 | 以原版程序启动（无归档），日志给出原因；记录失败，不会每次重启都重试（`AUDIT_REBUILD=true` 重试，`AUDIT_REQUIRE_PATCH=true` 则拒绝启动） |

资源（4 核实测）：首次启动约 5 分钟，编译期间系统内存峰值约 3.5 GB（`AUDIT_BUILD_JOBS=1`），运行期与原版相同。

国内网络：`GOPROXY` / `GOSUMDB` 默认与上游 Dockerfile 相同（goproxy.cn）；可设 `NPM_CONFIG_REGISTRY`、`APK_MIRROR`、`AUDIT_SOURCE_URL`（`@COMMIT@` 会被替换为提交号）。

更新补丁：`deploy/audit-standalone/make-patch.sh [上游基线提交]`。

## 7. 运维

- **容量**：归档不做自动清理，按 archive 中的日期目录（`yyyy-MM/dd`）整体删除即可；`.index` 中指向已删除归档的条目在页面上显示为“文件缺失”。
- **分盘**：`active_dir` 与 `archive_dir` 可以在不同文件系统；归档的临时文件写在 archive 同目录下，rename 不跨盘。
- **旧格式**：本版本之前的四文件布局（`{root}/{yyyy-MM}/.../*-client_request.zst`）不再读取。
- **停机**：`SIGTERM` 后先关闭 HTTP 服务，再等待归档落盘（`finalize_timeout`）。
- **单实例**：会话日期标记与索引基于本地文件系统，按单实例部署设计。
- **关闭**：`AUDIT_ENABLED=false`，所有钩子透传，行为与未打补丁时一致。
