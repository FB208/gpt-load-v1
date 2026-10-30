package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpt-load/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func newConversionContext(t *testing.T, method, path string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(method, path, nil)
	return c, recorder
}

// TestShouldConvertChatToResponses verifies only chat completions on enabled openai groups are converted.
func TestShouldConvertChatToResponses(t *testing.T) {
	enabled := &models.Group{ChannelType: "openai", ChatToResponses: true}
	for _, tc := range []struct {
		method, path string
		group        *models.Group
		want         bool
	}{
		{http.MethodPost, "/proxy/g/v1/chat/completions", enabled, true},
		{http.MethodPost, "/proxy/g/chat/completions", enabled, true},
		{http.MethodGet, "/proxy/g/v1/models", enabled, false},
		{http.MethodPost, "/proxy/g/v1/embeddings", enabled, false},
		{http.MethodPost, "/proxy/g/v1/chat/completions", &models.Group{ChannelType: "openai"}, false},
		{http.MethodPost, "/proxy/g/v1/chat/completions", &models.Group{ChannelType: "anthropic", ChatToResponses: true}, false},
	} {
		c, _ := newConversionContext(t, tc.method, tc.path)
		if got := shouldConvertChatToResponses(c, tc.group); got != tc.want {
			t.Errorf("%s %s (%+v) = %v, want %v", tc.method, tc.path, tc.group, got, tc.want)
		}
	}
}

// TestNewChatResponsesConversion verifies the body conversion and upstream path rewrite.
func TestNewChatResponsesConversion(t *testing.T) {
	c, _ := newConversionContext(t, http.MethodPost, "/proxy/g/v1/chat/completions?x=1")
	group := &models.Group{Name: "g", ChannelType: "openai", ChatToResponses: true, ReasoningSummary: true}
	body := []byte(`{"model":"m","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)

	conversion, converted, err := newChatResponsesConversion(c, group, body, true)
	if err != nil {
		t.Fatal(err)
	}
	if conversion.requestURL.Path != "/proxy/g/v1/responses" || conversion.requestURL.RawQuery != "x=1" {
		t.Errorf("request URL = %s", conversion.requestURL)
	}
	if string(conversion.originalBody) != string(body) {
		t.Errorf("original body changed: %s", conversion.originalBody)
	}
	// The stream flag follows the detected stream mode, not only the body.
	if !gjson.GetBytes(converted, "stream").Bool() || gjson.GetBytes(converted, "reasoning.summary").String() != "auto" {
		t.Errorf("converted body = %s", converted)
	}

	if _, _, err := newChatResponsesConversion(c, group, []byte(`{"model":"m","n":3,"messages":[]}`), false); err == nil {
		t.Error("expected n>1 to be rejected")
	}
}

func upstreamResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {contentType}, "Content-Length": {"999"}, "X-Request-Id": {"req_1"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestHandleConvertedStream verifies SSE events are rewritten as chat chunks ending with [DONE].
func TestHandleConvertedStream(t *testing.T) {
	c, recorder := newConversionContext(t, http.MethodPost, "/proxy/g/v1/chat/completions")
	conversion := &chatResponsesConversion{originalBody: []byte(`{"model":"m","stream":true,"stream_options":{"include_usage":true}}`)}
	upstream := strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_1","created_at":1,"model":"m"}}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
		"",
	}, "\n")

	(&ProxyServer{}).handleConvertedResponse(c, upstreamResponse(http.StatusOK, "text/event-stream", upstream), true, conversion)

	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	if recorder.Header().Get("Content-Length") != "" || recorder.Header().Get("X-Request-Id") != "req_1" {
		t.Errorf("unexpected headers: %v", recorder.Header())
	}
	frames := strings.Split(strings.TrimSpace(recorder.Body.String()), "\n\n")
	if len(frames) != 4 || frames[3] != "data: [DONE]" {
		t.Fatalf("unexpected frames: %q", frames)
	}
	if got := gjson.Get(strings.TrimPrefix(frames[0], "data: "), "choices.0.delta.content").String(); got != "hi" {
		t.Errorf("first chunk content = %q", got)
	}
	if got := gjson.Get(strings.TrimPrefix(frames[2], "data: "), "usage.total_tokens").Int(); got != 3 {
		t.Errorf("usage chunk = %s", frames[2])
	}
}

// TestHandleConvertedNonStream verifies a Responses object is rewritten as a chat completion.
func TestHandleConvertedNonStream(t *testing.T) {
	c, recorder := newConversionContext(t, http.MethodPost, "/proxy/g/v1/chat/completions")
	conversion := &chatResponsesConversion{originalBody: []byte(`{"model":"m"}`)}
	upstream := `{"id":"resp_1","status":"completed","model":"m","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}]}`

	(&ProxyServer{}).handleConvertedResponse(c, upstreamResponse(http.StatusOK, "application/json", upstream), false, conversion)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	if gjson.Get(body, "object").String() != "chat.completion" || gjson.Get(body, "choices.0.message.content").String() != "hello" {
		t.Errorf("body = %s", body)
	}

	c, recorder = newConversionContext(t, http.MethodPost, "/proxy/g/v1/chat/completions")
	(&ProxyServer{}).handleConvertedResponse(c, upstreamResponse(http.StatusOK, "application/json", `{"status":"failed","error":{"message":"boom"}}`), false, conversion)
	if recorder.Code != http.StatusBadGateway || gjson.Get(recorder.Body.String(), "error.message").String() != "boom" {
		t.Errorf("failed response = %d %s", recorder.Code, recorder.Body.String())
	}
}
