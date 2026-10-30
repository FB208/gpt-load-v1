// Portions of this file are adapted from CLIProxyAPI
// (https://github.com/router-for-me/CLIProxyAPI, internal/translator/codex/openai/chat-completions),
// Copyright (c) 2025.9-present Router-For.ME, licensed under the MIT License.

package openai_responses

import (
	"bytes"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var dataTag = []byte("data:")

// UpstreamError carries a Chat Completions error body for a Responses result that failed upstream.
type UpstreamError struct {
	Body []byte
}

func (e *UpstreamError) Error() string {
	return gjson.GetBytes(e.Body, "error.message").String()
}

type toolCallStreamState struct {
	Index            int
	Custom           bool
	ArgumentsEmitted bool
	Done             bool
}

// StreamConverter converts a Responses API event stream into chat.completion.chunk
// payloads. It keeps per-stream state, so use one converter per upstream response.
type StreamConverter struct {
	opts             Options
	includeUsage     bool
	legacyFunctions  bool
	reverseToolNames map[string]string

	responseID  string
	createdAt   int64
	model       string
	serviceTier string

	functionCallIndex int
	toolCallStates    map[string]*toolCallStreamState
	currentToolCall   *toolCallStreamState
	citationKeys      map[string]struct{}
	emittedTextRunes  int64
	partRuneOffset    int64
	roleSent          bool
	usage             []byte
}

// NewStreamConverter creates a converter for one upstream stream. originalRequest is
// the Chat Completions request body the client sent.
func NewStreamConverter(originalRequest []byte, opts Options) *StreamConverter {
	root := gjson.ParseBytes(originalRequest)
	return &StreamConverter{
		opts:              opts,
		includeUsage:      root.Get("stream_options.include_usage").Bool(),
		legacyFunctions:   usesLegacyFunctions(root),
		reverseToolNames:  buildReverseMapFromOriginalOpenAI(originalRequest),
		createdAt:         time.Now().Unix(),
		model:             root.Get("model").String(),
		functionCallIndex: -1,
		toolCallStates:    make(map[string]*toolCallStreamState),
		citationKeys:      make(map[string]struct{}),
	}
}

// ConvertLine converts one line of the upstream SSE stream into zero or more
// chat.completion.chunk payloads (without the "data: " framing).
func (s *StreamConverter) ConvertLine(line []byte) [][]byte {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, dataTag) {
		return nil
	}
	data := bytes.TrimSpace(line[len(dataTag):])
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) || !gjson.ValidBytes(data) {
		return nil
	}
	chunk := s.convertEvent(gjson.ParseBytes(data))
	if chunk == nil {
		return nil
	}
	if s.legacyFunctions {
		chunk = legacyFunctionChunk(chunk)
	}
	return [][]byte{chunk}
}

// Finish returns the trailing chunks once the upstream stream has ended: the usage
// chunk when the client asked for it with stream_options.include_usage.
func (s *StreamConverter) Finish() [][]byte {
	if !s.includeUsage || s.usage == nil {
		return nil
	}
	chunk := s.newChunk()
	chunk, _ = sjson.SetRawBytes(chunk, "choices", []byte(`[]`))
	chunk, _ = sjson.SetRawBytes(chunk, "usage", s.usage)
	return [][]byte{chunk}
}

