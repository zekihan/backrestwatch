package monitor

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/zekihan/backrestwatch/internal/config"
	"github.com/zekihan/backrestwatch/internal/evidence"
	"github.com/zekihan/backrestwatch/internal/kube"
)

type Collector interface {
	Collect(context.Context) (kube.Sample, error)
}

type Persistence interface {
	Load() (evidence.Ledger, error)
	Save(evidence.Ledger) error
}

type Report struct {
	Healthy      bool              `json:"healthy"`
	Reason       string            `json:"reason,omitempty"`
	SampledAt    *time.Time        `json:"sampledAt,omitempty"`
	Repositories map[string]Result `json:"repositories,omitempty"`
	Scope        string            `json:"scope"`
}

type Monitor struct {
	config    config.Config
	api       Collector
	store     Persistence
	logger    *slog.Logger
	refreshMu sync.Mutex
	mu        sync.RWMutex
	ledger    evidence.Ledger
	loaded    bool
	sampled   time.Time
	results   map[string]Result
	error     string
}

func New(c config.Config, api Collector, store Persistence, logger *slog.Logger) *Monitor {
	return &Monitor{config: c, api: api, store: store, logger: logger, error: "Waiting for initial Kubernetes sample", results: map[string]Result{}}
}

func (m *Monitor) Refresh(ctx context.Context) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(m.config.CollectionTimeoutSeconds)*time.Second)
	defer cancel()
	if !m.loaded {
		ledger, err := m.store.Load()
		if err != nil {
			m.fail("Evidence recovery failed; check the persistent volume and evidence file")
			return
		}
		m.ledger, m.loaded = ledger, true
	}
	sample, err := m.api.Collect(ctx)
	if err != nil {
		// Collector errors may carry sensitive HTTP bodies from an alternate implementation.
		reason := "Kubernetes sample failed; check API access, RBAC, connectivity and timeouts"
		var apiErr *kube.APIError
		if errors.As(err, &apiErr) {
			reason += ": " + apiErr.Error()
		}
		m.fail(reason)
		return
	}
	now := time.Now()
	results, ledger, err := Evaluate(m.config, sample, m.ledger, now)
	if err != nil {
		m.fail("Kubernetes sample rejected: " + err.Error())
		return
	}
	if err := m.store.Save(ledger); err != nil {
		m.fail("Evidence persistence failed; check volume capacity and write permissions")
		return
	}
	m.ledger = ledger
	m.mu.Lock()
	m.results, m.sampled, m.error = results, now, ""
	m.mu.Unlock()
	unhealthy := 0
	for key, result := range results {
		if !result.Healthy {
			unhealthy++
			m.logger.WarnContext(ctx, "Backup completion freshness unhealthy", "repository", key, "reason", result.Reason)
		}
	}
	m.logger.InfoContext(ctx, "Kubernetes sample published", "repositories", len(results), "unhealthy", unhealthy)
}

func (m *Monitor) fail(reason string) {
	m.mu.Lock()
	m.error = reason
	m.mu.Unlock()
	m.logger.Error("Sample unavailable", "reason", reason)
}

func (m *Monitor) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		m.Refresh(ctx)
		timer := time.NewTimer(time.Duration(m.config.PollIntervalSeconds) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (m *Monitor) Report(key string, now time.Time) (int, any) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if key != "" {
		known := false
		for _, cluster := range m.config.Clusters {
			for repo := range cluster.Repos {
				known = known || key == cluster.Namespace+"/"+cluster.Name+"/"+repo
			}
		}
		_, discovered := m.results[key]
		if !repositoryKey(key) || (!known && !discovered) {
			return http.StatusNotFound, Result{Reason: "Unknown backup repository"}
		}
	}
	report := Report{Scope: "backup completion metadata freshness", SampledAt: &m.sampled}
	if m.sampled.IsZero() {
		report.SampledAt = nil
	}
	elapsed := now.Sub(m.sampled).Seconds()
	if m.error != "" {
		report.Reason = m.error
	} else if m.sampled.IsZero() || elapsed < 0 || elapsed > float64(m.config.MaxSampleAgeSeconds) {
		report.Reason = "Kubernetes sample is stale"
	}
	results := make(map[string]Result, len(m.results))
	for name, result := range m.results {
		if result.AgeSeconds != nil {
			age := *result.AgeSeconds + max(0, elapsed)
			result.AgeSeconds = &age
			if result.Healthy && age > float64(result.MaxAgeSeconds) {
				result.Healthy, result.Reason = false, "Last successful backup exceeds maximum age"
			}
		}
		if report.Reason != "" {
			result.Healthy, result.Reason = false, report.Reason
		}
		results[name] = result
	}
	if key != "" {
		result, ok := results[key]
		if !ok {
			result.Reason = report.Reason
		}
		return status(result.Healthy), result
	}
	report.Repositories = results
	report.Healthy = report.Reason == "" && len(results) > 0
	for _, result := range results {
		report.Healthy = report.Healthy && result.Healthy
	}
	return status(report.Healthy), report
}

func status(healthy bool) int {
	if healthy {
		return http.StatusOK
	}
	return http.StatusServiceUnavailable
}
