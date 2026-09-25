把 ChatGPT Excel 的 Basispoints responses 接口接到 CLIProxyAPI。

客户端可以直接请求 `gpt-6-astra`、`gpt-5.6-sol`、`gpt-5.6-luna`、`gpt-5.6-terra`。凭证使用单独的 `type: excel` auth 文件。

用户消息里的 data URL 图片会先上传到 Basispoints attachments，再以 file_id 发送。上传文件名只使用 .jpg、.png、.gif、.webp。