func (s *StreamConverter) convertEvent(event gjson.Result) []byte {
	if tier := responseServiceTier(event.Get("response")); tier != "" {
		s.serviceTier = tier
	}

	dataType := event.Get("type").String()
	switch dataType {
	case "response.created", "response.in_progress":
		response := event.Get("response")
		if id := response.Get("id").String(); id != "" {
			s.responseID = id
		}
		if createdAt := response.Get("created_at").Int(); createdAt > 0 {
			s.createdAt = createdAt
		}
		if model := response.Get("model").String(); model != "" {
			s.model = model
		}
		return nil
	case "response.failed":
		return upstreamErrorPayload(event.Get("response.error"))
	case "error":
		if nested := event.Get("error"); nested.IsObject() {
			return upstreamErrorPayload(nested)
		}
		return upstreamErrorPayload(event)
	case "response.content_part.added":
		s.partRuneOffset = s.emittedTextRunes
		return nil
	}

	if usage := event.Get("response.usage"); usage.Exists() {
		s.usage = chatUsageFromResponses(usage)
	}

	template := s.newChunk()
	switch dataType {
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		delta := event.Get("delta").String()
		if !s.opts.ReasoningSummary || delta == "" {
			return nil
		}
		template = s.setDelta(template, "reasoning_content", delta)

	case "response.reasoning_summary_text.done", "response.reasoning_text.done":
		if !s.opts.ReasoningSummary {
			return nil
		}
		template = s.setDelta(template, "reasoning_content", "\n\n")

	case "response.output_text.delta":
		deltaResult := event.Get("delta")
		if !deltaResult.Exists() {
			return nil
		}
		delta := deltaResult.String()
		template = s.setDelta(template, "content", delta)
		s.emittedTextRunes += int64(utf8.RuneCountInString(delta))
		if logprobs := event.Get("logprobs"); logprobs.IsArray() && len(logprobs.Array()) > 0 {
			template, _ = sjson.SetRawBytes(template, "choices.0.logprobs.content", []byte(logprobs.Raw))
		}

	case "response.refusal.delta":
		delta := event.Get("delta").String()
		if delta == "" {
			return nil
		}
		template = s.setDelta(template, "refusal", delta)

	case "response.output_text.annotation.added", "response.output_text.done", "response.content_part.done":
		citations := buildURLCitations(annotationsFromEvent(event), s.partRuneOffset, s.citationKeys)
		if len(citations) == 0 {
			return nil
		}
		template = s.markRole(template)
		template, _ = sjson.SetRawBytes(template, "choices.0.delta.annotations", joinRawArray(citations))

	case "response.completed", "response.incomplete":
		finishReason := "stop"
		if dataType == "response.incomplete" {
			switch event.Get("response.incomplete_details.reason").String() {
			case "max_tokens", "max_output_tokens":
				finishReason = "length"
			case "content_filter":
				finishReason = "content_filter"
			}
		} else if s.functionCallIndex != -1 {
			finishReason = "tool_calls"
		}
		template = s.markRole(template)
		template, _ = sjson.SetBytes(template, "choices.0.finish_reason", finishReason)

	case "response.output_item.added":
		item := event.Get("item")
		if !item.Exists() || !isToolCallType(item.Get("type").String()) {
			return nil
		}
		s.functionCallIndex++
		state := &toolCallStreamState{Index: s.functionCallIndex, Custom: item.Get("type").String() == "custom_tool_call"}
		s.registerToolCallState(event, item, state)
		template = s.setToolCall(template, s.toolCallStart(state, item, ""))

	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		state := s.findToolCallState(event, gjson.Result{})
		delta := event.Get("delta").String()
		if state == nil || state.Done || delta == "" {
			return nil
		}
		state.ArgumentsEmitted = true
		template = s.setToolCall(template, toolCallArgumentsDelta(state, delta))

	case "response.function_call_arguments.done", "response.custom_tool_call_input.done":
		state := s.findToolCallState(event, gjson.Result{})
		if state == nil || state.Done || state.ArgumentsEmitted {
			return nil
		}
		// Fallback: no delta events were received, emit the full arguments as a single chunk.
		fullArgsField := "arguments"
		if dataType == "response.custom_tool_call_input.done" {
			fullArgsField = "input"
		}
		state.ArgumentsEmitted = true
		fullArgs := event.Get(fullArgsField).String()
		if fullArgs == "" {
			return nil
		}
		template = s.setToolCall(template, toolCallArgumentsDelta(state, fullArgs))

	case "response.output_item.done":
		item := event.Get("item")
		if !item.Exists() {
			return nil
		}
		itemType := item.Get("type").String()
		if itemType == "message" {
			citations := buildURLCitations(annotationsFromEvent(event), s.partRuneOffset, s.citationKeys)
			if len(citations) == 0 {
				return nil
			}
			template = s.markRole(template)
			template, _ = sjson.SetRawBytes(template, "choices.0.delta.annotations", joinRawArray(citations))
			return template
		}
		if !isToolCallType(itemType) {
			return nil
		}

		state := s.findToolCallState(event, item)
		if state != nil {
			if state.Done {
				return nil
			}
			state.Done = true
			if state.ArgumentsEmitted {
				return nil
			}
			// The tool was announced, but no argument event arrived. Emit only the
			// completed arguments so the id and name are not duplicated.
			state.ArgumentsEmitted = true
			fullArgs := toolCallArguments(item)
			if fullArgs == "" {
				return nil
			}
			return s.setToolCall(template, toolCallArgumentsDelta(state, fullArgs))
		}

		// Fallback path: the model skipped output_item.added, so emit the complete tool call now.
		s.functionCallIndex++
		state = &toolCallStreamState{Index: s.functionCallIndex, Custom: itemType == "custom_tool_call", ArgumentsEmitted: true, Done: true}
		s.registerToolCallState(event, item, state)
		template = s.setToolCall(template, s.toolCallStart(state, item, toolCallArguments(item)))

	default:
		return nil
	}

	return template
}

