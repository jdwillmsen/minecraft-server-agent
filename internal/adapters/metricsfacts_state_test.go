package adapters

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The watcher and !backup read the same exporter through the same rule, so
// the readings are pinned here against real exposition text.
func TestBackupStateReadsAgeAndPolicy(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cases := []struct {
		name      string
		body      string
		want      BackupState
		wantStale bool
	}{
		{"placeholder before the first run",
			"backup_last_success_timestamp_seconds 0\nbackup_max_age_seconds 93600\n",
			BackupState{}, false},
		{"fresh",
			fmt.Sprintf("backup_last_success_timestamp_seconds %d\nbackup_max_age_seconds 93600\n", now.Add(-time.Hour).Unix()),
			BackupState{Completed: true, Age: time.Hour, MaxAge: 26 * time.Hour}, false},
		{"stale",
			fmt.Sprintf("backup_last_success_timestamp_seconds %d\nbackup_max_age_seconds 93600\n", now.Add(-30*time.Hour).Unix()),
			BackupState{Completed: true, Age: 30 * time.Hour, MaxAge: 26 * time.Hour}, true},
		{"no policy is never stale",
			fmt.Sprintf("backup_last_success_timestamp_seconds %d\n", now.Add(-300*time.Hour).Unix()),
			BackupState{Completed: true, Age: 300 * time.Hour}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			f := NewMetricsFacts(NewMetricsClient("", srv.URL, time.Second))
			f.now = func() time.Time { return now }

			got, err := f.BackupState(context.Background())
			if err != nil {
				t.Fatalf("BackupState: %v", err)
			}
			if got != tc.want || got.Stale() != tc.wantStale {
				t.Errorf("BackupState = %+v (stale %v), want %+v (stale %v)", got, got.Stale(), tc.want, tc.wantStale)
			}
		})
	}
}

// Unconfigured is an error for a raw reading: a watcher must never compare
// the player-facing "not configured" sentence as if it were a version.
func TestRawReadingsRefuseWithoutTheirExporter(t *testing.T) {
	f := NewMetricsFacts(NewMetricsClient("", "", time.Second))
	if v, err := f.CurrentVersion(context.Background()); err == nil || v != "" {
		t.Errorf("CurrentVersion = (%q, %v), want an error", v, err)
	}
	if _, err := f.BackupState(context.Background()); err == nil {
		t.Error("BackupState without an exporter returned no error")
	}
}
