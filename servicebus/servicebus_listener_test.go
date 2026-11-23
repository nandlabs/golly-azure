package servicebus

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"oss.nandlabs.io/golly/messaging"
)

// shortWait keeps listener polls snappy (1s is the option's granularity).
var shortWait = messaging.Option{Key: OptWaitTimeSeconds, Value: 1}

func TestAddListener_DeliversAndSettles(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	t.Cleanup(func() { _ = p.Close() })
	obs := &recordingObserver{}
	p.SetObserver(obs)
	r := &fakeReceiver{}
	installReceiver(fc, r)
	r.push(receivedMsg("1"), receivedMsg("2"), receivedMsg("3"))

	var got atomic.Int32
	err := p.AddListener(mustURL(t, "servicebus://ns/events?subscription=s"), func(m messaging.Message) {
		got.Add(1)
		_ = m.Rsvp(true)
	}, shortWait, messaging.Option{Key: OptMaxMessages, Value: 2})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { c, _, _, _ := r.counts(); return c == 3 }, "3 messages completed")
	if got.Load() != 3 || r.maxSeen[0] != 2 || r.t.subscription != "s" {
		t.Fatalf("got=%d max=%v target=%+v", got.Load(), r.maxSeen, r.t)
	}
	_, receives, acks, _ := obs.snapshot()
	if len(receives) != 3 || acks != 3 {
		t.Fatalf("receives=%d acks=%d", len(receives), acks)
	}
}

func TestAddListener_Validation(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	u := mustURL(t, "servicebus://ns/q")
	if err := p.AddListener(u, nil); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("nil listener: %v", err)
	}
	if err := p.AddListener(mustURL(t, "servicebus://ns"), func(messaging.Message) {}); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("bad URL: %v", err)
	}
	if err := p.AddListener(u, func(messaging.Message) {}, messaging.Option{Key: messaging.ConsumerModeOpt, Value: messaging.ConsumerPush}); !errors.Is(err, ErrUnsupportedOption) {
		t.Fatalf("push: %v", err)
	}
	fc.receiverErr = errors.New("denied")
	if err := p.AddListener(u, func(messaging.Message) {}); err == nil {
		t.Fatal("expected receiver creation error")
	}
	connectClient = func(*url.URL, target) (sbClient, error) { return nil, errors.New("dial") }
	if err := p.AddListener(mustURL(t, "servicebus://ns9/q"), func(messaging.Message) {}); err == nil {
		t.Fatal("expected client error")
	}
}

func TestAddListener_TimeoutAndCtx(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	t.Cleanup(func() { _ = p.Close() })
	var rs []*fakeReceiver
	var mu sync.Mutex
	fc.newReceiverFn = func(target, azservicebus.ReceiveMode) (*fakeReceiver, error) {
		r := &fakeReceiver{}
		mu.Lock()
		rs = append(rs, r)
		mu.Unlock()
		return r, nil
	}
	if err := p.AddListener(mustURL(t, "servicebus://ns/a"), func(messaging.Message) {}, shortWait, messaging.Option{Key: OptTimeout, Value: 1}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := p.AddListenerCtx(ctx, mustURL(t, "servicebus://ns/b"), func(messaging.Message) {}, shortWait); err != nil {
		t.Fatal(err)
	}
	cancel()
	eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(rs) == 2 && rs[0].closed.Load() && rs[1].closed.Load()
	}, "both listeners stopped and closed their receivers")
	eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.listeners) == 0
	}, "stopped listeners are forgotten")
}

func TestListener_RemoveListeners(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	t.Cleanup(func() { _ = p.Close() })
	var rs []*fakeReceiver
	var mu sync.Mutex
	fc.newReceiverFn = func(target, azservicebus.ReceiveMode) (*fakeReceiver, error) {
		r := &fakeReceiver{}
		mu.Lock()
		rs = append(rs, r)
		mu.Unlock()
		return r, nil
	}
	uA := mustURL(t, "servicebus://ns/events?subscription=a")
	uA2 := mustURL(t, "servicebus://ns/events/subscriptions/a")
	uB := mustURL(t, "servicebus://ns/events?subscription=b")
	for _, u := range []string{uA.String(), uA.String(), uB.String()} {
		if err := p.AddListener(mustURL(t, u), func(messaging.Message) {}, shortWait); err != nil {
			t.Fatal(err)
		}
	}
	// Equivalent URL forms address the same listener group.
	if err := p.RemoveListeners(uA2); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return rs[0].closed.Load() && rs[1].closed.Load()
	}, "subscription a listeners stopped")
	mu.Lock()
	bClosed := rs[2].closed.Load()
	mu.Unlock()
	if bClosed {
		t.Fatal("subscription b listener must keep running")
	}
	// Idempotent.
	if err := p.RemoveListeners(uA); err != nil {
		t.Fatal(err)
	}
	if err := p.RemoveListeners(mustURL(t, "servicebus://ns")); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("bad URL: %v", err)
	}
}

