package adapters

import (
	"context"
	"sync"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/logging"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

// DefaultPermissionsCacheTTL bounds how long a PermissionResolver serves a
// cached permissions.json snapshot before fetching again. The bridge reads
// that file off a mounted volume on every request, so there is no reason
// to hit it once per command.
const DefaultPermissionsCacheTTL = 10 * time.Second

// PermissionResolver resolves a player's plugin.Permission from
// mc-console-bridge's GET /permissions, with a short cache.
type PermissionResolver struct {
	client *BridgeClient
	ttl    time.Duration
	log    *logging.Logger
	now    func() time.Time

	mu        sync.Mutex
	cached    map[string]string
	fetchedAt time.Time
}

// NewPermissionResolver builds a resolver over client, caching a fetched
// permissions.json snapshot for ttl.
func NewPermissionResolver(client *BridgeClient, ttl time.Duration, log *logging.Logger) *PermissionResolver {
	return &PermissionResolver{client: client, ttl: ttl, log: log, now: time.Now}
}

// Resolve returns actorXUID's permission level.
//
// chat.ServerOrigin is a sentinel for console-originated messages
// (e.g. `send-command say ...`), never a real XUID — it will never appear
// in permissions.json, and is trusted at PermissionOperator without ever
// reaching the bridge: the console is already the most privileged actor in
// this system, so mapping it to anything less would be a functional
// regression, and routing it through a lookup that can only ever miss
// would be pointless.
//
// Any other XUID absent from the bridge's map — never seen by the server,
// or a lookup failure — resolves to PermissionVisitor: the least
// privileged real level, so an unrecognised player is never granted more
// trust than someone the server has no record of at all, and a bridge
// outage fails closed rather than open.
func (r *PermissionResolver) Resolve(ctx context.Context, actorXUID string) plugin.Permission {
	if actorXUID == chat.ServerOrigin {
		return plugin.PermissionOperator
	}

	perms, err := r.get(ctx)
	if err != nil {
		r.log.Error("permissions_fetch_failed", logging.Fields{"actor": actorXUID, "error": err.Error()})
		return plugin.PermissionVisitor
	}

	level, ok := perms[actorXUID]
	if !ok {
		return plugin.PermissionVisitor
	}
	switch level {
	case "operator":
		return plugin.PermissionOperator
	case "member":
		return plugin.PermissionMember
	case "visitor":
		return plugin.PermissionVisitor
	default:
		r.log.Error("permissions_unknown_level", logging.Fields{"actor": actorXUID, "level": level})
		return plugin.PermissionVisitor
	}
}

// get returns a permissions.json snapshot, serving the cache when it is
// still within ttl and fetching a fresh one otherwise.
func (r *PermissionResolver) get(ctx context.Context) (map[string]string, error) {
	r.mu.Lock()
	if r.cached != nil && r.now().Sub(r.fetchedAt) < r.ttl {
		cached := r.cached
		r.mu.Unlock()
		return cached, nil
	}
	r.mu.Unlock()

	perms, err := r.client.getPermissions(ctx)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.cached = perms
	r.fetchedAt = r.now()
	r.mu.Unlock()
	return perms, nil
}
