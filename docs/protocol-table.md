# Zen 模型-协议判定表

来源: `GET https://models.opencode.ai/api.json`（客户端开机拉取，本地缓存 `models.json`）
规则: `model.provider.npm ?? provider.npm` → SDK → 协议
生成时间: 2026-09-26（111 zen + 41 go）

## 判定算法（opencode 客户端实际逻辑）

```
1. 取目录: GET https://models.opencode.ai/api.json
   - 可被环境变量 OPENCODE_MODELS_URL 覆盖源
   - 可被 OPENCODE_MODELS_PATH 指定本地文件
   - 可被 OPENCODE_DISABLE_MODELS_FETCH 禁用
   - 缓存到本地 Global.Path.cache/models.json（多 CLI 进程用 flock 互斥写）
2. npm = model.provider.npm ?? provider.npm
   - opencode (Zen): 默认 @ai-sdk/openai-compatible, api=https://opencode.ai/zen/v1
   - opencode-go:    默认 @ai-sdk/openai-compatible, api=https://opencode.ai/zen/go/v1
3. npm → SDK → 线协议:
   - @ai-sdk/openai            → Responses API  (POST /v1/responses)
   - @ai-sdk/openai-compatible → Chat Completions (POST /v1/chat/completions)
   - @ai-sdk/anthropic         → Messages       (POST /v1/messages)
   - @ai-sdk/google            → GenerateContent (POST /v1/models/<id>:generateContent)
4. 服务端独立校验: ZEN_MODELS 注册表的 formatFilter
   （chat→"oa-compat", responses→"openai", messages→"anthropic"），
   不匹配直接 401 ModelError。目录 npm 与服务端注册表通常一致。
```

## provider `opencode`（111 models）

