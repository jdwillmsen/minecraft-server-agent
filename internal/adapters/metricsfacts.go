package adapters

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

// MetricsFacts answers the server-information commands that the console
// cannot: !online, !version and !backup.
type MetricsFacts struct {
	client *MetricsClient
	// now is injected so the age arithmetic in BackupStatus is testable
	// without freezing the process clock.
	now func() time.Time
}

// NewMetricsFacts builds a MetricsFacts over client.
func NewMetricsFacts(client *MetricsClient) *MetricsFacts {
	return &MetricsFacts{client: client, now: time.Now}
}

const notConfigured = "That command isn't configured on this server."

// StatusEnabled reports whether mc-monitor is configured; it backs both
// ServerStatus and Version, which read the same exposition.
func (f *MetricsFacts) StatusEnabled() bool { return f.client != nil && f.client.mcMonitorURL != "" }

// BackupEnabled reports whether the backup exporter is configured.
func (f *MetricsFacts) BackupEnabled() bool { return f.client != nil && f.client.backupURL != "" }

// ServerStatus answers !online: how many players, whether the server is
// answering, and how quickly.
//
// Deliberately more than a count. !players already lists names, so a second
// command repeating them earns nothing; what it cannot tell you is whether
// the server is healthy and responsive, which is the other half of "is the
// server ok" that players actually mean when they ask.
func (f *MetricsFacts) ServerStatus(ctx context.Context) (string, error) {
	if f.client.mcMonitorURL == "" {
		return notConfigured, nil
	}
	m, err := f.client.fetch(ctx, f.client.mcMonitorURL)
	if err != nil {
		return "", fmt.Errorf("metrics facts: server status: %w", err)
	}

	healthy, ok := first(m, "minecraft_status_healthy")
	if !ok {
		return "", fmt.Errorf("metrics facts: server status: mc-monitor published no minecraft_status_healthy")
	}
	if healthy.value != 1 {
		// Reachable exporter, unreachable server. Worth saying plainly: the
		// player asked because something felt wrong, and it is.
		return "Server is NOT responding to status checks. Staff have been alerted by monitoring.", nil
	}

	var parts []string
	online, hasOnline := first(m, "minecraft_status_players_online_count")
	max, hasMax := first(m, "minecraft_status_players_max_count")
	if hasOnline && hasMax {
		parts = append(parts, fmt.Sprintf("%d/%d players online", int(online.value), int(max.value)))
	}
	parts = append(parts, "server healthy")

	if rt, ok := first(m, "minecraft_status_response_time_seconds"); ok {
		parts = append(parts, fmt.Sprintf("responding in %s", formatLatency(rt.value)))
	}
	if v := serverVersion(m); v != "" {
		parts = append(parts, "running "+v)
	}
	return strings.Join(parts, ", ") + ".", nil
}

// Version answers !version with the server build mc-monitor last saw.
//
// The version is a label on every minecraft_status_* series rather than a
// metric value of its own, which is why this reads a label instead of a
// number.
func (f *MetricsFacts) Version(ctx context.Context) (string, error) {
	if f.client.mcMonitorURL == "" {
		return notConfigured, nil
	}
	m, err := f.client.fetch(ctx, f.client.mcMonitorURL)
	if err != nil {
		return "", fmt.Errorf("metrics facts: version: %w", err)
	}
	v := serverVersion(m)
	if v == "" {
		return "", fmt.Errorf("metrics facts: version: no server_version label on any minecraft_status series")
	}
	return "Bedrock " + v + ".", nil
}

// BackupStatus answers !backup: when the world was last saved, how big the
// archive was, and whether it was taken cleanly.
//
// Age is reported rather than a timestamp because a timestamp requires the
// reader to know the server's timezone and do arithmetic in their head, and
// the question behind !backup is almost always "how much could I lose".
func (f *MetricsFacts) BackupStatus(ctx context.Context) (string, error) {
	if f.client.backupURL == "" {
		return notConfigured, nil
	}
	m, err := f.client.fetch(ctx, f.client.backupURL)
	if err != nil {
		return "", fmt.Errorf("metrics facts: backup status: %w", err)
	}

	ts, ok := first(m, "backup_last_success_timestamp_seconds")
	if !ok {
		return "", fmt.Errorf("metrics facts: backup status: exporter published no backup_last_success_timestamp_seconds")
	}
	if ts.value <= 0 {
		// The exporter publishes zeros as placeholders before the first real
		// run. Reporting "56 years ago" would be technically derived from the
		// data and useless.
		return "No backup has completed yet.", nil
	}

	age := f.now().Sub(time.Unix(int64(ts.value), 0))
	parts := []string{"Last world backup " + formatAge(age) + " ago"}

	if size, ok := first(m, "backup_last_artifact_bytes"); ok && size.value > 0 {
		parts = append(parts, formatBytes(size.value))
	}
	if q, ok := first(m, "backup_last_artifact_quiesced"); ok {
		if q.value == 1 {
			parts = append(parts, "taken with the world held (clean)")
		} else {
			// Surfaced rather than hidden: an unquiesced copy is still
			// restorable, but it is not the same guarantee, and a player
			// asking about backups is asking exactly this.
			parts = append(parts, "taken without holding the world (still restorable)")
		}
	}

	msg := strings.Join(parts, ", ") + "."
	if maxAge, ok := first(m, "backup_max_age_seconds"); ok && maxAge.value > 0 {
		if age > time.Duration(maxAge.value)*time.Second {
			msg += " WARNING: that is older than this server's backup policy allows."
		}
	}
	return msg, nil
}

// serverVersion pulls the server_version label off whichever
// minecraft_status_* series carries it.
func serverVersion(m map[string][]sample) string {
	for _, name := range []string{
		"minecraft_status_healthy",
		"minecraft_status_players_online_count",
		"minecraft_status_players_max_count",
	} {
		if s, ok := first(m, name); ok {
			if v := s.labels["server_version"]; v != "" {
				return v
			}
		}
	}
	return ""
}

func formatLatency(seconds float64) string {
	ms := seconds * 1000
	if ms < 1 {
		return "under 1ms"
	}
	return fmt.Sprintf("%.0fms", ms)
}

// formatAge renders a duration the way someone reads it aloud, because these
// strings go to players in chat rather than into a log.
func formatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return pluralise(int(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return pluralise(int(d.Hours()), "hour")
	default:
		return pluralise(int(d.Hours()/24), "day")
	}
}

func pluralise(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func formatBytes(b float64) string {
	const unit = 1024.0
	if b < unit {
		return fmt.Sprintf("%.0f B", b)
	}
	exp := int(math.Log(b) / math.Log(unit))
	if exp > 4 {
		exp = 4
	}
	return fmt.Sprintf("%.1f %cB", b/math.Pow(unit, float64(exp)), "KMGTP"[exp-1])
}
