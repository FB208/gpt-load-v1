// Portions of this file are adapted from CLIProxyAPI
// (https://github.com/router-for-me/CLIProxyAPI, internal/translator/codex/openai/chat-completions),
// Copyright (c) 2025.9-present Router-For.ME, licensed under the MIT License.

// Package openai_responses converts OpenAI Chat Completions requests into
// Responses API requests, and Responses API results back into Chat Completions
// results, so Chat Completions clients can use models that require /v1/responses.
package openai_responses

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Options controls optional behaviour of the conversion.
type Options struct {
	// ReasoningSummary requests reasoning summaries upstream and returns them as reasoning_content.
	ReasoningSummary bool
}

// RequestError reports a Chat Completions request that cannot be expressed in the Responses API.
type RequestError struct {
	Message string
}

func (e *RequestError) Error() string {
	return e.Message
}

// passthroughFields keep the same name and meaning in both APIs.
var passthroughFields = []string{
	"temperature",
	"top_p",
	"metadata",
	"user",
	"safety_identifier",
	"prompt_cache_key",
	"prompt_cache_retention",
	"service_tier",
	"parallel_tool_calls",
}

// handledFields are the top-level Chat Completions fields the conversion consumes.
// Any other field has no Responses API equivalent and is dropped.
var handledFields = map[string]struct{}{
	"model":                 {},
	"messages":              {},
	"stream":                {},
	"stream_options":        {},
	"n":                     {},
	"max_tokens":            {},
	"max_completion_tokens": {},
	"reasoning_effort":      {},
	"verbosity":             {},
	"text":                  {},
	"response_format":       {},
	"tools":                 {},
	"tool_choice":           {},
	"functions":             {},
	"function_call":         {},
	"web_search_options":    {},
	"logprobs":              {},
	"top_logprobs":          {},
	"store":                 {},
	"modalities":            {},
}

func init() {
	for _, field := range passthroughFields {
		handledFields[field] = struct{}{}
	}
}

