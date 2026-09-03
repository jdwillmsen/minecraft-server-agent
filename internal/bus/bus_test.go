package bus

import (
	"testing"
	"time"
)

type testEvent struct {
	kind    string
	payload string
}

func (e testEvent) Kind() string { return e.kind }

func TestPublishDeliversToSubscriber(t *testing.T) {
	b := New()
	ch, _ := b.Subscribe("chat", 4)

	b.Publish(testEvent{kind: "chat", payload: "hello"})

	select {
	case ev := <-ch:
		got, ok := ev.(testEvent)
		if !ok || got.payload != "hello" {
			t.Errorf("got %+v, want payload=hello", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestPublishOnlyReachesMatchingKind(t *testing.T) {
	b := New()
	chatCh, _ := b.Subscribe("chat", 4)
	joinCh, _ := b.Subscribe("join", 4)

	b.Publish(testEvent{kind: "chat", payload: "hi"})

	select {
	case <-chatCh:
	case <-time.After(time.Second):
		t.Fatal("chat subscriber did not receive its event")
	}

	select {
	case ev := <-joinCh:
		t.Fatalf("join subscriber should not have received an event, got %+v", ev)
	default:
	}
}

func TestPublishFansOutToMultipleSubscribers(t *testing.T) {
	b := New()
	a, _ := b.Subscribe("chat", 4)
	c, _ := b.Subscribe("chat", 4)

	b.Publish(testEvent{kind: "chat", payload: "fanout"})

	for _, ch := range []<-chan Event{a, c} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("subscriber did not receive fanned-out event")
		}
	}
}

func TestPublishWithNoSubscribersDoesNotPanic(t *testing.T) {
	b := New()
	b.Publish(testEvent{kind: "nobody-listening"})
}

func TestPublishDropsWhenBufferFull(t *testing.T) {
	b := New()
	ch, _ := b.Subscribe("chat", 1)

	// Fill the buffer, then publish again without draining; this must not
	// block.
	done := make(chan struct{})
	go func() {
		b.Publish(testEvent{kind: "chat", payload: "first"})
		b.Publish(testEvent{kind: "chat", payload: "second"})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a full subscriber buffer")
	}

	// Only the first event should be present; the second was dropped.
	ev := <-ch
	got := ev.(testEvent)
	if got.payload != "first" {
		t.Errorf("payload = %q, want first", got.payload)
	}
	select {
	case leftover := <-ch:
		t.Errorf("unexpected second event delivered: %+v", leftover)
	default:
	}
}

func TestSubscriberCount(t *testing.T) {
	b := New()
	if got := b.SubscriberCount("chat"); got != 0 {
		t.Errorf("SubscriberCount before any Subscribe = %d, want 0", got)
	}
	b.Subscribe("chat", 1)
	b.Subscribe("chat", 1)
	if got := b.SubscriberCount("chat"); got != 2 {
		t.Errorf("SubscriberCount = %d, want 2", got)
	}
}

func TestUnsubscribe_RemovesTheChannel(t *testing.T) {
	b := New()
	_, unsubscribe := b.Subscribe("chat", 1)
	if got := b.SubscriberCount("chat"); got != 1 {
		t.Fatalf("SubscriberCount before unsubscribe = %d, want 1", got)
	}

	unsubscribe()

	if got := b.SubscriberCount("chat"); got != 0 {
		t.Errorf("SubscriberCount after unsubscribe = %d, want 0", got)
	}
	// Publishing afterward must not block or panic trying to reach a
	// channel nobody drains anymore.
	done := make(chan struct{})
	go func() {
		b.Publish(testEvent{kind: "chat", payload: "after-unsubscribe"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked after the only subscriber unsubscribed")
	}
}

func TestUnsubscribe_LeavesOtherSubscribersIntact(t *testing.T) {
	b := New()
	a, unsubA := b.Subscribe("chat", 4)
	c, _ := b.Subscribe("chat", 4)

	unsubA()
	b.Publish(testEvent{kind: "chat", payload: "still-here"})

	select {
	case ev := <-c:
		if ev.(testEvent).payload != "still-here" {
			t.Errorf("got %+v, want payload=still-here", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("remaining subscriber did not receive the event")
	}
	select {
	case ev := <-a:
		t.Fatalf("unsubscribed channel received an event: %+v", ev)
	default:
	}
}

func TestUnsubscribe_IsIdempotent(t *testing.T) {
	b := New()
	_, unsubscribe := b.Subscribe("chat", 1)
	unsubscribe()
	unsubscribe() // must not panic on double-close-style misuse
	if got := b.SubscriberCount("chat"); got != 0 {
		t.Errorf("SubscriberCount = %d, want 0", got)
	}
}
