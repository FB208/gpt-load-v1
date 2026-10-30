package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"gpt-load/internal/models"
	"gpt-load/internal/services/openai_responses"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

const chatCompletionsPathSuffix = "/chat/completions"

// chatResponsesConversion carries what is needed to serve a Chat Completions
// request through the upstream Responses API.
type chatResponsesConversion struct {
	originalBody []byte
	requestURL   *url.URL
	options      openai_responses.Options
}

// shouldConvertChatToResponses reports whether the request is a chat completion
// on a group that serves chat completions through the Responses API.
func shouldConvertChatToResponses(c *gin.Context, group *models.Group) bool {
	return group.ChatToResponses &&
		group.ChannelType == "openai" &&
		c.Request.Method == http.MethodPost &&
		strings.HasSuffix(c.Request.URL.Path, chatCompletionsPathSuffix)
}

// newChatResponsesConversion converts the Chat Completions body into a Responses API
// body and points the upstream path at /responses.
func newChatResponsesConversion(c *gin.Context, group *models.Group, bodyBytes []byte, isStream bool) (*chatResponsesConversion, []byte, error) {
	options := openai_responses.Options{ReasoningSummary: group.ReasoningSummary}
	converted, dropped, err := openai_responses.ConvertChatRequest(bodyBytes, options)
	if err != nil {
		return nil, nil, err
	}
	if len(dropped) > 0 {
		logrus.Debugf("Chat-to-Responses conversion for group %s dropped unsupported fields: %s", group.Name, strings.Join(dropped, ", "))
	}
	// IsStreamRequest also honours the Accept header and query, so the upstream must follow it.
	converted, _ = sjson.SetBytes(converted, "stream", isStream)

	requestURL := *c.Request.URL
	requestURL.Path = strings.TrimSuffix(requestURL.Path, chatCompletionsPathSuffix) + "/responses"
	requestURL.RawPath = ""

	return &chatResponsesConversion{
		originalBody: bodyBytes,
		requestURL:   &requestURL,
		options:      options,
	}, converted, nil
}

// handleConvertedResponse writes a successful Responses API result back to the client as Chat Completions.
func (ps *ProxyServer) handleConvertedResponse(c *gin.Context, resp *http.Response, isStream bool, conversion *chatResponsesConversion) {
	for key, values := range resp.Header {
		switch http.CanonicalHeaderKey(key) {
		case "Content-Length", "Content-Encoding", "Content-Type":
			continue
		}
		for _, value := range values {
			c.Header(key, value)
		}
	}

	if isStream {
		ps.handleResponsesStreamAsChat(c, resp, conversion)
	} else {
		ps.handleResponsesAsChat(c, resp, conversion)
	}
}

func (ps *ProxyServer) handleResponsesStreamAsChat(c *gin.Context, resp *http.Response, conversion *chatResponsesConversion) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(resp.StatusCode)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		logrus.Error("Streaming unsupported by the writer for chat-to-responses conversion")
		return
	}

	converter := openai_responses.NewStreamConverter(conversion.originalBody, conversion.options)
	writeChunks := func(chunks [][]byte) bool {
		for _, chunk := range chunks {
			if _, err := c.Writer.Write(append(append([]byte("data: "), chunk...), '\n', '\n')); err != nil {
				logUpstreamError("writing converted stream to client", err)
				return false
			}
		}
		if len(chunks) > 0 {
			flusher.Flush()
		}
		return true
	}

	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && !writeChunks(converter.ConvertLine(line)) {
			return
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			logUpstreamError("reading from upstream", err)
			return
		}
	}

	if !writeChunks(converter.Finish()) {
		return
	}
	if _, err := c.Writer.Write([]byte("data: [DONE]\n\n")); err != nil {
		logUpstreamError("writing converted stream to client", err)
		return
	}
	flusher.Flush()
}

func (ps *ProxyServer) handleResponsesAsChat(c *gin.Context, resp *http.Response, conversion *chatResponsesConversion) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logUpstreamError("reading responses body", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "failed to read upstream response", "type": "server_error"}})
		return
	}
	body = handleGzipCompression(resp, body)

	converted, err := openai_responses.ConvertResponse(conversion.originalBody, body, conversion.options)
	if err != nil {
		var upstreamErr *openai_responses.UpstreamError
		if errors.As(err, &upstreamErr) {
			c.Data(http.StatusBadGateway, "application/json", upstreamErr.Body)
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": err.Error(), "type": "server_error"}})
		return
	}
	c.Data(resp.StatusCode, "application/json", bytes.TrimSpace(converted))
}
