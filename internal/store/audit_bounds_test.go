package store

import (
	"bytes"
	"testing"
)

func TestWebhookPayloadDecompressionBounds(t *testing.T) {
	for _, size := range []int{3 << 20, 25 << 20, (25 << 20) + 1} {
		body := bytes.Repeat([]byte("x"), size)
		gz, err := gzipBytes(body)
		if err != nil {
			t.Fatal(err)
		}
		got, err := gunzipBytes(gz)
		if size > 25<<20 {
			if err == nil {
				t.Fatal("oversized gzip accepted")
			}
		} else if err != nil || !bytes.Equal(got, body) {
			t.Errorf("%d-byte round trip: len=%d err=%v", size, len(got), err)
		}
	}
}