func TestListener_RemoveNamedListener(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	t.Cleanup(func() { _ = p.Close() })
	var rs []*fakeReceiver
	var mu sync.Mutex
	fc.newReceiverFn = func(target, azservicebus.ReceiveMode) (*fakeReceiver, error) {
		r := &fakeReceiver{}
		mu.Lock()
		rs = append(rs, r)
		mu.Unlock()
		return r, nil
	}
	u := mustURL(t, "servicebus://ns/q")
	named := messaging.NewOptionsBuilder().AddNamedListener("workers").Build()
	if err := p.AddListener(u, func(messaging.Message) {}, append(named, shortWait)...); err != nil {
		t.Fatal(err)
	}
	if err := p.AddListener(u, func(messaging.Message) {}, shortWait); err != nil {
		t.Fatal(err)
	}
	if err := p.RemoveNamedListener(u, "workers"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return rs[0].closed.Load() }, "named listener stopped")
	mu.Lock()
	unnamedClosed := rs[1].closed.Load()
	mu.Unlock()
	if unnamedClosed {
		t.Fatal("unnamed listener must keep running")
	}
	if err := p.RemoveNamedListener(u, "unknown"); err != nil {
		t.Fatal(err)
	}
	if err := p.RemoveNamedListener(u, ""); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return rs[1].closed.Load() }, "unnamed listener removed by empty name")
	if err := p.RemoveNamedListener(mustURL(t, "servicebus://ns"), "x"); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("bad URL: %v", err)
	}
	// Manager-level removal routes to the provider.
	if err := messaging.GetManager().RemoveNamedListener(mustURL(t, "servicebus://ns/none"), "x"); err != nil {
		t.Fatalf("manager RemoveNamedListener: %v", err)
	}
}

func TestListener_ErrorsBackoffAndRecreate(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	t.Cleanup(func() { _ = p.Close() })
	obs := &recordingObserver{}
	p.SetObserver(obs)

	var created atomic.Int32
	var delivered atomic.Int32
	fc.newReceiverFn = func(target, azservicebus.ReceiveMode) (*fakeReceiver, error) {
		n := created.Add(1)
		r := &fakeReceiver{}
		switch n {
		case 1:
			// Initial receiver: one transient error, then a fatal one.
			var calls atomic.Int32
			r.recvFn = func(context.Context, int) ([]*azservicebus.ReceivedMessage, error) {
				if calls.Add(1) == 1 {
					return nil, errors.New("transient")
				}
				return nil, sbErr(azservicebus.CodeConnectionLost)
			}
		case 2:
			return nil, errors.New("recreate failed once")
		default:
			r.push(receivedMsg("after-recovery"))
		}
		return r, nil
	}
	if err := p.AddListener(mustURL(t, "servicebus://ns/q"), func(m messaging.Message) {
		delivered.Add(1)
		_ = m.Rsvp(true)
	}, shortWait); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return delivered.Load() == 1 }, "listener recovered after fatal error")
	_, receives, _, _ := obs.snapshot()
	if len(receives) < 3 {
		t.Fatalf("expected error receives to be observed, got %v", receives)
	}
}

func TestListener_AcceptNextSessionRotates(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	t.Cleanup(func() { _ = p.Close() })

	var mu sync.Mutex
	var sessions []*fakeReceiver
	var accepts atomic.Int32
	fc.acceptFn = func(ctx context.Context, _ target, id string, _ azservicebus.ReceiveMode) (*fakeReceiver, error) {
		n := accepts.Add(1)
		switch {
		case n == 1:
			return nil, sbErr(azservicebus.CodeTimeout) // no session yet
		case n == 2:
			return nil, errors.New("accept failed") // error + backoff
		case n <= 4:
			r := &fakeReceiver{}
			// Return the message once, then report "empty" immediately.
			var once atomic.Bool
			id := "S" + string(rune('0'+n))
			r.recvFn = func(context.Context, int) ([]*azservicebus.ReceivedMessage, error) {
				if once.CompareAndSwap(false, true) {
					m := receivedMsg(id)
					m.SessionID = &id
					return []*azservicebus.ReceivedMessage{m}, nil
				}
				return nil, nil
			}
			mu.Lock()
			sessions = append(sessions, r)
			mu.Unlock()
			return r, nil
		default:
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}
	var keys sync.Map
	err := p.AddListener(mustURL(t, "servicebus://ns/q"), func(m messaging.Message) {
		keys.Store(messaging.RoutingKeyOf(m), true)
		_ = m.Rsvp(true)
	}, shortWait, messaging.Option{Key: OptAcceptNextSession, Value: true})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		_, a := keys.Load("S3")
		_, b := keys.Load("S4")
		return a && b
	}, "messages from two sessions delivered")
	eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(sessions) == 2 && sessions[0].closed.Load() && sessions[1].closed.Load()
	}, "drained sessions released")
}

func TestListener_SpecificSession(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	sess := &fakeReceiver{}
	fc.acceptFn = func(_ context.Context, _ target, id string, _ azservicebus.ReceiveMode) (*fakeReceiver, error) {
		sess.sessionID = id
		return sess, nil
	}
	sess.push(receivedMsg("a"))
	var got atomic.Int32
	if err := p.AddListener(mustURL(t, "servicebus://ns/q"), func(m messaging.Message) {
		got.Add(1)
	}, shortWait, messaging.Option{Key: OptSessionID, Value: "tenant"}, messaging.Option{Key: OptReceiveMode, Value: ReceiveModeReceiveAndDelete}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return got.Load() == 1 }, "session message delivered")
	if sess.sessionID != "tenant" || sess.mode != azservicebus.ReceiveModeReceiveAndDelete {
		t.Fatalf("session %q mode %v", sess.sessionID, sess.mode)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if !sess.closed.Load() {
		t.Fatal("Close should stop the session listener")
	}
}
