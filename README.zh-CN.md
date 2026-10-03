# muse-proxy

[English](./README.md)

一个挡在 **OpenCode Zen** 前面的轻量 Go 网关。同时讲三种推理协议——
**Chat Completions**、**Responses**、**Anthropic Messages**——按
OpenCode 客户端同一份能力目录把每个模型路由到它的**原生协议**，
客户端要别的协议就实时转换。

同协议流量**逐字节透传**——图片、reasoning 参数、工具定义绝不重建。
只用标准库，约 2000 行，单个静态二进制，无数据库。

## 为什么做这个

免费层 Zen 模型只接**流式、agent 形状**的请求，而且每个模型家族的
**原生协议都不一样**（`muse-spark-*` 要 Responses，`mimo-*`/`big-pickle`
要 Chat，`minimax-*-free` 要 Messages——见[协议判定表](./docs/protocol-table.md)）。
调错端点就会吃 `FreeTierError`、`ModelError` 或假的
`Endpoint is unavailable`。这个代理把这些全藏起来：随便用哪个协议，
它自动转换。

## 功能

- **三合一 API**：`POST /v1/chat/completions`、`/v1/responses`、`/v1/messages`
- **目录路由**：从 `https://models.opencode.ai/api.json` 解析
  `model → 原生协议`（`model.provider.npm ?? provider.npm`），落盘缓存，24h 刷新
- **自动转换**：请求体经小 IR 翻译；流式响应 SSE→SSE 实时转码；
  `stream:false` 把任意原生流折叠成单个 JSON
- **agent 形状保障**：客户端声明的工具优先原样保留，但每个工具保证有
  parameters（缺的补 `{"type":"object","properties":{}}`），缺失的核心工具
 （bash/edit/glob/grep/read）追加最小定义，免费层门禁可过
- **不存内容只存 hash 的会话持久化**：请求文本前 10k 字符 SHA-256 →
  稳定 `ses_*` / `prj_*`；落盘只有 hash，3 天清理
- **智能出口**：优先本机直连，429/5xx/传输失败时切代理池；池内每次都建
  新连接（轮转型代理池按连接分配出口，keep-alive 会钉死单节点），ALPN
  钉死 HTTP/1.1
- **订阅池**：`proxy_sources` 指向纯文本代理列表；每个源在启动时拉取、
  并发测活、只收录活节点（可定时刷新），死节点永不进入轮转
- **空回拦截**：无文本、无工具调用的 2xx 一律不透传，内部换新连接重发；
  且保持实时流式——只扣留前缀，一旦出现真实内容（或超过扣留时限）立即
  冲刷并转为实时透传，客户端能及时收到首个事件
- **可观测**：`GET /healthz`（目录年龄/出口状态/hash 数），
  `x-request-id` / `x-muse-session` 回显头，每请求一行日志

## 快速开始

```bash
cp config.example.json config.json
# 编辑 config.json: server_keys, zen_key, proxies
go build -o muse-proxy .
./muse-proxy -config config.json
```

Docker（不映射宿主端口，直接进后端网络）：

```bash
cp config.example.json config.json  # 先改密钥
docker compose up -d --build
docker compose logs -f
```

Compose 把 `./config.json` 只读挂载为种子，`./data` 存 hash库 + 目录缓存。
种子只在首次启动导入 state 卷。

## 调用

```bash
# 任意模型用任意协议，自动转换。
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer YOUR_LOCAL_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"muse-spark-1.3-contributor-free",
       "messages":[{"role":"user","content":"hi"}],
       "stream":true,
       "tools":[{"type":"function","function":{
         "name":"bash","description":"run",
         "parameters":{"type":"object","properties":{}}}}]}'
```

