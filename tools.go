package main

import (
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	transportName    = "run_officejs"
	relayPrefix      = "codex_client__"
	markerCallPrefix = "call_ghcp_excel_marker_"
	nativeCallPrefix = "call_ghcp_excel_native_"
	markerOpen       = "<codex_tool_call>"
	markerClose      = "</codex_tool_call>"
	maxItemIDLength  = 64
	nativeCacheLimit = 512
)

var transportNames = map[string]bool{
	transportName:                true,
	"functions." + transportName: true,
}

type toolSpec struct {
	Key       string
	Name      string
	Namespace string
	Type      string
	Spec      map[string]any
}

func clientTools(source map[string]any) map[string]toolSpec {
	if strings.EqualFold(strings.TrimSpace(stringField(source, "tool_choice")), "none") {
		return map[string]toolSpec{}
	}
	return collectTools(source["tools"], "")
}

func collectTools(value any, namespace string) map[string]toolSpec {
	out := map[string]toolSpec{}
	tools, ok := value.([]any)
	if !ok {
		return out
	}
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		toolType := strings.ToLower(strings.TrimSpace(stringField(tool, "type")))
		name := strings.TrimSpace(stringField(tool, "name"))
		if (toolType == "function" || toolType == "custom") && name != "" {
			key := name
			if namespace != "" {
				key = namespace + "." + name
			}
			out[key] = toolSpec{Key: key, Name: name, Namespace: namespace, Type: toolType, Spec: tool}
		}
		if toolType == "namespace" && name != "" {
			for key, spec := range collectTools(tool["tools"], name) {
				out[key] = spec
			}
		}
	}
	return out
}

func catalogJSON(tools map[string]toolSpec) string {
	keys := make([]string, 0, len(tools))
	for key := range tools {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]any, 0, len(keys))
	for _, key := range keys {
		spec := tools[key]
		entry := map[string]any{"type": spec.Type, "name": spec.Key}
		if spec.Namespace != "" {
			entry["namespace"] = spec.Namespace
			entry["tool"] = spec.Name
		}
		if description := stringField(spec.Spec, "description"); description != "" {
			entry["description"] = description
		}
		if spec.Type == "function" {
			parameters := spec.Spec["parameters"]
			if parameters == nil {
				parameters = spec.Spec["inputSchema"]
			}
			if parameters == nil {
				parameters = spec.Spec["input_schema"]
			}
			if _, ok := parameters.(map[string]any); !ok {
				parameters = map[string]any{}
			}
			entry["parameters"] = parameters
		} else if format, ok := spec.Spec["format"].(map[string]any); ok {
			entry["format"] = format
		}
		entries = append(entries, entry)
	}
	return mustCompact(entries)
}

func originalClientToolName(name string, tools map[string]toolSpec) (string, bool) {
	if strings.HasPrefix(name, relayPrefix) {
		candidate := name[len(relayPrefix):]
		_, ok := tools[candidate]
		return candidate, ok
	}
	_, ok := tools[name]
	return name, ok
}

var (
	nativeMu    sync.Mutex
	nativeOrder []string
	nativeItems = map[string]map[string]any{}
)

func rememberNativeCall(item map[string]any) {
	callID := stringField(item, "call_id")
	if callID == "" {
		return
	}
	cloned, ok := cloneValue(item).(map[string]any)
	if !ok {
		return
	}
	nativeMu.Lock()
	defer nativeMu.Unlock()
	if _, exists := nativeItems[callID]; !exists {
		nativeOrder = append(nativeOrder, callID)
	}
	nativeItems[callID] = cloned
	for len(nativeOrder) > nativeCacheLimit {
		drop := nativeOrder[0]
		nativeOrder = nativeOrder[1:]
		delete(nativeItems, drop)
	}
}

func rememberedNativeCall(callID string) (map[string]any, bool) {
	if callID == "" {
		return nil, false
	}
	nativeMu.Lock()
	defer nativeMu.Unlock()
	item, ok := nativeItems[callID]
	if !ok {
		return nil, false
	}
	cloned, ok := cloneValue(item).(map[string]any)
	return cloned, ok
}

