package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Client struct {
	BaseURL   string
	TokenFile string
	HTTP      *http.Client
	UserAgent string
}

// APIError contains only client-generated diagnostics, never response bodies or credentials.
type APIError struct{ message string }

func (e *APIError) Error() string { return e.message }

func New(base, tokenFile, caFile, version string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("kubernetes API URL must be an HTTPS origin")
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read Kubernetes CA file")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("kubernetes CA file contains no certificates")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		DialContext:         (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout, IdleConnTimeout: 90 * time.Second,
		MaxIdleConnsPerHost: 4, MaxConnsPerHost: 4}
	return &Client{BaseURL: strings.TrimRight(base, "/"), TokenFile: tokenFile, UserAgent: "backrestwatch/" + version,
		HTTP: &http.Client{Transport: transport, Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) Collect(ctx context.Context) (Sample, error) {
	var sample Sample
	var err error
	sample.Clusters, err = list[Cluster](ctx, c, "/apis/"+ClusterAPI+"/postgresclusters", "")
	if err != nil {
		return Sample{}, &APIError{message: err.Error()}
	}
	sample.Jobs, err = list[Job](ctx, c, "/apis/batch/v1/jobs", Prefix+"pgbackrest-backup")
	if err != nil {
		return Sample{}, &APIError{message: err.Error()}
	}
	sample.CronJobs, err = list[CronJob](ctx, c, "/apis/batch/v1/cronjobs", Prefix+"pgbackrest-backup=scheduled")
	if err != nil {
		return Sample{}, &APIError{message: err.Error()}
	}
	return sample, nil
}

func list[T any](ctx context.Context, c *Client, path, selector string) ([]T, error) {
	query := url.Values{"limit": {"200"}}
	if selector != "" {
		query.Set("labelSelector", selector)
	}
	var items []T
	seen := map[string]bool{}
	resourceVersion := ""
	for range 100 {
		data, err := c.page(ctx, path, query)
		if err != nil {
			return nil, err
		}
		var page struct {
			Metadata *struct {
				Continue        string `json:"continue"`
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
			Items *[]T `json:"items"`
		}
		if json.Unmarshal(data, &page) != nil || page.Metadata == nil || page.Items == nil {
			return nil, fmt.Errorf("invalid Kubernetes list response for %s", path)
		}
		if resourceVersion != "" && resourceVersion != page.Metadata.ResourceVersion {
			return nil, fmt.Errorf("kubernetes list snapshot changed for %s", path)
		}
		resourceVersion = page.Metadata.ResourceVersion
		items = append(items, (*page.Items)...)
		if len(items) > 20000 {
			return nil, fmt.Errorf("kubernetes list exceeds item budget for %s", path)
		}
		next := page.Metadata.Continue
		if next == "" {
			return items, nil
		}
		if seen[next] {
			return nil, fmt.Errorf("repeated Kubernetes continuation token for %s", path)
		}
		seen[next] = true
		query.Set("continue", next)
	}
	return nil, fmt.Errorf("kubernetes pagination exceeds page budget for %s", path)
}

func (c *Client) page(ctx context.Context, path string, query url.Values) ([]byte, error) {
	for attempt := range 3 {
		data, retry, err := c.request(ctx, path, query)
		if err == nil || !retry || attempt == 2 {
			return data, err
		}
		delay := (100 * time.Millisecond << attempt) + time.Duration(rand.IntN(100))*time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("kubernetes collection cancelled or timed out")
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("kubernetes retries exhausted")
}

func (c *Client) request(ctx context.Context, path string, query url.Values) ([]byte, bool, error) {
	// Projected service-account tokens rotate independently of the process.
	token, err := os.ReadFile(c.TokenFile)
	if err != nil || strings.TrimSpace(string(token)) == "" {
		return nil, false, fmt.Errorf("cannot read Kubernetes service-account token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path+"?"+query.Encode(), nil)
	if err != nil {
		return nil, false, fmt.Errorf("cannot construct Kubernetes request")
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	response, err := c.HTTP.Do(req)
	if err != nil {
		return nil, ctx.Err() == nil, fmt.Errorf("kubernetes transport failed for %s", path)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		retry := response.StatusCode == 429 || response.StatusCode >= 500 || response.StatusCode == 401
		return nil, retry, fmt.Errorf("kubernetes GET %s returned HTTP %d", path, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 16*1024*1024+1))
	if err != nil || len(data) > 16*1024*1024 {
		return nil, false, fmt.Errorf("kubernetes list page exceeds read budget or is truncated for %s", path)
	}
	return data, false, nil
}
