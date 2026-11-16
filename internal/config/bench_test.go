package config

import (
	"fmt"
	"strings"
	"testing"
)

func benchmarkDocument(upstreams, routes int) []byte {
	var b strings.Builder
	b.WriteString("server:\n  addr: \":8080\"\nadmin:\n  enabled: false\nmetrics:\n  enabled: false\n")
	b.WriteString("auth:\n  api_keys:\n    - id: demo\n      hash: 264c5c83f07d30c7ccc1083d7e416705272eafa1fc15167e09bed57b7697faed\n")
	b.WriteString("upstreams:\n")
	for i := 0; i < upstreams; i++ {
		fmt.Fprintf(&b, "  - name: upstream-%02d\n    targets:\n      - url: http://127.0.0.1:%d\n", i, 9000+i)
	}
	b.WriteString("routes:\n")
	for i := 0; i < routes; i++ {
		fmt.Fprintf(&b, "  - name: route-%02d\n    match:\n      path_prefix: /api/v%d\n    upstream: upstream-%02d\n", i, i, i%upstreams)
	}
	return []byte(b.String())
}

func BenchmarkParseConfig(b *testing.B) {
	document := benchmarkDocument(10, 50)

	b.ReportAllocs()
	b.SetBytes(int64(len(document)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Parse(document); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidateDefaults(b *testing.B) {
	document := benchmarkDocument(10, 50)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cfg, err := Parse(document)
		if err != nil {
			b.Fatal(err)
		}
		if err := cfg.Validate(); err != nil {
			b.Fatal(err)
		}
	}
}
