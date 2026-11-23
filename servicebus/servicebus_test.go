package servicebus

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"oss.nandlabs.io/golly/messaging"
)

func TestProvider_Basics(t *testing.T) {
	p := &Provider{}
	if p.Id() != ServiceBusProviderID {
		t.Fatalf("Id = %q", p.Id())
	}
	if s := p.Schemes(); len(s) != 1 || s[0] != ServiceBusScheme {
		t.Fatalf("Schemes = %v", s)
	}
	if err := p.Setup(); err != nil {
		t.Fatal(err)
	}
	var _ messaging.ProducerCtx = p
	var _ messaging.ReceiverCtx = p
	var _ messaging.ObservableProvider = p
	var _ messaging.ListenerRemover = p
	m, err := p.NewMessage(ServiceBusScheme)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.(messaging.Keyed); !ok {
		t.Fatal("message is not Keyed")
	}
	// Outbound Rsvp is a no-op.
	if err := m.Rsvp(true); err != nil {
		t.Fatal(err)
	}
}

func TestProvider_RegisteredWithManager(t *testing.T) {
	m, err := messaging.GetManager().NewMessage(ServiceBusScheme)
	if err != nil {
		t.Fatalf("manager NewMessage: %v", err)
	}
	if _, ok := m.(*MessageServiceBus); !ok {
		t.Fatalf("unexpected message type %T", m)
	}
}

func TestSend_BuildsMessage(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	obs := &recordingObserver{}
	p.SetObserver(obs)

	msg := newMsg(t, p, "hello")
	msg.SetStrHeader("source", "test")
	msg.SetIntHeader("n", 3)
	msg.SetBoolHeader("flag", true)
	at := time.Now().Add(time.Minute)
	u := mustURL(t, "servicebus://ns/orders")
	err := p.Send(u, msg, messaging.NewOptionsBuilder().
		Add(OptSubject, "subj").
		Add(OptCorrelationID, "corr").
		Add(OptContentType, "text/plain").
		Add(OptReplyTo, "replies").
		Add(OptTimeToLive, time.Hour).
		Add(OptScheduledEnqueueTime, at).
		Build()...)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	s := fc.sender("orders")
	if s.sentCount() != 1 {
		t.Fatalf("sent %d", s.sentCount())
	}
	got := s.sent[0]
	if string(got.Body) != "hello" || *got.MessageID != msg.Id() || *got.Subject != "subj" || *got.CorrelationID != "corr" ||
		*got.ContentType != "text/plain" || *got.ReplyTo != "replies" || *got.TimeToLive != time.Hour || !got.ScheduledEnqueueTime.Equal(at) {
		t.Fatalf("unexpected message %+v", got)
	}
	if got.SessionID != nil || got.PartitionKey != nil {
		t.Fatal("unkeyed message must not carry session / partition key")
	}
	if got.ApplicationProperties["source"] != "test" || got.ApplicationProperties["n"] != int64(3) || got.ApplicationProperties["flag"] != true {
		t.Fatalf("unexpected properties %v", got.ApplicationProperties)
	}
	sends, _, _, _ := obs.snapshot()
	if len(sends) != 1 || sends[0] != nil {
		t.Fatalf("observer sends %v", sends)
	}

	// Sender is cached per entity.
	if err := p.Send(u, newMsg(t, p, "again"), messaging.NewOptionsBuilder().Add(OptMessageID, "fixed").Build()...); err != nil {
		t.Fatal(err)
	}
	if s.sentCount() != 2 || *s.sent[1].MessageID != "fixed" {
		t.Fatalf("second send: %d", s.sentCount())
	}
	if len(fc.senders) != 1 {
		t.Fatalf("expected 1 sender, got %d", len(fc.senders))
	}
}

