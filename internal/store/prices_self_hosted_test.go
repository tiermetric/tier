package store

import "testing"

func TestSelfHostedClassParameterCount(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		{"1b", "self-hosted-small"},
		{"3b", "self-hosted-small"},
		{"7b", "self-hosted-medium"},
		{"8b", "self-hosted-medium"},
		{"11b", "self-hosted-medium"},
		{"13b", "self-hosted-medium"},
		{"21b", "self-hosted-medium"},
		{"31b", "self-hosted-medium"},
		{"33b", "self-hosted-medium"},
		{"70b", "self-hosted-large"},
		{"72b", "self-hosted-large"},
		{"73b", "self-hosted-large"},
		{"405b", "self-hosted-large"},
		{"1.5b", "self-hosted-small"},
		{"0.5b", "self-hosted-small"},
		{"model-6.9b", "self-hosted-small"},
		{"model-69.9b", "self-hosted-medium"},
		{"model-13B-instruct", "self-hosted-medium"},
		{"llama-3.1-70b-instruct", "self-hosted-large"},
		{"model13b-instruct", "self-hosted-medium"},
		{"model-13b2", ""},
		{"model-405b2", ""},
		{"acme-nemo-4bit", ""},
		{"acme-nemo-4BIT", ""},
		{"qwen2.5-coder-32b", "self-hosted-medium"},
		// MoE names use the expert count (7B), not the total, as on main.
		{"mixtral-8x7b", "self-hosted-medium"},
		{"nemotron-ultra", "self-hosted-large"},
		{"nemotron-ultra-8b", "self-hosted-large"},
		{"deepseek-r1-full", "self-hosted-large"},
		{"deepseek-r1-full-8b", "self-hosted-large"},
		{"phi-4-mini", "self-hosted-small"},
		{"phi-4-mini-8b", "self-hosted-small"},
		{"text-embeddings", "self-hosted-small"},
		{"text-embeddings-8b", "self-hosted-small"},
		{"RERANKER", "self-hosted-small"},
		{"qwen3-reranker-8b", "self-hosted-small"},
		{"unknown-model", ""},
		{"model-13", ""},
		{"", ""},
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			if got := selfHostedClass(c.model); got != c.want {
				t.Errorf("selfHostedClass(%q) = %q, want %q", c.model, got, c.want)
			}
		})
	}
}