// ConvertChatRequest converts a Chat Completions request body into a Responses API
// request body. It also returns the top-level Chat fields that were dropped because
// the Responses API has no equivalent for them.
func ConvertChatRequest(rawJSON []byte, opts Options) ([]byte, []string, error) {
	if !gjson.ValidBytes(rawJSON) {
		return nil, nil, &RequestError{Message: "request body is not valid JSON"}
	}
	root := gjson.ParseBytes(rawJSON)
	if !root.IsObject() {
		return nil, nil, &RequestError{Message: "request body must be a JSON object"}
	}
	if n := root.Get("n"); n.Exists() && n.Int() > 1 {
		return nil, nil, &RequestError{Message: "n>1 is not supported when chat completions are served by the responses API"}
	}
	dropped := droppedFields(root)

	rawJSON = normalizeLegacyFunctions(rawJSON)
	root = gjson.ParseBytes(rawJSON)
	tools := root.Get("tools")
	toolResults := tools.Array()

	out := []byte(`{}`)
	if model := root.Get("model"); model.Exists() {
		out, _ = sjson.SetRawBytes(out, "model", []byte(model.Raw))
	}
	stream := root.Get("stream").Bool()
	out, _ = sjson.SetBytes(out, "stream", stream)
	if v := root.Get("stream_options.include_obfuscation"); stream && v.Exists() && v.Type != gjson.Null {
		out, _ = sjson.SetRawBytes(out, "stream_options.include_obfuscation", []byte(v.Raw))
	}

	for _, field := range passthroughFields {
		if v := root.Get(field); v.Exists() && v.Type != gjson.Null {
			out, _ = sjson.SetRawBytes(out, field, []byte(v.Raw))
		}
	}

	// Map token limits; max_completion_tokens supersedes the deprecated max_tokens.
	if v := root.Get("max_completion_tokens"); v.Exists() && v.Type != gjson.Null {
		out, _ = sjson.SetRawBytes(out, "max_output_tokens", []byte(v.Raw))
	} else if v := root.Get("max_tokens"); v.Exists() && v.Type != gjson.Null {
		out, _ = sjson.SetRawBytes(out, "max_output_tokens", []byte(v.Raw))
	}

	// Map reasoning effort only when the client asked for it, as Chat Completions does.
	effort := root.Get("reasoning_effort")
	if effort.Exists() && effort.Type != gjson.Null {
		out, _ = sjson.SetRawBytes(out, "reasoning.effort", []byte(effort.Raw))
	}
	if opts.ReasoningSummary && effort.String() != "none" {
		out, _ = sjson.SetBytes(out, "reasoning.summary", "auto")
	}

	if root.Get("logprobs").Bool() {
		out, _ = sjson.SetBytes(out, "include", []string{"message.output_text.logprobs"})
		if v := root.Get("top_logprobs"); v.Exists() && v.Type != gjson.Null {
			out, _ = sjson.SetRawBytes(out, "top_logprobs", []byte(v.Raw))
		}
	}

	// Build request-local tool metadata and name shortening map.
	originalToolNameMap := map[string]string{}
	customToolNames := map[string]struct{}{}
	functionToolNames := map[string]struct{}{}
	{
		if tools.IsArray() && len(toolResults) > 0 {
			for _, tool := range toolResults {
				switch tool.Get("type").String() {
				case "function":
					functionToolNames[tool.Get("function.name").String()] = struct{}{}
				case "custom":
					customToolNames[customToolName(tool)] = struct{}{}
				}
			}
			// A normalized function envelope cannot disambiguate declarations that share a name.
			// Preserve function behavior for such ambiguous names.
			for name := range functionToolNames {
				delete(customToolNames, name)
			}
		}
		allNames := collectRequestToolNames(rawJSON)
		if len(allNames) > 0 {
			originalToolNameMap = buildShortNameMap(allNames)
		}
	}
	shortName := func(name string) string {
		if short, ok := originalToolNameMap[name]; ok {
			return short
		}
		return shortenNameIfNeeded(name)
	}

	resolveToolCall := func(toolCall gjson.Result) (callType, name, input string, valid bool) {
		switch toolCall.Get("type").String() {
		case "custom":
			return "custom", toolCall.Get("custom.name").String(), toolCall.Get("custom.input").String(), true
		case "function":
			name = toolCall.Get("function.name").String()
			callType = "function"
			if _, custom := customToolNames[name]; custom {
				callType = "custom"
			}
			return callType, name, toolCall.Get("function.arguments").String(), true
		default:
			return "", "", "", false
		}
	}

	messages := root.Get("messages")
	if !messages.IsArray() {
		return nil, nil, &RequestError{Message: "messages must be an array"}
	}
	type pendingToolCall struct {
		callID       string
		sourceCallID string
		callType     string
		consumed     bool
	}
	var pendingToolCalls []pendingToolCall
	var drops userTurnDrops
	ambiguousToolCallIDs := map[string]struct{}{}

	// Build input from messages, handling all message types including tool calls.
	arr := messages.Array()
	inputItems := make([][]byte, 0, len(arr))
	for i := 0; i < len(arr); i++ {
		m := arr[i]
		role := m.Get("role").String()

		switch role {
		case "tool":
			// Handle tool response messages as top-level tool call output objects.
			toolCallID := m.Get("tool_call_id").String()
			if _, ambiguous := ambiguousToolCallIDs[toolCallID]; toolCallID != "" && ambiguous {
				continue
			}

			pendingIndex := -1
			for index := range pendingToolCalls {
				pendingCall := &pendingToolCalls[index]
				if pendingCall.consumed {
					continue
				}
				if toolCallID == "" || pendingCall.sourceCallID == toolCallID || pendingCall.callID == toolCallID {
					pendingIndex = index
					break
				}
			}

			if pendingIndex < 0 {
				continue
			}
			pendingCall := &pendingToolCalls[pendingIndex]
			pendingCall.consumed = true
			outputType := "function_call_output"
			if pendingCall.callType == "custom" {
				outputType = "custom_tool_call_output"
			}

			toolOutput := []byte(`{}`)
			toolOutput, _ = sjson.SetBytes(toolOutput, "type", outputType)
			toolOutput, _ = sjson.SetBytes(toolOutput, "call_id", pendingCall.callID)
			toolOutput = setToolCallOutputContent(toolOutput, m.Get("content"))
			inputItems = append(inputItems, toolOutput)

		default:
			// A new conversational message starts a new tool-call batch.
			pendingToolCalls = nil
			ambiguousToolCallIDs = map[string]struct{}{}

			msg := []byte(`{}`)
			msg, _ = sjson.SetBytes(msg, "type", "message")
			if role == "system" {
				msg, _ = sjson.SetBytes(msg, "role", "developer")
			} else {
				msg, _ = sjson.SetBytes(msg, "role", role)
			}

			contentItems := make([][]byte, 0, 4)
			// Counts the parts this user turn really sends; an empty text part does not.
			turnSendable := 0
			textPartType := "input_text"
			if role == "assistant" {
				textPartType = "output_text"
			}

			c := m.Get("content")
			if c.Exists() && c.Type == gjson.String && c.String() != "" {
				turnSendable++
				part := []byte(`{}`)
				part, _ = sjson.SetBytes(part, "type", textPartType)
				part, _ = sjson.SetBytes(part, "text", c.String())
				contentItems = append(contentItems, part)
			} else if c.Exists() && c.IsArray() {
				for _, it := range c.Array() {
					t := it.Get("type").String()
					switch t {
					case "text":
						part := []byte(`{}`)
						part, _ = sjson.SetBytes(part, "type", textPartType)
						part, _ = sjson.SetBytes(part, "text", it.Get("text").String())
						contentItems = append(contentItems, part)
						if it.Get("text").String() != "" {
							turnSendable++
						}
					case "refusal":
						if role == "assistant" {
							part := []byte(`{"type":"refusal"}`)
							part, _ = sjson.SetBytes(part, "refusal", it.Get("refusal").String())
							contentItems = append(contentItems, part)
						}
					case "image_url":
						// Map image inputs to input_image for Responses API.
						if role == "user" {
							part := []byte(`{}`)
							part, _ = sjson.SetBytes(part, "type", "input_image")
							if u := it.Get("image_url.url"); u.Exists() {
								part, _ = sjson.SetBytes(part, "image_url", u.String())
							} else if u := it.Get("image_url"); u.Type == gjson.String {
								part, _ = sjson.SetBytes(part, "image_url", u.String())
							}
							if detail := it.Get("image_url.detail"); detail.Type == gjson.String && detail.String() != "" {
								part, _ = sjson.SetBytes(part, "detail", detail.String())
							}
							contentItems = append(contentItems, part)
							turnSendable++
						}
					case "file":
						if role == "user" {
							if part, ok := inputFilePart(it); ok {
								contentItems = append(contentItems, part)
								turnSendable++
							} else {
								drops.drop(t)
							}
						}
					case "input_audio":
						if role == "user" {
							audioData := it.Get("input_audio.data").String()
							audioFormat := it.Get("input_audio.format").String()
							if audioData != "" {
								part := []byte(`{}`)
								part, _ = sjson.SetBytes(part, "type", "input_audio")
								part, _ = sjson.SetBytes(part, "data", audioData)
								if audioFormat != "" {
									part, _ = sjson.SetBytes(part, "format", audioFormat)
								}
								contentItems = append(contentItems, part)
								turnSendable++
							} else {
								drops.drop(t)
							}
						}
					}
				}
			}
			if role == "user" {
				drops.endTurn(turnSendable)
			}

			// Don't emit empty assistant messages when only tool_calls are present;
			// the Responses API needs function_call items directly, otherwise call_id matching fails.
			if role != "assistant" || len(contentItems) > 0 {
				msg, _ = sjson.SetRawBytes(msg, "content", joinRawArray(contentItems))
				inputItems = append(inputItems, msg)
			}

			// Handle tool calls for assistant messages as separate top-level objects.
			if role != "assistant" {
				continue
			}
			toolCalls := m.Get("tool_calls")
			if !toolCalls.IsArray() {
				continue
			}
			toolCallsArr := toolCalls.Array()
			callIDCounts := map[string]int{}
			usedCallIDs := map[string]struct{}{}
			for _, tc := range toolCallsArr {
				_, _, _, valid := resolveToolCall(tc)
				callID := tc.Get("id").String()
				if valid && callID != "" {
					callIDCounts[callID]++
					usedCallIDs[callID] = struct{}{}
				}
			}
			for callID, count := range callIDCounts {
				if count > 1 {
					ambiguousToolCallIDs[callID] = struct{}{}
				}
			}

			for j := 0; j < len(toolCallsArr); j++ {
				tc := toolCallsArr[j]
				toolCallType, toolCallName, toolCallInput, valid := resolveToolCall(tc)
				if !valid {
					continue
				}
				sourceCallID := tc.Get("id").String()
				if _, ambiguous := ambiguousToolCallIDs[sourceCallID]; sourceCallID != "" && ambiguous {
					continue
				}
				callID := sourceCallID
				if callID == "" {
					baseCallID := "call_missing_" + strconv.Itoa(i) + "_" + strconv.Itoa(j)
					callID = baseCallID
					for suffix := 1; ; suffix++ {
						if _, used := usedCallIDs[callID]; !used {
							break
						}
						callID = baseCallID + "_" + strconv.Itoa(suffix)
					}
					usedCallIDs[callID] = struct{}{}
				}
				// The Responses API rejects call_id values longer than 64 characters.
				if shortID := shortenCallIDIfNeeded(callID); shortID != callID {
					for attempt := 1; ; attempt++ {
						if _, used := usedCallIDs[shortID]; !used {
							break
						}
						shortID = shortenCallIDIfNeeded(callID + "_" + strconv.Itoa(attempt))
					}
					usedCallIDs[shortID] = struct{}{}
					callID = shortID
				}
				pendingToolCalls = append(pendingToolCalls, pendingToolCall{
					callID:       callID,
					sourceCallID: sourceCallID,
					callType:     toolCallType,
				})

				switch toolCallType {
				case "function":
					funcCall := []byte(`{}`)
					funcCall, _ = sjson.SetBytes(funcCall, "type", "function_call")
					funcCall, _ = sjson.SetBytes(funcCall, "call_id", callID)
					funcCall, _ = sjson.SetBytes(funcCall, "name", shortName(toolCallName))
					funcCall, _ = sjson.SetBytes(funcCall, "arguments", toolCallInput)
					inputItems = append(inputItems, funcCall)
				case "custom":
					customCall := []byte(`{}`)
					customCall, _ = sjson.SetBytes(customCall, "type", "custom_tool_call")
					customCall, _ = sjson.SetBytes(customCall, "call_id", callID)
					customCall, _ = sjson.SetBytes(customCall, "name", shortName(toolCallName))
					customCall, _ = sjson.SetBytes(customCall, "input", toolCallInput)
					inputItems = append(inputItems, customCall)
				}
			}
		}
	}
	out, _ = sjson.SetRawBytes(out, "input", []byte(`[]`))
	out = setRawArrayItems(out, "input", inputItems)

	// Map response_format and verbosity to Responses API text settings.
	if rf := root.Get("response_format"); rf.Exists() {
		switch rf.Get("type").String() {
		case "text":
			out, _ = sjson.SetBytes(out, "text.format.type", "text")
		case "json_object":
			out, _ = sjson.SetBytes(out, "text.format.type", "json_object")
		case "json_schema":
			if js := rf.Get("json_schema"); js.Exists() {
				out, _ = sjson.SetBytes(out, "text.format.type", "json_schema")
				for _, key := range []string{"name", "description", "strict"} {
					if v := js.Get(key); v.Exists() {
						out, _ = sjson.SetRawBytes(out, "text.format."+key, []byte(v.Raw))
					}
				}
				if v := js.Get("schema"); v.Exists() {
					out, _ = sjson.SetRawBytes(out, "text.format.schema", []byte(v.Raw))
				}
			}
		}
	}
	verbosity := root.Get("verbosity")
	if !verbosity.Exists() {
		verbosity = root.Get("text.verbosity")
	}
	if verbosity.Exists() && verbosity.Type != gjson.Null {
		out, _ = sjson.SetRawBytes(out, "text.verbosity", []byte(verbosity.Raw))
	}

	// Map tools (flatten function and custom fields).
	toolItems := make([][]byte, 0, len(toolResults)+1)
	for _, t := range toolResults {
		toolType := t.Get("type").String()
		switch {
		case toolType == "custom":
			item := []byte(`{"type":"custom"}`)
			if custom := t.Get("custom"); custom.IsObject() {
				custom.ForEach(func(key, value gjson.Result) bool {
					item, _ = sjson.SetRawBytes(item, key.String(), []byte(value.Raw))
					return true
				})
			} else {
				item = []byte(t.Raw)
			}
			item, _ = sjson.SetBytes(item, "name", shortName(customToolName(t)))
			toolItems = append(toolItems, item)
		case toolType == "function":
			item := []byte(`{}`)
			item, _ = sjson.SetBytes(item, "type", "function")
			fn := t.Get("function")
			if fn.Exists() {
				if v := fn.Get("name"); v.Exists() {
					item, _ = sjson.SetBytes(item, "name", shortName(v.String()))
				}
				if v := fn.Get("description"); v.Exists() {
					item, _ = sjson.SetRawBytes(item, "description", []byte(v.Raw))
				}
				if v := fn.Get("parameters"); v.Exists() {
					item, _ = sjson.SetRawBytes(item, "parameters", []byte(v.Raw))
				}
				if v := fn.Get("strict"); v.Exists() && v.Type != gjson.Null {
					item, _ = sjson.SetRawBytes(item, "strict", []byte(v.Raw))
				} else {
					// Chat Completions defaults strict to false while the Responses API
					// defaults it to true, so an omitted value must be forwarded explicitly.
					item, _ = sjson.SetBytes(item, "strict", false)
				}
			}
			toolItems = append(toolItems, item)
		case toolType != "" && t.IsObject():
			// Pass through built-in tools (e.g. {"type":"web_search"}) directly for the Responses API.
			toolItems = append(toolItems, []byte(t.Raw))
		}
	}
	if wso := root.Get("web_search_options"); wso.IsObject() {
		toolItems = append(toolItems, webSearchTool(wso))
	}
	if len(toolItems) > 0 {
		out, _ = sjson.SetRawBytes(out, "tools", joinRawArray(toolItems))
	}

	// Map tool_choice when present.
	// Chat Completions: a string ("auto"/"none"/"required") or an object such as
	// {"type":"function","function":{"name":"..."}}.
	// Responses API: keep built-in tool choices as-is and flatten named choices to {"type":"...","name":"..."}.
	if tc := root.Get("tool_choice"); tc.Exists() {
		switch {
		case tc.Type == gjson.String:
			out, _ = sjson.SetBytes(out, "tool_choice", tc.String())
		case tc.IsObject():
			tcType := tc.Get("type").String()
			switch tcType {
			case "function", "custom":
				name := ""
				if tcType == "function" {
					name = tc.Get("function.name").String()
					if name == "" {
						name = tc.Get("name").String()
					}
					if _, custom := customToolNames[name]; custom {
						tcType = "custom"
					}
				} else {
					name = customToolName(tc)
				}
				choice := []byte(`{}`)
				choice, _ = sjson.SetBytes(choice, "type", tcType)
				if name != "" {
					choice, _ = sjson.SetBytes(choice, "name", shortName(name))
				}
				out, _ = sjson.SetRawBytes(out, "tool_choice", choice)
			case "allowed_tools":
				out, _ = sjson.SetRawBytes(out, "tool_choice", allowedToolsChoice(tc, shortName))
			case "":
			default:
				// Built-in tool choices (e.g. {"type":"web_search"}) are already Responses-compatible.
				out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(tc.Raw))
			}
		}
	}

	// Chat Completions does not store completions unless asked to, while the
	// Responses API stores responses by default.
	if v := root.Get("store"); v.Exists() && v.Type != gjson.Null {
		out, _ = sjson.SetRawBytes(out, "store", []byte(v.Raw))
	} else {
		out, _ = sjson.SetBytes(out, "store", false)
	}

	if err := drops.err(); err != nil {
		return nil, nil, err
	}
	return out, dropped, nil
}

