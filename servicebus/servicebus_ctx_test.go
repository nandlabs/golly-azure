package servicebus

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"oss.nandlabs.io/golly/messaging"
)

func receivedMsg(body string) *azservicebus.ReceivedMessage {
	seq := int64(42)
	now := time.Now()
	return &azservicebus.ReceivedMessage{
		Body:           []byte(body),
		MessageID:      "id-" + body,
		SequenceNumber: &seq,
		EnqueuedTime:   &now,
		DeliveryCount:  1,
	}
}

func installReceiver(fc *fakeClient, r *fakeReceiver) {
	fc.newReceiverFn = func(target, azservicebus.ReceiveMode) (*fakeReceiver, error) { return r, nil }
}

func TestReceive_QueueAndHeaders(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	obs := &recordingObserver{}
	p.SetObserver(obs)
	r := &fakeReceiver{}
	installReceiver(fc, r)

	rm := receivedMsg("hello")
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	type custom struct{ A int }
	rm.ApplicationProperties = map[string]any{
		"s": "v", "b": true, "i": 1, "i8": int8(2), "i16": int16(3), "i32": int32(4), "i64": int64(5),
		"u8": uint8(6), "u16": uint16(7), "u32": uint32(8), "u64": uint64(9),
		"f32": float32(1.5), "f64": 2.5, "bin": []byte{1}, "t": when, "nil": nil, "c": custom{A: 1},
	}
	rm.SessionID = strp("sess-1")
	r.push(rm)

	u := mustURL(t, "servicebus://ns/orders")
	got, err := p.Receive(u)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if got.ReadAsStr() != "hello" {
		t.Fatalf("body = %q", got.ReadAsStr())
	}
	if r.t.entity != "orders" || r.t.subscription != "" || r.mode != azservicebus.ReceiveModePeekLock {
		t.Fatalf("receiver target %+v mode %v", r.t, r.mode)
	}
	if r.maxSeen[0] != 1 {
		t.Fatalf("Receive should request 1 message, got %d", r.maxSeen[0])
	}
	checks := map[string]func() bool{
		"s":   func() bool { v, _ := got.GetStrHeader("s"); return v == "v" },
		"b":   func() bool { v, _ := got.GetBoolHeader("b"); return v },
		"i":   func() bool { v, _ := got.GetIntHeader("i"); return v == 1 },
		"i8":  func() bool { v, _ := got.GetInt8Header("i8"); return v == 2 },
		"i16": func() bool { v, _ := got.GetInt16Header("i16"); return v == 3 },
		"i32": func() bool { v, _ := got.GetInt32Header("i32"); return v == 4 },
		"i64": func() bool { v, _ := got.GetInt64Header("i64"); return v == 5 },
		"u64": func() bool { v, _ := got.GetInt64Header("u64"); return v == 9 },
		"u8":  func() bool { v, _ := got.GetInt64Header("u8"); return v == 6 },
		"u16": func() bool { v, _ := got.GetInt64Header("u16"); return v == 7 },
		"u32": func() bool { v, _ := got.GetInt64Header("u32"); return v == 8 },
		"f32": func() bool { v, _ := got.GetFloatHeader("f32"); return v == 1.5 },
		"f64": func() bool { v, _ := got.GetFloat64Header("f64"); return v == 2.5 },
		"bin": func() bool { v, _ := got.GetHeader("bin"); return len(v) == 1 },
		"t":   func() bool { v, _ := got.GetStrHeader("t"); return v == when.Format(time.RFC3339Nano) },
		"c":   func() bool { v, _ := got.GetStrHeader("c"); return v == "{1}" },
		"nil": func() bool { _, ok := got.GetStrHeader("nil"); return !ok },
	}
	for k, fn := range checks {
		if !fn() {
			t.Errorf("header %s not mapped", k)
		}
	}
	sb := got.(*MessageServiceBus)
	if messaging.RoutingKeyOf(got) != "sess-1" || sb.SessionID() != "sess-1" || sb.MessageID() != "id-hello" ||
		sb.SequenceNumber() != 42 || sb.DeliveryCount() != 1 || sb.EnqueuedTime().IsZero() || sb.ReceivedMessage() != rm {
		t.Fatal("received accessors / routing key not populated")
	}
	_, receives, _, _ := obs.snapshot()
	if len(receives) != 1 || receives[0] != nil {
		t.Fatalf("observer receives %v", receives)
	}

	// The receiver is cached and reused.
	r.push(receivedMsg("second"))
	if _, err := p.Receive(u); err != nil {
		t.Fatal(err)
	}
	if fc.receiverCount() != 1 {
		t.Fatalf("expected receiver reuse, created %d", fc.receiverCount())
	}
}

