// Command servicebus demonstrates the golly Azure Service Bus messaging
// provider: queues, topics / subscriptions, batches, listeners, sessions,
// scheduled delivery and settlement (complete / abandon / dead-letter).
//
// Environment:
//
//	SERVICEBUS_NAMESPACE          namespace name or FQDN (default "golly-example")
//	SERVICEBUS_CONNECTION_STRING  optional; when unset DefaultAzureCredential is used
//	SERVICEBUS_QUEUE              queue name (default "golly-sb-example")
//	SERVICEBUS_TOPIC              topic name (default "golly-sb-topic")
//	SERVICEBUS_SUBSCRIPTION       subscription name (default "golly-sb-sub")
//	SERVICEBUS_SKIP_CREATE        set to skip entity creation (required for the emulator,
//	                              whose entities come from its Config.json)
//
// With the Service Bus emulator:
//
//	SERVICEBUS_CONNECTION_STRING='Endpoint=sb://localhost;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=SAS_KEY_VALUE;UseDevelopmentEmulator=true;' \
//	SERVICEBUS_QUEUE=queue.1 SERVICEBUS_TOPIC=topic.1 SERVICEBUS_SUBSCRIPTION=subscription.1 \
//	SERVICEBUS_SKIP_CREATE=1 go run ./examples/servicebus
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus/admin"
	"oss.nandlabs.io/golly-azure/azcfg"
	"oss.nandlabs.io/golly-azure/servicebus"
	"oss.nandlabs.io/golly/messaging"
)