// droppedFields lists the top-level Chat fields that have no Responses API equivalent.
func droppedFields(root gjson.Result) []string {
	var dropped []string
	root.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		if value.Type == gjson.Null {
			return true
		}
		if name == "modalities" {
			for _, modality := range value.Array() {
				if modality.String() != "text" {
					dropped = append(dropped, name)
					break
				}
			}
			return true
		}
		if _, ok := handledFields[name]; !ok {
			dropped = append(dropped, name)
		}
		return true
	})
	sort.Strings(dropped)
	return dropped
}

// normalizeLegacyFunctions rewrites the deprecated functions/function_call
// fields into their tools/tool_choice equivalents so a single path converts both.
func normalizeLegacyFunctions(rawJSON []byte) []byte {
	root := gjson.ParseBytes(rawJSON)
	if functions := root.Get("functions"); functions.IsArray() && !root.Get("tools").Exists() {
		tools := make([][]byte, 0, len(functions.Array()))
		for _, fn := range functions.Array() {
			tool := []byte(`{"type":"function"}`)
			tool, _ = sjson.SetRawBytes(tool, "function", []byte(fn.Raw))
			tools = append(tools, tool)
		}
		rawJSON, _ = sjson.SetRawBytes(rawJSON, "tools", joinRawArray(tools))
	}
	if fc := root.Get("function_call"); fc.Exists() && !root.Get("tool_choice").Exists() {
		switch {
		case fc.Type == gjson.String:
			rawJSON, _ = sjson.SetBytes(rawJSON, "tool_choice", fc.String())
		case fc.IsObject():
			choice := []byte(`{"type":"function","function":{"name":""}}`)
			choice, _ = sjson.SetBytes(choice, "function.name", fc.Get("name").String())
			rawJSON, _ = sjson.SetRawBytes(rawJSON, "tool_choice", choice)
		}
	}
	for i, msg := range root.Get("messages").Array() {
		path := "messages." + strconv.Itoa(i)
		switch msg.Get("role").String() {
		case "assistant":
			fc := msg.Get("function_call")
			if !fc.IsObject() || msg.Get("tool_calls").Exists() {
				continue
			}
			call := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)
			call, _ = sjson.SetBytes(call, "function.name", fc.Get("name").String())
			call, _ = sjson.SetBytes(call, "function.arguments", fc.Get("arguments").String())
			rawJSON, _ = sjson.SetRawBytes(rawJSON, path+".tool_calls", joinRawArray([][]byte{call}))
		case "function":
			// Legacy function results carry no call id; an empty id pairs with the pending call.
			rawJSON, _ = sjson.SetBytes(rawJSON, path+".role", "tool")
		}
	}
	return rawJSON
}

