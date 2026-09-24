package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type executorRequest struct {
	Model       string `json:"Model"`
	Stream      bool   `json:"Stream"`
	Payload     []byte `json:"Payload"`
	StorageJSON []byte `json:"StorageJSON"`
	StreamID    string `json:"stream_id"`
	CallbackID  string `json:"host_callback_id"`
}

func execute(raw []byte, stream bool) ([]byte, error) {
	if !loadedConfig().enabled() {
		return errorEnvelope("plugin_disabled", "ex-plugin is disabled", http.StatusForbidden), nil
	}
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorEnvelope("invalid_request", err.Error(), http.StatusBadRequest), nil
	}
	source, err := requestSource(req)
	if err != nil {
		return errorEnvelope("invalid_request", err.Error(), http.StatusBadRequest), nil
	}
	if req.Model != "" && stringField(source, "model") == "" {
		source["model"] = req.Model
	}
	if stream {
		source["stream"] = true
	}
	cred, _, err := parseCredential(req.StorageJSON)
	if err != nil {
		return errorEnvelope("invalid_api_key", err.Error(), http.StatusUnauthorized), nil
	}
	if cred.AccessToken == "" {
		return errorEnvelope("invalid_api_key", "excel auth is not configured", http.StatusUnauthorized), nil
	}
	body := prepareResponsesBody(source, cred.ToolsVersionID)
	upstream, err := marshalCompact(body)
	if err != nil {
		return errorEnvelope("invalid_request", err.Error(), http.StatusBadRequest), nil
	}
	headers, order, err := cred.requestHeaders(stream || truthy(body["stream"]))
	if err != nil {
		return errorEnvelope("invalid_api_key", err.Error(), http.StatusUnauthorized), nil
	}
	public := publicModelID(firstModel(source))
	if stream {
		return startStream(req, headers, order, upstream, source, public)
	}
	return executeOnce(headers, order, upstream, source, public)
}

func requestSource(req executorRequest) (map[string]any, error) {
	if len(req.Payload) == 0 {
		return map[string]any{}, nil
	}
	object, err := decodeObject(req.Payload)
	if err != nil {
		return nil, fmt.Errorf("request payload is not a JSON object")
	}
	if _, ok := object["input"]; ok || object["messages"] == nil {
		return object, nil
	}
	return chatToResponses(object), nil
}

func chatToResponses(source map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range source {
		out[key] = value
	}
	var input []any
	var instructions []string
	messages, _ := source["messages"].([]any)
	for _, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := stringField(message, "role")
		text := outputText(message["content"])
		switch role {
		case "system", "developer":
			if text != "" {
				instructions = append(instructions, text)
			}
		case "tool":
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": firstString(message, "tool_call_id", "call_id"),
				"output":  text,
			})
		case "assistant":
			if calls, ok := message["tool_calls"].([]any); ok {
				for _, call := range calls {
					object, ok := call.(map[string]any)
					if !ok {
						continue
					}
					function, _ := object["function"].(map[string]any)
					name := stringField(function, "name")
					if name == "" {
						name = stringField(object, "name")
					}
					arguments := function["arguments"]
					if arguments == nil {
						arguments = object["arguments"]
					}
					if _, ok := arguments.(string); !ok && arguments != nil {
						arguments = mustCompact(arguments)
					}
					input = append(input, map[string]any{
						"type":      "function_call",
						"call_id":   firstString(object, "id", "call_id"),
						"name":      name,
						"arguments": arguments,
					})
				}
			}
			if text != "" {
				input = append(input, messageItem("assistant", text))
			}
		default:
			if text != "" {
				input = append(input, messageItem("user", text))
			}
		}
	}
	if len(instructions) > 0 && stringField(out, "instructions") == "" {
		out["instructions"] = strings.Join(instructions, "\n")
	}
	out["input"] = input
	if tools, ok := source["tools"].([]any); ok {
		converted := make([]any, 0, len(tools))
		for _, tool := range tools {
			object, ok := tool.(map[string]any)
			if !ok {
				continue
			}
			if stringField(object, "type") == "function" {
				if function, ok := object["function"].(map[string]any); ok && stringField(object, "name") == "" {
					flat := map[string]any{"type": "function", "name": stringField(function, "name")}
					if description := stringField(function, "description"); description != "" {
						flat["description"] = description
					}
					if parameters, ok := function["parameters"]; ok {
						flat["parameters"] = parameters
					}
					converted = append(converted, flat)
					continue
				}
			}
			converted = append(converted, object)
		}
		out["tools"] = converted
	}
	return out
}

