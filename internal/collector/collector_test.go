package collector

import "testing"

func TestIdempotencyKey_DeterministicAndSourcePrefixed(t *testing.T) {
	k1 := IdempotencyKey(SourceJSONL, "sess-abc")
	k2 := IdempotencyKey(SourceJSONL, "sess-abc")
	if k1 != k2 {
		t.Fatalf("same inputs produced different keys: %s != %s", k1, k2)
	}
	if len(k1) != 64 {
		t.Fatalf("expected 64-char hex digest, got %d chars: %s", len(k1), k1)
	}

	// Different source → different key for same content.
	kProxy := IdempotencyKey(SourceProxy, "sess-abc")
	if kProxy == k1 {
		t.Fatalf("source prefix did not affect key: jsonl and proxy produced same digest")
	}

	// Different content → different key.
	kOther := IdempotencyKey(SourceJSONL, "sess-xyz")
	if kOther == k1 {
		t.Fatalf("different session id produced same key")
	}
}

func TestIdempotencyKey_EmptyInputsReturnEmpty(t *testing.T) {
	if got := IdempotencyKey("", "anything"); got != "" {
		t.Errorf("empty source should yield empty key, got %q", got)
	}
	if got := IdempotencyKey(SourceJSONL); got != "" {
		t.Errorf("zero parts should yield empty key, got %q", got)
	}
}

func TestIdempotencyKey_MultipleParts(t *testing.T) {
	// Different join structure should produce different keys (defends against
	// the trivial concat collision: ("a","b") vs ("a:b",)).
	k1 := IdempotencyKey(SourceProxy, "a", "b")
	k2 := IdempotencyKey(SourceProxy, "a:b")
	if k1 == k2 {
		t.Errorf("naive concat collision: (a,b) and (a:b) produced the same key")
	}
}