| model | npm override | 协议 | endpoint |
|---|---|---|---|
| ling-3.0-flash-fin-free | — | chat | /v1/chat/completions |
| gpt-5.4 | @ai-sdk/openai | responses | /v1/responses |
| qwen3.6-plus-free | @ai-sdk/anthropic | messages | /v1/messages |
| claude-haiku-4-5 | @ai-sdk/anthropic | messages | /v1/messages |
| gpt-5.4-pro | @ai-sdk/openai | responses | /v1/responses |
| mimo-v2-pro-free | — | chat | /v1/chat/completions |
| muse-spark-1.2-contributor-free | @ai-sdk/openai | responses | /v1/responses |
| muse-spark-1.3 | @ai-sdk/openai | responses | /v1/responses |
| glm-5-free | — | chat | /v1/chat/completions |
| trinity-large-preview-free | — | chat | /v1/chat/completions |
| gpt-5.5-pro | @ai-sdk/openai | responses | /v1/responses |
| grok-4.7 | @ai-sdk/openai | responses | /v1/responses |
| gpt-5.4-nano | @ai-sdk/openai | responses | /v1/responses |
| gpt-5.2-codex | @ai-sdk/openai | responses | /v1/responses |
| ling-3.0-tiny-free | — | chat | /v1/chat/completions |
| gpt-5.1-codex | @ai-sdk/openai | responses | /v1/responses |
| glm-5.3-flash | — | chat | /v1/chat/completions |
| kimi-k2.5-free | — | chat | /v1/chat/completions |
| glm-4.7-free | — | chat | /v1/chat/completions |
| glm-4.6 | — | chat | /v1/chat/completions |
| laguna-s-2.1-free | — | chat | /v1/chat/completions |
| qwen3.8-max | — | chat | /v1/chat/completions |
| kimi-k3 | — | chat | /v1/chat/completions |
| gpt-5-codex | @ai-sdk/openai | responses | /v1/responses |
| qwen3-coder | — | chat | /v1/chat/completions |
| minimax-m3-free | @ai-sdk/anthropic | messages | /v1/messages |
| qwen3.5-plus | @ai-sdk/anthropic | messages | /v1/messages |
| claude-opus-4-5 | @ai-sdk/anthropic | messages | /v1/messages |
| glm-5 | — | chat | /v1/chat/completions |
| deepseek-v4.1-flash | — | chat | /v1/chat/completions |
| gpt-5.3-codex | @ai-sdk/openai | responses | /v1/responses |
| minimax-m2.5 | — | chat | /v1/chat/completions |
| gpt-5-nano | @ai-sdk/openai | responses | /v1/responses |
| deepseek-v4-flash-vision-exp | — | chat | /v1/chat/completions |
| kimi-k2.6 | — | chat | /v1/chat/completions |
| claude-sonnet-4-5 | @ai-sdk/anthropic | messages | /v1/messages |
| claude-opus-5-5 | @ai-sdk/anthropic | messages | /v1/messages |
| claude-fable-5-1 | @ai-sdk/anthropic | messages | /v1/messages |
| gemini-3.6-flash | @ai-sdk/google | google | /v1/models/<id>:generateContent |
| kimi-k2-thinking | — | chat | /v1/chat/completions |
| minimax-m2.5-free | @ai-sdk/anthropic | messages | /v1/messages |
| gpt-5.3-codex-spark | @ai-sdk/openai | responses | /v1/responses |
| gemini-3.5-flash-lite | @ai-sdk/google | google | /v1/models/<id>:generateContent |
| claude-opus-4-1 | @ai-sdk/anthropic | messages | /v1/messages |
| gpt-6-astra | @ai-sdk/openai | responses | /v1/responses |
| grok-4.5 | @ai-sdk/openai | responses | /v1/responses |
| ring-2.6-1t-free | — | chat | /v1/chat/completions |
| kimi-k2.5 | — | chat | /v1/chat/completions |
| longcat-2.0-free | — | chat | /v1/chat/completions |
| gpt-5.1 | @ai-sdk/openai | responses | /v1/responses |
| claude-opus-5 | @ai-sdk/anthropic | messages | /v1/messages |
| claude-3-5-haiku | @ai-sdk/anthropic | messages | /v1/messages |
| hy3-preview-free | — | chat | /v1/chat/completions |
| gemini-3-flash | @ai-sdk/google | google | /v1/models/<id>:generateContent |
| minimax-m2.7 | — | chat | /v1/chat/completions |
| gemini-3.5-flash | @ai-sdk/google | google | /v1/models/<id>:generateContent |
| space-bunny-free | — | chat | /v1/chat/completions |
| claude-fable-5 | @ai-sdk/anthropic | messages | /v1/messages |
| nemotron-3-super-free | — | chat | /v1/chat/completions |
| minimax-m2.1-free | @ai-sdk/anthropic | messages | /v1/messages |
| mimo-v2.6-flash-free | — | chat | /v1/chat/completions |
| gemini-3-pro | @ai-sdk/google | google | /v1/models/<id>:generateContent |
| nemotron-3-ultra-free | — | chat | /v1/chat/completions |
| claude-sonnet-4 | @ai-sdk/anthropic | messages | /v1/messages |
| muse-spark-1.2 | @ai-sdk/openai | responses | /v1/responses |
| gpt-5.4-mini | @ai-sdk/openai | responses | /v1/responses |
| glm-4.7 | — | chat | /v1/chat/completions |
| minimax-m3 | — | chat | /v1/chat/completions |
| gpt-5.6-luna | @ai-sdk/openai | responses | /v1/responses |
| qwen3.8-flash | @ai-sdk/anthropic | messages | /v1/messages |
| gpt-5.1-codex-max | @ai-sdk/openai | responses | /v1/responses |
| mimo-v2.5-free | — | chat | /v1/chat/completions |
| grok-code | — | chat | /v1/chat/completions |
| gpt-5.2 | @ai-sdk/openai | responses | /v1/responses |
| claude-opus-4-8 | @ai-sdk/anthropic | messages | /v1/messages |
| gpt-5.5 | @ai-sdk/openai | responses | /v1/responses |
| ling-2.6-flash-free | — | chat | /v1/chat/completions |
| claude-sonnet-5 | @ai-sdk/anthropic | messages | /v1/messages |
| deepseek-v4-flash-free | — | chat | /v1/chat/completions |
| hy3-free | — | chat | /v1/chat/completions |
| nemotron-3.5-lightning-free | — | chat | /v1/chat/completions |
| claude-opus-4-6 | @ai-sdk/anthropic | messages | /v1/messages |
| gemini-3.7-flash | @ai-sdk/google | google | /v1/models/<id>:generateContent |
| glm-5.2 | — | chat | /v1/chat/completions |
| kimi-k2 | — | chat | /v1/chat/completions |
| glm-5.1 | — | chat | /v1/chat/completions |
| mimo-v2-omni-free | — | chat | /v1/chat/completions |
| grok-build-0.1 | @ai-sdk/openai | responses | /v1/responses |
| ling-3.0-flash-free | — | chat | /v1/chat/completions |
| gemini-3.8-flash | @ai-sdk/google | google | /v1/models/<id>:generateContent |
| north-mini-code-free | — | chat | /v1/chat/completions |
| gpt-6-luna | @ai-sdk/openai | responses | /v1/responses |
| deepseek-v4-pro | — | chat | /v1/chat/completions |
| claude-sonnet-4-6 | @ai-sdk/anthropic | messages | /v1/messages |
| big-pickle | — | chat | /v1/chat/completions |
| qwen3.6-plus | @ai-sdk/anthropic | messages | /v1/messages |
| gpt-5.6-terra | @ai-sdk/openai | responses | /v1/responses |
| mimo-v2-flash-free | — | chat | /v1/chat/completions |
| gpt-5.1-codex-mini | @ai-sdk/openai | responses | /v1/responses |
| glm-5.3 | — | chat | /v1/chat/completions |
| claude-opus-4-7 | @ai-sdk/anthropic | messages | /v1/messages |
| kimi-k2.7-code | — | chat | /v1/chat/completions |
| gpt-5 | @ai-sdk/openai | responses | /v1/responses |
| grok-4.6 | @ai-sdk/openai | responses | /v1/responses |
| gemini-3.1-pro | @ai-sdk/google | google | /v1/models/<id>:generateContent |
| minimax-m2.1 | — | chat | /v1/chat/completions |
| gpt-5.6-sol | @ai-sdk/openai | responses | /v1/responses |
| x-preview-f-free | — | chat | /v1/chat/completions |
| muse-spark-1.3-contributor-free | @ai-sdk/openai | responses | /v1/responses |
| deepseek-v4-flash | — | chat | /v1/chat/completions |
| gpt-6-sol | @ai-sdk/openai | responses | /v1/responses |