func executeOnce(headers map[string][]string, order []string, body []byte, source map[string]any, publicModel string) ([]byte, error) {
	raw, err := doHost(hostHTTPDo, httpRequest(headers, order, body))
	if err != nil {
		return errorEnvelope("upstream_error", err.Error(), http.StatusBadGateway), nil
	}
	resp, err := decodeHTTPResponse(raw)
	if err != nil {
		return errorEnvelope("upstream_error", err.Error(), http.StatusBadGateway), nil
	}
	if resp.StatusCode >= 400 {
		return errorEnvelope(upstreamErrorCode(resp.StatusCode), upstreamErrorMessage(resp.Body), resp.StatusCode), nil
	}
	payload, err := completedResponse(resp)
	if err != nil {
		return errorEnvelope("upstream_error", err.Error(), http.StatusBadGateway), nil
	}
	if _, ok := payload["model"]; ok {
		payload["model"] = publicModel
	}
	text := responseOutputText(payload)
	toolCall := extractMarkerCall(text, clientTools(source))
	if toolCall == nil {
		toolCall = extractNativeClientToolCall(payload, source)
	}
	if toolCall != nil {
		payload = responseWithToolCall(payload, toolCall, publicModel)
	}
	encoded, err := marshalCompact(payload)
	if err != nil {
		return errorEnvelope("upstream_error", err.Error(), http.StatusBadGateway), nil
	}
	return okEnvelope(map[string]any{
		"Payload": encoded,
		"Headers": map[string][]string{"Content-Type": {"application/json"}},
	})
}

func startStream(req executorRequest, headers map[string][]string, order []string, body []byte, source map[string]any, publicModel string) ([]byte, error) {
	if strings.TrimSpace(req.StreamID) == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream", http.StatusBadRequest), nil
	}
	go runStream(req.StreamID, headers, order, body, source, publicModel)
	return okEnvelope(map[string]any{
		"headers": map[string][]string{"Content-Type": {"text/event-stream"}},
	})
}

func runStream(streamID string, headers map[string][]string, order []string, body []byte, source map[string]any, publicModel string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			closePluginStream(streamID, fmt.Sprintf("stream panic: %v", recovered))
		}
	}()
	raw, err := doHost(hostHTTPDoStream, httpRequest(headers, order, body))
	if err != nil {
		closePluginStream(streamID, err.Error())
		return
	}
	var opened struct {
		StatusCode int    `json:"status_code"`
		StreamID   string `json:"stream_id"`
		Body       []byte `json:"body"`
	}
	if err := json.Unmarshal(raw, &opened); err != nil {
		closePluginStream(streamID, err.Error())
		return
	}
	if opened.StatusCode >= 400 {
		message := upstreamErrorMessage(opened.Body)
		if opened.StreamID != "" {
			message = readErrorStream(opened.StreamID, message)
		}
		closePluginStream(streamID, message)
		return
	}
	if opened.StreamID == "" {
		closePluginStream(streamID, "basispoints response did not include a stream")
		return
	}
	defer func() { _ = closeHostStream(opened.StreamID) }()
	parser := &sseParser{}
	transformer := newToolStream(source, publicModel)
	for {
		chunkRaw, err := doHost(hostHTTPStreamRead, map[string]any{"stream_id": opened.StreamID})
		if err != nil {
			closePluginStream(streamID, err.Error())
			return
		}
		var chunk struct {
			Payload []byte `json:"payload"`
			Error   string `json:"error"`
			Done    bool   `json:"done"`
		}
		if err := json.Unmarshal(chunkRaw, &chunk); err != nil {
			closePluginStream(streamID, err.Error())
			return
		}
		if chunk.Error != "" {
			closePluginStream(streamID, chunk.Error)
			return
		}
		for _, frame := range transformer.consume(parser.push(chunk.Payload)) {
			if err := emitPluginStream(streamID, frame); err != nil {
				closePluginStream(streamID, err.Error())
				return
			}
		}
		if chunk.Done {
			break
		}
	}
	for _, frame := range transformer.consume(parser.flush()) {
		if err := emitPluginStream(streamID, frame); err != nil {
			closePluginStream(streamID, err.Error())
			return
		}
	}
	for _, frame := range transformer.finish() {
		if err := emitPluginStream(streamID, frame); err != nil {
			closePluginStream(streamID, err.Error())
			return
		}
	}
	closePluginStream(streamID, "")
}