// customToolName reads a custom tool name in either the Chat ({"custom":{"name"}}) or Responses ({"name"}) shape.
func customToolName(tool gjson.Result) string {
	if name := tool.Get("custom.name").String(); name != "" {
		return name
	}
	return tool.Get("name").String()
}

// allowedToolsChoice flattens a Chat allowed_tools choice into the Responses shape.
func allowedToolsChoice(tc gjson.Result, shortName func(string) string) []byte {
	allowed := tc.Get("allowed_tools")
	if !allowed.IsObject() {
		return []byte(tc.Raw)
	}
	choice := []byte(`{"type":"allowed_tools"}`)
	if mode := allowed.Get("mode"); mode.Exists() {
		choice, _ = sjson.SetRawBytes(choice, "mode", []byte(mode.Raw))
	}
	tools := make([][]byte, 0, len(allowed.Get("tools").Array()))
	for _, tool := range allowed.Get("tools").Array() {
		switch tool.Get("type").String() {
		case "function":
			name := tool.Get("function.name").String()
			if name == "" {
				name = tool.Get("name").String()
			}
			item := []byte(`{"type":"function"}`)
			item, _ = sjson.SetBytes(item, "name", shortName(name))
			tools = append(tools, item)
		case "custom":
			item := []byte(`{"type":"custom"}`)
			item, _ = sjson.SetBytes(item, "name", shortName(customToolName(tool)))
			tools = append(tools, item)
		default:
			tools = append(tools, []byte(tool.Raw))
		}
	}
	choice, _ = sjson.SetRawBytes(choice, "tools", joinRawArray(tools))
	return choice
}

