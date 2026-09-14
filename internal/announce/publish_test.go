package announce

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// publishStore is the smallest store Publish can be observed through.
type publishStore struct {
	enabled   bool
	inserted  []Announcement
	delivered []string
	insertErr error
}

func (s *publishStore) Insert(_ context.Context, a Announcement) (int64, error) {
	if s.insertErr != nil {
		return 0, s.insertErr
	}
	s.inserted = append(s.inserted, a)
	return int64(len(s.inserted)), nil
}
func (s *publishStore) PendingFor(context.Context, string, string, time.Time) ([]Announcement, error) {
	return nil, nil
}
func (s *publishStore) MarkDelivered(_ context.Context, _ int64, xuid string, _ time.Time) error {
	s.delivered = append(s.delivered, xuid)
	return nil
}
func (s *publishStore) Enabled() bool { return s.enabled }

type publishVoice struct{ said, told []string }

func (v *publishVoice) Tell(_ context.Context, xuid, _ string) error {
	v.told = append(v.told, xuid)
	return nil
}
func (v *publishVoice) Say(_ context.Context, m string) error {
	v.said = append(v.said, m)
	return nil
}

type publishRoster []string

func (r publishRoster) Online() []string { return r }

func (r publishRoster) IsOnline(xuid string) bool {
	for _, x := range r {
		if x == xuid {
			return true
		}
	}
	return false
}

func (r publishRoster) Knows() bool { return true }

type publishPerms map[string]string

func (p publishPerms) Resolve(_ context.Context, xuid string) string { return p[xuid] }

func TestPublishStoresThenSendsAndDerivesDelivery(t *testing.T) {
	s := &publishStore{enabled: true}
	v := &publishVoice{}
	d := NewDeliverer(s, v, publishRoster{"a", "b"}, publishPerms{"a": "operator"}, logging.New("error"))

	// A caller claiming broadcast for a permission target is overruled: the
	// row and the send must both whisper.
	id, sent, err := d.Publish(context.Background(), Announcement{
		Body: "backup is stale", Source: SourceEvent, TargetKind: TargetPermission,
		TargetValue: "operator", Delivery: DeliveryBroadcast,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if id != 1 || sent != 1 {
		t.Errorf("Publish = (id %d, sent %d), want (1, 1)", id, sent)
	}
	if got := s.inserted[0].Delivery; got != DeliveryWhisper {
		t.Errorf("stored delivery = %s, want whisper", got)
	}
	if len(v.said) != 0 || len(v.told) != 1 || v.told[0] != "a" {
		t.Errorf("said %v told %v, want one whisper to the operator", v.said, v.told)
	}
}

func TestPublishWithoutAStoreSaysSoAndSendsNothing(t *testing.T) {
	v := &publishVoice{}
	d := NewDeliverer(Nop{}, v, publishRoster{"a"}, publishPerms{}, logging.New("error"))
	if _, _, err := d.Publish(context.Background(), Announcement{Body: "x", TargetKind: TargetEveryone}); !errors.Is(err, ErrDisabled) {
		t.Errorf("err = %v, want ErrDisabled", err)
	}
	if len(v.said)+len(v.told) != 0 {
		t.Error("an announcement that was never stored was sent")
	}
}

func TestPublishSendsNothingWhenTheInsertFails(t *testing.T) {
	s := &publishStore{enabled: true, insertErr: errors.New("connection refused")}
	v := &publishVoice{}
	d := NewDeliverer(s, v, publishRoster{"a"}, publishPerms{}, logging.New("error"))
	if _, _, err := d.Publish(context.Background(), Announcement{Body: "x", TargetKind: TargetEveryone}); err == nil {
		t.Error("a failed insert was reported as published")
	}
	if len(v.said) != 0 {
		t.Error("an announcement with no row behind it was broadcast")
	}
}