func (s *StreamConverter) newChunk() []byte {
	chunk := []byte(`{"id":"","object":"chat.completion.chunk","created":0,"model":"","choices":[{"index":0,"delta":{},"finish_reason":null}]}`)
	chunk, _ = sjson.SetBytes(chunk, "id", s.responseID)
	chunk, _ = sjson.SetBytes(chunk, "created", s.createdAt)
	chunk, _ = sjson.SetBytes(chunk, "model", s.model)
	if s.serviceTier != "" {
		chunk, _ = sjson.SetBytes(chunk, "service_tier", s.serviceTier)
	}
	return chunk
}

// markRole adds the assistant role to the first chunk of the stream, as Chat Completions does.
func (s *StreamConverter) markRole(chunk []byte) []byte {
	if s.roleSent {
		return chunk
	}
	s.roleSent = true
	chunk, _ = sjson.SetBytes(chunk, "choices.0.delta.role", "assistant")
	return chunk
}

func (s *StreamConverter) setDelta(chunk []byte, field, value string) []byte {
	chunk = s.markRole(chunk)
	chunk, _ = sjson.SetBytes(chunk, "choices.0.delta."+field, value)
	return chunk
}

func (s *StreamConverter) setToolCall(chunk, call []byte) []byte {
	chunk = s.markRole(chunk)
	chunk, _ = sjson.SetRawBytes(chunk, "choices.0.delta.tool_calls", joinRawArray([][]byte{call}))
	return chunk
}

// toolCallStart builds the first delta of a tool call, carrying its id and original name.
func (s *StreamConverter) toolCallStart(state *toolCallStreamState, item gjson.Result, arguments string) []byte {
	name := restoreToolName(s.reverseToolNames, item.Get("name").String())
	if state.Custom {
		call := []byte(`{"index":0,"id":"","type":"custom","custom":{"name":"","input":""}}`)
		call, _ = sjson.SetBytes(call, "index", state.Index)
		call, _ = sjson.SetBytes(call, "id", item.Get("call_id").String())
		call, _ = sjson.SetBytes(call, "custom.name", name)
		call, _ = sjson.SetBytes(call, "custom.input", arguments)
		return call
	}
	call := []byte(`{"index":0,"id":"","type":"function","function":{"name":"","arguments":""}}`)
	call, _ = sjson.SetBytes(call, "index", state.Index)
	call, _ = sjson.SetBytes(call, "id", item.Get("call_id").String())
	call, _ = sjson.SetBytes(call, "function.name", name)
	call, _ = sjson.SetBytes(call, "function.arguments", arguments)
	return call
}

func toolCallArgumentsDelta(state *toolCallStreamState, arguments string) []byte {
	if state.Custom {
		call := []byte(`{"index":0,"custom":{"input":""}}`)
		call, _ = sjson.SetBytes(call, "index", state.Index)
		call, _ = sjson.SetBytes(call, "custom.input", arguments)
		return call
	}
	call := []byte(`{"index":0,"function":{"arguments":""}}`)
	call, _ = sjson.SetBytes(call, "index", state.Index)
	call, _ = sjson.SetBytes(call, "function.arguments", arguments)
	return call
}

func (s *StreamConverter) registerToolCallState(event, item gjson.Result, state *toolCallStreamState) {
	if itemID := event.Get("item_id").String(); itemID != "" {
		s.toolCallStates["item:"+itemID] = state
	}
	if itemID := item.Get("id").String(); itemID != "" {
		s.toolCallStates["item:"+itemID] = state
	}
	if outputIndex := event.Get("output_index"); outputIndex.Exists() {
		s.toolCallStates["output:"+outputIndex.Raw] = state
	}
	s.currentToolCall = state
}

func (s *StreamConverter) findToolCallState(event, item gjson.Result) *toolCallStreamState {
	if itemID := event.Get("item_id").String(); itemID != "" {
		if state := s.toolCallStates["item:"+itemID]; state != nil {
			return state
		}
	}
	if itemID := item.Get("id").String(); itemID != "" {
		if state := s.toolCallStates["item:"+itemID]; state != nil {
			return state
		}
	}
	if outputIndex := event.Get("output_index"); outputIndex.Exists() {
		if state := s.toolCallStates["output:"+outputIndex.Raw]; state != nil {
			return state
		}
	}
	return s.currentToolCall
}