func main() {
	namespace := envOrDefault("SERVICEBUS_NAMESPACE", "golly-example")
	queueName := envOrDefault("SERVICEBUS_QUEUE", "golly-sb-example")
	topicName := envOrDefault("SERVICEBUS_TOPIC", "golly-sb-topic")
	subName := envOrDefault("SERVICEBUS_SUBSCRIPTION", "golly-sb-sub")
	connStr := os.Getenv("SERVICEBUS_CONNECTION_STRING")

	cfg := azcfg.NewConfig()
	if connStr != "" {
		cfg.SetConnectionString(connStr)
		fmt.Println("Using connection string authentication")
	} else {
		fmt.Println("Using DefaultAzureCredential")
	}
	// Register under the namespace (URL host) so every servicebus://<namespace>/... URL resolves it.
	azcfg.Manager.Register(namespace, cfg)

	fmt.Println("=== golly Azure Service Bus Example ===")
	fmt.Printf("Namespace: %s | Queue: %s | Topic: %s/%s\n", namespace, queueName, topicName, subName)

	step("1. Ensure queue, topic and subscription exist")
	if os.Getenv("SERVICEBUS_SKIP_CREATE") == "" {
		ensureEntities(cfg, namespace, queueName, topicName, subName)
	} else {
		fmt.Println("   Skipped (SERVICEBUS_SKIP_CREATE set)")
	}

	mgr := messaging.GetManager()
	setupSignalHandler(mgr)
	queueURL := mustParse(fmt.Sprintf("servicebus://%s/%s", namespace, queueName))
	topicURL := mustParse(fmt.Sprintf("servicebus://%s/%s", namespace, topicName))
	subURL := mustParse(fmt.Sprintf("servicebus://%s/%s?subscription=%s", namespace, topicName, subName))

	waitOpts := messaging.NewOptionsBuilder().Add(servicebus.OptWaitTimeSeconds, 5).Build()

	step("2. Send a single message with headers")
	msg, err := mgr.NewMessage("servicebus")
	check(err, "NewMessage")
	_, err = msg.SetBodyStr("Hello from golly Service Bus!")
	check(err, "SetBodyStr")
	msg.SetStrHeader("source", "servicebus-example")
	msg.SetInt64Header("priority", 5)
	err = mgr.Send(queueURL, msg, messaging.NewOptionsBuilder().
		Add(servicebus.OptSubject, "greeting").
		Add(servicebus.OptContentType, "text/plain").
		Build()...)
	check(err, "Send")
	fmt.Println("   Message sent")

	step("3. Receive and complete (Rsvp true)")
	received, err := mgr.Receive(queueURL, waitOpts...)
	check(err, "Receive")
	fmt.Printf("   Body:   %s\n", received.ReadAsStr())
	if src, ok := received.GetStrHeader("source"); ok {
		fmt.Printf("   Header: source=%s\n", src)
	}
	if prio, ok := received.GetInt64Header("priority"); ok {
		fmt.Printf("   Header: priority=%d\n", prio)
	}
	check(received.Rsvp(true), "Rsvp(true)")
	fmt.Println("   Message completed")

	step("4. Send a batch of 15 messages")
	var msgs []messaging.Message
	for i := 1; i <= 15; i++ {
		m, err := mgr.NewMessage("servicebus")
		check(err, "NewMessage batch")
		_, _ = m.SetBodyStr(fmt.Sprintf("Batch message #%d", i))
		m.SetIntHeader("index", i)
		msgs = append(msgs, m)
	}
	check(mgr.SendBatch(queueURL, msgs), "SendBatch")
	fmt.Println("   15 messages sent (packed into as few AMQP batches as fit)")

	step("5. Receive a batch")
	batch, err := mgr.ReceiveBatch(queueURL, messaging.NewOptionsBuilder().
		Add(servicebus.OptBatchSize, 10).
		Add(servicebus.OptWaitTimeSeconds, 5).
		Build()...)
	check(err, "ReceiveBatch")
	fmt.Printf("   Received %d messages\n", len(batch))
	for _, m := range batch {
		_ = m.Rsvp(true)
	}

	step("6. JSON message")
	jsonMsg, _ := mgr.NewMessage("servicebus")
	check(jsonMsg.WriteJSON(map[string]any{"orderId": "ORD-12345", "total": 99.99}), "WriteJSON")
	check(mgr.Send(queueURL, jsonMsg, messaging.NewOptionsBuilder().Add(servicebus.OptContentType, "application/json").Build()...), "Send JSON")
	drainOne(mgr, queueURL, waitOpts, "JSON")

	step("7. Scheduled delivery (3 seconds)")
	delayed, _ := mgr.NewMessage("servicebus")
	_, _ = delayed.SetBodyStr("I was scheduled 3 seconds ago")
	check(mgr.Send(queueURL, delayed, messaging.NewOptionsBuilder().
		Add(servicebus.OptScheduledEnqueueTime, time.Now().Add(3*time.Second)).
		Add(servicebus.OptTimeToLive, 10*time.Minute).
		Build()...), "Send scheduled")
	fmt.Println("   Scheduled message sent")
	drainOne(mgr, queueURL, messaging.NewOptionsBuilder().Add(servicebus.OptWaitTimeSeconds, 15).Build(), "scheduled")

	step("8. Abandon then dead-letter (Rsvp false)")
	rej, _ := mgr.NewMessage("servicebus")
	_, _ = rej.SetBodyStr("I will be rejected")
	check(mgr.Send(queueURL, rej), "Send reject")
	first, err := mgr.Receive(queueURL, waitOpts...)
	check(err, "Receive for abandon")
	check(first.Rsvp(false), "Rsvp(false)")
	fmt.Println("   Abandoned → redelivered")
	second, err := mgr.Receive(queueURL, waitOpts...)
	check(err, "Receive redelivered")
	if sb, ok := second.(*servicebus.MessageServiceBus); ok {
		fmt.Printf("   Redelivered (DeliveryCount=%d)\n", sb.DeliveryCount())
	}
	check(second.Rsvp(false, messaging.Option{Key: servicebus.OptDeadLetterReason, Value: "example"}), "dead-letter")
	fmt.Println("   Dead-lettered")
	dlqURL := mustParse(fmt.Sprintf("servicebus://%s/%s/$deadletterqueue", namespace, queueName))
	if dl, err := mgr.Receive(dlqURL, waitOpts...); err == nil {
		fmt.Printf("   From DLQ: %s\n", dl.ReadAsStr())
		_ = dl.Rsvp(true)
	} else {
		fmt.Printf("   (DLQ empty: %v)\n", err)
	}

	step("9. Keyed message → SessionID")
	keyed, _ := mgr.NewMessage("servicebus")
	_, _ = keyed.SetBodyStr("routed by key")
	if k, ok := keyed.(messaging.Keyed); ok {
		k.SetRoutingKey("tenant-42")
	}
	check(mgr.Send(queueURL, keyed), "Send keyed")
	fmt.Println("   Sent with SessionID=tenant-42 (FIFO per key on session-enabled entities)")
	drainOne(mgr, queueURL, waitOpts, "keyed")

	step("10. Publish to a topic, listen on a subscription")
	var count atomic.Int32
	check(mgr.AddListener(subURL, func(m messaging.Message) {
		n := count.Add(1)
		fmt.Printf("   [Listener] #%d: %s\n", n, m.ReadAsStr())
		_ = m.Rsvp(true)
	}, messaging.NewOptionsBuilder().
		Add(servicebus.OptWaitTimeSeconds, 2).
		AddNamedListener("example").
		Build()...), "AddListener")
	for i := 1; i <= 5; i++ {
		m, _ := mgr.NewMessage("servicebus")
		_, _ = m.SetBodyStr(fmt.Sprintf("Topic event #%d", i))
		check(mgr.Send(topicURL, m), "Send topic")
	}
	deadline := time.Now().Add(20 * time.Second)
	for count.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf("   Listener processed %d messages\n", count.Load())
	check(mgr.RemoveNamedListener(subURL, "example"), "RemoveNamedListener")
	fmt.Println("   Listener removed")

	step("11. Drain remaining queue messages")
	drained := 0
	drainOpts := messaging.NewOptionsBuilder().Add(servicebus.OptWaitTimeSeconds, 2).Add(servicebus.OptBatchSize, 50).Build()
	for {
		b, err := mgr.ReceiveBatch(queueURL, drainOpts...)
		if err != nil {
			break
		}
		for _, m := range b {
			_ = m.Rsvp(true)
			drained++
		}
	}
	fmt.Printf("   Drained %d messages\n", drained)

	step("12. Graceful shutdown")
	check(mgr.Close(), "Close")
	fmt.Println("   Manager closed: listeners stopped, connections released")

	fmt.Println("\n=== All Service Bus examples completed successfully! ===")
}