func resetNativeCalls() {
	nativeMu.Lock()
	defer nativeMu.Unlock()
	nativeOrder = nil
	nativeItems = map[string]map[string]any{}
}

func extractNativeClientToolCall(response map[string]any, source map[string]any) map[string]any {
	if response == nil {
		return nil
	}
	tools := clientTools(source)
	output, ok := response["output"].([]any)
	if !ok {
		return nil
	}
	var calls []map[string]any
	for _, item := range output {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch stringField(call, "type") {
		case "function_call", "custom_tool_call":
			calls = append(calls, call)
		}
	}
	if len(calls) != 1 {
		return nil
	}
	native := calls[0]
	envelope := transportEnvelope(native)
	if isTransportName(stringField(native, "name")) && envelope == nil {
		return nil
	}
	rawName := stringField(native, "name")
	if envelope != nil {
		rawName = envelopeToolName(envelope)
	}
	name, ok := originalClientToolName(rawName, tools)
	if !ok {
		return nil
	}
	spec := tools[name]
	callID := stringField(native, "call_id")
	if callID == "" {
		callID = nativeCallPrefix + randomHex(16)
	}
	itemID := stringField(native, "id")
	if spec.Type == "function" {
		arguments, ok := functionArguments(native, envelope, name)
		if !ok || !valueMatchesSchema(arguments, functionSchema(spec.Spec)) {
			return nil
		}
		rememberNativeCall(native)
		if itemID == "" {
			itemID = functionItemID(callID)
		}
		result := map[string]any{
			"type":      "function_call",
			"id":        itemID,
			"call_id":   callID,
			"name":      spec.Name,
			"arguments": mustCompact(arguments),
		}
		if spec.Namespace != "" {
			result["namespace"] = spec.Namespace
		}
		return result
	}
	if spec.Type == "custom" {
		var input any
		if envelope != nil {
			input = envelope["input"]
		} else {
			if stringField(native, "type") != "custom_tool_call" {
				return nil
			}
			input = native["input"]
		}
		text, ok := input.(string)
		if !ok {
			return nil
		}
		rememberNativeCall(native)
		if envelope != nil || itemID == "" {
			itemID = "ctc_" + callID
		}
		return map[string]any{
			"type":    "custom_tool_call",
			"id":      itemID,
			"call_id": callID,
			"name":    name,
			"input":   text,
		}
	}
	return nil
}

func functionArguments(native, envelope map[string]any, name string) (map[string]any, bool) {
	var raw any
	if envelope != nil {
		raw = envelopeArguments(envelope)
	} else {
		if stringField(native, "type") != "function_call" {
			return nil, false
		}
		raw = native["arguments"]
	}
	arguments, ok := decodeArguments(raw)
	if !ok {
		return nil, false
	}
	if envelope == nil {
		arguments = normalizePlanArguments(name, arguments)
	}
	return arguments, true
}

func envelopeToolName(envelope map[string]any) string {
	if name := strings.TrimSpace(stringField(envelope, "name")); name != "" {
		return name
	}
	return strings.TrimSpace(stringField(envelope, "tool"))
}

func envelopeArguments(envelope map[string]any) any {
	if value, ok := envelope["arguments"]; ok {
		return value
	}
	if value, ok := envelope["args"]; ok {
		return value
	}
	return nil
}

func decodeArguments(raw any) (map[string]any, bool) {
	switch typed := raw.(type) {
	case map[string]any:
		return typed, true
	case string:
		value, err := parseJSONValue(typed)
		if err != nil {
			return nil, false
		}
		object, ok := value.(map[string]any)
		return object, ok
	default:
		return nil, false
	}
}

func functionSchema(spec map[string]any) any {
	if schema, ok := spec["parameters"].(map[string]any); ok {
		return schema
	}
	if schema, ok := spec["inputSchema"].(map[string]any); ok {
		return schema
	}
	if schema, ok := spec["input_schema"].(map[string]any); ok {
		return schema
	}
	return nil
}

func isTransportName(name string) bool {
	return transportNames[name]
}

