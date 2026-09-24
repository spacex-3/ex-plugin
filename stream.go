package main

import (
	"bytes"
	"strings"
)

type sseEvent struct {
	Name string
	Data string
}

type sseParser struct {
	buf bytes.Buffer
}

func (p *sseParser) push(chunk []byte) []sseEvent {
	p.buf.Write(chunk)
	raw := p.buf.String()
	var events []sseEvent
	for {
		index := strings.Index(raw, "\n\n")
		if index < 0 {
			break
		}
		block := raw[:index]
		raw = raw[index+2:]
		if event, ok := parseSSEBlock(block); ok {
			events = append(events, event)
		}
	}
	p.buf.Reset()
	p.buf.WriteString(raw)
	return events
}

func (p *sseParser) flush() []sseEvent {
	block := strings.TrimSpace(p.buf.String())
	p.buf.Reset()
	if block == "" {
		return nil
	}
	event, ok := parseSSEBlock(block)
	if !ok {
		return nil
	}
	return []sseEvent{event}
}

func parseSSEBlock(block string) (sseEvent, bool) {
	if strings.TrimSpace(block) == "" {
		return sseEvent{}, false
	}
	var name string
	var data []string
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			name = strings.TrimSpace(line[len("event:"):])
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(line[len("data:"):], " "))
		}
	}
	if name == "" && len(data) == 0 {
		return sseEvent{}, false
	}
	return sseEvent{Name: name, Data: strings.Join(data, "\n")}, true
}

func encodeSSE(name string, payload any) []byte {
	body, err := marshalCompact(payload)
	if err != nil {
		return nil
	}
	var buf bytes.Buffer
	if name != "" {
		buf.WriteString("event: ")
		buf.WriteString(name)
		buf.WriteByte('\n')
	}
	buf.WriteString("data: ")
	buf.Write(body)
	buf.WriteString("\n\n")
	return buf.Bytes()
}

type toolStream struct {
	source      map[string]any
	publicModel string
	tools       map[string]toolSpec
	fullText    string
	emitted     int
	markerMode  bool
	held        [][]byte
	template    map[string]any
	doneSeen    bool
	toolIndex   *int
}

func newToolStream(source map[string]any, publicModel string) *toolStream {
	return &toolStream{
		source:      source,
		publicModel: publicModel,
		tools:       clientTools(source),
	}
}

func (s *toolStream) consume(events []sseEvent) [][]byte {
	var out [][]byte
	for _, event := range events {
		out = append(out, s.consumeOne(event)...)
	}
	return out
}

func (s *toolStream) finish() [][]byte {
	var out [][]byte
	out = append(out, s.flushText()...)
	out = append(out, s.held...)
	s.held = nil
	if s.doneSeen {
		out = append(out, []byte("data: [DONE]\n\n"))
	}
	return out
}

func (s *toolStream) consumeOne(event sseEvent) [][]byte {
	if event.Data == "[DONE]" {
		s.doneSeen = true
		return nil
	}
	payload, err := parseJSONValue(event.Data)
	if err != nil {
		return nil
	}
	object, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	eventType := strings.ToLower(strings.TrimSpace(event.Name))
	if eventType == "" {
		eventType = strings.ToLower(strings.TrimSpace(stringField(object, "type")))
	}
	if eventType == "" {
		eventType = "message"
	}
	object["type"] = eventType
	if len(s.tools) == 0 {
		s.rewriteModel(object)
		frame := encodeSSE(eventType, object)
		if eventType == "response.completed" || eventType == "response.failed" || eventType == "response.incomplete" {
			return [][]byte{frame}
		}
		return [][]byte{frame}
	}
	switch eventType {
	case "response.output_text.delta":
		return s.onTextDelta(object)
	case "response.output_text.done":
		if s.markerMode {
			s.held = append(s.held, encodeSSE(eventType, object))
			return nil
		}
		return append(s.flushText(), encodeSSE(eventType, object))
	case "response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		s.held = append(s.held, encodeSSE(eventType, object))
		return nil
	case "response.output_item.added", "response.output_item.done":
		return s.onOutputItem(eventType, object)
	case "response.completed", "response.failed", "response.incomplete":
		return s.onTerminal(eventType, object)
	default:
		s.rewriteModel(object)
		return [][]byte{encodeSSE(eventType, object)}
	}
}

func (s *toolStream) onTextDelta(object map[string]any) [][]byte {
	if delta, ok := object["delta"].(string); ok {
		s.fullText += delta
	}
	s.template = map[string]any{}
	for _, key := range []string{"item_id", "output_index", "content_index"} {
		if value, ok := object[key]; ok {
			s.template[key] = value
		}
	}
	if s.markerMode {
		return nil
	}
	searchFrom := s.emitted - len(markerOpen) + 1
	if searchFrom < 0 {
		searchFrom = 0
	}
	if pos := strings.Index(s.fullText[searchFrom:], markerOpen); pos >= 0 {
		markerAt := searchFrom + pos
		s.markerMode = true
		pending := ""
		if markerAt > s.emitted {
			pending = s.fullText[s.emitted:markerAt]
		}
		s.emitted = markerAt
		if pending == "" {
			return nil
		}
		return [][]byte{s.textDelta(pending)}
	}
	boundary := len(s.fullText) - markerHold(s.fullText)
	if boundary <= s.emitted {
		return nil
	}
	pending := s.fullText[s.emitted:boundary]
	s.emitted = boundary
	return [][]byte{s.textDelta(pending)}
}

