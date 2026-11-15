package config

import (
	"gopkg.in/yaml.v3"
)

// Redacted returns a shallow copy safe to expose through the admin API.
// Configuration never contains literal secrets (only env/file references), but
// API-key hashes are removed because they are credential material.
func (c *Config) Redacted() *Config {
	cp := *c
	if c.Auth.APIKeys != nil {
		cp.Auth.APIKeys = make([]APIKey, len(c.Auth.APIKeys))
		copy(cp.Auth.APIKeys, c.Auth.APIKeys)
		for i := range cp.Auth.APIKeys {
			if cp.Auth.APIKeys[i].Hash != "" {
				cp.Auth.APIKeys[i].Hash = "[REDACTED]"
			}
		}
	}
	return &cp
}

// RedactedYAML renders the redacted config as YAML for the admin endpoint.
func RedactedYAML(c *Config) ([]byte, error) {
	return yaml.Marshal(c.Redacted())
}
