# ex-plugin

CLIProxyAPI plugin for the ChatGPT Excel Basispoints endpoint
`https://bps.openai.com/basispoints/api/responses`.

Add this store source in CPA:

```text
https://raw.githubusercontent.com/spacex-3/ex-plugin/main/registry.json
```

The plugin id is `ex-plugin`. It registers `gpt-6-astra`, `gpt-5.6-sol`,
`gpt-5.6-luna`, and `gpt-5.6-terra`. Those names are forwarded unchanged.
The `-excel` aliases are also accepted and stripped before the upstream call.
`max` and `ultra` reasoning efforts are sent as `xhigh`.

Existing `type: codex` auth files are not used. Add a separate auth file with
`"type": "excel"` and the same ChatGPT access token. See [README_CN.md](README_CN.md).

```bash
go test ./...
make package
```