func transportEnvelope(native map[string]any) map[string]any {
	if stringField(native, "type") != "function_call" || !isTransportName(stringField(native, "name")) {
		return nil
	}
	arguments, ok := decodeArguments(native["arguments"])
	if !ok {
		return nil
	}
	envelope := decodeTransportCode(arguments["code"])
	for i := 0; i < 2; i++ {
		if envelope == nil || !isTransportName(stringField(envelope, "name")) {
			break
		}
		nested, ok := decodeArguments(envelope["arguments"])
		if !ok {
			return nil
		}
		envelope = decodeTransportCode(nested["code"])
	}
	if envelope != nil && isTransportName(stringField(envelope, "name")) {
		return nil
	}
	return envelope
}

func decodeTransportCode(code any) map[string]any {
	if object, ok := code.(map[string]any); ok {
		return object
	}
	text, ok := code.(string)
	if !ok {
		return nil
	}
	candidates := []string{text}
	if repaired := repairInvalidJSONBackslashes(text); repaired != text {
		candidates = append(candidates, repaired)
	}
	for _, candidate := range candidates {
		if object := firstJSONObject(candidate); object != nil {
			return object
		}
	}
	return nil
}

func firstJSONObject(text string) map[string]any {
	if value, err := parseJSONValue(text); err == nil {
		if object, ok := value.(map[string]any); ok {
			return object
		}
	}
	for index, character := range text {
		if character != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(text[index:]))
		dec.UseNumber()
		var value any
		if err := dec.Decode(&value); err != nil {
			continue
		}
		if object, ok := value.(map[string]any); ok {
			return object
		}
	}
	return nil
}

func repairInvalidJSONBackslashes(text string) string {
	var repaired strings.Builder
	inString := false
	for index := 0; index < len(text); {
		character := text[index]
		if !inString {
			repaired.WriteByte(character)
			if character == '"' {
				inString = true
			}
			index++
			continue
		}
		if character == '"' {
			repaired.WriteByte(character)
			inString = false
			index++
			continue
		}
		if character != '\\' {
			repaired.WriteByte(character)
			index++
			continue
		}
		next := byte(0)
		if index+1 < len(text) {
			next = text[index+1]
		}
		valid := strings.ContainsRune(`"\/bfnrt`, rune(next))
		if next == 'u' && index+6 <= len(text) {
			valid = true
			for _, digit := range text[index+2 : index+6] {
				if !strings.ContainsRune("0123456789abcdefABCDEF", digit) {
					valid = false
					break
				}
			}
		}
		if valid && next != 0 {
			repaired.WriteByte(character)
			repaired.WriteByte(next)
			index += 2
			continue
		}
		repaired.WriteByte('\\')
		repaired.WriteByte('\\')
		index++
	}
	return repaired.String()
}

func valueMatchesSchema(value, schema any) bool {
	object, ok := schema.(map[string]any)
	if !ok || len(object) == 0 {
		return true
	}
	switch expected := object["type"].(type) {
	case []any:
		for _, candidate := range expected {
			cloned := map[string]any{}
			for key, item := range object {
				cloned[key] = item
			}
			cloned["type"] = candidate
			if valueMatchesSchema(value, cloned) {
				return enumAllows(value, object["enum"])
			}
		}
		return false
	case string:
		if !valueMatchesType(value, expected, object) {
			return false
		}
	}
	return enumAllows(value, object["enum"])
}