func TestSend_KeyedMapping(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	u := mustURL(t, "servicebus://ns/orders")
	s := fc.sender("orders")

	keyed := func() *MessageServiceBus {
		m := newMsg(t, p, "k")
		m.SetRoutingKey("tenant-42")
		return m
	}
	// Default: SessionID.
	if err := p.Send(u, keyed()); err != nil {
		t.Fatal(err)
	}
	if s.sent[0].SessionID == nil || *s.sent[0].SessionID != "tenant-42" || s.sent[0].PartitionKey != nil {
		t.Fatalf("default mapping: %+v", s.sent[0])
	}
	// PartitionKey mapping.
	if err := p.Send(u, keyed(), messaging.Option{Key: OptKeyMapping, Value: KeyMappingPartitionKey}); err != nil {
		t.Fatal(err)
	}
	if s.sent[1].SessionID != nil || *s.sent[1].PartitionKey != "tenant-42" {
		t.Fatalf("partition mapping: %+v", s.sent[1])
	}
	// None.
	if err := p.Send(u, keyed(), messaging.Option{Key: OptKeyMapping, Value: KeyMappingNone}); err != nil {
		t.Fatal(err)
	}
	if s.sent[2].SessionID != nil || s.sent[2].PartitionKey != nil {
		t.Fatalf("none mapping: %+v", s.sent[2])
	}
	// Explicit SessionID wins over the routing key; matching PartitionKey OK.
	if err := p.Send(u, keyed(), messaging.Option{Key: OptSessionID, Value: "explicit"}, messaging.Option{Key: OptPartitionKey, Value: "explicit"}); err != nil {
		t.Fatal(err)
	}
	if *s.sent[3].SessionID != "explicit" || *s.sent[3].PartitionKey != "explicit" {
		t.Fatalf("explicit: %+v", s.sent[3])
	}
	// Mismatched session / partition key.
	if err := p.Send(u, keyed(), messaging.Option{Key: OptPartitionKey, Value: "other"}); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("expected ErrInvalidOption, got %v", err)
	}
}

func TestSend_Errors(t *testing.T) {
	fc, fa := withFakes(t)
	p := &Provider{}
	obs := &recordingObserver{}
	p.SetObserver(obs)

	if err := p.Send(mustURL(t, "servicebus://ns"), newMsg(t, p, "x")); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("expected ErrInvalidURL, got %v", err)
	}
	u := mustURL(t, "servicebus://ns/orders")
	if err := p.Send(u, newMsg(t, p, "x"), messaging.Option{Key: messaging.AckTimeoutOpt, Value: time.Second}); !errors.Is(err, ErrUnsupportedOption) {
		t.Fatalf("expected ErrUnsupportedOption, got %v", err)
	}
	if err := p.Send(u, newMsg(t, p, "x"), messaging.Option{Key: OptTimeToLive, Value: "x"}); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("expected ErrInvalidOption, got %v", err)
	}
	// ExactlyOnce requires duplicate detection.
	fa.info = &entityInfo{}
	eo := messaging.Option{Key: messaging.DeliveryGuaranteeOpt, Value: messaging.ExactlyOnce}
	if err := p.Send(u, newMsg(t, p, "x"), eo); !errors.Is(err, ErrEntityMismatch) {
		t.Fatalf("expected ErrEntityMismatch, got %v", err)
	}
	fa.info = &entityInfo{requiresDuplicateDetection: true}
	if err := p.Send(u, newMsg(t, p, "x"), eo); err != nil {
		t.Fatalf("exactly-once send: %v", err)
	}
	if len(fa.forSend) == 0 || !fa.forSend[0] {
		t.Fatal("expected send-side entity lookup")
	}

	// SDK send error is wrapped; fatal errors evict the cached sender.
	s := fc.sender("orders")
	s.sendFn = func(context.Context, *azservicebus.Message) error { return sbErr(azservicebus.CodeConnectionLost) }
	if err := p.Send(u, newMsg(t, p, "x")); err == nil || !strings.Contains(err.Error(), "send failed") {
		t.Fatalf("expected send failure, got %v", err)
	}
	if !s.closed.Load() {
		t.Fatal("fatal error should close and evict the sender")
	}
	p.mu.Lock()
	_, cached := p.senders["ns/orders"]
	p.mu.Unlock()
	if cached {
		t.Fatal("sender still cached after fatal error")
	}
	sends, _, _, _ := obs.snapshot()
	if sends[len(sends)-1] == nil {
		t.Fatal("observer should see the send error")
	}

	// Sender / client creation failures.
	fc.senderErr = errors.New("boom")
	if err := p.Send(u, newMsg(t, p, "x")); err == nil {
		t.Fatal("expected sender creation error")
	}
	connectClient = func(*url.URL, target) (sbClient, error) { return nil, errors.New("dial") }
	if err := p.Send(mustURL(t, "servicebus://other/q"), newMsg(t, p, "x")); err == nil {
		t.Fatal("expected client creation error")
	}
}

