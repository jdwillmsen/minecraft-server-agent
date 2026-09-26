package presence

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/audit"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// fakeStore keeps the version rules of the real one in memory, so the
// service, loop, API and chat tests exercise the same conflicts Postgres
// would produce.
type fakeStore struct {
	mu     sync.Mutex
	rows   map[string]presenceapi.Override
	status map[string]presenceapi.Status
	err    error
	// overridesErr fails only the override read, leaving statuses readable.
	overridesErr error
	// statusesErr fails only the status read.
	statusesErr error
	// bumpOnRemove simulates a write landing between a read and a removal.
	bumpOnRemove bool
	// reparkOnRemove simulates an unpark and a re-park landing between a
	// read and a removal: the new row starts again at version 1.
	reparkOnRemove func(presenceapi.Override) presenceapi.Override
}

var _ Store = (*fakeStore)(nil)

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]presenceapi.Override{}, status: map[string]presenceapi.Status{}}
}

func (f *fakeStore) fail(err error)          { f.mu.Lock(); f.err = err; f.mu.Unlock() }
func (f *fakeStore) failOverrides(err error) { f.mu.Lock(); f.overridesErr = err; f.mu.Unlock() }
func (f *fakeStore) failStatuses(err error)  { f.mu.Lock(); f.statusesErr = err; f.mu.Unlock() }
func (f *fakeStore) Enabled() bool           { return true }

func (f *fakeStore) row(id string) (presenceapi.Override, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ov, ok := f.rows[id]
	return ov, ok
}

func (f *fakeStore) Overrides(context.Context) (map[string]presenceapi.Override, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if f.overridesErr != nil {
		return nil, f.overridesErr
	}
	out := make(map[string]presenceapi.Override, len(f.rows))
	for k, v := range f.rows {
		out[k] = v
	}
	return out, nil
}

func (f *fakeStore) upsert(id string, ov presenceapi.Override) Change {
	var prev *presenceapi.Override
	if p, ok := f.rows[id]; ok {
		prev = &p
		ov.Version = p.Version + 1
	} else {
		ov.Version = 1
	}
	f.rows[id] = ov
	return Change{ActorID: id, Prev: prev, Now: &ov}
}

func (f *fakeStore) Set(_ context.Context, id string, ov presenceapi.Override, expect int64) (Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return Change{}, f.err
	}
	var have int64
	if p, ok := f.rows[id]; ok {
		have = p.Version
		if have != expect {
			return Change{ActorID: id, Prev: &p}, ErrConflict
		}
	} else if expect != 0 {
		return Change{ActorID: id}, ErrConflict
	}
	return f.upsert(id, ov), nil
}

func (f *fakeStore) SetMany(_ context.Context, ovs map[string]presenceapi.Override) ([]Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	ids := make([]string, 0, len(ovs))
	for id := range ovs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []Change
	for _, id := range ids {
		out = append(out, f.upsert(id, ovs[id]))
	}
	return out, nil
}

func (f *fakeStore) Clear(_ context.Context, ids []string) ([]Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []Change
	for _, id := range ids {
		if p, ok := f.rows[id]; ok {
			delete(f.rows, id)
			out = append(out, Change{ActorID: id, Prev: &p})
		}
	}
	return out, nil
}

func (f *fakeStore) Remove(_ context.Context, id string, version int64, setAt time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	p, ok := f.rows[id]
	if f.bumpOnRemove && ok {
		p.Version++
		f.rows[id] = p
	}
	if f.reparkOnRemove != nil && ok {
		p = f.reparkOnRemove(p)
		p.Version = 1
		f.rows[id] = p
	}
	if !ok || p.Version != version || !p.SetAt.Equal(setAt) {
		return false, nil
	}
	delete(f.rows, id)
	return true, nil
}

// Statuses honours ctx as a real query would, so a tick whose leadership
// has ended fails here too.
func (f *fakeStore) Statuses(ctx context.Context) (map[string]presenceapi.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.statusesErr != nil {
		return nil, f.statusesErr
	}
	out := make(map[string]presenceapi.Status, len(f.status))
	for k, v := range f.status {
		out[k] = v
	}
	return out, nil
}

func (f *fakeStore) PutStatus(_ context.Context, id string, s presenceapi.Status) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.status[id] = s
	return nil
}

type fakeAudit struct {
	mu      sync.Mutex
	records []audit.Record
}

func (a *fakeAudit) Write(_ context.Context, r audit.Record) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.records = append(a.records, r)
	return nil
}
func (a *fakeAudit) Enabled() bool { return true }
func (a *fakeAudit) all() []audit.Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]audit.Record(nil), a.records...)
}

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := NewRegistry(threeActors(), "agent")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

// newTestService runs on *clock, so a test moves time by assigning to it.
func newTestService(t *testing.T, store *fakeStore, clock *time.Time) (*Service, *fakeAudit) {
	t.Helper()
	aud := &fakeAudit{}
	s := NewService(testRegistry(t), store, aud, logging.New("error"))
	s.now = func() time.Time { return *clock }
	return s, aud
}
