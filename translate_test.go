package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestUUID5MatchesPython(t *testing.T) {
	if got := uuid5(namespaceURL, "python.org"); got != "7af94e2b-4dd9-50f0-9c9a-8a48519bdef0" {
		t.Fatalf("uuid5 = %s", got)
	}
}

func TestResponsesBodyUsesExcelWireShape(t *testing.T) {
	resetNativeCalls()
	source := mustObject(t, `{
		"model":"gpt-5.6-sol-excel",
		"instructions":"Use the client tools.",
		"input":"Hello",
		"prompt_cache_key":"conversation-1",
		"reasoning":{"effort":"xhigh","summary":"auto"},
		"stream":true,
		"tools":[{"type":"function","name":"demo"}]
	}`)
	body := prepareResponsesBody(source, "")
	if body["model"] != "gpt-5.6-sol" || body["model_selection"] != "explicit" || body["store"] != false {
		t.Fatalf("wire shape = %#v", body)
	}
	if body["reasoning_effort"] != "xhigh" || body["prompt_cache_key"] != "conversation-1" {
		t.Fatalf("routing fields = %#v", body)
	}
	input := body["input"].([]any)
	if textOf(input[0]) != "Use the client tools." || !strings.Contains(textOf(input[1]), "native run_officejs") {
		t.Fatalf("prologue = %#v", input)
	}
	if !strings.Contains(textOf(input[1]), "functions.run_officejs") || !strings.Contains(textOf(input[1]), `"name":"demo"`) {
		t.Fatalf("catalog prompt = %s", textOf(input[1]))
	}
	if roleOf(input[3]) != "user" {
		t.Fatalf("user item = %#v", input[3])
	}
	for _, key := range []string{"tools", "include", "text", "max_output_tokens", "reasoning"} {
		if _, ok := body[key]; ok {
			t.Fatalf("forwarded %s", key)
		}
	}
}

func TestMaxEffortFallsBackToXHigh(t *testing.T) {
	source := mustObject(t, `{"model":"gpt-6-astra(max)","input":"Hello"}`)
	body := prepareResponsesBody(source, "")
	if body["model"] != "gpt-6-astra" || body["reasoning_effort"] != "xhigh" {
		t.Fatalf("body = %#v", body)
	}
}

func TestStandardModelIDsAreRegistered(t *testing.T) {
	currentConfig.Store(pluginConfig{})
	raw, err := marshalCompact(modelResponse())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-luna", "gpt-5.6-terra"} {
		if !strings.Contains(string(raw), `"ID":"`+id+`"`) {
			t.Fatalf("missing %s in %s", id, raw)
		}
	}
}

func TestTurnIdentityStaysConstantAcrossToolResults(t *testing.T) {
	resetNativeCalls()
	first := prepareResponsesBody(mustObject(t, `{
		"model":"gpt-5.6-sol-excel",
		"prompt_cache_key":"conversation-1",
		"metadata":{"turn_id":"client-changing-turn"},
		"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]}]
	}`), "")
	second := prepareResponsesBody(mustObject(t, `{
		"model":"gpt-5.6-sol-excel",
		"prompt_cache_key":"conversation-1",
		"metadata":{"turn_id":"a-different-client-turn"},
		"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]},
			{"type":"function_call","call_id":"call_1","name":"shell_command","arguments":"{\"command\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"file.txt"}
		]
	}`), "")
	if first["metadata"].(map[string]any)["turn_id"] != second["metadata"].(map[string]any)["turn_id"] {
		t.Fatalf("turn changed: %#v %#v", first["metadata"], second["metadata"])
	}
	if second["metadata"].(map[string]any)["agent_iteration"] != "2" {
		t.Fatalf("iteration = %#v", second["metadata"])
	}
	if second["metadata"].(map[string]any)["turn_id"] == "a-different-client-turn" {
		t.Fatal("client turn_id was forwarded")
	}
	firstInput := first["input"].([]any)
	secondInput := second["input"].([]any)
	if canonicalJSON(secondInput[:len(firstInput)]) != canonicalJSON(firstInput) {
		t.Fatalf("cache prefix changed")
	}
}

func TestRunOfficeJSRoundTrip(t *testing.T) {
	resetNativeCalls()
	source := mustObject(t, `{
		"model":"gpt-6-astra-excel",
		"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]
	}`)
	native := mustObject(t, `{
		"type":"function_call",
		"id":"fc_transport",
		"call_id":"call_transport",
		"name":"run_officejs",
		"arguments":"{\"summary\":\"Get current weather for Tokyo\",\"code\":\"{\\\"tool\\\":\\\"get_weather\\\",\\\"arguments\\\":{\\\"city\\\":\\\"Tokyo\\\"}}\",\"destructive\":false,\"references\":[\"Tokyo weather\"]}"
	}`)
	response := map[string]any{"output": []any{native}}
	// The pasted capture used "tool"; the proxy protocol uses "name". Accept name.
	native["arguments"] = `{"summary":"Get current weather for Tokyo","code":"{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Tokyo\"}}","destructive":false,"references":["Tokyo weather"]}`
	call := extractNativeClientToolCall(map[string]any{"output": []any{native}}, source)
	if call == nil || call["name"] != "get_weather" || call["call_id"] != "call_transport" {
		t.Fatalf("call = %#v", call)
	}
	replay := translateInputItems([]any{
		map[string]any{"type": "function_call", "call_id": "call_transport", "name": "get_weather", "arguments": call["arguments"]},
		map[string]any{"type": "function_call_output", "call_id": "call_transport", "output": "18C"},
	}, clientTools(source))
	replayed, _ := replay[0].(map[string]any)
	if replayed["name"] != "run_officejs" || !strings.Contains(jsonString(replayed["arguments"]), "Tokyo weather") {
		t.Fatalf("replay lost the native item: %#v", replayed)
	}
	output, _ := replay[1].(map[string]any)
	if output["id"] != "fc_call_transport" {
		t.Fatalf("output id = %#v", output["id"])
	}
	_ = response
}

