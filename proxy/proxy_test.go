package proxy

import (
	"encoding/json"
	"testing"
)

func TestConvertToOpenAIFormat(t *testing.T) {
	codexResp := map[string]interface{}{
		"id": "resp_123",
		"output": []interface{}{
			map[string]interface{}{
				"type": "message",
				"content": []interface{}{
					map[string]interface{}{
						"type": "output_text",
						"text": "Hello world",
					},
				},
			},
		},
		"usage": map[string]interface{}{
			"input_tokens":  10,
			"output_tokens": 5,
		},
	}

	respBody, _ := json.Marshal(codexResp)
	chatReq := map[string]interface{}{"model": "o3-pro"}

	result := convertToOpenAIFormat(respBody, chatReq)

	var openaiResp map[string]interface{}
	if err := json.Unmarshal(result, &openaiResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if openaiResp["id"] != "resp_123" {
		t.Errorf("id = %v, want resp_123", openaiResp["id"])
	}
	if openaiResp["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", openaiResp["object"])
	}
	if openaiResp["model"] != "o3-pro" {
		t.Errorf("model = %v, want o3-pro", openaiResp["model"])
	}

	choices, ok := openaiResp["choices"].([]interface{})
	if !ok || len(choices) != 1 {
		t.Fatalf("choices: got %v", openaiResp["choices"])
	}

	choice := choices[0].(map[string]interface{})
	msg := choice["message"].(map[string]interface{})
	if msg["content"] != "Hello world" {
		t.Errorf("content = %v, want Hello world", msg["content"])
	}
	if msg["role"] != "assistant" {
		t.Errorf("role = %v, want assistant", msg["role"])
	}
}

func TestExtractContent(t *testing.T) {
	tests := []struct {
		name string
		resp map[string]interface{}
		want string
	}{
		{
			name: "standard output",
			resp: map[string]interface{}{
				"output": []interface{}{
					map[string]interface{}{
						"type": "message",
						"content": []interface{}{
							map[string]interface{}{"type": "output_text", "text": "part1"},
							map[string]interface{}{"type": "output_text", "text": "part2"},
						},
					},
				},
			},
			want: "part1part2",
		},
		{
			name: "text field fallback",
			resp: map[string]interface{}{"text": "direct text"},
			want: "direct text",
		},
		{
			name: "empty response",
			resp: map[string]interface{}{},
			want: "",
		},
		{
			name: "text type variant",
			resp: map[string]interface{}{
				"output": []interface{}{
					map[string]interface{}{
						"type": "message",
						"content": []interface{}{
							map[string]interface{}{"type": "text", "text": "variant"},
						},
					},
				},
			},
			want: "variant",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractContent(tt.resp)
			if got != tt.want {
				t.Errorf("extractContent = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildStreamChunk(t *testing.T) {
	chunk := buildStreamChunk("id-1", "o3-pro", 1000, true, "hello", "")

	var result map[string]interface{}
	if err := json.Unmarshal(chunk, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if result["id"] != "id-1" {
		t.Errorf("id = %v", result["id"])
	}
	if result["object"] != "chat.completion.chunk" {
		t.Errorf("object = %v", result["object"])
	}

	choices := result["choices"].([]interface{})
	delta := choices[0].(map[string]interface{})["delta"].(map[string]interface{})

	if delta["role"] != "assistant" {
		t.Error("first chunk should include role")
	}
	if delta["content"] != "hello" {
		t.Errorf("content = %v", delta["content"])
	}

	// Non-first chunk without role
	chunk2 := buildStreamChunk("id-1", "o3-pro", 1000, false, "world", "")
	var result2 map[string]interface{}
	json.Unmarshal(chunk2, &result2)
	choices2 := result2["choices"].([]interface{})
	delta2 := choices2[0].(map[string]interface{})["delta"].(map[string]interface{})
	if _, hasRole := delta2["role"]; hasRole {
		t.Error("subsequent chunk should not include role")
	}

	// Final chunk with finish_reason
	chunk3 := buildStreamChunk("id-1", "o3-pro", 1000, false, "", "stop")
	var result3 map[string]interface{}
	json.Unmarshal(chunk3, &result3)
	choices3 := result3["choices"].([]interface{})
	fr := choices3[0].(map[string]interface{})["finish_reason"]
	if fr != "stop" {
		t.Errorf("finish_reason = %v, want stop", fr)
	}
}
