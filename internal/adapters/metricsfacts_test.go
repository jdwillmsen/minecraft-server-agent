package adapters

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The exposition below is copied verbatim from the live exporters on
// 2026-09-08, not invented. Every field these commands read -- the
// server_version label, the /metrics.txt path, the zero-vs-absent distinction
// -- was wrong in an earlier draft written from assumption, so the fixtures
// are real output.
const liveMCMonitor = `# HELP minecraft_status_healthy Whether the server responded
# TYPE minecraft_status_healthy gauge
minecraft_status_healthy{server_edition="bedrock",server_host="127.0.0.1",server_port="31132",server_version="1.26.45"} 1
minecraft_status_players_max_count{server_edition="bedrock",server_host="127.0.0.1",server_port="31132",server_version="1.26.45"} 20
minecraft_status_players_online_count{server_edition="bedrock",server_host="127.0.0.1",server_port="31132",server_version="1.26.45"} 5
minecraft_status_response_time_seconds{server_edition="bedrock",server_host="127.0.0.1",server_port="31132",server_version="1.26.45"} 0.000160695
`

const liveBackup = `backup_last_success_timestamp_seconds{backup_job="jdwillmsen-minecraft-fwb-prd-backup",artifact="world"} 1788819879
backup_last_artifact_bytes{backup_job="jdwillmsen-minecraft-fwb-prd-backup",artifact="world"} 398258140
backup_max_age_seconds{backup_job="jdwillmsen-minecraft-fwb-prd-backup",artifact="world"} 172800
backup_last_artifact_quiesced{backup_job="jdwillmsen-minecraft-fwb-prd-backup",artifact="world"} 1
`

func serve(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func factsFor(t *testing.T, mcURL, backupURL string, now time.Time) *MetricsFacts {
	t.Helper()
	f := NewMetricsFacts(NewMetricsClient(mcURL, backupURL, 2*time.Second))
	f.now = func() time.Time { return now }
	return f
}

func TestServerStatusReportsCountsHealthAndVersion(t *testing.T) {
	srv := serve(t, liveMCMonitor)
	got, err := factsFor(t, srv.URL, "", time.Now()).ServerStatus(context.Background())
	if err != nil {
		t.Fatalf("ServerStatus: %v", err)
	}
	for _, want := range []string{"5/20 players online", "server healthy", "1.26.45"} {
		if !strings.Contains(got, want) {
			t.Errorf("ServerStatus = %q, missing %q", got, want)
		}
	}
}

// An unreachable server must be stated plainly rather than reported as zero
// players on a healthy server, which is what reading the count alone would do.
func TestServerStatusSaysSoWhenUnhealthy(t *testing.T) {
	body := strings.Replace(liveMCMonitor, `server_version="1.26.45"} 1`, `server_version="1.26.45"} 0`, 1)
	got, err := factsFor(t, serve(t, body).URL, "", time.Now()).ServerStatus(context.Background())
	if err != nil {
		t.Fatalf("ServerStatus: %v", err)
	}
	if !strings.Contains(got, "NOT responding") {
		t.Errorf("ServerStatus = %q, want an explicit not-responding message", got)
	}
}

func TestVersionReadsTheLabelNotAValue(t *testing.T) {
	got, err := factsFor(t, serve(t, liveMCMonitor).URL, "", time.Now()).Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != "Bedrock 1.26.45." {
		t.Errorf("Version = %q, want %q", got, "Bedrock 1.26.45.")
	}
}

func TestBackupStatusReportsAgeSizeAndQuiesced(t *testing.T) {
	// Two hours after the fixture's backup timestamp.
	now := time.Unix(1788819879, 0).Add(2 * time.Hour)
	got, err := factsFor(t, "", serve(t, liveBackup).URL, now).BackupStatus(context.Background())
	if err != nil {
		t.Fatalf("BackupStatus: %v", err)
	}
	for _, want := range []string{"2 hours ago", "379.8 MB", "clean"} {
		if !strings.Contains(got, want) {
			t.Errorf("BackupStatus = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "WARNING") {
		t.Errorf("BackupStatus = %q, warned about a backup well inside policy", got)
	}
}

// backup_max_age_seconds is policy, not decoration: a backup older than it
// has to be called out, because the whole point of asking is "how much could
// I lose".
func TestBackupStatusWarnsWhenOlderThanPolicy(t *testing.T) {
	now := time.Unix(1788819879, 0).Add(72 * time.Hour) // policy is 48h
	got, err := factsFor(t, "", serve(t, liveBackup).URL, now).BackupStatus(context.Background())
	if err != nil {
		t.Fatalf("BackupStatus: %v", err)
	}
	if !strings.Contains(got, "WARNING") {
		t.Errorf("BackupStatus = %q, want a policy warning at 72h against a 48h limit", got)
	}
}

// The exporter publishes zeros before the first real run. Reporting an age
// derived from the epoch would be arithmetic on a placeholder.
func TestBackupStatusTreatsZeroTimestampAsNeverRun(t *testing.T) {
	body := strings.Replace(liveBackup, "} 1788819879", "} 0", 1)
	got, err := factsFor(t, "", serve(t, body).URL, time.Now()).BackupStatus(context.Background())
	if err != nil {
		t.Fatalf("BackupStatus: %v", err)
	}
	if !strings.Contains(got, "No backup has completed yet") {
		t.Errorf("BackupStatus = %q, want a never-run message", got)
	}
}

// A wrong path returns a 404 HTML body that parses as zero metrics. Without
// the status check that reads as "the backup never ran" rather than
// "you are asking the wrong URL" -- the failure this test exists to prevent.
func TestWrongPathFailsLoudlyRatherThanLookingEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	_, err := factsFor(t, "", srv.URL, time.Now()).BackupStatus(context.Background())
	if err == nil {
		t.Fatal("BackupStatus: want an error for a 404 endpoint, got none")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %v, want it to name the status code", err)
	}
}

// An agent deployed without the exporters is degraded, not broken.
func TestUnconfiguredEndpointsReportThemselves(t *testing.T) {
	f := factsFor(t, "", "", time.Now())
	for name, call := range map[string]func() (string, error){
		"ServerStatus": func() (string, error) { return f.ServerStatus(context.Background()) },
		"Version":      func() (string, error) { return f.Version(context.Background()) },
		"BackupStatus": func() (string, error) { return f.BackupStatus(context.Background()) },
	} {
		got, err := call()
		if err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
		}
		if !strings.Contains(got, "isn't configured") {
			t.Errorf("%s = %q, want an unconfigured message", name, got)
		}
	}
}

// Label values may contain commas; splitting naively would corrupt the
// version this reads.
func TestLabelParsingSurvivesCommasInValues(t *testing.T) {
	body := `minecraft_status_healthy{server_note="a,b",server_version="1.26.45"} 1` + "\n"
	got, err := factsFor(t, serve(t, body).URL, "", time.Now()).Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != "Bedrock 1.26.45." {
		t.Errorf("Version = %q, want the version intact past a comma-bearing label", got)
	}
}