// ConvertResponse converts a non-streaming Responses API object into a chat.completion
// object. originalRequest is the Chat Completions request body the client sent. A
// failed response is returned as an *UpstreamError carrying a Chat Completions error body.
func ConvertResponse(originalRequest, responseJSON []byte, opts Options) ([]byte, error) {
	if !gjson.ValidBytes(responseJSON) {
		return nil, &UpstreamError{Body: upstreamErrorPayload(gjson.Parse(`{"message":"upstream returned an invalid responses API body"}`))}
	}
	responseResult := gjson.ParseBytes(responseJSON)
	if eventType := responseResult.Get("type").String(); eventType == "response.completed" || eventType == "response.incomplete" {
		responseResult = responseResult.Get("response")
	}
	if responseResult.Get("status").String() == "failed" {
		return nil, &UpstreamError{Body: upstreamErrorPayload(responseResult.Get("error"))}
	}

	requestResult := gjson.ParseBytes(originalRequest)
	reverseToolNames := buildReverseMapFromOriginalOpenAI(originalRequest)

	template := []byte(`{"id":"","object":"chat.completion","created":0,"model":"","choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":null},"logprobs":null,"finish_reason":null}]}`)
	template, _ = sjson.SetBytes(template, "id", responseResult.Get("id").String())
	if createdAt := responseResult.Get("created_at"); createdAt.Exists() {
		template, _ = sjson.SetBytes(template, "created", createdAt.Int())
	} else {
		template, _ = sjson.SetBytes(template, "created", time.Now().Unix())
	}
	if model := responseResult.Get("model"); model.Exists() {
		template, _ = sjson.SetBytes(template, "model", model.String())
	} else {
		template, _ = sjson.SetBytes(template, "model", requestResult.Get("model").String())
	}
	if tier := responseServiceTier(responseResult); tier != "" {
		template, _ = sjson.SetBytes(template, "service_tier", tier)
	}
	if usage := responseResult.Get("usage"); usage.Exists() {
		template, _ = sjson.SetRawBytes(template, "usage", chatUsageFromResponses(usage))
	}

	// Process the output array for content and tool calls.
	var toolCalls [][]byte
	var contentText, refusalText strings.Builder
	var reasoningParts []string
	var annotations, logprobs [][]byte
	annotationKeys := make(map[string]struct{})
	var contentRuneOffset int64
	hasContent := false

	for _, outputItem := range responseResult.Get("output").Array() {
		switch outputItem.Get("type").String() {
		case "reasoning":
			if !opts.ReasoningSummary {
				continue
			}
			for _, summaryItem := range outputItem.Get("summary").Array() {
				if summaryItem.Get("type").String() == "summary_text" && summaryItem.Get("text").String() != "" {
					reasoningParts = append(reasoningParts, summaryItem.Get("text").String())
				}
			}
			for _, contentItem := range outputItem.Get("content").Array() {
				if contentItem.Get("type").String() == "reasoning_text" && contentItem.Get("text").String() != "" {
					reasoningParts = append(reasoningParts, contentItem.Get("text").String())
				}
			}
		case "message":
			for _, contentItem := range outputItem.Get("content").Array() {
				switch contentItem.Get("type").String() {
				case "output_text":
					text := contentItem.Get("text").String()
					hasContent = true
					contentText.WriteString(text)
					annotations = append(annotations, buildURLCitations(
						annotationResults(contentItem.Get("annotations")),
						contentRuneOffset,
						annotationKeys,
					)...)
					contentRuneOffset += int64(utf8.RuneCountInString(text))
					for _, logprob := range contentItem.Get("logprobs").Array() {
						logprobs = append(logprobs, []byte(logprob.Raw))
					}
				case "refusal":
					refusalText.WriteString(contentItem.Get("refusal").String())
				}
			}
		case "function_call", "custom_tool_call":
			name := restoreToolName(reverseToolNames, outputItem.Get("name").String())
			if outputItem.Get("type").String() == "custom_tool_call" {
				call := []byte(`{"id":"","type":"custom","custom":{"name":"","input":""}}`)
				call, _ = sjson.SetBytes(call, "id", outputItem.Get("call_id").String())
				call, _ = sjson.SetBytes(call, "custom.name", name)
				call, _ = sjson.SetBytes(call, "custom.input", toolCallArguments(outputItem))
				toolCalls = append(toolCalls, call)
				continue
			}
			call := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)
			call, _ = sjson.SetBytes(call, "id", outputItem.Get("call_id").String())
			call, _ = sjson.SetBytes(call, "function.name", name)
			call, _ = sjson.SetBytes(call, "function.arguments", toolCallArguments(outputItem))
			toolCalls = append(toolCalls, call)
		}
	}

	if hasContent {
		template, _ = sjson.SetBytes(template, "choices.0.message.content", contentText.String())
	}
	if refusalText.Len() > 0 {
		template, _ = sjson.SetBytes(template, "choices.0.message.refusal", refusalText.String())
	}
	if len(reasoningParts) > 0 {
		template, _ = sjson.SetBytes(template, "choices.0.message.reasoning_content", strings.Join(reasoningParts, "\n\n"))
	}
	if len(annotations) > 0 {
		template, _ = sjson.SetRawBytes(template, "choices.0.message.annotations", joinRawArray(annotations))
	}
	if len(logprobs) > 0 || requestResult.Get("logprobs").Bool() {
		template, _ = sjson.SetRawBytes(template, "choices.0.logprobs", []byte(`{"content":[]}`))
		template, _ = sjson.SetRawBytes(template, "choices.0.logprobs.content", joinRawArray(logprobs))
	}

	finishReason := "stop"
	switch responseResult.Get("status").String() {
	case "incomplete":
		switch responseResult.Get("incomplete_details.reason").String() {
		case "max_tokens", "max_output_tokens":
			finishReason = "length"
		case "content_filter":
			finishReason = "content_filter"
		}
	default:
		if len(toolCalls) > 0 {
			finishReason = "tool_calls"
		}
	}

	if usesLegacyFunctions(requestResult) && len(toolCalls) > 0 {
		// Legacy function calling returns a single function_call instead of tool_calls.
		call := gjson.ParseBytes(toolCalls[0])
		functionCall := []byte(`{"name":"","arguments":""}`)
		functionCall, _ = sjson.SetBytes(functionCall, "name", call.Get("function.name").String())
		functionCall, _ = sjson.SetBytes(functionCall, "arguments", call.Get("function.arguments").String())
		template, _ = sjson.SetRawBytes(template, "choices.0.message.function_call", functionCall)
		if finishReason == "tool_calls" {
			finishReason = "function_call"
		}
	} else if len(toolCalls) > 0 {
		template, _ = sjson.SetRawBytes(template, "choices.0.message.tool_calls", joinRawArray(toolCalls))
	}
	template, _ = sjson.SetBytes(template, "choices.0.finish_reason", finishReason)

	return template, nil
}

