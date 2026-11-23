package servicebus

import (
	"fmt"
	"os"
	"testing"
	"time"

	"oss.nandlabs.io/golly-azure/azcfg"
	"oss.nandlabs.io/golly/messaging"
)

// Emulator smoke tests run against the Azure Service Bus emulator (or a real
// namespace) and are skipped unless SERVICEBUS_TEST_CONNECTION_STRING is set.
//
// Emulator connection string:
//
//	Endpoint=sb://localhost;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=SAS_KEY_VALUE;UseDevelopmentEmulator=true;
//
// The entities default to the emulator's sample config ("queue.1",
// "topic.1" / "subscription.1") and can be overridden with
// SERVICEBUS_TEST_QUEUE, SERVICEBUS_TEST_TOPIC and
// SERVICEBUS_TEST_SUBSCRIPTION.
func emulatorNamespace(t *testing.T) string {
	t.Helper()
	cs := os.Getenv("SERVICEBUS_TEST_CONNECTION_STRING")
	if cs == "" {
		t.Skip("SERVICEBUS_TEST_CONNECTION_STRING not set; skipping Service Bus emulator test")
	}
	const ns = "sbtest"
	cfg := azcfg.NewConfig()
	cfg.SetConnectionString(cs)
	azcfg.Manager.Register(ns, cfg)
	t.Cleanup(func() { azcfg.Manager.Unregister(ns) })
	return ns
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func TestEmulator_QueueRoundTrip(t *testing.T) {
	ns := emulatorNamespace(t)
	p := &Provider{}
	t.Cleanup(func() { _ = p.Close() })
	u := mustURL(t, fmt.Sprintf("servicebus://%s/%s", ns, envOr("SERVICEBUS_TEST_QUEUE", "queue.1")))

	msg := newMsg(t, p, "emulator-hello")
	msg.SetStrHeader("source", "emulator-test")
	msg.SetInt64Header("n", 7)
	if err := p.Send(u, msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got, err := p.Receive(u, messaging.Option{Key: OptWaitTimeSeconds, Value: 10})
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if got.ReadAsStr() != "emulator-hello" {
		t.Fatalf("body = %q", got.ReadAsStr())
	}
	if v, _ := got.GetStrHeader("source"); v != "emulator-test" {
		t.Fatalf("header source = %q", v)
	}
	if v, _ := got.GetInt64Header("n"); v != 7 {
		t.Fatalf("header n = %d", v)
	}
	if err := got.Rsvp(true); err != nil {
		t.Fatalf("Rsvp: %v", err)
	}

	var batch []messaging.Message
	for i := 0; i < 5; i++ {
		batch = append(batch, newMsg(t, p, fmt.Sprintf("batch-%d", i)))
	}
	if err := p.SendBatch(u, batch); err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	received := 0
	deadline := time.Now().Add(30 * time.Second)
	for received < 5 && time.Now().Before(deadline) {
		msgs, err := p.ReceiveBatch(u, messaging.Option{Key: OptWaitTimeSeconds, Value: 5})
		if err != nil {
			continue
		}
		for _, m := range msgs {
			_ = m.Rsvp(true)
			received++
		}
	}
	if received < 5 {
		t.Fatalf("received %d/5 batch messages", received)
	}
}

func TestEmulator_TopicSubscription(t *testing.T) {
	ns := emulatorNamespace(t)
	p := &Provider{}
	t.Cleanup(func() { _ = p.Close() })
	topic := envOr("SERVICEBUS_TEST_TOPIC", "topic.1")
	sub := envOr("SERVICEBUS_TEST_SUBSCRIPTION", "subscription.1")
	sendURL := mustURL(t, fmt.Sprintf("servicebus://%s/%s", ns, topic))
	recvURL := mustURL(t, fmt.Sprintf("servicebus://%s/%s?subscription=%s", ns, topic, sub))

	done := make(chan string, 1)
	if err := p.AddListener(recvURL, func(m messaging.Message) {
		_ = m.Rsvp(true)
		select {
		case done <- m.ReadAsStr():
		default:
		}
	}, messaging.Option{Key: OptWaitTimeSeconds, Value: 2}); err != nil {
		t.Fatalf("AddListener: %v", err)
	}
	if err := p.Send(sendURL, newMsg(t, p, "fan-out")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case body := <-done:
		if body != "fan-out" {
			t.Fatalf("body = %q", body)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("listener did not receive the message")
	}
	if err := p.RemoveListeners(recvURL); err != nil {
		t.Fatal(err)
	}
}
