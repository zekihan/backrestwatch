package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStrictConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	valid := `{"clusters":[{"namespace":"db","name":"db","repos":{"repo1":{},"repo2":{"maxAgeSeconds":36000}}}]}`
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil || c.MaxAgeSeconds != 28800 || c.Clusters[0].Repos["repo2"].MaxAgeSeconds != 36000 {
		t.Fatal(c, err)
	}
	for _, data := range []string{`{}`, `{"clusters":[]}`, `{"clusters":null}`, `{"unknown":true}`, `{"clusters":[],"clusters":[]}`, valid + valid,
		`{"clusters":[{"namespace":"db","name":"db","repos":{"repo1":{"maxAgeSeconds":-1}}}]}`,
		`{"clusters":[{"namespace":"db/other","name":"db","repos":{"repo1":{}}}]}`,
		`{"clusters":[{"namespace":"db","name":"db","repos":{"repo5":{}}}]}`,
		`{"clusters":[{"namespace":"db","name":"db","repos":{}}]}`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatal("invalid config accepted", data)
		}
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Clusters = append(c.Clusters, c.Clusters[0]) },
		func(c *Config) { c.RequestTimeoutSeconds = 0 },
		func(c *Config) { c.PollIntervalSeconds = c.MaxSampleAgeSeconds },
		func(c *Config) { c.RequestTimeoutSeconds = c.CollectionTimeoutSeconds + 1 },
		func(c *Config) { c.ListenAddress = "invalid" },
		func(c *Config) { c.StateFile = "relative.json" },
	} {
		candidate := c
		mutate(&candidate)
		if candidate.Validate() == nil {
			t.Fatal(candidate)
		}
	}
}
