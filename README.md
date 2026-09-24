# ex-plugin

CLIProxyAPI 插件。把 ChatGPT 的 Excel 插件后端 `https://bps.openai.com/basispoints/api/responses` 暴露成 Responses 模型，并保留客户端自己的工具调用。

Basispoints 不接受请求里的 `tools`，带上会直接 `422`。插件会拿掉客户端工具，把目录写进 developer 消息，再让模型通过它自己声明的 `run_officejs` 把真正的工具名和参数放进 `code`。代理截下这个调用，还原成标准 `function_call` 交给客户端执行，下一跳再把结果按原来的 `run_officejs` item 回放。同一次用户回合里 `turn_id` 保持不变，只增加 `agent_iteration`，避免后端把已完成的 plan 当成新 turn 重来。

`reasoning.effort` 没有 `max`。请求里的 `max` / `ultra` 会改成 `xhigh`。

## 模型

公开模型名带 `-excel`，避免和 CPA 里已有的 Codex 模型抢路由。上游实际收到的是去掉后缀的名字。

| 客户端模型 | 上游模型 | Excel 后端 |
| --- | --- | --- |
| `gpt-6-astra-excel` | `gpt-6-astra` | 可用 |
| `gpt-5.6-sol-excel` | `gpt-5.6-sol` | 可用 |
| `gpt-5.6-luna-excel` | `gpt-5.6-luna` | 可用 |
| `gpt-5.6-terra-excel` | `gpt-5.6-terra` | 可用 |

`gpt-6-sol`、`gpt-6-luna`、`gpt-6-terra`、`gpt-5.5` 在这个后端返回 403，插件不注册它们。需要直接暴露不带后缀的上游 id 时，把 `expose_upstream_ids` 设为 `true`。只有这台 CPA 不再用同名 Codex 模型时才应该打开。

## 构建

需要 Go 1.26+ 和 C 编译器。

```bash
go test ./...
make build
```

产物是 `dist/ex-plugin.dylib`（macOS）、`dist/ex-plugin.so`（Linux）或 `dist/ex-plugin.dll`（Windows）。文件名必须是 `ex-plugin` 加平台后缀，放到 CPA 的 `plugins` 目录。

## 配置

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    ex-plugin:
      enabled: true
      priority: 100
      responses_url: https://bps.openai.com/basispoints/api/responses
      auth_mode: chatgpt
      forward_prompt_cache_key: true
      catalog_at_prompt_end: false
      expose_upstream_ids: false
```

`tools_version_id` 可选。有 Basispoints 工具版本号时会放进 metadata 的 `bps_tools_version_id`。

## 凭证

在 CPA 的 auth 目录放一个 JSON，`type` 使用 `excel`：

```json
{
  "type": "excel",
  "access_token": "CHATGPT_ACCESS_TOKEN",
  "refresh_token": "OPTIONAL",
  "auth_mode": "chatgpt"
}
```

`account_id` 可以省略。插件会从 access token 里 `https://api.openai.com/auth` 的 `chatgpt_account_id` 读取，请求里同时带：

- `authorization: Bearer <access_token>`
- `chatgpt-account-id`
- `x-openai-account-id`
- `x-basispoints-auth-mode: chatgpt`

有 `refresh_token` 时，插件按 Codex 的 OAuth client 向 `https://auth.openai.com/oauth/token` 刷新。没有 refresh token 时，access token 过期后需要重新写入。

`type: codex` 的现有凭证不会被这个插件接管。

## 行为

- 输入和输出格式是 `openai-response` 与 `codex`。客户端走 `/v1/responses`。CPA 能把其他协议翻译成这两种格式时，也会进这个执行器。
- 客户端 `tools` 不会转发给 Basispoints。
- 只转换响应里的一个工具调用。模型能力里的并行工具调用是关闭的。
- `run_officejs` 的 `code` 是嵌套 JSON 字符串。`name` / `arguments` 和捕获里见过的 `tool` / `args` 都能还原。
- 回放时优先使用上游原始 item，包括它自己的 `id`、`summary` 和 `references`。插件进程重启后，缓存丢失，会按同样的 call id 重建一个兼容信封。
- 带 `encrypted_content` 的 reasoning 会原样回放；没有加密内容的 reasoning 会丢掉，因为 `store: false` 时上游会拒绝它。

## 测试

```bash
go test ./...
```
