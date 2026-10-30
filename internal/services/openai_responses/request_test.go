package openai_responses

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func convertRequest(t *testing.T, body string, opts Options) (gjson.Result, []string) {
	t.Helper()
	out, dropped, err := ConvertChatRequest([]byte(body), opts)
	if err != nil {
		t.Fatalf("ConvertChatRequest returned error: %v", err)
	}
	if !gjson.ValidBytes(out) {
		t.Fatalf("converted body is not valid JSON: %s", out)
	}
	return gjson.ParseBytes(out), dropped
}

func expectJSON(t *testing.T, result gjson.Result, path, want string) {
	t.Helper()
	if got := result.Get(path).Raw; got != want {
		t.Errorf("%s = %s, want %s", path, got, want)
	}
}

// TestConvertChatRequestParameters verifies the generation parameter mapping and dropped fields.
func TestConvertChatRequestParameters(t *testing.T) {
	out, dropped := convertRequest(t, `{
		"model":"gpt-6-luna","stream":true,"stream_options":{"include_usage":true},
		"temperature":0.2,"top_p":0.9,"max_tokens":100,"max_completion_tokens":200,
		"reasoning_effort":"high","verbosity":"low","service_tier":"flex","user":"u1",
		"metadata":{"k":"v"},"prompt_cache_key":"pck","parallel_tool_calls":false,
		"stop":["x"],"seed":1,"frequency_penalty":0.5,"modalities":["text"],"n":1,
		"messages":[{"role":"user","content":"hi"}]
	}`, Options{})

	expectJSON(t, out, "model", `"gpt-6-luna"`)
	expectJSON(t, out, "stream", `true`)
	expectJSON(t, out, "temperature", `0.2`)
	expectJSON(t, out, "top_p", `0.9`)
	expectJSON(t, out, "max_output_tokens", `200`)
	expectJSON(t, out, "reasoning", `{"effort":"high"}`)
	expectJSON(t, out, "text.verbosity", `"low"`)
	expectJSON(t, out, "service_tier", `"flex"`)
	expectJSON(t, out, "user", `"u1"`)
	expectJSON(t, out, "metadata", `{"k":"v"}`)
	expectJSON(t, out, "prompt_cache_key", `"pck"`)
	expectJSON(t, out, "parallel_tool_calls", `false`)
	expectJSON(t, out, "store", `false`)
	for _, field := range []string{"stream_options", "stop", "seed", "frequency_penalty", "n", "max_tokens", "messages", "include"} {
		if out.Get(field).Exists() {
			t.Errorf("field %s should not be forwarded", field)
		}
	}
	if want := []string{"frequency_penalty", "seed", "stop"}; !reflect.DeepEqual(dropped, want) {
		t.Errorf("dropped = %v, want %v", dropped, want)
	}
}

// TestConvertChatRequestDefaults verifies fields the Chat API leaves unset stay unset.
func TestConvertChatRequestDefaults(t *testing.T) {
	out, _ := convertRequest(t, `{"model":"m","store":true,"messages":[{"role":"user","content":"hi"}]}`, Options{})
	expectJSON(t, out, "stream", `false`)
	expectJSON(t, out, "store", `true`)
	for _, field := range []string{"reasoning", "parallel_tool_calls", "tools", "tool_choice", "text", "instructions"} {
		if out.Get(field).Exists() {
			t.Errorf("field %s should not be set", field)
		}
	}
}

// TestConvertChatRequestMessages verifies role and multimodal content mapping.
func TestConvertChatRequestMessages(t *testing.T) {
	out, _ := convertRequest(t, `{"model":"m","messages":[
		{"role":"system","content":"sys"},
		{"role":"developer","content":[{"type":"text","text":"dev"}]},
		{"role":"user","content":[
			{"type":"text","text":"look"},
			{"type":"image_url","image_url":{"url":"https://img","detail":"high"}},
			{"type":"file","file":{"file_id":"file-1","filename":"a.pdf"}}
		]},
		{"role":"assistant","content":"done"}
	]}`, Options{})

	expectJSON(t, out, "input.0", `{"type":"message","role":"developer","content":[{"type":"input_text","text":"sys"}]}`)
	expectJSON(t, out, "input.1.role", `"developer"`)
	expectJSON(t, out, "input.2.content.1", `{"type":"input_image","image_url":"https://img","detail":"high"}`)
	expectJSON(t, out, "input.2.content.2", `{"type":"input_file","file_id":"file-1","filename":"a.pdf"}`)
	expectJSON(t, out, "input.3.content.0", `{"type":"output_text","text":"done"}`)
}

// TestConvertChatRequestToolHistory verifies tool declarations, calls and results.
func TestConvertChatRequestToolHistory(t *testing.T) {
	out, _ := convertRequest(t, `{"model":"m",
		"tools":[{"type":"function","function":{"name":"get_weather","description":"d","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"get_weather"}},
		"messages":[
			{"role":"user","content":"weather?"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"a\"}"}},
				{"id":"call_2","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"b\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_2","content":"rain"},
			{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"sun"}]}
		]}`, Options{})

	expectJSON(t, out, "tools", `[{"type":"function","name":"get_weather","description":"d","parameters":{"type":"object"},"strict":false}]`)
	expectJSON(t, out, "tool_choice", `{"type":"function","name":"get_weather"}`)
	expectJSON(t, out, "input.#", `5`)
	expectJSON(t, out, "input.1", `{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"a\"}"}`)
	expectJSON(t, out, "input.2.call_id", `"call_2"`)
	expectJSON(t, out, "input.3", `{"type":"function_call_output","call_id":"call_2","output":"rain"}`)
	expectJSON(t, out, "input.4", `{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"sun"}]}`)
}