// webSearchTool maps Chat web_search_options onto a Responses web_search tool.
func webSearchTool(options gjson.Result) []byte {
	tool := []byte(`{"type":"web_search"}`)
	if v := options.Get("search_context_size"); v.Exists() && v.Type != gjson.Null {
		tool, _ = sjson.SetRawBytes(tool, "search_context_size", []byte(v.Raw))
	}
	if location := options.Get("user_location"); location.IsObject() {
		approximate := location.Get("approximate")
		mapped := []byte(`{"type":"approximate"}`)
		for _, key := range []string{"country", "region", "city", "timezone"} {
			if v := approximate.Get(key); v.Exists() && v.Type != gjson.Null {
				mapped, _ = sjson.SetRawBytes(mapped, key, []byte(v.Raw))
			}
		}
		tool, _ = sjson.SetRawBytes(tool, "user_location", mapped)
	}
	return tool
}

func setToolCallOutputContent(funcOutput []byte, content gjson.Result) []byte {
	switch {
	case content.Type == gjson.String:
		structuredContent := gjson.Parse(content.String())
		if hasToolOutputImagePart(structuredContent) {
			return setToolCallOutputContent(funcOutput, structuredContent)
		}
		funcOutput, _ = sjson.SetBytes(funcOutput, "output", content.String())
	case content.IsArray():
		outputItems := make([][]byte, 0, 4)
		for _, item := range content.Array() {
			outputItems = append(outputItems, toolOutputContentPart(item))
		}
		funcOutput, _ = sjson.SetRawBytes(funcOutput, "output", joinRawArray(outputItems))
	default:
		fallbackOutput := content.Raw
		if fallbackOutput == "" {
			fallbackOutput = content.String()
		}
		funcOutput, _ = sjson.SetBytes(funcOutput, "output", fallbackOutput)
	}
	return funcOutput
}