func TestReceive_PartitionKeyBecomesRoutingKey(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	r := &fakeReceiver{}
	installReceiver(fc, r)
	rm := receivedMsg("x")
	rm.PartitionKey = strp("pk")
	r.push(rm)
	got, err := p.Receive(mustURL(t, "servicebus://ns/q"))
	if err != nil {
		t.Fatal(err)
	}
	if messaging.RoutingKeyOf(got) != "pk" {
		t.Fatalf("routing key = %q", messaging.RoutingKeyOf(got))
	}
}

func TestReceive_SubscriptionTargets(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	r := &fakeReceiver{}
	installReceiver(fc, r)
	for _, raw := range []string{
		"servicebus://ns/events?subscription=audit",
		"servicebus://ns/events/subscriptions/audit",
	} {
		r.push(receivedMsg("e"))
		if _, err := p.Receive(mustURL(t, raw)); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if r.t.entity != "events" || r.t.subscription != "audit" {
			t.Fatalf("%s: target %+v", raw, r.t)
		}
	}
	// OptSubscription + dead-letter sub-queue.
	r.push(receivedMsg("e"))
	if _, err := p.Receive(mustURL(t, "servicebus://ns/events"), messaging.Option{Key: OptSubscription, Value: "billing"}, messaging.Option{Key: OptSubQueue, Value: "deadletter"}); err != nil {
		t.Fatal(err)
	}
	if r.t.subscription != "billing" || r.t.subQueue != azservicebus.SubQueueDeadLetter {
		t.Fatalf("target %+v", r.t)
	}
}