func httpRequest(headers map[string][]string, order []string, body []byte) map[string]any {
	return map[string]any{
		"method":  http.MethodPost,
		"url":     loadedConfig().responsesURL(),
		"headers": headers,
		"body":    body,
		"wire_profile": map[string]any{
			"disable_auto_compression": true,
			"header_profile":           order,
		},
	}
}

type httpResponse struct {
	StatusCode int    `json:"status_code"`
	Body       []byte `json:"body"`
}

func decodeHTTPResponse(raw []byte) (httpResponse, error) {
	var resp httpResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return httpResponse{}, err
	}
	if resp.StatusCode == 0 {
		resp.StatusCode = http.StatusOK
	}
	return resp, nil
}

func completedResponse(resp httpResponse) (map[string]any, error) {
	if bytesLookLikeSSE(resp.Body) {
		parser := &sseParser{}
		events := parser.push(resp.Body)
		events = append(events, parser.flush()...)
		var completed map[string]any
		for _, event := range events {
			if event.Data == "[DONE]" {
				continue
			}
			payload, err := parseJSONValue(event.Data)
			if err != nil {
				continue
			}
			object, ok := payload.(map[string]any)
			if !ok {
				continue
			}
			eventName := strings.ToLower(event.Name)
			if eventName == "" {
				eventName = strings.ToLower(stringField(object, "type"))
			}
			if eventName == "response.completed" {
				if response, ok := object["response"].(map[string]any); ok {
					completed = response
				}
			}
		}
		if completed == nil {
			return nil, fmt.Errorf("upstream response did not include a completed responses payload")
		}
		return completed, nil
	}
	object, err := decodeObject(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("upstream response was not JSON")
	}
	if response, ok := object["response"].(map[string]any); ok && stringField(object, "type") == "response.completed" {
		return response, nil
	}
	return object, nil
}

func bytesLookLikeSSE(raw []byte) bool {
	text := strings.TrimSpace(string(raw))
	return strings.HasPrefix(text, "data:") || strings.HasPrefix(text, "event:")
}

func upstreamErrorCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "invalid_api_key"
	case http.StatusForbidden:
		return "insufficient_quota"
	case http.StatusTooManyRequests:
		return "rate_limit_exceeded"
	case http.StatusNotFound:
		return "model_not_found"
	default:
		if status >= 500 {
			return "internal_server_error"
		}
		return "invalid_request"
	}
}

func upstreamErrorMessage(body []byte) string {
	if len(body) == 0 {
		return "basispoints request failed"
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err == nil {
		if errObject, ok := payload["error"].(map[string]any); ok {
			if message := stringField(errObject, "message"); message != "" {
				return message
			}
		}
		if message := stringField(payload, "message"); message != "" {
			return message
		}
	}
	return truncate(strings.TrimSpace(string(body)), 500)
}

func readErrorStream(streamID, fallback string) string {
	defer func() { _ = closeHostStream(streamID) }()
	var buf []byte
	for i := 0; i < 8; i++ {
		raw, err := doHost(hostHTTPStreamRead, map[string]any{"stream_id": streamID})
		if err != nil {
			break
		}
		var chunk struct {
			Payload []byte `json:"payload"`
			Done    bool   `json:"done"`
		}
		if json.Unmarshal(raw, &chunk) != nil {
			break
		}
		buf = append(buf, chunk.Payload...)
		if chunk.Done || len(buf) > 4096 {
			break
		}
	}
	if len(buf) == 0 {
		return fallback
	}
	return upstreamErrorMessage(buf)
}

func emitPluginStream(streamID string, payload []byte) error {
	_, err := doHost(hostStreamEmit, map[string]any{"stream_id": streamID, "payload": payload})
	return err
}

func closePluginStream(streamID, message string) {
	_, _ = doHost(hostStreamClose, map[string]any{"stream_id": streamID, "error": strings.TrimSpace(message)})
}

func closeHostStream(streamID string) error {
	_, err := doHost(hostHTTPStreamClose, map[string]any{"stream_id": streamID})
	return err
}