func toolOutputContentPart(item gjson.Result) []byte {
	itemType := item.Get("type").String()
	switch itemType {
	case "text", "input_text", "output_text":
		part := []byte(`{}`)
		part, _ = sjson.SetBytes(part, "type", "input_text")
		part, _ = sjson.SetBytes(part, "text", item.Get("text").String())
		return part
	case "image_url", "input_image":
		imageURL := item.Get("image_url.url").String()
		fileID := item.Get("image_url.file_id").String()
		if itemType == "input_image" {
			imageURL = item.Get("image_url").String()
			fileID = item.Get("file_id").String()
		}
		if imageURL == "" && fileID == "" {
			return toolOutputFallbackPart(item)
		}
		part := []byte(`{}`)
		part, _ = sjson.SetBytes(part, "type", "input_image")
		if imageURL != "" {
			part, _ = sjson.SetBytes(part, "image_url", imageURL)
		}
		if fileID != "" {
			part, _ = sjson.SetBytes(part, "file_id", fileID)
		}
		detail := item.Get("image_url.detail").String()
		if itemType == "input_image" {
			detail = item.Get("detail").String()
		}
		if detail != "" {
			part, _ = sjson.SetBytes(part, "detail", detail)
		}
		return part
	case "file":
		if part, ok := inputFilePart(item); ok {
			return part
		}
		return toolOutputFallbackPart(item)
	default:
		return toolOutputFallbackPart(item)
	}
}

