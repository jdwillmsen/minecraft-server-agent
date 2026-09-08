package adapters

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MetricsClient reads Prometheus text-format exposition from the two
// exporters that already run alongside the server.
//
// These commands cannot be served from the console. Bedrock has no `uptime`
// or `version` console command and knows nothing about backups, so the data
// has to come from somewhere the console cannot reach. mc-monitor and the
// backup exporter already publish exactly it, in-namespace and unauthenticated,
// and Prometheus already scrapes both -- so reading them directly adds no
// infrastructure and no dependency on the monitoring stack being healthy.
//
// Scraping the exporters rather than querying Prometheus is deliberate: a
// player asking !backup while Prometheus is down should still get an answer,
// because the thing they are asking about is fine.
type MetricsClient struct {
	mcMonitorURL string
	backupURL    string
	timeout      time.Duration
	http         *http.Client
}

// NewMetricsClient builds a client over the two exporter endpoints. Either
// URL may be empty, in which case the commands backed by it report that they
// are unconfigured rather than failing -- an agent deployed without the
// exporters is degraded, not broken.
func NewMetricsClient(mcMonitorURL, backupURL string, timeout time.Duration) *MetricsClient {
	return &MetricsClient{
		mcMonitorURL: strings.TrimRight(mcMonitorURL, "/"),
		backupURL:    strings.TrimRight(backupURL, "/"),
		timeout:      timeout,
		http:         &http.Client{},
	}
}

// sample is one Prometheus series: its labels and its value.
type sample struct {
	labels map[string]string
	value  float64
}

// fetch retrieves and parses one exposition endpoint.
//
// The parser is deliberately minimal rather than a Prometheus library
// dependency: these two endpoints publish plain gauges with no histograms,
// summaries, or exemplars, and pulling in a parser for that would be a large
// dependency serving four chat commands.
func (c *MetricsClient) fetch(ctx context.Context, url string) (map[string][]sample, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("metrics: build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("metrics: get %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Named explicitly because the backup exporter serves /metrics.txt
		// rather than /metrics, and a misconfigured path returns a 404 body
		// that parses as zero metrics -- which would otherwise read as "the
		// backup has never run" instead of "you are asking the wrong URL".
		return nil, fmt.Errorf("metrics: %s returned HTTP %d", url, resp.StatusCode)
	}

	out := map[string][]sample{}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, s, ok := parseSample(line)
		if !ok {
			continue
		}
		out[name] = append(out[name], s)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("metrics: read %s: %w", url, err)
	}
	return out, nil
}

// parseSample splits one exposition line into a metric name, its labels and
// its value. Returns ok=false for anything it does not understand, so a line
// this does not handle is skipped rather than failing the whole scrape.
func parseSample(line string) (string, sample, bool) {
	valueSep := strings.LastIndex(line, " ")
	if valueSep < 0 {
		return "", sample{}, false
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(line[valueSep+1:]), 64)
	if err != nil {
		return "", sample{}, false
	}

	head := strings.TrimSpace(line[:valueSep])
	name := head
	labels := map[string]string{}

	if open := strings.Index(head, "{"); open >= 0 {
		close := strings.LastIndex(head, "}")
		if close < open {
			return "", sample{}, false
		}
		name = head[:open]
		for _, pair := range splitLabels(head[open+1 : close]) {
			eq := strings.Index(pair, "=")
			if eq < 0 {
				continue
			}
			key := strings.TrimSpace(pair[:eq])
			val := strings.Trim(strings.TrimSpace(pair[eq+1:]), `"`)
			labels[key] = val
		}
	}
	return name, sample{labels: labels, value: value}, true
}

// splitLabels splits a label set on commas that are not inside a quoted
// value. A plain strings.Split would break on any label value containing a
// comma, which the server_version label is not guaranteed to avoid.
func splitLabels(s string) []string {
	var out []string
	var cur strings.Builder
	inQuotes := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			cur.WriteRune(r)
		case r == ',' && !inQuotes:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// first returns the first sample of a metric, or ok=false when absent.
func first(m map[string][]sample, name string) (sample, bool) {
	s, ok := m[name]
	if !ok || len(s) == 0 {
		return sample{}, false
	}
	return s[0], true
}
