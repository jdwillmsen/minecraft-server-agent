package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakePresenceAPI struct{ enabled bool }

func (f fakePresenceAPI) Enabled() bool { return f.enabled }
func (f fakePresenceAPI) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusTeapot)
}

func TestMountPresence(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		srv, err := New("127.0.0.1:0")
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		mounted := srv.MountPresence(fakePresenceAPI{enabled: enabled})
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/actors", nil))
		_ = srv.ln.Close()
		if mounted != enabled {
			t.Errorf("enabled=%v: MountPresence reported %v", enabled, mounted)
		}
		want := http.StatusNotFound
		if enabled {
			want = http.StatusTeapot
		}
		if rec.Code != want {
			t.Errorf("enabled=%v: /v1/actors answered %d, want %d", enabled, rec.Code, want)
		}
	}
}