func TestReceiveBatch(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	r := &fakeReceiver{}
	installReceiver(fc, r)
	for i := 0; i < 5; i++ {
		r.push(receivedMsg("b"))
	}
	u := mustURL(t, "servicebus://ns/q")
	msgs, err := p.ReceiveBatch(u, messaging.Option{Key: OptBatchSize, Value: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || r.maxSeen[0] != 3 {
		t.Fatalf("got %d msgs, max %v", len(msgs), r.maxSeen)
	}
	msgs, err = p.ReceiveBatchCtx(context.Background(), u)
	if err != nil || len(msgs) != 2 || r.maxSeen[1] != defaultBatchSize {
		t.Fatalf("second batch: %d %v %v", len(msgs), err, r.maxSeen)
	}
}

func TestReceive_NoMessagesAndErrors(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	obs := &recordingObserver{}
	p.SetObserver(obs)
	r := &fakeReceiver{}
	installReceiver(fc, r)
	u := mustURL(t, "servicebus://ns/q")

	// Empty result → ErrNoMessages (not reported to the observer).
	r.recvFn = func(context.Context, int) ([]*azservicebus.ReceivedMessage, error) { return nil, nil }
	if _, err := p.Receive(u); !errors.Is(err, ErrNoMessages) {
		t.Fatalf("expected ErrNoMessages, got %v", err)
	}
	// Timeout code → ErrNoMessages.
	r.recvFn = func(context.Context, int) ([]*azservicebus.ReceivedMessage, error) {
		return nil, sbErr(azservicebus.CodeTimeout)
	}
	if _, err := p.ReceiveBatch(u); !errors.Is(err, ErrNoMessages) {
		t.Fatalf("expected ErrNoMessages, got %v", err)
	}
	// Wait-time expiry (fake blocks until ctx deadline) → ErrNoMessages.
	r.recvFn = nil
	start := time.Now()
	if _, err := p.Receive(u, messaging.Option{Key: OptWaitTimeSeconds, Value: 1}); !errors.Is(err, ErrNoMessages) {
		t.Fatalf("expected ErrNoMessages, got %v", err)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatal("wait time not honoured")
	}
	_, receives, _, _ := obs.snapshot()
	if len(receives) != 0 {
		t.Fatalf("empty receives must not hit the observer: %v", receives)
	}

	// Other errors are wrapped and observed; fatal ones evict the receiver.
	r.recvFn = func(context.Context, int) ([]*azservicebus.ReceivedMessage, error) {
		return nil, sbErr(azservicebus.CodeConnectionLost)
	}
	if _, err := p.Receive(u); err == nil || errors.Is(err, ErrNoMessages) {
		t.Fatalf("expected receive failure, got %v", err)
	}
	if !r.closed.Load() {
		t.Fatal("fatal error should close the cached receiver")
	}
	_, receives, _, _ = obs.snapshot()
	if len(receives) != 1 || receives[0] == nil {
		t.Fatalf("observer receives %v", receives)
	}

	// Option / URL / receiver creation errors.
	if _, err := p.Receive(mustURL(t, "servicebus://ns")); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("expected ErrInvalidURL, got %v", err)
	}
	if _, err := p.Receive(u, messaging.Option{Key: OptReceiveMode, Value: "nope"}); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("expected ErrInvalidOption, got %v", err)
	}
	if _, err := p.Receive(u, messaging.Option{Key: messaging.ConsumerModeOpt, Value: messaging.ConsumerPush}); !errors.Is(err, ErrUnsupportedOption) {
		t.Fatalf("expected ErrUnsupportedOption, got %v", err)
	}
	fc.receiverErr = errors.New("denied")
	if _, err := p.Receive(mustURL(t, "servicebus://ns/other")); err == nil || errors.Is(err, ErrNoMessages) {
		t.Fatalf("expected receiver creation error, got %v", err)
	}
	connectClient = func(*url.URL, target) (sbClient, error) { return nil, errors.New("dial") }
	if _, err := p.Receive(mustURL(t, "servicebus://ns2/q")); err == nil {
		t.Fatal("expected client error")
	}
}

