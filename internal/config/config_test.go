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
		func(c *Config) { c.PollIntervalSeconds = 90; c.CollectionTimeoutSeconds = 45 },
		func(c *Config) { c.RequestTimeoutSeconds = c.CollectionTimeoutSeconds + 1 },
		func(c *Config) { c.ListenAddress = "invalid" },
		func(c *Config) { c.ListenAddress = ":99999" },
		func(c *Config) { c.ListenAddress = ":0" },
		func(c *Config) { c.ListenAddress = ":abc" },
		func(c *Config) { c.ListenAddress = ":" },
		func(c *Config) { c.ListenAddress = ":+8080" },
		func(c *Config) { c.StateFile = "relative.json" },
		func(c *Config) { c.BackupsTokenFile = "" },
		func(c *Config) { c.BackupsTokenFile = "relative" },
	} {
		candidate := c
		mutate(&candidate)
		if candidate.Validate() == nil {
			t.Fatal(candidate)
		}
	}
}

func TestConfigurationAllowsTimingBudgetBelowSampleAge(t *testing.T) {
	c := Default()
	c.PollIntervalSeconds = 89
	c.CollectionTimeoutSeconds = 45
	c.RequestTimeoutSeconds = 10
	c.MaxSampleAgeSeconds = 180
	c.Clusters = []Cluster{{Namespace: "db", Name: "db", Repos: map[string]Repository{"repo1": {}}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigurationRejectsCaseInsensitiveDuplicates(t *testing.T) {
	const inventory = `"clusters":[{"namespace":"db","name":"db","repos":{"repo1":{}}}]`
	for name, data := range map[string]string{
		"top-level override":   `{"maxAgeSeconds":3600,"MaxAgeSeconds":7200,` + inventory + `}`,
		"reversed override":    `{"MaxAgeSeconds":7200,"maxAgeSeconds":3600,` + inventory + `}`,
		"two aliases":          `{"MAXAGESECONDS":3600,"MaxAgeSeconds":7200,` + inventory + `}`,
		"escaped alias":        `{"maxAgeSeconds":3600,"\u004daxAgeSeconds":7200,` + inventory + `}`,
		"Unicode alias":        `{"maxAgeSeconds":3600,"maxAge\u017Feconds":7200,` + inventory + `}`,
		"cluster inventory":    `{` + inventory + `,"Clusters":[{"namespace":"other","name":"db","repos":{"repo1":{}}}]}`,
		"cluster namespace":    `{"clusters":[{"namespace":"db","Namespace":"other","name":"db","repos":{"repo1":{}}}]}`,
		"cluster name":         `{"clusters":[{"namespace":"db","name":"db","Name":"other","repos":{"repo1":{}}}]}`,
		"repository inventory": `{"clusters":[{"namespace":"db","name":"db","repos":{"repo1":{}},"Repos":{"repo2":{}}}]}`,
		"repository override":  `{"clusters":[{"namespace":"db","name":"db","repos":{"repo1":{"maxAgeSeconds":3600,"MaxAgeSeconds":7200}}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("case-insensitive duplicate accepted", data)
			}
		})
	}
}
