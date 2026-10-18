# 请求报文归档（Request Archive）

对经过网关的每个客户端请求，完整采集 **客户端请求 / 实际上游请求 / 上游响应 / 客户端响应** 四份 Header + Body（HTTP、SSE、WebSocket），以 zstd 整体压缩写入本地磁盘，并在管理台「使用记录」每一行提供查看入口。

- 后端：独立包 `backend/internal/audit`，通过 4 个钩子接入（Gin、RoundTripper、OnUsage、OTel trace）。
- 前端：独立构建补丁 `frontend-audit/`，构建期注入，不修改 `frontend/` 下任何 Vue/TS 源文件。
- 部署：仍是同一个服务、同一个进程，单实例运行；无扩展、无包装服务、无新增进程。

---

## 1. 配置

归档读取全局配置的 `audit:` 段（`config.yaml`），或同名环境变量（`.` → `_`，大写）。**全部有默认值，零配置即可开启。**

| 键 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `audit.enabled` | `AUDIT_ENABLED` | `true` | `false` 时所有钩子直接透传，不产生文件 |
| `audit.root_dir` | `AUDIT_ROOT_DIR` | `./data/audit` | 归档根目录（相对进程工作目录；Docker 镜像中即 `/app/data/audit`，随数据卷持久化） |
| `audit.session_headers` | `AUDIT_SESSION_HEADERS` | 见下 | 会话 ID 请求头，按顺序取第一个非空值；环境变量用逗号分隔 |
| `audit.redact_headers` | `AUDIT_REDACT_HEADERS` | 见下 | 仅鉴权类 Header，落盘前脱敏 |
| `audit.redact_query_params` | `AUDIT_REDACT_QUERY_PARAMS` | `key,api_key,access_token` | URL 中的鉴权参数（如 Gemini `?key=`） |
| `audit.session_cache_size` | `AUDIT_SESSION_CACHE_SIZE` | `10000` | 会话→日期目录 LRU 缓存上限（有界） |
| `audit.memory_buffer_bytes` | `AUDIT_MEMORY_BUFFER_BYTES` | `524288` | 单文件内存累积阈值；超过后切换后台流式压缩 |
| `audit.encoder_level` | `AUDIT_ENCODER_LEVEL` | `fastest` | zstd 级别：`fastest` / `default` / `better` / `best` |
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
  root_dir: /data/sub2api/audit
  encoder_level: fastest
```

---

## 2. 目录与文件格式

```
{root}/
  {yyyy-MM}/{dd}/{sessionId}/{clientRequestId}-{n}-client_request.zst
                                                   -upstream_request.zst
                                                   -upstream_response.zst
                                                   -client_response.zst
  {yyyy-MM}/{dd}/nosession/...           # 无会话请求：当天共用
  .sessions/{sha256[:2]}/{sha256}        # 会话首个日期目录（原子 link 创建）
  .index/{h[:2]}/{h[2:4]}/{h}.idx        # 请求 ID / 用量 request_id → 归档位置（追加写）
```

- **请求热路径只做内存操作**：目录 / 文件创建、压缩、索引写入都在请求结束后的后台 worker 中完成（有界并发）；单个文件原始内容超过 `memory_buffer_bytes` 时提前切换为后台流式压缩落盘。进行中的请求在结束前通常尚未落盘。
- **每请求四份文件，O_EXCL 创建，永不覆盖**；同一请求 ID 重复出现时 `n` 递增（`-1`、`-2`…），查看时以“候选列表”呈现关联歧义。
- **同会话固定首次日期目录**：跨天、重启后仍写入首次出现的日期目录；定位只读一个标记文件，**不扫描历史目录**；内存缓存为有界 LRU。
- 日期按服务配置的时区（`timezone` / `TZ`）计算。
- 文件内容：一个完整 zstd 流（带 CRC），解压后为 `S2AAUD01` 魔数 + 帧序列。每段报文以 `H`（JSON 头，已脱敏）开始，`B` 为正文原始字节分块，`E`（JSON 尾：字节数、xxhash64、是否正常结束、错误）结束；缺少 `E` 即“不完整”。写入路径只算廉价的 xxhash64，面向使用者的 SHA-256 在读取 / 下载时计算（下载响应头 `X-Archive-Body-SHA256`，并以 `X-Archive-Intact` 标示与写入记录是否一致）。上游两份文件中每个调度尝试（attempt）一段，重试/账户切换都可逐段查看。
- WebSocket：客户端侧在劫持连接上记录原始帧字节（展示时解析掩码、分片与 permessage-deflate 上下文接管）；上游侧以消息粒度记录（连接池复用时按请求切换归属并标注 `ws_reused`）。
- 上游 `Content-Encoding` 原样归档（如 gzip 字节），展示阶段才解码；存储压缩（zstd）、上游内容编码、传输压缩三者互不混用。

手工查看（无需服务）：

```bash
zstd -dc 2026-10/14/sess/abc-1-client_response.zst | less   # 二进制帧，含 JSON 头
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
| `GET /api/v1/admin/audit-archive/status` | 是否启用、根目录、写入统计 |
| `GET /api/v1/admin/audit-archive/usage/:id[?ref=]` | 按用量记录查找并解析归档；`resolution` = `unique` / `best_match_with_alternatives` / `ambiguous` / `not_found` / `chosen` |
| `GET /api/v1/admin/audit-archive/lookup?request_id=` | 按任意请求 ID 查候选 |
| `GET /api/v1/admin/audit-archive/archive?ref=` | 解析指定归档 |
| `GET /api/v1/admin/audit-archive/body?ref=&kind=&index=&decode=0\|1` | 流式下载某段正文；`decode=0` 为原始字节（响应头 `X-Archive-Body-SHA256` 可校验），`decode=1` 按 Content-Encoding 解码 |

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

- **容量**：归档不做自动清理，按目录（`yyyy-MM/dd`）整体删除即可；`.index` 中指向已删除归档的条目在页面上显示为“文件缺失”。
- **停机**：`SIGTERM` 后先关闭 HTTP 服务，再等待归档落盘（`finalize_timeout`）。
- **单实例**：会话日期标记与索引基于本地文件系统，按单实例部署设计。
- **关闭**：`AUDIT_ENABLED=false`，所有钩子透传，行为与未打补丁时一致。