func valueMatchesType(value any, expected string, schema map[string]any) bool {
	switch expected {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return false
		}
		if required, ok := schema["required"].([]any); ok {
			for _, key := range required {
				name, ok := key.(string)
				if ok {
					if _, exists := object[name]; !exists {
						return false
					}
				}
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		if schema["additionalProperties"] == false && properties != nil {
			for key := range object {
				if _, ok := properties[key]; !ok {
					return false
				}
			}
		}
		if properties != nil {
			for key, nested := range object {
				if nestedSchema, ok := properties[key]; ok && !valueMatchesSchema(nested, nestedSchema) {
					return false
				}
			}
		}
		return true
	case "array":
		items, ok := value.([]any)
		if !ok {
			return false
		}
		if itemSchema, ok := schema["items"]; ok {
			for _, item := range items {
				if !valueMatchesSchema(item, itemSchema) {
					return false
				}
			}
		}
		return true
	case "string":
		_, ok := value.(string)
		return ok
	case "integer":
		return isJSONInteger(value)
	case "number":
		return isJSONNumber(value)
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	default:
		return true
	}
}

func enumAllows(value, enum any) bool {
	items, ok := enum.([]any)
	if !ok {
		return true
	}
	for _, item := range items {
		if canonicalJSON(item) == canonicalJSON(value) {
			return true
		}
	}
	return false
}

var planStatusAliases = map[string]string{
	"pending": "pending", "not_started": "pending", "todo": "pending", "planned": "pending", "queued": "pending", "blocked": "pending",
	"in_progress": "in_progress", "active": "in_progress", "started": "in_progress", "doing": "in_progress", "current": "in_progress",
	"completed": "completed", "complete": "completed", "done": "completed", "finished": "completed",
}

func normalizePlanStatus(status any) (string, bool) {
	text, ok := status.(string)
	if !ok {
		return "", false
	}
	key := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(text)), "-", "_"), " ", "_")
	if mapped, ok := planStatusAliases[key]; ok {
		return mapped, true
	}
	return text, true
}

func normalizePlanArguments(name string, arguments map[string]any) map[string]any {
	if name != "update_plan" {
		return arguments
	}
	plan, ok := arguments["plan"].([]any)
	if !ok {
		return arguments
	}
	normalizedPlan := make([]any, 0, len(plan))
	for _, item := range plan {
		stepObject, ok := item.(map[string]any)
		if !ok {
			continue
		}
		step := stringField(stepObject, "step")
		if step == "" {
			step = stringField(stepObject, "description")
		}
		if step == "" {
			step = stringField(stepObject, "title")
		}
		status, ok := normalizePlanStatus(stepObject["status"])
		if step != "" && ok {
			normalizedPlan = append(normalizedPlan, map[string]any{"step": step, "status": status})
		}
	}
	normalized := map[string]any{"plan": normalizedPlan}
	explanation := stringField(arguments, "explanation")
	if explanation == "" {
		explanation = stringField(arguments, "summary")
	}
	if explanation != "" {
		normalized["explanation"] = explanation
	}
	return normalized
}

func restorePlanArguments(name string, arguments any) any {
	if name != "update_plan" {
		return arguments
	}
	parsed := arguments
	if text, ok := arguments.(string); ok {
		value, err := parseJSONValue(text)
		if err != nil {
			return arguments
		}
		parsed = value
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return arguments
	}
	plan, ok := object["plan"].([]any)
	if !ok {
		return arguments
	}
	nativePlan := make([]any, 0, len(plan))
	for index, item := range plan {
		stepObject, ok := item.(map[string]any)
		if !ok {
			continue
		}
		step := stringField(stepObject, "step")
		status := stringField(stepObject, "status")
		if step == "" || status == "" {
			continue
		}
		nativePlan = append(nativePlan, map[string]any{
			"id":          "step" + strconv.Itoa(index+1),
			"description": step,
			"status":      status,
			"result":      "",
		})
	}
	explanation := stringField(object, "explanation")
	if explanation == "" {
		explanation = "Update task plan"
	}
	return mustCompact(map[string]any{"summary": explanation, "plan": nativePlan})
}

func extractMarkerCall(text string, tools map[string]toolSpec) map[string]any {
	if text == "" || len(tools) == 0 {
		return nil
	}
	start := strings.Index(text, markerOpen)
	if start < 0 {
		return nil
	}
	rest := text[start+len(markerOpen):]
	end := strings.Index(rest, markerClose)
	if end < 0 {
		return nil
	}
	object := firstJSONObject(rest[:end])
	if object == nil {
		return nil
	}
	name, ok := originalClientToolName(strings.TrimSpace(stringField(object, "name")), tools)
	if !ok {
		return nil
	}
	spec := tools[name]
	callID := markerCallPrefix + randomHex(16)
	if spec.Type == "function" {
		arguments, ok := decodeArguments(object["arguments"])
		if !ok {
			return nil
		}
		return map[string]any{
			"type":      "function_call",
			"id":        functionItemID(callID),
			"call_id":   callID,
			"name":      name,
			"arguments": mustCompact(arguments),
		}
	}
	if spec.Type == "custom" {
		input, ok := object["input"].(string)
		if !ok {
			return nil
		}
		return map[string]any{
			"type":    "custom_tool_call",
			"id":      "ctc_" + callID,
			"call_id": callID,
			"name":    name,
			"input":   input,
		}
	}
	return nil
}