`server_keys` 是客户端调本网关的凭证，和上游 `zen_key` 是两回事，
绝不外泄。健康检查免鉴权。

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/healthz` | 就绪 + 目录/出口/存储统计 |
| `GET` | `/v1/models` | 上游模型列表，原样透传 |
| `POST` | `/v1/chat/completions` | Chat Completions |
| `POST` | `/v1/responses` | Responses |
| `POST` | `/v1/messages` | Anthropic Messages（也接受 `x-api-key`） |

一个源的写法：

```json
{
  "url": "https://example.com/proxies.txt",
  "protocol": "",
  "test_url": "https://opencode.ai/zen/v1/models",
  "timeout_seconds": 8,
  "concurrency": 48,
  "max_keep": 40,
  "refresh_hours": 6
}
```

- `protocol` 覆盖格式识别（`http`/`https`/`socks5`）；无 scheme 的行
 （`host:port`、`ip:port:user:pass`）用它，缺省 `http`。
- `test_url` 会经由每个代理请求一次；除 429/5xx 外任何 HTTP 响应都算活。
 URL 里的账密（Basic）可直接用。
- `max_keep` 只保留最快的 N 个（`0` = 全收）。
- `refresh_hours: 0` 表示只在启动时检查一次。
- `/healthz` 有每个池的 `sources/live/rejected` 计数；每次刷新整体替换，
 死节点不会累积。

## 配置说明

| 字段 | 默认值 | 含义 |
|---|---|---|
| `listen` | `127.0.0.1:8080` | 监听地址 |
| `server_keys` | （必填） | 本地客户端密钥 |
| `zen_key` | （必填） | 上游 Zen 凭证（免费模型填 `public` 即可） |
| `upstream` | `https://opencode.ai/zen` | Zen 基地址 |
| `proxies` | `["direct"]` | 出口池：`direct`、`http(s)://`、`socks5(h)://`（URL 可带账密） |
| `reuse_proxy_connections` | `false` | 仅固定单节点代理才开；代理池必须每次新建连接才能轮转 |
| `prefer_direct` | `true` | 优先本机出口，429/5xx/传输失败时切池 |
| `direct_cooldown_seconds` | `120` | 429 后直连冷却多久（更大的 `Retry-After` 优先） |
| `pool_max_attempts` | `3` | 每个请求在池内重试几次（每次都是新连接） |
| `proxy_sources` | `[]` | 订阅 URL（每行一个代理）；每个源启动时拉取+并发测活（阻塞，最多 3 分钟），只收录活节点 |
| `retry_empty` | `true` | 空 2xx 不透传，内部重发 |
| `max_empty_retries` | `2` | 空回的额外内部尝试次数（用完仍把最后一次透传） |
| `empty_guard_timeout_seconds` | `30` | 流式回包在"判定是否为空"期间最多被扣留多久；超时即冲刷已扣留的前缀并转为实时流（`0` = 等到出现内容或流结束） |
| `hash_store_path` | `./hashes.json` | 内容 hash→会话映射（只存 hash，0600 权限） |
| `hash_ttl_days` | `3` | 超过多久没见就清理 |
| `hash_max_entries` | `50000` | 上限，先淘汰最旧 |
| `hash_prefix_chars` | `10000` | 取请求文本前多少字符做 hash |
| `catalog_url` | `https://models.opencode.ai/api.json` | 能力目录 |
| `catalog_path` | `./catalog.json` | 持久化的模型→协议表 |
| `catalog_refresh_hours` | `24` | 刷新间隔（失败保留旧表） |
| `request_timeout_seconds` | `600` | 单请求总预算（含全部尝试） |

`LISTEN` 环境变量覆盖 `listen`。未知 JSON 字段直接拒绝。

## 会话标识

优先级：显式 `x-opencode-session` / `x-session-affinity` /
`X-Session-Id` 头 → `metadata.session_id` → hash 库命中 → 现场生成。
非规范形状一律哈希成官方 `ses_<12hex><14base62>`（免费层拒收其他形状）。
Project 同理，对应 `prj_<24hex>`。

## 开发

```bash
gofmt -l . && go vet ./... && go test ./...
go build -o muse-proxy .
```

交叉编译：`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build
-trimpath -ldflags="-s -w -X main.version=vX.Y.Z" -o muse-proxy .`

## 致谢

协议研究基于 [anomalyco/opencode](https://github.com/anomalyco/opencode)
与 [jasonxu114514/opencode2api](https://github.com/jasonxu114514/opencode2api)。
完整模型→协议映射与上游错误分类见 [docs/protocol-table.md](./docs/protocol-table.md)。
