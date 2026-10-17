package proxy

import (
	"fmt"
	"testing"
)

func TestAnthropicCacheWriteProvenanceJSONAndSSE(t *testing.T) {
	for _, nested := range []bool{false, true} {
		extra := ""
		want := "legacy"
		if nested {
			extra = `,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":1000}`
			want = "nested"
		}
		body := []byte(fmt.Sprintf(`{"id":"msg_proof","model":"claude-sonnet-4","usage":{"input_tokens":100,"output_tokens":1,"cache_creation_input_tokens":1000%s}}`, extra))
		ev := parseAnthropic(body, "alice", "1231", "")
		if ev == nil || ev.CacheWriteEvidence != want {
			t.Fatalf("JSON evidence %+v", ev)
		}
		parser := &anthropicStreamParser{}
		parser.OnEvent("message_start", []byte(fmt.Sprintf(`{"type":"message_start","message":%s}`, body)))
		ev = parser.Finalise("alice", "1231", "")
		if ev == nil || ev.CacheWriteEvidence != want {
			t.Fatalf("SSE evidence %+v", ev)
		}
	}
}