// inputFilePart maps a Chat Completions file part onto a Responses input_file
// part. It reports false when the part names no file id, bytes or url.
func inputFilePart(item gjson.Result) ([]byte, bool) {
	fileID := item.Get("file.file_id").String()
	fileData := item.Get("file.file_data").String()
	fileURL := item.Get("file.file_url").String()
	if fileID == "" && fileData == "" && fileURL == "" {
		return nil, false
	}
	part := []byte(`{}`)
	part, _ = sjson.SetBytes(part, "type", "input_file")
	if fileID != "" {
		part, _ = sjson.SetBytes(part, "file_id", fileID)
	}
	if fileData != "" {
		part, _ = sjson.SetBytes(part, "file_data", fileData)
	}
	if fileURL != "" {
		part, _ = sjson.SetBytes(part, "file_url", fileURL)
	}
	if filename := item.Get("file.filename").String(); filename != "" {
		part, _ = sjson.SetBytes(part, "filename", filename)
	}
	return part, true
}

func hasToolOutputImagePart(content gjson.Result) bool {
	if !content.IsArray() {
		return false
	}
	for _, item := range content.Array() {
		switch item.Get("type").String() {
		case "image_url":
			if item.Get("image_url.url").String() != "" || item.Get("image_url.file_id").String() != "" {
				return true
			}
		case "input_image":
			if item.Get("image_url").String() != "" || item.Get("file_id").String() != "" {
				return true
			}
		}
	}
	return false
}

