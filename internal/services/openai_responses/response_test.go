package openai_responses

import (
	"errors"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// runStream feeds SSE lines through a converter and returns every chunk it emits.
func runStream(t *testing.T, request string, opts Options, events ...string) []gjson.Result {
	t.Helper()
	converter := NewStreamConverter([]byte(request), opts)
	var chunks []gjson.Result
	for _, event := range events {
		for _, line := range []string{"event: ignored", "data: " + event, ""} {
			for _, chunk := range converter.ConvertLine([]byte(line)) {
				if !gjson.ValidBytes(chunk) {
					t.Fatalf("chunk is not valid JSON: %s", chunk)
				}
				chunks = append(chunks, gjson.ParseBytes(chunk))
			}
		}
	}
	for _, chunk := range converter.Finish() {
		chunks = append(chunks, gjson.ParseBytes(chunk))
	}
	return chunks
}

const streamCreated = `{"type":"response.created","response":{"id":"resp_1","created_at":1700000000,"model":"gpt-6-luna","service_tier":"auto"}}`

// TestStreamConverterText verifies text deltas, the first-chunk role, finish reason and usage chunk.
func TestStreamConverterText(t *testing.T) {
	completed := `{"type":"response.completed","response":{"status":"completed","service_tier":"default","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":2}}}}`
	chunks := runStream(t, `{"model":"m","stream":true,"stream_options":{"include_usage":true}}`, Options{},
		streamCreated,
		`{"type":"response.output_text.delta","delta":"Hel"}`,
		`{"type":"response.output_text.delta","delta":"lo"}`,
		completed,
	)
	if len(chunks) != 4 {
		t.Fatalf("got %d chunks, want 4", len(chunks))
	}
	expectJSON(t, chunks[0], "id", `"resp_1"`)
	expectJSON(t, chunks[0], "object", `"chat.completion.chunk"`)
	expectJSON(t, chunks[0], "created", `1700000000`)
	expectJSON(t, chunks[0], "model", `"gpt-6-luna"`)
	expectJSON(t, chunks[0], "choices.0.delta", `{"role":"assistant","content":"Hel"}`)
	expectJSON(t, chunks[1], "choices.0.delta", `{"content":"lo"}`)
	expectJSON(t, chunks[2], "choices.0.finish_reason", `"stop"`)
	// The requested tier echoed by response.created is not reported; the actual tier is.
	if chunks[0].Get("service_tier").Exists() {
		t.Errorf("early chunk reports requested service_tier: %s", chunks[0].Raw)
	}
	expectJSON(t, chunks[2], "service_tier", `"default"`)
	expectJSON(t, chunks[3], "service_tier", `"default"`)
	expectJSON(t, chunks[3], "choices", `[]`)
	expectJSON(t, chunks[3], "usage", `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":2}}`)

	// Without include_usage no usage chunk is sent.
	chunks = runStream(t, `{"model":"m","stream":true}`, Options{}, streamCreated, completed)
	if len(chunks) != 1 || chunks[0].Get("usage").Exists() {
		t.Fatalf("unexpected chunks without include_usage: %v", chunks)
	}
}

// TestStreamConverterToolCalls verifies parallel tool calls with streamed arguments.
func TestStreamConverterToolCalls(t *testing.T) {
	chunks := runStream(t, `{"model":"m","stream":true,"tools":[{"type":"function","function":{"name":"get_weather"}}]}`, Options{},
		streamCreated,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"city\":"}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_2","type":"function_call","call_id":"call_2","name":"get_weather","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"\"a\"}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"arguments":"{\"city\":\"a\"}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"a\"}"}}`,
		// The second call never streams arguments; output_item.done carries them.
		`{"type":"response.output_item.done","output_index":1,"item":{"id":"fc_2","type":"function_call","call_id":"call_2","name":"get_weather","arguments":"{\"city\":\"b\"}"}}`,
		`{"type":"response.completed","response":{"status":"completed"}}`,
	)
	want := []string{
		`{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}`,
		`{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}`,
		`{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"get_weather","arguments":""}}]}`,
		`{"tool_calls":[{"index":0,"function":{"arguments":"\"a\"}"}}]}`,
		`{"tool_calls":[{"index":1,"function":{"arguments":"{\"city\":\"b\"}"}}]}`,
		`{}`,
	}
	if len(chunks) != len(want) {
		t.Fatalf("got %d chunks, want %d", len(chunks), len(want))
	}
	for i, delta := range want {
		expectJSON(t, chunks[i], "choices.0.delta", delta)
	}
	expectJSON(t, chunks[5], "choices.0.finish_reason", `"tool_calls"`)
}

// TestStreamConverterReasoning verifies reasoning summaries are only returned when enabled.
func TestStreamConverterReasoning(t *testing.T) {
	events := []string{
		streamCreated,
		`{"type":"response.reasoning_summary_text.delta","delta":"think"}`,
		`{"type":"response.reasoning_summary_text.done","text":"think"}`,
		`{"type":"response.output_text.delta","delta":"answer"}`,
	}
	chunks := runStream(t, `{"model":"m","stream":true}`, Options{}, events...)
	if len(chunks) != 1 || chunks[0].Get("choices.0.delta.reasoning_content").Exists() {
		t.Fatalf("reasoning leaked while disabled: %v", chunks)
	}
	chunks = runStream(t, `{"model":"m","stream":true}`, Options{ReasoningSummary: true}, events...)
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(chunks))
	}
	expectJSON(t, chunks[0], "choices.0.delta", `{"role":"assistant","reasoning_content":"think"}`)
	expectJSON(t, chunks[1], "choices.0.delta.reasoning_content", `"\n\n"`)
	expectJSON(t, chunks[2], "choices.0.delta.content", `"answer"`)
}

// TestStreamConverterErrorsAndRefusals verifies failure, error and refusal events.
func TestStreamConverterErrorsAndRefusals(t *testing.T) {
	chunks := runStream(t, `{"model":"m","stream":true}`, Options{},
		streamCreated,
		`{"type":"response.refusal.delta","delta":"no"}`,
		`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"boom"}}}`,
		`{"type":"error","code":"rate_limit_exceeded","message":"slow down","param":null}`,
	)
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(chunks))
	}
	expectJSON(t, chunks[0], "choices.0.delta.refusal", `"no"`)
	expectJSON(t, chunks[1], "error", `{"message":"boom","type":"server_error","param":null,"code":"server_error"}`)
	expectJSON(t, chunks[2], "error.code", `"rate_limit_exceeded"`)
}

// TestStreamConverterIncomplete verifies the finish reason for truncated output.
func TestStreamConverterIncomplete(t *testing.T) {
	chunks := runStream(t, `{"model":"m","stream":true}`, Options{},
		`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	expectJSON(t, chunks[0], "choices.0.finish_reason", `"length"`)
}