// usesLegacyFunctions reports whether the client used the deprecated functions field instead of tools.
func usesLegacyFunctions(request gjson.Result) bool {
	return request.Get("functions").IsArray() && !request.Get("tools").Exists()
}

// legacyFunctionChunk rewrites a tool_calls chunk into the legacy function_call shape.
func legacyFunctionChunk(chunk []byte) []byte {
	if gjson.GetBytes(chunk, "choices.0.finish_reason").String() == "tool_calls" {
		chunk, _ = sjson.SetBytes(chunk, "choices.0.finish_reason", "function_call")
	}
	calls := gjson.GetBytes(chunk, "choices.0.delta.tool_calls")
	if !calls.Exists() {
		return chunk
	}
	chunk, _ = sjson.DeleteBytes(chunk, "choices.0.delta.tool_calls")
	call := calls.Get("0")
	// Legacy function calling returns at most one call.
	if call.Get("index").Int() != 0 || call.Get("type").String() == "custom" || call.Get("custom").Exists() {
		return chunk
	}
	functionCall := []byte(`{}`)
	if name := call.Get("function.name"); name.Exists() {
		functionCall, _ = sjson.SetBytes(functionCall, "name", name.String())
	}
	functionCall, _ = sjson.SetBytes(functionCall, "arguments", call.Get("function.arguments").String())
	chunk, _ = sjson.SetRawBytes(chunk, "choices.0.delta.function_call", functionCall)
	return chunk
}