func TestSendCtx_CancellationPropagates(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	fc.sender("orders").sendFn = func(ctx context.Context, _ *azservicebus.Message) error {
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := p.SendCtx(ctx, mustURL(t, "servicebus://ns/orders"), newMsg(t, p, "x"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}

func TestSendBatch_SplitsIntoBatches(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	obs := &recordingObserver{}
	p.SetObserver(obs)
	s := fc.sender("events")
	s.batchMax = 3

	var msgs []messaging.Message
	for i := 0; i < 7; i++ {
		m := newMsg(t, p, "m")
		m.SetRoutingKey("k")
		msgs = append(msgs, m)
	}
	u := mustURL(t, "servicebus://ns/events?subscription=ignored-for-send")
	if err := p.SendBatch(u, msgs, messaging.Option{Key: OptMessageID, Value: "ignored"}); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if len(s.batches) != 3 || len(s.batches[0]) != 3 || len(s.batches[2]) != 1 {
		t.Fatalf("unexpected batches %d", len(s.batches))
	}
	if *s.batches[0][0].MessageID != msgs[0].Id() || *s.batches[0][0].SessionID != "k" {
		t.Fatalf("batch message not built from golly message: %+v", s.batches[0][0])
	}
	sends, _, _, _ := obs.snapshot()
	if len(sends) != 7 {
		t.Fatalf("expected 7 OnSend, got %d", len(sends))
	}
	if err := p.SendBatch(u, nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
}

func TestSendBatch_Errors(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	obs := &recordingObserver{}
	p.SetObserver(obs)
	u := mustURL(t, "servicebus://ns/events")
	s := fc.sender("events")

	big := newMsg(t, p, strings.Repeat("x", 2048))
	if err := p.SendBatch(u, []messaging.Message{newMsg(t, p, "a"), big}); !errors.Is(err, azservicebus.ErrMessageTooLarge) {
		t.Fatalf("expected ErrMessageTooLarge, got %v", err)
	}
	if len(s.batches) != 1 {
		t.Fatalf("first message should have been sent before the oversize one, batches=%d", len(s.batches))
	}
	sends, _, _, _ := obs.snapshot()
	if len(sends) != 2 || sends[0] != nil || sends[1] == nil {
		t.Fatalf("observer: %v", sends)
	}

	s.batchErr = errors.New("rejected")
	if err := p.SendBatch(u, []messaging.Message{newMsg(t, p, "a")}); err == nil || !strings.Contains(err.Error(), "batch send failed") {
		t.Fatalf("expected batch send failure, got %v", err)
	}
	s.batchErr = nil
	s.newBatch = errors.New("no batch")
	if err := p.SendBatch(u, []messaging.Message{newMsg(t, p, "a")}); err == nil || !strings.Contains(err.Error(), "create batch") {
		t.Fatalf("expected create batch failure, got %v", err)
	}
	s.newBatch = nil

	m := newMsg(t, p, "a")
	m.SetRoutingKey("k")
	if err := p.SendBatch(u, []messaging.Message{m}, messaging.Option{Key: OptPartitionKey, Value: "other"}); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("expected ErrInvalidOption, got %v", err)
	}
	if err := p.SendBatch(mustURL(t, "servicebus://ns"), []messaging.Message{m}); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("expected ErrInvalidURL, got %v", err)
	}
	fc.senderErr = errors.New("nope")
	if err := p.SendBatch(mustURL(t, "servicebus://ns/new"), []messaging.Message{newMsg(t, p, "a")}); err == nil {
		t.Fatal("expected sender error")
	}
}

func TestSendBatch_NonProviderMessage(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	base, _ := messaging.NewBaseMessage()
	bm := &plainMsg{BaseMessage: base}
	_, _ = bm.SetBodyStr("plain")
	bm.SetStrHeader("h", "v") // not tracked: plain BaseMessage headers cannot be enumerated
	if err := p.SendBatch(mustURL(t, "servicebus://ns/q"), []messaging.Message{bm}); err != nil {
		t.Fatal(err)
	}
	got := fc.sender("q").batches[0][0]
	if !bytes.Equal(got.Body, []byte("plain")) || got.ApplicationProperties != nil {
		t.Fatalf("unexpected message %+v", got)
	}
}

func TestProvider_CloseReleasesResources(t *testing.T) {
	fc, _ := withFakes(t)
	p := &Provider{}
	u := mustURL(t, "servicebus://ns/orders")
	if err := p.Send(u, newMsg(t, p, "x")); err != nil {
		t.Fatal(err)
	}
	r := &fakeReceiver{}
	r.push(&azservicebus.ReceivedMessage{Body: []byte("a")})
	fc.newReceiverFn = func(target, azservicebus.ReceiveMode) (*fakeReceiver, error) { return r, nil }
	if _, err := p.Receive(u); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if !fc.closed.Load() || !fc.sender("orders").closed.Load() || !r.closed.Load() {
		t.Fatal("Close must close client, senders and receivers")
	}
	if err := p.AddListener(u, func(messaging.Message) {}); !errors.Is(err, messaging.ErrProviderClosed) {
		t.Fatalf("expected ErrProviderClosed, got %v", err)
	}
	if err := p.Setup(); err != nil {
		t.Fatal(err)
	}
	// Re-armed provider reconnects lazily.
	if err := p.Send(u, newMsg(t, p, "y")); err != nil {
		t.Fatalf("send after Setup: %v", err)
	}
}

func TestMessageServiceBus_Headers(t *testing.T) {
	p := &Provider{}
	m := newMsg(t, p, "x")
	m.SetHeader("bin", []byte{1, 2})
	m.SetStrHeader("s", "v")
	m.SetBoolHeader("b", true)
	m.SetIntHeader("i", 1)
	m.SetInt8Header("i8", 2)
	m.SetInt16Header("i16", 3)
	m.SetInt32Header("i32", 4)
	m.SetInt64Header("i64", 5)
	m.SetFloatHeader("f32", 1.5)
	m.SetFloat64Header("f64", 2.5)
	props := m.ApplicationProperties()
	want := map[string]any{"s": "v", "b": true, "i": int64(1), "i8": int8(2), "i16": int16(3), "i32": int32(4), "i64": int64(5), "f32": float32(1.5), "f64": 2.5}
	for k, v := range want {
		if props[k] != v {
			t.Errorf("prop %s = %#v, want %#v", k, props[k], v)
		}
	}
	if !bytes.Equal(props["bin"].([]byte), []byte{1, 2}) {
		t.Errorf("bin prop = %v", props["bin"])
	}
	// Returned map is a copy.
	props["s"] = "changed"
	if m.ApplicationProperties()["s"] != "v" {
		t.Fatal("ApplicationProperties must return a copy")
	}
	if v, _ := m.GetInt32Header("i32"); v != 4 {
		t.Fatal("base header not set")
	}
	if (&MessageServiceBus{}).ApplicationProperties() != nil {
		t.Fatal("empty message should have nil properties")
	}
	// Accessors on an outbound message.
	if m.ReceivedMessage() != nil || m.MessageID() != "" || m.SessionID() != "" || m.SequenceNumber() != 0 || m.DeliveryCount() != 0 || !m.EnqueuedTime().IsZero() {
		t.Fatal("outbound accessors should be zero")
	}
}

// plainMsg is a non-provider message (headers cannot be enumerated).
type plainMsg struct{ *messaging.BaseMessage }

func (*plainMsg) Rsvp(bool, ...messaging.Option) error { return nil }