// TestConvertChatRequestLegacyFunctions verifies the deprecated functions API is mapped onto tools.
func TestConvertChatRequestLegacyFunctions(t *testing.T) {
	out, _ := convertRequest(t, `{"model":"m",
		"functions":[{"name":"lookup","parameters":{"type":"object"}}],
		"function_call":{"name":"lookup"},
		"messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":null,"function_call":{"name":"lookup","arguments":"{}"}},
			{"role":"function","name":"lookup","content":"result"}
		]}`, Options{})

	expectJSON(t, out, "tools.0.name", `"lookup"`)
	expectJSON(t, out, "tool_choice", `{"type":"function","name":"lookup"}`)
	expectJSON(t, out, "input.1.type", `"function_call"`)
	callID := out.Get("input.1.call_id").String()
	if callID == "" || out.Get("input.2.call_id").String() != callID {
		t.Errorf("legacy function result is not paired with its call: %s", out.Get("input").Raw)
	}
	expectJSON(t, out, "input.2.output", `"result"`)
}

// TestConvertChatRequestFormatsAndTools verifies structured output, logprobs, web search and custom tools.
func TestConvertChatRequestFormatsAndTools(t *testing.T) {
	out, _ := convertRequest(t, `{"model":"m","logprobs":true,"top_logprobs":3,
		"response_format":{"type":"json_schema","json_schema":{"name":"s","strict":true,"schema":{"type":"object"}}},
		"web_search_options":{"search_context_size":"low","user_location":{"type":"approximate","approximate":{"country":"CN","city":"Beijing"}}},
		"tools":[{"type":"custom","custom":{"name":"run","description":"d","format":{"type":"text"}}}],
		"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"required","tools":[{"type":"custom","custom":{"name":"run"}}]}},
		"messages":[{"role":"user","content":"x"}]}`, Options{})

	expectJSON(t, out, "text.format", `{"type":"json_schema","name":"s","strict":true,"schema":{"type":"object"}}`)
	expectJSON(t, out, "include", `["message.output_text.logprobs"]`)
	expectJSON(t, out, "top_logprobs", `3`)
	expectJSON(t, out, "tools.0", `{"type":"custom","name":"run","description":"d","format":{"type":"text"}}`)
	expectJSON(t, out, "tools.1", `{"type":"web_search","search_context_size":"low","user_location":{"type":"approximate","country":"CN","city":"Beijing"}}`)
	expectJSON(t, out, "tool_choice", `{"type":"allowed_tools","mode":"required","tools":[{"type":"custom","name":"run"}]}`)

	out, _ = convertRequest(t, `{"model":"m","response_format":{"type":"json_object"},"messages":[{"role":"user","content":"x"}]}`, Options{})
	expectJSON(t, out, "text.format", `{"type":"json_object"}`)
}

// TestConvertChatRequestReasoningSummary verifies the reasoning summary option.
func TestConvertChatRequestReasoningSummary(t *testing.T) {
	out, _ := convertRequest(t, `{"model":"m","reasoning_effort":"low","messages":[{"role":"user","content":"x"}]}`, Options{ReasoningSummary: true})
	expectJSON(t, out, "reasoning", `{"effort":"low","summary":"auto"}`)

	out, _ = convertRequest(t, `{"model":"m","reasoning_effort":"none","messages":[{"role":"user","content":"x"}]}`, Options{ReasoningSummary: true})
	expectJSON(t, out, "reasoning", `{"effort":"none"}`)
}

// TestConvertChatRequestLongToolNames verifies names over 64 characters are shortened consistently.
func TestConvertChatRequestLongToolNames(t *testing.T) {
	long := "mcp__server__" + strings.Repeat("x", 70)
	out, _ := convertRequest(t, `{"model":"m","tools":[{"type":"function","function":{"name":"`+long+`"}}],
		"messages":[{"role":"user","content":"x"},{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"`+long+`","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"ok"}]}`, Options{})

	short := out.Get("tools.0.name").String()
	if len(short) > 64 || short == long {
		t.Fatalf("tool name was not shortened: %s", short)
	}
	expectJSON(t, out, "input.1.name", `"`+short+`"`)
}

// TestConvertChatRequestErrors verifies requests the Responses API cannot serve are rejected.
func TestConvertChatRequestErrors(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","n":2,"messages":[{"role":"user","content":"x"}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"file","file":{}}]}]}`,
		`{"model":"m"}`,
		`not json`,
	} {
		_, _, err := ConvertChatRequest([]byte(body), Options{})
		var requestErr *RequestError
		if !errors.As(err, &requestErr) {
			t.Errorf("ConvertChatRequest(%s) error = %v, want *RequestError", body, err)
		}
	}
}
