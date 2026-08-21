package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseEmptyCompletion_Hit(t *testing.T) {
	body := `{"id":"chatcmpl_fm0lu3sc1sb","object":"chat.completion","created":1787213597,"model":"muse-spark-1.2-contributor-free","choices":[{"index":0,"message":{"role":"assistant"},"finish_reason":null}]}`
	hit, info := ParseEmptyCompletion([]byte(body))
	if !hit {
		t.Fatal("want hit for empty completion")
	}
	if info.Model != "muse-spark-1.2-contributor-free" || info.ID != "chatcmpl_fm0lu3sc1sb" {
		t.Fatalf("unexpected info: %+v", info)
	}
}

func TestParseEmptyCompletion_EmptyContentString(t *testing.T) {
	body := `{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":null}]}`
	hit, _ := ParseEmptyCompletion([]byte(body))
	if !hit {
		t.Fatal("want hit for empty string content")
	}
}

func TestParseEmptyCompletion_NormalError(t *testing.T) {
	body := `{"error":{"type":"invalid_request_error","message":"bad request"}}`
	hit, _ := ParseEmptyCompletion([]byte(body))
	if hit {
		t.Fatal("want no hit for normal error body")
	}
}

func TestParseEmptyCompletion_HasContent(t *testing.T) {
	body := `{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`
	hit, _ := ParseEmptyCompletion([]byte(body))
	if hit {
		t.Fatal("want no hit for completion with content")
	}
}

func TestParseEmptyCompletion_FinishReasonSet(t *testing.T) {
	body := `{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant"},"finish_reason":"stop"}]}`
	hit, _ := ParseEmptyCompletion([]byte(body))
	if hit {
		t.Fatal("want no hit when finish_reason is set")
	}
}

func TestParseEmptyCompletion_NotJSON(t *testing.T) {
	hit, _ := ParseEmptyCompletion([]byte("not json"))
	if hit {
		t.Fatal("want no hit for non-JSON body")
	}
}

func TestBuildEmptyCompletionError(t *testing.T) {
	original := []byte(`{"id":"chatcmpl_x","object":"chat.completion","model":"muse-spark-1.2-contributor-free","choices":[{"index":0,"message":{"role":"assistant"},"finish_reason":null}]}`)
	info := EmptyCompletionInfo{ID: "chatcmpl_x", Model: "muse-spark-1.2-contributor-free"}
	out := BuildEmptyCompletionError(info, original, "vps-node-sg-local", 400)

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	errObj, ok := parsed["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error object: %v", parsed)
	}
	msg, _ := errObj["message"].(string)
	for _, want := range []string{"vps-node-sg-local", "400", "muse-spark-1.2-contributor-free", "chatcmpl_x"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q: %s", want, msg)
		}
	}
	if !strings.Contains(msg, "chat.completion") {
		t.Fatalf("message should include original body: %s", msg)
	}
}