## 实测对照（2026-09-26）

| 模型 | chat+tools | responses+tools | messages+tools | 判定 |
|---|---|---|---|---|
| mimo-v2.6-flash-free | 200 | 400 | 400 | chat ✅ 表对 |
| mimo-v2.5-free | 200 | 400 | 400 | chat ✅ 表对 |
| big-pickle | 200 | 400 | 400 | chat ✅ 表对 |
| nemotron-3-ultra-free | 200 | 400 | 400 | chat ✅ 表对 |
| muse-spark-1.3-contributor-free | 503 | 200 | 500 | responses ✅ 表对 |
| minimax-m2.5-free | 400 Model不可用 | 400 Model不可用 | 400 Model不可用 | messages（表对，但后端无可用节点） |

## 上游错误分类（写兼容代码用）

| HTTP | body | 含义 | 对策 |
|---|---|---|---|
| 403 | FreeTierError | 免费层形状门：无 tools / 非流式 / 非法 session 形状 | 补 agent 形状（stream+tools+规范 session）后重试 |
| 401 | ModelError | 注册表无此 format 条目（真·不支持） | 换表内协议重试；都不行则放弃 |
| 401 | AuthError | key 缺失/无效 | 换 key |
| 402 | Insufficient funds | 计费通过，余额不足 | 充钱或换免费模型 |
| 503 | Endpoint is unavailable | 推理路由无健康后端（波动） | 换出口 IP / 等待重试 |
| 400 | Model is unavailable | 模型注册了但当前无可用 provider | 换模型或等待 |
| 400 | 空 body（+ x-opencode-log-id） | 边缘层拒绝（疑似并发限流） | 降并发 / 换 key / 换出口 IP |
| 429 | FreeUsageLimitError + retry-after | 明确限流 | 按 Retry-After 退避 |
