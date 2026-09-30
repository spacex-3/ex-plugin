package main

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

var namespaceURL = mustUUID("6ba7b811-9dad-11d1-80b4-00c04fd430c8")

func prepareResponsesBody(source map[string]any, toolsVersion string) map[string]any {
	cfg := loadedConfig()
	model := firstModel(source)
	output := map[string]any{
		"model":           upstreamModelID(model),
		"model_selection": "explicit",
		"stream":          truthy(source["stream"]),
		"store":           false,
	}
	rawInput := source["input"]
	tools := clientTools(source)
	inputItems := translateInputItems(rawInput, tools)
	historyRoot := conversationFingerprint(inputItems)

	var prologue []any
	if instructions := strings.TrimSpace(stringField(source, "instructions")); instructions != "" {
		prologue = append(prologue, messageItem("developer", instructions))
	}
	catalogText := externalClientInstructions
	if len(tools) > 0 {
		catalogText = protocolInstructions(catalogJSON(tools))
	}
	catalog := messageItem("developer", catalogText)
	if cfg.catalogAtPromptEnd() {
		inputItems = appendBeforeCompaction(append(prologue, inputItems...), []any{catalog})
	} else {
		prologue = append(prologue, catalog)
		if reminder := protocolReminder(tools); reminder != "" {
			prologue = append(prologue, messageItem("developer", reminder))
		}
		inputItems = append(prologue, inputItems...)
	}
	output["input"] = inputItems

	if cacheKey := cacheKey(source); cacheKey != "" && cfg.forwardPromptCacheKey() {
		output["prompt_cache_key"] = cacheKey
	}
	effort := ""
	if reasoning, ok := source["reasoning"].(map[string]any); ok {
		effort = normalizeEffort(reasoning["effort"])
	}
	if effort == "" {
		effort = normalizeEffort(source["reasoning_effort"])
	}
	if effort == "" {
		effort = effortFromModelSuffix(model)
	}
	if effort == "" {
		effort = "medium"
	}
	output["reasoning_effort"] = effort
	if context, ok := source["context_management"].([]any); ok {
		output["context_management"] = context
	} else {
		output["context_management"] = []any{map[string]any{"type": "compaction", "compact_threshold": 200000}}
	}

	metadata := map[string]any{}
	if rawMetadata, ok := source["metadata"].(map[string]any); ok {
		for key, value := range rawMetadata {
			if !metadataValueOK(value) {
				continue
			}
			metadata[truncate(key, 64)] = truncate(jsonString(value), 512)
		}
	}
	fingerprint, iteration := agentTurnState(rawInput)
	conversation := cacheKey(source)
	if conversation == "" {
		conversation = historyRoot
	}
	metadata["agent_iteration"] = iteration
	metadata["task_id"] = uuid5(namespaceURL, "ghcp-proxy/gpt-excel/"+conversation)
	metadata["turn_id"] = uuid5(namespaceURL, "ghcp-proxy/gpt-excel/"+conversation+"/turn/"+fingerprint)
	if toolsVersion != "" && validToolsVersion(toolsVersion) {
		metadata["bps_tools_version_id"] = toolsVersion
	}
	output["metadata"] = metadata
	return output
}

func firstModel(source map[string]any) string {
	if model := strings.TrimSpace(stringField(source, "model")); model != "" {
		return model
	}
	return defaultPublicModel
}

func metadataValueOK(value any) bool {
	switch value.(type) {
	case string, bool, jsonNumberLike, int, int64, float64:
		return true
	default:
		return false
	}
}

type jsonNumberLike = interface{ String() string }

func translateInputItems(raw any, tools map[string]toolSpec) []any {
	switch typed := raw.(type) {
	case string:
		return []any{messageItem("user", typed)}
	case []any:
		return translateItemList(typed, tools)
	default:
		return []any{}
	}
}

func translateItemList(raw []any, tools map[string]toolSpec) []any {
	origins := map[string]string{}
	result := make([]any, 0, len(raw))
	for _, item := range raw {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		object = stripPassthrough(object)
		itemType := strings.ToLower(strings.TrimSpace(stringField(object, "type")))
		switch itemType {
		case "function_call", "custom_tool_call":
			result = append(result, translateCallItem(object, origins))
		case "function_call_output", "custom_tool_call_output":
			result = append(result, normalizeToolOutput(object, origins))
		case "reasoning":
			if encrypted, ok := object["encrypted_content"].(string); ok && encrypted != "" {
				result = append(result, map[string]any{
					"type":              "reasoning",
					"summary":           []any{},
					"encrypted_content": encrypted,
				})
			}
		case "item_reference":
			continue
		default:
			result = append(result, object)
		}
	}
	return result
}