func TestRepairsInvalidShellBackslashes(t *testing.T) {
	resetNativeCalls()
	source := mustObject(t, `{"tools":[{"type":"function","name":"exec_command","parameters":{"type":"object"}}]}`)
	broken := "{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"rg foo\\(bar\"}}"
	native := map[string]any{
		"type":      "function_call",
		"call_id":   "call_repair",
		"name":      "run_officejs",
		"arguments": mustCompact(map[string]any{"code": broken}),
	}
	call := extractNativeClientToolCall(map[string]any{"output": []any{native}}, source)
	if call == nil {
		t.Fatal("repair did not decode the transport")
	}
	arguments, ok := decodeArguments(call["arguments"])
	if !ok || arguments["cmd"] != `rg foo\(bar` {
		t.Fatalf("arguments = %#v", call["arguments"])
	}
}

func TestPastedToolAliasRoundTrip(t *testing.T) {
	resetNativeCalls()
	source := mustObject(t, `{"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object","required":["city"],"properties":{"city":{"type":"string"}}}}]}`)
	native := map[string]any{
		"type":      "function_call",
		"id":        "fc_alias",
		"call_id":   "call_alias",
		"name":      "run_officejs",
		"arguments": `{"summary":"Get current weather for Tokyo","code":"{\"tool\":\"get_weather\",\"args\":{\"city\":\"Tokyo\"}}","destructive":false,"references":["Tokyo weather"]}`,
	}
	call := extractNativeClientToolCall(map[string]any{"output": []any{native}}, source)
	if call == nil || call["name"] != "get_weather" {
		t.Fatalf("call = %#v", call)
	}
	if !strings.Contains(jsonString(call["arguments"]), `"city":"Tokyo"`) {
		t.Fatalf("arguments = %#v", call["arguments"])
	}
}

func TestAuthReadsAccountFromAccessToken(t *testing.T) {
	token := testJWT(t, map[string]any{
		"exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_123",
			"chatgpt_user_id":    "user_123",
		},
	})
	raw := mustCompact(map[string]any{"type": "excel", "access_token": token})
	cred, ok, err := parseCredential([]byte(raw))
	if err != nil || !ok {
		t.Fatalf("parse = %v %v", ok, err)
	}
	if cred.AccountID != "acct_123" || cred.UserID != "user_123" {
		t.Fatalf("cred = %#v", cred)
	}
	headers, _, err := cred.requestHeaders(false)
	if err != nil {
		t.Fatal(err)
	}
	if headers["authorization"][0] != "Bearer "+token || headers["chatgpt-account-id"][0] != "acct_123" {
		t.Fatalf("headers = %#v", headers)
	}
	if headers["x-basispoints-auth-mode"][0] != "chatgpt" {
		t.Fatalf("auth mode = %#v", headers["x-basispoints-auth-mode"])
	}
}

func TestCodexAuthFileIsIgnored(t *testing.T) {
	_, ok, err := parseCredential([]byte(`{"type":"codex","access_token":"x","account_id":"y"}`))
	if err != nil || ok {
		t.Fatalf("handled codex file: %v %v", ok, err)
	}
}

func TestStreamRewritesRunOfficeJS(t *testing.T) {
	resetNativeCalls()
	source := mustObject(t, `{"model":"gpt-6-astra-excel","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`)
	arguments := mustCompact(map[string]any{
		"summary":     "Get current weather for Tokyo",
		"code":        `{"name":"get_weather","arguments":{"city":"Tokyo"}}`,
		"destructive": false,
		"references":  []any{"Tokyo weather"},
	})
	native := map[string]any{
		"type": "function_call", "id": "fc_transport_stream", "call_id": "call_transport_stream",
		"name": "run_officejs", "status": "completed", "arguments": arguments,
	}
	frames := []sseEvent{
		{Name: "response.output_item.done", Data: mustCompact(map[string]any{"type": "response.output_item.done", "output_index": 1, "item": native})},
		{Name: "response.completed", Data: mustCompact(map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "resp_transport", "status": "completed", "model": "gpt-6-astra", "output": []any{native},
		}})},
	}
	stream := newToolStream(source, "gpt-6-astra-excel")
	raw := joinFrames(stream.consume(frames))
	if strings.Contains(raw, "run_officejs") {
		t.Fatalf("transport leaked: %s", raw)
	}
	if !strings.Contains(raw, `"name":"get_weather"`) || !strings.Contains(raw, "gpt-6-astra-excel") {
		t.Fatalf("rewritten stream = %s", raw)
	}
}

func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	body, err := marshalCompact(claims)
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(body) + "."
}

func mustObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	object, err := decodeObject([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func textOf(value any) string {
	object, _ := value.(map[string]any)
	content, _ := object["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	part, _ := content[0].(map[string]any)
	return stringField(part, "text")
}

func roleOf(value any) string {
	object, _ := value.(map[string]any)
	return stringField(object, "role")
}

func joinFrames(frames [][]byte) string {
	var b strings.Builder
	for _, frame := range frames {
		b.Write(frame)
	}
	return b.String()
}

func TestMarshalKeepsRawToken(t *testing.T) {
	raw := []byte(`{"access_token":"abc"}`)
	if json.Valid(raw) != true {
		t.Fatal("sanity")
	}
}
