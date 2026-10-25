package config

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

type Repository struct {
	MaxAgeSeconds int `json:"maxAgeSeconds,omitempty"`
}

type Cluster struct {
	Namespace string                `json:"namespace"`
	Name      string                `json:"name"`
	Repos     map[string]Repository `json:"repos"`
}

type Config struct {
	BackupsTokenFile         string            `json:"backupsTokenFile"`
	EvidencePublicKeys       map[string]string `json:"evidencePublicKeys,omitempty"`
	ListenAddress            string            `json:"listenAddress"`
	StateFile                string            `json:"stateFile"`
	PollIntervalSeconds      int               `json:"pollIntervalSeconds"`
	MaxSampleAgeSeconds      int               `json:"maxSampleAgeSeconds"`
	MaxAgeSeconds            int               `json:"maxAgeSeconds"`
	RequestTimeoutSeconds    int               `json:"requestTimeoutSeconds"`
	CollectionTimeoutSeconds int               `json:"collectionTimeoutSeconds"`
	Clusters                 []Cluster         `json:"clusters"`
}

func Default() Config {
	return Config{BackupsTokenFile: "/var/run/backrestwatch-auth/token", ListenAddress: ":8080", StateFile: "/data/evidence.json", PollIntervalSeconds: 60,
		MaxSampleAgeSeconds: 180, MaxAgeSeconds: 28800, RequestTimeoutSeconds: 10, CollectionTimeoutSeconds: 45}
}

func Load(path string) (Config, error) {
	c := Default()
	f, err := os.Open(path)
	if err != nil {
		return c, fmt.Errorf("open configuration: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return c, fmt.Errorf("configuration exceeds read budget or cannot be read")
	}
	if err := UniqueJSON(data); err != nil {
		return c, fmt.Errorf("invalid configuration JSON: %w", err)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("decode configuration: %w", err)
	}
	return c, c.Validate()
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
var repoName = regexp.MustCompile(`^repo[1-4]$`)

func Name(s string) bool { return len(s) <= 63 && dnsLabel.MatchString(s) }
func Repo(s string) bool { return repoName.MatchString(s) }

func (c Config) Validate() error {
	if !filepath.IsAbs(c.BackupsTokenFile) || filepath.Base(c.BackupsTokenFile) == "." {
		return fmt.Errorf("backupsTokenFile must be an absolute file path")
	}
	if len(c.EvidencePublicKeys) > 16 {
		return fmt.Errorf("at most 16 evidence public keys are supported")
	}
	for id, encoded := range c.EvidencePublicKeys {
		key, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if !Name(id) || err != nil || len(key) != ed25519.PublicKeySize {
			return fmt.Errorf("invalid evidence public key")
		}
	}

	_, port, err := net.SplitHostPort(c.ListenAddress)
	if err != nil {
		return fmt.Errorf("listenAddress must be host:port with a valid port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || strings.Trim(port, "0123456789") != "" || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("listenAddress port must be between 1 and 65535")
	}
	if !filepath.IsAbs(c.StateFile) || filepath.Base(c.StateFile) == "." {
		return fmt.Errorf("stateFile must be an absolute file path")
	}
	for name, value := range map[string]int{"pollIntervalSeconds": c.PollIntervalSeconds,
		"maxSampleAgeSeconds": c.MaxSampleAgeSeconds, "maxAgeSeconds": c.MaxAgeSeconds,
		"requestTimeoutSeconds": c.RequestTimeoutSeconds, "collectionTimeoutSeconds": c.CollectionTimeoutSeconds} {
		if value < 1 || value > 366*24*3600 {
			return fmt.Errorf("%s must be between 1 and 31622400", name)
		}
	}
	if c.RequestTimeoutSeconds > c.CollectionTimeoutSeconds || c.PollIntervalSeconds+2*c.CollectionTimeoutSeconds >= c.MaxSampleAgeSeconds {
		return fmt.Errorf("request timeout must fit collection timeout; poll interval plus two collection timeouts must be shorter than sample lifetime")
	}
	if len(c.Clusters) == 0 || len(c.Clusters) > 1000 {
		return fmt.Errorf("clusters must contain an explicit inventory of 1 to 1000 clusters")
	}
	seen := map[string]bool{}
	for _, cluster := range c.Clusters {
		key := cluster.Namespace + "/" + cluster.Name
		if !Name(cluster.Namespace) || !Name(cluster.Name) || seen[key] || len(cluster.Repos) == 0 {
			return fmt.Errorf("invalid or duplicate cluster inventory: %s", key)
		}
		seen[key] = true
		for repo, override := range cluster.Repos {
			if !Repo(repo) || override.MaxAgeSeconds < 0 || override.MaxAgeSeconds > 366*24*3600 {
				return fmt.Errorf("invalid repository or freshness override: %s/%s", key, repo)
			}
		}
	}
	return nil
}

// UniqueJSON rejects case-insensitive duplicate keys and trailing JSON documents.
// Key comparison matches encoding/json's case-insensitive struct field matching.
func UniqueJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					key, err := d.Token()
					if err != nil {
						return err
					}
					name, ok := key.(string)
					name = foldKey(name)
					if !ok || seen[name] {
						return fmt.Errorf("duplicate or invalid object key")
					}
					seen[name] = true
					if err := walk(); err != nil {
						return err
					}
				}
			case '[':
				for d.More() {
					if err := walk(); err != nil {
						return err
					}
				}
			default:
				return fmt.Errorf("unexpected delimiter")
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON content")
	}
	return nil
}

func foldKey(key string) string {
	return strings.Map(func(r rune) rune {
		// Choose one representative for the entire Unicode case-folding cycle,
		// including aliases such as the long s that simple lowercasing misses.
		folded := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < folded {
				folded = next
			}
		}
		return folded
	}, key)
}