func TestReceiveCtx_CancellationPropagates(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	installReceiver(fc, &fakeReceiver{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := p.ReceiveCtx(ctx, mustURL(t, "servicebus://ns/q"), messaging.Option{Key: OptWaitTimeSeconds, Value: 10})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancellation not observed promptly")
	}
}

func TestRsvp_Settlement(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	obs := &recordingObserver{}
	p.SetObserver(obs)
	r := &fakeReceiver{}
	installReceiver(fc, r)
	u := mustURL(t, "servicebus://ns/q")

	recv := func() messaging.Message {
		r.push(receivedMsg("m"))
		m, err := p.Receive(u)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	m := recv()
	if err := m.Rsvp(true); err != nil {
		t.Fatal(err)
	}
	// Second settlement is a no-op.
	if err := m.Rsvp(false); err != nil {
		t.Fatal(err)
	}
	if err := recv().Rsvp(false); err != nil {
		t.Fatal(err)
	}
	if err := recv().Rsvp(false, messaging.Option{Key: messaging.RequeueOpt, Value: false}); err != nil {
		t.Fatal(err)
	}
	if err := recv().Rsvp(false, messaging.Option{Key: OptDeadLetterReason, Value: "bad"}, messaging.Option{Key: OptDeadLetterDescription, Value: "desc"}); err != nil {
		t.Fatal(err)
	}
	if err := recv().Rsvp(false, messaging.Option{Key: OptDefer, Value: true}); err != nil {
		t.Fatal(err)
	}
	if err := recv().Rsvp(false, messaging.Option{Key: messaging.RequeueOpt, Value: true}); err != nil {
		t.Fatal(err)
	}
	c, a, d, df := r.counts()
	if c != 1 || a != 2 || d != 2 || df != 1 {
		t.Fatalf("complete=%d abandon=%d deadletter=%d defer=%d", c, a, d, df)
	}
	if r.deadLettered[0].Reason != nil || *r.deadLettered[1].Reason != "bad" || *r.deadLettered[1].ErrorDescription != "desc" {
		t.Fatal("dead-letter options not passed")
	}
	_, _, acks, nacks := obs.snapshot()
	if acks != 1 || len(nacks) != 5 || !nacks[0] || nacks[1] || nacks[2] || nacks[3] || !nacks[4] {
		t.Fatalf("acks=%d nacks=%v", acks, nacks)
	}

	// Settlement errors are returned and allow a retry.
	r.settleErr = sbErr(azservicebus.CodeLockLost)
	m = recv()
	if err := m.Rsvp(true); err == nil {
		t.Fatal("expected complete error")
	}
	if err := m.Rsvp(false); err == nil {
		t.Fatal("expected abandon error")
	}
	r.settleErr = nil
	if err := m.(*MessageServiceBus).RsvpCtx(context.Background(), true); err != nil {
		t.Fatalf("retry settle: %v", err)
	}
}

func TestReceive_ReceiveAndDeleteMode(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	r := &fakeReceiver{}
	installReceiver(fc, r)
	r.push(receivedMsg("a"), receivedMsg("b"))
	u := mustURL(t, "servicebus://ns/q")

	m, err := p.Receive(u, messaging.Option{Key: OptReceiveMode, Value: ReceiveModeReceiveAndDelete})
	if err != nil {
		t.Fatal(err)
	}
	if r.mode != azservicebus.ReceiveModeReceiveAndDelete {
		t.Fatalf("mode = %v", r.mode)
	}
	if err := m.Rsvp(true); err != nil {
		t.Fatal(err)
	}
	if c, _, _, _ := r.counts(); c != 0 {
		t.Fatal("ReceiveAndDelete messages must not be settled")
	}
	// DeliveryGuarantee AtMostOnce → ReceiveAndDelete.
	if _, err := p.Receive(u, messaging.Option{Key: messaging.DeliveryGuaranteeOpt, Value: messaging.AtMostOnce}); err != nil {
		t.Fatal(err)
	}
	if fc.receiverCount() != 1 {
		t.Fatal("AtMostOnce should reuse the ReceiveAndDelete receiver")
	}
	// Conflict with explicit PeekLock.
	_, err = p.Receive(u, messaging.Option{Key: messaging.DeliveryGuaranteeOpt, Value: messaging.AtMostOnce}, messaging.Option{Key: OptReceiveMode, Value: ReceiveModePeekLock})
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("expected conflict error, got %v", err)
	}
}

func TestReceive_BrokerVerification(t *testing.T) {
	fc, fa := withFakes(t)
	p := &Provider{}
	r := &fakeReceiver{}
	installReceiver(fc, r)
	u := mustURL(t, "servicebus://ns/q")
	fa.info = &entityInfo{maxDeliveryCount: 3, hasMaxDeliveryCount: true, lockDuration: time.Minute, hasLockDuration: true}

	if _, err := p.Receive(u, messaging.Option{Key: messaging.MaxDeliveryAttemptsOpt, Value: 5}); !errors.Is(err, ErrEntityMismatch) {
		t.Fatalf("expected ErrEntityMismatch, got %v", err)
	}
	if fa.forSend[0] {
		t.Fatal("expected receive-side lookup")
	}
	r.push(receivedMsg("ok"))
	if _, err := p.Receive(u, messaging.Option{Key: messaging.MaxDeliveryAttemptsOpt, Value: 3}, messaging.Option{Key: messaging.AckTimeoutOpt, Value: 30 * time.Second}); err != nil {
		t.Fatalf("verified receive: %v", err)
	}
	if err := p.AddListener(u, func(messaging.Message) {}, messaging.Option{Key: messaging.AckTimeoutOpt, Value: 2 * time.Minute}); !errors.Is(err, ErrEntityMismatch) {
		t.Fatalf("listener verification: %v", err)
	}
}

func TestReceive_AcceptNextSession(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	var sess *fakeReceiver
	fc.acceptFn = func(_ context.Context, _ target, id string, _ azservicebus.ReceiveMode) (*fakeReceiver, error) {
		sess = &fakeReceiver{sessionID: id}
		s1, s2 := receivedMsg("1"), receivedMsg("2")
		s1.SessionID, s2.SessionID = strp("A"), strp("A")
		sess.push(s1, s2)
		return sess, nil
	}
	u := mustURL(t, "servicebus://ns/q")
	next := messaging.Option{Key: OptAcceptNextSession, Value: true}
	msgs, err := p.ReceiveBatch(u, next)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || sess.sessionID != "" || messaging.RoutingKeyOf(msgs[0]) != "A" {
		t.Fatalf("unexpected session receive: %d %q", len(msgs), sess.sessionID)
	}
	// The session receiver stays open until every message is settled.
	_ = msgs[0].Rsvp(true)
	if sess.closed.Load() {
		t.Fatal("session closed too early")
	}
	_ = msgs[1].Rsvp(false)
	if !sess.closed.Load() {
		t.Fatal("session should be released after all messages are settled")
	}

	// ReceiveAndDelete releases the session immediately.
	if _, err := p.Receive(u, next, messaging.Option{Key: OptReceiveMode, Value: ReceiveModeReceiveAndDelete}); err != nil {
		t.Fatal(err)
	}
	if !sess.closed.Load() {
		t.Fatal("ReceiveAndDelete session should be closed after receive")
	}

	// Empty session is released and reported as ErrNoMessages.
	fc.acceptFn = func(context.Context, target, string, azservicebus.ReceiveMode) (*fakeReceiver, error) {
		sess = &fakeReceiver{recvFn: func(context.Context, int) ([]*azservicebus.ReceivedMessage, error) { return nil, nil }}
		return sess, nil
	}
	if _, err := p.Receive(u, next); !errors.Is(err, ErrNoMessages) || !sess.closed.Load() {
		t.Fatalf("empty session: %v closed=%v", err, sess.closed.Load())
	}
	// No session available within the wait time.
	fc.acceptFn = func(context.Context, target, string, azservicebus.ReceiveMode) (*fakeReceiver, error) {
		return nil, sbErr(azservicebus.CodeTimeout)
	}
	if _, err := p.Receive(u, next); !errors.Is(err, ErrNoMessages) {
		t.Fatalf("expected ErrNoMessages, got %v", err)
	}
	// Unsettled session receivers are closed by Close.
	fc.acceptFn = func(context.Context, target, string, azservicebus.ReceiveMode) (*fakeReceiver, error) {
		sess = &fakeReceiver{}
		sess.push(receivedMsg("z"))
		return sess, nil
	}
	if _, err := p.Receive(u, next); err != nil {
		t.Fatal(err)
	}
	_ = p.Close()
	if !sess.closed.Load() {
		t.Fatal("Close should release outstanding session receivers")
	}
}

func TestReceive_SpecificSessionIsCached(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	accepts := 0
	sess := &fakeReceiver{}
	fc.acceptFn = func(_ context.Context, _ target, id string, _ azservicebus.ReceiveMode) (*fakeReceiver, error) {
		accepts++
		if id != "S1" {
			t.Errorf("session id = %q", id)
		}
		return sess, nil
	}
	u := mustURL(t, "servicebus://ns/q")
	opt := messaging.Option{Key: OptSessionID, Value: "S1"}
	for i := 0; i < 2; i++ {
		sess.push(receivedMsg("s"))
		if _, err := p.Receive(u, opt); err != nil {
			t.Fatal(err)
		}
	}
	if accepts != 1 {
		t.Fatalf("expected the session receiver to be cached, accepted %d", accepts)
	}
}