// ensureEntities creates the queue, topic and subscription when missing,
// using the Service Bus management plane.
func ensureEntities(cfg *azcfg.Config, namespace, queue, topic, sub string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var (
		client *admin.Client
		err    error
	)
	if cfg.ConnectionString != "" {
		client, err = admin.NewClientFromConnectionString(cfg.ConnectionString, nil)
	} else {
		cred, cerr := cfg.Credential()
		check(cerr, "Credential")
		fqdn := namespace
		if !strings.Contains(fqdn, ".") {
			fqdn += ".servicebus.windows.net"
		}
		client, err = admin.NewClient(fqdn, cred, nil)
	}
	check(err, "admin client")

	if q, err := client.GetQueue(ctx, queue, nil); err == nil && q == nil {
		_, err = client.CreateQueue(ctx, queue, nil)
		check(err, "CreateQueue")
		fmt.Printf("   Created queue %s\n", queue)
	} else {
		check(err, "GetQueue")
	}
	if tp, err := client.GetTopic(ctx, topic, nil); err == nil && tp == nil {
		_, err = client.CreateTopic(ctx, topic, nil)
		check(err, "CreateTopic")
		fmt.Printf("   Created topic %s\n", topic)
	} else {
		check(err, "GetTopic")
	}
	if s, err := client.GetSubscription(ctx, topic, sub, nil); err == nil && s == nil {
		_, err = client.CreateSubscription(ctx, topic, sub, nil)
		check(err, "CreateSubscription")
		fmt.Printf("   Created subscription %s/%s\n", topic, sub)
	} else {
		check(err, "GetSubscription")
	}
}

func drainOne(mgr messaging.Manager, u *url.URL, opts []messaging.Option, label string) {
	m, err := mgr.Receive(u, opts...)
	if errors.Is(err, servicebus.ErrNoMessages) {
		fmt.Printf("   (%s message not received yet)\n", label)
		return
	}
	check(err, "Receive "+label)
	fmt.Printf("   Received %s message: %s\n", label, m.ReadAsStr())
	_ = m.Rsvp(true)
}

func mustParse(raw string) *url.URL {
	u, err := url.Parse(raw)
	check(err, "url.Parse")
	return u
}

func step(name string) {
	fmt.Printf("\n-- %s --\n", name)
}

func check(err error, ctx string) {
	if err != nil {
		log.Fatalf("[ERROR] %s: %v", ctx, err)
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func setupSignalHandler(mgr messaging.Manager) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Printf("\nReceived %s, shutting down...\n", sig)
		_ = mgr.Close()
		os.Exit(0)
	}()
}
