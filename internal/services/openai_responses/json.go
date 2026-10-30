// Portions of this file are adapted from CLIProxyAPI
// (https://github.com/router-for-me/CLIProxyAPI, internal/translator/common),
// Copyright (c) 2025.9-present Router-For.ME, licensed under the MIT License.

package openai_responses

import (
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// joinRawArray joins raw JSON values into a JSON array.
func joinRawArray(items [][]byte) []byte {
	if len(items) == 0 {
		return []byte("[]")
	}
	size := len(items) + 1
	for _, item := range items {
		size += len(item)
	}
	out := make([]byte, 0, size)
	out = append(out, '[')
	for i, item := range items {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, item...)
	}
	return append(out, ']')
}

// setRawArrayItems replaces the JSON array at path with raw items.
func setRawArrayItems(data []byte, path string, items [][]byte) []byte {
	if len(items) == 0 {
		return data
	}
	data, _ = sjson.SetRawBytes(data, path, joinRawArray(items))
	return data
}

// userTurnDrops tracks content parts that cannot be sent upstream. A user turn
// left with nothing to send is refused rather than forwarded empty.
type userTurnDrops struct {
	turn  string
	first string
}

// drop records a part of the current user turn that cannot be sent.
func (d *userTurnDrops) drop(partType string) {
	if d.turn == "" {
		d.turn = partType
	}
}

// endTurn closes the current user turn; sendable is the number of parts it contributed.
func (d *userTurnDrops) endTurn(sendable int) {
	if d.turn != "" && sendable <= 0 && d.first == "" {
		d.first = d.turn
	}
	d.turn = ""
}

// err returns the refusal for the first emptied user turn, or nil.
func (d *userTurnDrops) err() error {
	if d.first == "" {
		return nil
	}
	return &RequestError{Message: fmt.Sprintf("user message only contains %s content that cannot be sent to the responses API", d.first)}
}

// chatUsageFromResponses maps a Responses usage object onto the Chat Completions usage shape.
func chatUsageFromResponses(usage gjson.Result) []byte {
	out := []byte(`{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`)
	out, _ = sjson.SetBytes(out, "prompt_tokens", usage.Get("input_tokens").Int())
	out, _ = sjson.SetBytes(out, "completion_tokens", usage.Get("output_tokens").Int())
	if total := usage.Get("total_tokens"); total.Exists() {
		out, _ = sjson.SetBytes(out, "total_tokens", total.Int())
	} else {
		out, _ = sjson.SetBytes(out, "total_tokens", usage.Get("input_tokens").Int()+usage.Get("output_tokens").Int())
	}
	if cached := usage.Get("input_tokens_details.cached_tokens"); cached.Exists() {
		out, _ = sjson.SetBytes(out, "prompt_tokens_details.cached_tokens", cached.Int())
	}
	if reasoning := usage.Get("output_tokens_details.reasoning_tokens"); reasoning.Exists() {
		out, _ = sjson.SetBytes(out, "completion_tokens_details.reasoning_tokens", reasoning.Int())
	}
	return out
}
