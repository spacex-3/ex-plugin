# ex-plugin

CLIProxyAPI 插件。用 ChatGPT access token 请求 Excel 插件后端 `https://bps.openai.com/basispoints/api/responses`，并保留客户端自己的工具调用。

在 CPA 插件商店添加：

```text
https://raw.githubusercontent.com/spacex-3/ex-plugin/main/registry.json
```

安装走 GitHub Release。归档名是 `ex-plugin_<version>_<goos>_<goarch>.zip`，里面的动态库文件名是 `ex-plugin.so`、`ex-plugin.dylib` 或 `ex-plugin.dll`。

Basispoints 不接受请求里的 `tools`，带上会直接 422。插件会拿掉客户端工具，把目录写进 developer 消息，再让模型通过它声明的 `run_officejs` 把真正的工具名和参数放进 `code`。代理截下这个调用，还原成标准 `function_call` 交给客户端执行，下一跳再把结果按原来的 `run_officejs` item 回放。同一次用户回合里 `turn_id` 保持不变，只增加 `agent_iteration`。

`reasoning.effort` 没有 `max`。`max` 和 `ultra` 会改成 `xhigh`。

用户消息里的内嵌 `data:` 图片会先上传到与 responses 同目录的 `attachments`，请求体里只保留返回的 `file_id`。Basispoints 不接受把图片字节直接放进 responses。

## 模型

插件直接注册这些标准模型名，上游请求也使用同一个名字：

- `gpt-6-astra`
- `gpt-5.6-sol`
- `gpt-5.6-luna`
- `gpt-5.6-terra`

`gpt-6-astra-excel` 这类旧别名仍然接受，发出去之前会去掉 `-excel`。`gpt-6-sol`、`gpt-6-luna`、`gpt-6-terra`、`gpt-5.5` 在这个后端返回 403，不注册。和 Codex 渠道的同名模型由你自己的优先级决定走哪边。

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
```

`tools_version_id` 可选，会放进 metadata 的 `bps_tools_version_id`。

## 凭证

现有 `type: codex` 的 auth 文件不会被这个插件读取。用同一套 ChatGPT token 再放一份 `type: excel` 的文件：

```json
{
  "type": "excel",
  "access_token": "CHATGPT_ACCESS_TOKEN",
  "refresh_token": "OPTIONAL",
  "auth_mode": "chatgpt"
}
```

`account_id` 可以省略。插件从 access token 里 `https://api.openai.com/auth` 的 `chatgpt_account_id` 读取。请求头包括 `authorization`、`chatgpt-account-id`、`x-openai-account-id` 和 `x-basispoints-auth-mode: chatgpt`。

有 `refresh_token` 时按 Codex OAuth client 向 `https://auth.openai.com/oauth/token` 刷新。

## 本地构建

```bash
go test ./...
make package
```