func toolOutputFallbackPart(item gjson.Result) []byte {
	text := item.Raw
	if text == "" {
		text = item.String()
	}
	part := []byte(`{}`)
	part, _ = sjson.SetBytes(part, "type", "input_text")
	part, _ = sjson.SetBytes(part, "text", text)
	return part
}

// sanitizeToolName replaces any character outside [a-zA-Z0-9_-] with an underscore.
func sanitizeToolName(name string) string {
	if name == "" {
		return ""
	}
	var sb strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

// shortenNameIfNeeded normalizes invalid characters and applies the shortening rule for a single name.
// If the name length exceeds 64, it will try to preserve the "mcp__" prefix and last segment.
// Otherwise it truncates to 64 characters.
func shortenNameIfNeeded(name string) string {
	const limit = 64
	sanitized := sanitizeToolName(name)
	if len(sanitized) <= limit {
		return sanitized
	}
	if strings.HasPrefix(sanitized, "mcp__") {
		// Keep prefix and last segment after '__'.
		idx := strings.LastIndex(sanitized, "__")
		if idx > 0 {
			candidate := "mcp__" + sanitized[idx+2:]
			if len(candidate) > limit {
				return candidate[:limit]
			}
			return candidate
		}
	}
	return sanitized[:limit]
}

// collectRequestToolNames extracts unique tool names across tools declarations,
// tool_choice, and historical assistant tool_calls in a deterministic order.
func collectRequestToolNames(rawJSON []byte) []string {
	var names []string
	seen := map[string]struct{}{}
	addName := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; !ok {
			names = append(names, name)
			seen[name] = struct{}{}
		}
	}

	// 1. tools declarations
	tools := gjson.GetBytes(rawJSON, "tools")
	if tools.IsArray() {
		for _, tool := range tools.Array() {
			switch tool.Get("type").String() {
			case "function":
				addName(tool.Get("function.name").String())
			case "custom":
				addName(customToolName(tool))
			}
		}
	}

	// 2. tool_choice
	tc := gjson.GetBytes(rawJSON, "tool_choice")
	if tc.IsObject() {
		switch tc.Get("type").String() {
		case "function":
			fnName := tc.Get("function.name").String()
			if fnName == "" {
				fnName = tc.Get("name").String()
			}
			addName(fnName)
		case "custom":
			addName(customToolName(tc))
		}
	}

	// 3. assistant tool_calls in messages
	messages := gjson.GetBytes(rawJSON, "messages")
	if messages.IsArray() {
		for _, msg := range messages.Array() {
			if msg.Get("role").String() != "assistant" {
				continue
			}
			toolCalls := msg.Get("tool_calls")
			if !toolCalls.IsArray() {
				continue
			}
			for _, tc := range toolCalls.Array() {
				fnName := tc.Get("function.name").String()
				if fnName != "" {
					addName(fnName)
				} else {
					addName(tc.Get("custom.name").String())
				}
			}
		}
	}

	return names
}

// buildShortNameMap generates unique short names (<=64) for the given list of names.
// It preserves the "mcp__" prefix with the last segment when possible and ensures uniqueness
// by appending suffixes like "_1", "_2" if needed.
func buildShortNameMap(names []string) map[string]string {
	const limit = 64
	used := map[string]struct{}{}
	m := map[string]string{}

	makeUnique := func(cand string) string {
		if _, ok := used[cand]; !ok {
			return cand
		}
		base := cand
		for i := 1; ; i++ {
			suffix := "_" + strconv.Itoa(i)
			allowed := limit - len(suffix)
			if allowed < 0 {
				allowed = 0
			}
			tmp := base
			if len(tmp) > allowed {
				tmp = tmp[:allowed]
			}
			tmp = tmp + suffix
			if _, ok := used[tmp]; !ok {
				return tmp
			}
		}
	}

	for _, n := range names {
		uniq := makeUnique(shortenNameIfNeeded(n))
		used[uniq] = struct{}{}
		m[n] = uniq
	}
	return m
}

// shortenCallIDIfNeeded keeps client tool call IDs within the Responses API
// call_id limit with a deterministic, low-collision mapping.
func shortenCallIDIfNeeded(id string) string {
	const limit = 64
	if len(id) <= limit {
		return id
	}

	sum := sha256.Sum256([]byte(id))
	suffix := "_" + hex.EncodeToString(sum[:8])
	return id[:limit-len(suffix)] + suffix
}
