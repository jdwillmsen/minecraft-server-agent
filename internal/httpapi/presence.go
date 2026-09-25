package httpapi

import "net/http"

// PresenceAPI is the actor presence API: the routes under /v1, and whether
// it has anything to serve.
type PresenceAPI interface {
	http.Handler
	Enabled() bool
}

// MountPresence serves api under /v1/ when it is enabled, and reports
// whether it did. On the same terms as MountAnnouncements: with no tokens
// nothing is mounted, and the paths answer the mux's own 404.
//
// Mounted for the process rather than for a turn as the live agent: every
// route reads and writes Postgres, which a standby reaches as well as the
// leader does.
func (s *Server) MountPresence(api PresenceAPI) bool {
	if api == nil || !api.Enabled() {
		return false
	}
	s.mux.Handle("/v1/", api)
	return true
}
