# muse-proxy

[简体中文](./README.zh-CN.md)

A tiny Go gateway in front of **OpenCode Zen**. It speaks all three inference
protocols — **Chat Completions**, **Responses**, **Anthropic Messages** —
routes each model to its **native protocol** (looked up from the same
capability directory the OpenCode client uses), and converts on the fly when
the client asks for something else.

Same-protocol traffic is forwarded **byte-for-byte** — images, reasoning
knobs and tool definitions are never rebuilt. Stdlib only, ~2k lines,
single static binary, no database.

## Why this exists

Free-tier Zen models only serve **streaming, agent-shaped** requests, and
each model family speaks a **different native protocol** (`muse-spark-*`
wants Responses, `mimo-*`/`big-pickle` want Chat, `minimax-*-free` wants
Messages — see [protocol table](./docs/protocol-table.md)). Call the wrong
endpoint and you get `FreeTierError`, `ModelError`, or a fake
`Endpoint is unavailable`. This proxy hides all of that: pick any protocol,
it converts.

## Features

- **3-in-1 API**: `POST /v1/chat/completions`, `/v1/responses`, `/v1/messages`
- **Catalog routing**: resolves `model → native protocol` from
  `https://models.opencode.ai/api.json` (`model.provider.npm ??
  provider.npm`), cached on disk, refreshed every 24h
- **Auto conversion**: request bodies translate through a small IR;
  streaming responses transcode SSE→SSE live; `stream:false` collapses any
  native stream into a single JSON document
- **Agent-shape guarantee**: client tools always win, but every tool is
  ensured a parameters schema, and missing core tools (`bash/edit/glob/grep/read`)
  are appended as minimal definitions so free-tier requests pass the gate
- **Session persistence without storing content**: SHA-256 of the first 10k
  chars of request text → stable `ses_*` / `prj_*` IDs; only hashes hit disk,
  entries expire after 3 days
- **Smart egress**: prefer direct, fail over to a proxy pool on
  429/5xx/transport errors; pool attempts always dial fresh connections
  (rotating pools assign egress per connection — keep-alive would pin you
  to one node), with ALPN pinned to HTTP/1.1
- **Subscription pools**: point `proxy_sources` at plaintext proxy lists;
  every source is fetched, health-checked concurrently and filtered at
  startup (and on schedule), so dead nodes never enter rotation
- **Empty-reply guard**: 2xx answers with no text and no tool calls are
  retried internally on a fresh connection instead of passed through.
  Streaming stays live: only the prefix is held, and it is flushed as
  soon as real content (or the hold deadline) arrives, so the client
  keeps receiving first events promptly.
- **Observability**: `GET /healthz` (catalog age, egress state, hash count),
  `x-request-id` / `x-muse-session` echo headers, one log line per request

## Quick start

```bash
cp config.example.json config.json
# edit config.json: server_keys, zen_key, proxies
go build -o muse-proxy .
./muse-proxy -config config.json
```

Docker (no published ports — join your backend network instead):

```bash
cp config.example.json config.json  # edit keys first
docker compose up -d --build
docker compose logs -f
```

Compose mounts `./config.json` read-only as the seed and `./data` for the
hash store + catalog cache. The seed is imported into the state volume only
on first start.

## Calling it

```bash
# Any protocol works for any model; conversion is automatic.
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

`server_keys` authenticate clients to this gateway. They are separate from
`zen_key` (the upstream credential) and never leave the server. Health
checks need no authentication.

| Method | Path | Description |
|---|---|---|
| `GET` | `/healthz` | readiness + catalog/egress/store stats |
| `GET` | `/v1/models` | upstream model list, passed through |
| `POST` | `/v1/chat/completions` | Chat Completions |
| `POST` | `/v1/responses` | Responses |
| `POST` | `/v1/messages` | Anthropic Messages (`x-api-key` also accepted) |

A source entry:

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

- `protocol` overrides scheme detection (`http`/`https`/`socks5`);
  scheme-less lines (`host:port`, `ip:port:user:pass`) default to it
  (`http` when unset).
- `test_url` is fetched through each proxy; any HTTP response except
  429/5xx counts as alive. Basic/digest proxy credentials in the URL work.
- `max_keep` keeps the fastest N nodes (`0` = all live ones).
- `refresh_hours: 0` means check once at startup only.
- `/healthz` reports per-pool `sources/live/rejected` counters; dead
  nodes are replaced wholesale on every refresh, never accumulated.

## Configuration reference

| Field | Default | Meaning |
|---|---|---|
| `listen` | `127.0.0.1:8080` | API listen address |
| `server_keys` | (required) | local client keys |
| `zen_key` | (required) | upstream Zen credential (`public` works for free models) |
| `upstream` | `https://opencode.ai/zen` | Zen base URL |
| `proxies` | `["direct"]` | egress pool: `direct`, `http(s)://`, `socks5(h)://` (URL credentials OK) |
| `reuse_proxy_connections` | `false` | only enable for a fixed single-node proxy; pools need fresh dials to rotate |
| `prefer_direct` | `true` | try local egress first, fail over to pool on 429/5xx/transport errors |
| `direct_cooldown_seconds` | `120` | skip direct this long after a 429 (honors larger `Retry-After`) |
| `pool_max_attempts` | `3` | retries through the pool per request (each a fresh connection) |
| `proxy_sources` | `[]` | subscription URLs (one proxy per line); each source is fetched + health-checked at startup (blocking, max 3 min) and only live proxies join the pool |
| `retry_empty` | `true` | drop empty 2xx replies instead of forwarding them; re-send internally |
| `max_empty_retries` | `2` | extra internal attempts for empty replies (last reply forwarded even if empty) |
| `empty_guard_timeout_seconds` | `10` | how long a streaming reply may be held while deciding it is non-empty; on expiry the held prefix is flushed and the stream goes live (`0` = wait for content or stream end) |
| `hash_store_path` | `./hashes.json` | content-hash → session/project map (hashes only, mode 0600) |
| `hash_ttl_days` | `3` | entries unseen this long are purged |
| `hash_max_entries` | `50000` | cap; stalest evicted first |
| `hash_prefix_chars` | `10000` | request-text prefix hashed for identity |
| `catalog_url` | `https://models.opencode.ai/api.json` | capability directory |
| `catalog_path` | `./catalog.json` | persisted model→protocol table |
| `catalog_refresh_hours` | `24` | refresh interval (failures keep the old table) |
| `request_timeout_seconds` | `600` | total budget per request, attempts included |

`LISTEN` env overrides `listen`. Unknown JSON fields are rejected.

## Session identity

Precedence: explicit `x-opencode-session` / `x-session-affinity` /
`X-Session-Id` headers → `metadata.session_id` → hash-store match →
freshly minted. Non-canonical IDs are hashed into the official
`ses_<12hex><14base62>` shape (the free tier rejects anything else).
Projects resolve the same way into `prj_<24hex>`.

## Development

```bash
gofmt -l . && go vet ./... && go test ./...
go build -o muse-proxy .
```

Cross-compile: `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build
-trimpath -ldflags="-s -w -X main.version=vX.Y.Z" -o muse-proxy .`

## Acknowledgements

Protocol research against [anomalyco/opencode](https://github.com/anomalyco/opencode)
and [jasonxu114514/opencode2api](https://github.com/jasonxu114514/opencode2api).
See [docs/protocol-table.md](./docs/protocol-table.md) for the full
model→protocol mapping and the upstream error taxonomy.