func responseWithToolCall(response, toolCall map[string]any, modelID string) map[string]any {
	result := map[string]any{}
	for key, value := range response {
		result[key] = value
	}
	if stringField(result, "id") == "" {
		result["id"] = "resp_" + randomHex(16)
	}
	if stringField(result, "object") == "" {
		result["object"] = "response"
	}
	if result["created_at"] == nil {
		result["created_at"] = time.Now().Unix()
	}
	result["status"] = "completed"
	result["model"] = modelID
	completed := map[string]any{}
	for key, value := range toolCall {
		completed[key] = value
	}
	completed["status"] = "completed"
	var output []any
	replaced := false
	if existing, ok := result["output"].([]any); ok {
		for _, item := range existing {
			object, ok := item.(map[string]any)
			if !ok {
				continue
			}
			itemType := stringField(object, "type")
			if !replaced && (itemType == "function_call" || itemType == "custom_tool_call") {
				output = append(output, completed)
				replaced = true
				continue
			}
			output = append(output, object)
		}
	}
	if !replaced {
		output = []any{completed}
	}
	result["output"] = output
	result["error"] = nil
	result["incomplete_details"] = nil
	return result
}

func functionItemID(callID string) string {
	candidate := "fc_" + callID
	if len(candidate) <= maxItemIDLength {
		return candidate
	}
	sum := sha256.Sum256([]byte(callID))
	encoded := hex.EncodeToString(sum[:])
	return "fc_" + encoded[:maxItemIDLength-len("fc_")]
}

func fallbackTransportCall(item map[string]any) map[string]any {
	name := stringField(item, "name")
	var envelope map[string]any
	if stringField(item, "type") == "custom_tool_call" {
		input, _ := item["input"].(string)
		envelope = map[string]any{"name": name, "input": input}
	} else {
		arguments, ok := decodeArguments(item["arguments"])
		if !ok {
			arguments = map[string]any{}
		}
		envelope = map[string]any{"name": name, "arguments": arguments}
	}
	callID := stringField(item, "call_id")
	if callID == "" {
		callID = nativeCallPrefix + randomHex(16)
	}
	nativeArguments := map[string]any{
		"summary":          "Run client tool " + name,
		"extended_summary": "Relay " + name + " through the external Codex client",
		"code":             mustCompact(envelope),
		"destructive":      false,
		"references":       []any{},
	}
	return map[string]any{
		"type":      "function_call",
		"id":        functionItemID(callID),
		"call_id":   callID,
		"name":      transportName,
		"arguments": mustCompact(nativeArguments),
		"status":    "completed",
	}
}

func outputText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var parts []string
		for _, part := range typed {
			switch item := part.(type) {
			case string:
				parts = append(parts, item)
			case map[string]any:
				if text, ok := item["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "")
	default:
		return ""
	}
}

func responseOutputText(response map[string]any) string {
	if response == nil {
		return ""
	}
	output, ok := response["output"].([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, item := range output {
		message, ok := item.(map[string]any)
		if !ok || stringField(message, "type") != "message" {
			continue
		}
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, part := range content {
			object, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := object["text"].(string); ok {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "")
}

func sortStrings(values []string) {
	sort.Strings(values)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := cryptorand.Read(buf); err != nil {
		for i := range buf {
			buf[i] = byte(i)
		}
	}
	return hex.EncodeToString(buf)
}

func stripPassthrough(item map[string]any) map[string]any {
	if _, ok := item["internal_chat_message_metadata_passthrough"]; !ok {
		return item
	}
	cloned := map[string]any{}
	for key, value := range item {
		if key == "internal_chat_message_metadata_passthrough" {
			continue
		}
		cloned[key] = value
	}
	return cloned
}