func translateCallItem(item map[string]any, origins map[string]string) any {
	name := stringField(item, "name")
	callID := stringField(item, "call_id")
	if remembered, ok := rememberedNativeCall(callID); ok {
		if nativeName := stringField(remembered, "name"); nativeName != "" {
			origins[callID] = nativeName
		}
		return remembered
	}
	if name != "" && strings.HasPrefix(callID, markerCallPrefix) {
		upstream := relayPrefix + name
		origins[callID] = upstream
		cloned := cloneObject(item)
		cloned["name"] = upstream
		return cloned
	}
	if name == "" {
		return item
	}
	if name == "update_plan" {
		origins[callID] = name
		cloned := cloneObject(item)
		cloned["arguments"] = restorePlanArguments(name, item["arguments"])
		return cloned
	}
	origins[callID] = transportName
	return fallbackTransportCall(item)
}

func normalizeToolOutput(item map[string]any, origins map[string]string) map[string]any {
	normalized := cloneObject(item)
	callID := stringField(normalized, "call_id")
	origin := origins[callID]
	if origin == "update_plan" {
		normalized["output"] = `{"status":"ok"}`
	}
	if origin == transportName && callID != "" && stringField(normalized, "type") == "custom_tool_call_output" {
		normalized["type"] = "function_call_output"
	}
	if callID != "" && stringField(normalized, "type") == "function_call_output" {
		canonical := functionItemID(callID)
		if stringField(normalized, "id") != canonical {
			normalized["id"] = canonical
		}
	}
	text := outputText(normalized["output"])
	if origin == transportName && strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), "unsupported call: run_officejs") {
		normalized["output"] = transportRetryGuidance
		return normalized
	}
	if strings.TrimSpace(text) == "" {
		switch normalized["output"].(type) {
		case nil, string:
			normalized["output"] = "(tool call succeeded with no output)"
		}
	}
	return normalized
}

func cloneObject(item map[string]any) map[string]any {
	cloned := map[string]any{}
	for key, value := range item {
		cloned[key] = value
	}
	return cloned
}

func messageItem(role, text string) map[string]any {
	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}
	return map[string]any{
		"type": roleType(role),
		"role": role,
		"content": []any{
			map[string]any{"type": contentType, "text": text},
		},
	}
}

func roleType(role string) string {
	if role == "assistant" || role == "user" || role == "developer" || role == "system" {
		return "message"
	}
	return "message"
}

func appendBeforeCompaction(items, injected []any) []any {
	if len(items) > 0 {
		if last, ok := items[len(items)-1].(map[string]any); ok && stringField(last, "type") == "compaction_trigger" {
			out := append([]any{}, items[:len(items)-1]...)
			out = append(out, injected...)
			return append(out, items[len(items)-1])
		}
	}
	return append(append([]any{}, items...), injected...)
}

func conversationFingerprint(items []any) string {
	for _, item := range items {
		if _, ok := item.(map[string]any); ok {
			return sha256Hex(canonicalJSON(item))
		}
	}
	return "anonymous"
}

func cacheKey(source map[string]any) string {
	for _, key := range []string{"prompt_cache_key", "promptCacheKey", "session_id", "sessionId"} {
		if value := strings.TrimSpace(stringField(source, key)); value != "" {
			return value
		}
	}
	if metadata, ok := source["client_metadata"].(map[string]any); ok {
		for _, key := range []string{"session_id", "sessionId"} {
			if value := strings.TrimSpace(stringField(metadata, key)); value != "" {
				return value
			}
		}
	}
	return ""
}

func agentTurnState(raw any) (string, string) {
	switch typed := raw.(type) {
	case string:
		return sha256Hex(canonicalJSON(typed)), "1"
	case []any:
		lastUser := -1
		for index, item := range typed {
			object, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if strings.EqualFold(stringField(object, "role"), "user") {
				lastUser = index
			}
		}
		prefix := typed
		if lastUser >= 0 {
			prefix = typed[:lastUser+1]
		} else if len(typed) > 0 {
			prefix = typed[:1]
		} else {
			prefix = []any{}
		}
		fingerprint := sha256Hex(canonicalJSON(prefix))
		outputs := 0
		start := lastUser + 1
		if lastUser < 0 {
			start = 1
		}
		if start < 0 {
			start = 0
		}
		for _, item := range typed[start:] {
			object, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch stringField(object, "type") {
			case "function_call_output", "custom_tool_call_output":
				outputs++
			}
		}
		return fingerprint, itoa(outputs + 1)
	default:
		return "anonymous", "1"
	}
}

func itoa(value int) string {
	return strings.TrimSpace(strings.ReplaceAll(canonicalJSON(value), `"`, ""))
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes", "on":
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func uuid5(namespace [16]byte, name string) string {
	hash := sha1.New()
	hash.Write(namespace[:])
	hash.Write([]byte(name))
	sum := hash.Sum(nil)[:16]
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return formatUUID(sum)
}

func formatUUID(raw []byte) string {
	hexed := hex.EncodeToString(raw)
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32]
}

func mustUUID(text string) [16]byte {
	raw, err := hex.DecodeString(strings.ReplaceAll(text, "-", ""))
	var out [16]byte
	if err != nil || len(raw) != 16 {
		return out
	}
	copy(out[:], raw)
	return out
}