func (s *toolStream) onOutputItem(eventType string, object map[string]any) [][]byte {
	item, _ := object["item"].(map[string]any)
	itemType := ""
	if item != nil {
		itemType = stringField(item, "type")
	}
	if itemType == "function_call" || itemType == "custom_tool_call" {
		if index, ok := outputIndexFrom(object["output_index"]); ok && index >= 0 {
			s.toolIndex = &index
		}
		s.held = append(s.held, encodeSSE(eventType, object))
		return nil
	}
	if eventType == "response.output_item.done" && s.markerMode && itemType == "message" {
		s.held = append(s.held, encodeSSE(eventType, object))
		return nil
	}
	return [][]byte{encodeSSE(eventType, object)}
}

type jsonInteger = interface{ Int64() (int64, error) }

func (s *toolStream) onTerminal(eventType string, object map[string]any) [][]byte {
	response, _ := object["response"].(map[string]any)
	var toolCall map[string]any
	if eventType == "response.completed" {
		text := s.fullText
		if text == "" {
			text = responseOutputText(response)
		}
		toolCall = extractMarkerCall(text, s.tools)
		if toolCall == nil {
			toolCall = extractNativeClientToolCall(response, s.source)
		}
	}
	if toolCall != nil {
		s.held = nil
		s.emitted = len(s.fullText)
		payload := responseWithToolCall(response, toolCall, s.publicModel)
		index := 0
		if s.toolIndex != nil {
			index = *s.toolIndex
		}
		return toolCallFrames(toolCall, payload, index)
	}
	var out [][]byte
	out = append(out, s.flushText()...)
	out = append(out, s.held...)
	s.held = nil
	s.markerMode = false
	s.rewriteModel(object)
	out = append(out, encodeSSE(eventType, object))
	return out
}

func (s *toolStream) flushText() [][]byte {
	if s.emitted >= len(s.fullText) {
		return nil
	}
	pending := s.fullText[s.emitted:]
	s.emitted = len(s.fullText)
	if pending == "" {
		return nil
	}
	return [][]byte{s.textDelta(pending)}
}

func (s *toolStream) textDelta(text string) []byte {
	payload := map[string]any{"type": "response.output_text.delta", "delta": text}
	for key, value := range s.template {
		payload[key] = value
	}
	return encodeSSE("response.output_text.delta", payload)
}

func (s *toolStream) rewriteModel(object map[string]any) {
	if response, ok := object["response"].(map[string]any); ok && s.publicModel != "" {
		if _, exists := response["model"]; exists {
			response["model"] = s.publicModel
		}
	}
}

func markerHold(text string) int {
	maxProbe := len(markerOpen) - 1
	if len(text) < maxProbe {
		maxProbe = len(text)
	}
	for probe := maxProbe; probe > 0; probe-- {
		if strings.HasSuffix(text, markerOpen[:probe]) {
			return probe
		}
	}
	return 0
}

func toolCallFrames(toolCall, response map[string]any, outputIndex int) [][]byte {
	item := cloneObject(toolCall)
	item["status"] = "in_progress"
	valueKey := "arguments"
	deltaEvent := "response.function_call_arguments.delta"
	doneEvent := "response.function_call_arguments.done"
	if stringField(toolCall, "type") != "function_call" {
		item["input"] = ""
		valueKey = "input"
		deltaEvent = "response.custom_tool_call_input.delta"
		doneEvent = "response.custom_tool_call_input.done"
	} else {
		item["arguments"] = ""
	}
	value := toolCall[valueKey]
	return [][]byte{
		encodeSSE("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": outputIndex,
			"item":         item,
		}),
		encodeSSE(deltaEvent, map[string]any{
			"type":         deltaEvent,
			"output_index": outputIndex,
			"item_id":      toolCall["id"],
			"delta":        value,
		}),
		encodeSSE(doneEvent, map[string]any{
			"type":         doneEvent,
			"output_index": outputIndex,
			"item_id":      toolCall["id"],
			valueKey:       value,
		}),
		encodeSSE("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": outputIndex,
			"item":         completedItem(toolCall),
		}),
		encodeSSE("response.completed", map[string]any{
			"type":     "response.completed",
			"response": response,
		}),
	}
}

func completedItem(toolCall map[string]any) map[string]any {
	item := cloneObject(toolCall)
	item["status"] = "completed"
	return item
}

func outputIndexFrom(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case jsonInteger:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return int(parsed), true
	default:
		return 0, false
	}
}