// upstreamErrorPayload builds a Chat Completions error body from a Responses error object or error event.
func upstreamErrorPayload(errResult gjson.Result) []byte {
	payload := []byte(`{"error":{"message":"","type":"server_error","param":null,"code":null}}`)
	message := errResult.Get("message").String()
	if message == "" {
		message = "upstream responses API request failed"
	}
	payload, _ = sjson.SetBytes(payload, "error.message", message)
	if code := errResult.Get("code"); code.Exists() && code.Type != gjson.Null {
		payload, _ = sjson.SetRawBytes(payload, "error.code", []byte(code.Raw))
	}
	if param := errResult.Get("param"); param.Exists() && param.Type != gjson.Null {
		payload, _ = sjson.SetRawBytes(payload, "error.param", []byte(param.Raw))
	}
	return payload
}

func annotationResults(value gjson.Result) []gjson.Result {
	if !value.Exists() {
		return nil
	}
	if value.IsArray() {
		return value.Array()
	}
	return []gjson.Result{value}
}

func annotationsFromEvent(event gjson.Result) []gjson.Result {
	annotations := make([]gjson.Result, 0)
	for _, path := range []string{"annotation", "annotations", "part.annotations"} {
		annotations = append(annotations, annotationResults(event.Get(path))...)
	}
	item := event.Get("item")
	if item.Exists() {
		annotations = append(annotations, annotationResults(item.Get("annotations"))...)
		for _, contentItem := range item.Get("content").Array() {
			annotations = append(annotations, annotationResults(contentItem.Get("annotations"))...)
		}
	}
	return annotations
}

// buildURLCitations converts Responses url_citation annotations into Chat Completions
// annotations, shifting indexes by runeOffset and skipping ones already in seen.
func buildURLCitations(annotations []gjson.Result, runeOffset int64, seen map[string]struct{}) [][]byte {
	citations := make([][]byte, 0, len(annotations))
	for _, annotation := range annotations {
		if annotation.Get("type").String() != "url_citation" {
			continue
		}

		rawStartIndex := annotation.Get("start_index").Int()
		rawEndIndex := annotation.Get("end_index").Int()
		startIndex := rawStartIndex + runeOffset
		endIndex := rawEndIndex + runeOffset
		if startIndex < 0 || endIndex < startIndex {
			continue
		}

		url := annotation.Get("url").String()
		key := url + "\x00" + strconv.FormatInt(rawStartIndex, 10)
		if url == "" {
			key = annotation.Get("id").String() + "\x00" + strconv.FormatInt(rawStartIndex, 10)
		}
		keys := []string{key}
		if id := annotation.Get("id").String(); id != "" {
			keys = append(keys, "id\x00"+id)
		}
		if seen != nil {
			duplicate := false
			for _, key := range keys {
				if _, exists := seen[key]; exists {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
			for _, key := range keys {
				seen[key] = struct{}{}
			}
		}

		citation := []byte(`{"type":"url_citation","url_citation":{"url":"","title":"","start_index":0,"end_index":0}}`)
		citation, _ = sjson.SetBytes(citation, "url_citation.url", url)
		citation, _ = sjson.SetBytes(citation, "url_citation.title", annotation.Get("title").String())
		citation, _ = sjson.SetBytes(citation, "url_citation.start_index", startIndex)
		citation, _ = sjson.SetBytes(citation, "url_citation.end_index", endIndex)
		citations = append(citations, citation)
	}
	return citations
}

func isToolCallType(itemType string) bool {
	return itemType == "function_call" || itemType == "custom_tool_call"
}

func toolCallArguments(item gjson.Result) string {
	if item.Get("type").String() == "custom_tool_call" {
		return item.Get("input").String()
	}
	return item.Get("arguments").String()
}

// buildReverseMapFromOriginalOpenAI builds a map of shortened tool name -> original tool name
// from the original Chat Completions request using the same shortening logic.
func buildReverseMapFromOriginalOpenAI(original []byte) map[string]string {
	rev := map[string]string{}
	names := collectRequestToolNames(normalizeLegacyFunctions(original))
	if len(names) > 0 {
		for orig, short := range buildShortNameMap(names) {
			rev[short] = orig
		}
	}
	return rev
}

func restoreToolName(reverseToolNames map[string]string, name string) string {
	if orig, ok := reverseToolNames[name]; ok {
		return orig
	}
	return name
}

// responseServiceTier returns only an actual nonempty upstream tier.
func responseServiceTier(response gjson.Result) string {
	tier := response.Get("service_tier")
	if tier.Type != gjson.String {
		return ""
	}
	return strings.TrimSpace(tier.Str)
}
