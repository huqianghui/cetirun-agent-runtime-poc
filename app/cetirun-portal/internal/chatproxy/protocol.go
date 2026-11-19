// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chatproxy

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	strictjson "sigs.k8s.io/json"
)

const (
	RequestBytes = 1 << 20
	InputBytes   = 64 << 10
	OutputBytes  = 4 << 20
	FrameBytes   = 256 << 10
	ModelBytes   = 256 << 10
)

var ErrProtocol = errors.New("Agent returned an unsupported chat response")
var ErrAgent = errors.New("Agent reported an unsuccessful response")

type Limits struct {
	RequestBytes          int `json:"requestBytes"`
	MessageCount          int `json:"messageCount"`
	InputBytes            int `json:"inputBytes"`
	OutputBytes           int `json:"outputBytes"`
	RequestTimeoutSeconds int `json:"requestTimeoutSeconds"`
}

func DefaultLimits() Limits { return Limits{RequestBytes, 100, InputBytes, OutputBytes, 600} }

type Target struct {
	Eligible  bool   `json:"eligible"`
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
	AgentUID  string `json:"agentUid"`
	Revision  string `json:"bindingRevision,omitempty"`
	Limits    Limits `json:"limits"`
	BaseURL   string `json:"-"`
}

type Message struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content"`
}

type Request struct {
	Model      string    `json:"model"`
	Messages   []Message `json:"messages"`
	Stream     bool      `json:"stream"`
	MemoryMode string    `json:"-"`
}

func DecodeRequest(raw []byte) (Request, error) {
	var request Request
	if len(raw) > RequestBytes || !utf8.Valid(raw) {
		return request, errors.New("invalid_chat_request")
	}
	problems, err := strictjson.UnmarshalStrict(raw, &request)
	if err != nil || len(problems) != 0 || !validModel(request.Model) || len(request.Messages) == 0 || len(request.Messages) > 100 || len(request.Messages)%2 != 1 {
		return request, errors.New("invalid_chat_request")
	}
	// Reject null scalars instead of silently decoding them as Go zero values.
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	for _, value := range fields {
		if string(value) == "null" {
			return request, errors.New("invalid_chat_request")
		}
	}
	var messageFields []map[string]json.RawMessage
	_ = json.Unmarshal(fields["messages"], &messageFields)
	for _, message := range messageFields {
		if message["content"] == nil || string(message["content"]) == "null" || message["role"] == nil {
			return request, errors.New("invalid_chat_request")
		}
	}
	for i, message := range request.Messages {
		role := "user"
		if i%2 != 0 {
			role = "assistant"
		}
		if message.Role != role || (role == "user" && (len(message.Content) > InputBytes || strings.TrimSpace(message.Content) == "")) {
			return request, errors.New("invalid_chat_request")
		}
	}
	return request, nil
}

func validModel(value string) bool {
	return len(value) > 0 && len(value) <= 256 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

type Model struct {
	ID     string `json:"id"`
	Object string `json:"object"`
}

type Models struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

func DecodeModels(raw []byte) (Models, error) {
	var value Models
	if len(raw) > ModelBytes || !utf8.Valid(raw) {
		return value, ErrProtocol
	}
	problems, err := strictjson.UnmarshalStrict(raw, &value, strictjson.DisallowDuplicateFields)
	if err != nil || len(problems) != 0 || value.Object != "list" || value.Data == nil || len(value.Data) > 256 {
		return value, ErrProtocol
	}
	seen := map[string]bool{}
	for i := range value.Data {
		id := value.Data[i].ID
		if !validModel(id) || seen[id] {
			return Models{}, ErrProtocol
		}
		seen[id] = true
		value.Data[i].Object = "model"
	}
	return value, nil
}

type Choice struct {
	Index        int      `json:"index"`
	Delta        *Message `json:"delta,omitempty"`
	Message      *Message `json:"message,omitempty"`
	FinishReason *string  `json:"finish_reason"`
}

type Completion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
}

func normalize(raw []byte, id, model string, stream bool) (Completion, bool, error) {
	var body struct {
		Choices []struct {
			Index        int                        `json:"index"`
			Delta        map[string]json.RawMessage `json:"delta"`
			Message      map[string]json.RawMessage `json:"message"`
			FinishReason *string                    `json:"finish_reason"`
		} `json:"choices"`
		Error json.RawMessage `json:"error"`
		Usage json.RawMessage `json:"usage"`
	}
	result := Completion{ID: id, Model: model, Object: "chat.completion"}
	problems, err := strictjson.UnmarshalStrict(raw, &body, strictjson.DisallowDuplicateFields)
	if err != nil || len(problems) != 0 || !utf8.Valid(raw) {
		return result, false, ErrProtocol
	}
	if len(body.Error) != 0 && string(body.Error) != "null" {
		return result, false, ErrAgent
	}
	if stream && body.Choices != nil && len(body.Choices) == 0 && len(body.Usage) > 0 {
		return result, true, nil
	}
	if len(body.Choices) != 1 || body.Choices[0].Index != 0 {
		return result, false, ErrProtocol
	}
	choice := body.Choices[0]
	fields := choice.Message
	if stream {
		fields = choice.Delta
		result.Object = "chat.completion.chunk"
	}
	if fields == nil {
		return result, false, ErrProtocol
	}
	for _, key := range []string{"tool_calls", "function_call"} {
		if value, ok := fields[key]; ok && string(value) != "null" {
			return result, false, ErrProtocol
		}
	}
	message := &Message{}
	if raw, ok := fields["role"]; ok {
		if json.Unmarshal(raw, &message.Role) != nil || message.Role != "assistant" {
			return result, false, ErrProtocol
		}
	}
	if raw, ok := fields["content"]; ok && string(raw) != "null" {
		if json.Unmarshal(raw, &message.Content) != nil {
			return result, false, ErrProtocol
		}
	}
	if !stream && (message.Role != "assistant" || fields["content"] == nil || string(fields["content"]) == "null") {
		return result, false, ErrProtocol
	}
	if reason := choice.FinishReason; reason != nil && *reason != "stop" && *reason != "length" && *reason != "content_filter" {
		return result, false, ErrProtocol
	}
	if !stream && choice.FinishReason == nil {
		return result, false, ErrProtocol
	}
	out := Choice{Index: 0, FinishReason: choice.FinishReason}
	if stream {
		out.Delta = message
	} else {
		out.Message = message
	}
	result.Choices = []Choice{out}
	return result, false, nil
}
