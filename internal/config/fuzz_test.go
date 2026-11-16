package config

import (
	"os"
	"testing"
)

// FuzzParse asserts that arbitrary YAML never panics the strict parser. A
// config either fails validation or produces a fully defaulted, validated
// snapshot; both outcomes are acceptable.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"",
		"not: [valid",
		"upstreams: []\nroutes: []\n",
		"server:\n  addr: ':8080'\n",
	}
	for _, p := range []string{
		"../../configs/gateway.yaml",
		"../../configs/gateway.compose.yaml",
	} {
		if data, err := os.ReadFile(p); err == nil {
			seeds = append(seeds, string(data))
		}
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		cfg, err := Parse(data)
		if err != nil {
			return
		}
		if cfg == nil {
			t.Fatal("Parse returned nil config without an error")
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Parse returned an invalid config: %v", err)
		}
	})
}