// TestStreamConverterLegacyFunctions verifies tool calls are returned as function_call for legacy clients.
func TestStreamConverterLegacyFunctions(t *testing.T) {
	chunks := runStream(t, `{"model":"m","stream":true,"functions":[{"name":"lookup"}]}`, Options{},
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{}"}`,
		`{"type":"response.completed","response":{"status":"completed"}}`,
	)
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(chunks))
	}
	expectJSON(t, chunks[0], "choices.0.delta", `{"role":"assistant","function_call":{"name":"lookup","arguments":""}}`)
	expectJSON(t, chunks[1], "choices.0.delta", `{"function_call":{"arguments":"{}"}}`)
	expectJSON(t, chunks[2], "choices.0.finish_reason", `"function_call"`)
}

// TestStreamConverterRestoresToolNames verifies shortened tool names are restored for the client.
func TestStreamConverterRestoresToolNames(t *testing.T) {
	long := "mcp__server__" + strings.Repeat("x", 70)
	request := `{"model":"m","stream":true,"tools":[{"type":"function","function":{"name":"` + long + `"}}]}`
	short := buildShortNameMap([]string{long})[long]
	chunks := runStream(t, request, Options{},
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"`+short+`","arguments":""}}`)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	expectJSON(t, chunks[0], "choices.0.delta.tool_calls.0.function.name", `"`+long+`"`)
}

// TestConvertResponse verifies the non-streaming conversion of text, tool calls, citations and usage.
func TestConvertResponse(t *testing.T) {
	body := `{"id":"resp_1","object":"response","created_at":1700000000,"model":"gpt-6-luna","status":"completed",
		"output":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"a"},{"type":"summary_text","text":"b"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"See site","annotations":[{"type":"url_citation","url":"https://x","title":"X","start_index":4,"end_index":8}]}]},
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{}"},
			{"type":"custom_tool_call","call_id":"call_2","name":"run","input":"ls"}
		],
		"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`
	out, err := ConvertResponse([]byte(`{"model":"m"}`), []byte(body), Options{ReasoningSummary: true})
	if err != nil {
		t.Fatal(err)
	}
	result := gjson.ParseBytes(out)
	expectJSON(t, result, "id", `"resp_1"`)
	expectJSON(t, result, "object", `"chat.completion"`)
	expectJSON(t, result, "created", `1700000000`)
	expectJSON(t, result, "model", `"gpt-6-luna"`)
	expectJSON(t, result, "choices.0.message.content", `"See site"`)
	expectJSON(t, result, "choices.0.message.reasoning_content", `"a\n\nb"`)
	expectJSON(t, result, "choices.0.message.annotations", `[{"type":"url_citation","url_citation":{"url":"https://x","title":"X","start_index":4,"end_index":8}}]`)
	expectJSON(t, result, "choices.0.message.tool_calls", `[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}},{"id":"call_2","type":"custom","custom":{"name":"run","input":"ls"}}]`)
	expectJSON(t, result, "choices.0.finish_reason", `"tool_calls"`)
	expectJSON(t, result, "usage", `{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}`)

	out, err = ConvertResponse([]byte(`{"model":"m"}`), []byte(body), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "choices.0.message.reasoning_content").Exists() {
		t.Error("reasoning_content returned while disabled")
	}
}

// TestConvertResponseStatuses verifies incomplete, failed and legacy function responses.
func TestConvertResponseStatuses(t *testing.T) {
	out, err := ConvertResponse([]byte(`{"model":"m"}`), []byte(`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	expectJSON(t, gjson.ParseBytes(out), "choices.0.finish_reason", `"length"`)

	_, err = ConvertResponse([]byte(`{"model":"m"}`), []byte(`{"status":"failed","error":{"code":"server_error","message":"boom"}}`), Options{})
	var upstreamErr *UpstreamError
	if !errors.As(err, &upstreamErr) || gjson.GetBytes(upstreamErr.Body, "error.message").String() != "boom" {
		t.Fatalf("failed response error = %v", err)
	}

	out, err = ConvertResponse([]byte(`{"model":"m","functions":[{"name":"lookup"}]}`),
		[]byte(`{"status":"completed","output":[{"type":"function_call","call_id":"c","name":"lookup","arguments":"{}"}]}`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	result := gjson.ParseBytes(out)
	expectJSON(t, result, "choices.0.message.function_call", `{"name":"lookup","arguments":"{}"}`)
	expectJSON(t, result, "choices.0.finish_reason", `"function_call"`)
	if result.Get("choices.0.message.tool_calls").Exists() {
		t.Error("legacy response should not carry tool_calls")
	}
}
