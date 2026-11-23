package servicebus

import (
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"oss.nandlabs.io/golly/messaging"
)

func resolver(kv ...any) *messaging.OptionsResolver {
	b := messaging.NewOptionsBuilder()
	for i := 0; i+1 < len(kv); i += 2 {
		b.Add(kv[i].(string), kv[i+1])
	}
	return messaging.NewOptionsResolver(b.Build()...)
}

func TestParseSendSettings_All(t *testing.T) {
	at := time.Now().Add(time.Hour)
	ss, err := parseSendSettings(resolver(
		OptSessionID, "s1",
		OptPartitionKey, "s1",
		OptKeyMapping, "PartitionKey",
		OptMessageID, "mid",
		OptCorrelationID, "cid",
		OptSubject, "subj",
		OptContentType, "application/json",
		OptReplyTo, "replies",
		OptTimeToLive, 30*time.Second,
		OptScheduledEnqueueTime, at,
	))
	if err != nil {
		t.Fatalf("parseSendSettings: %v", err)
	}
	if !ss.hasSessionID || ss.sessionID != "s1" || ss.partitionKey != "s1" || ss.keyMapping != KeyMappingPartitionKey ||
		ss.messageID != "mid" || ss.correlationID != "cid" || ss.subject != "subj" || ss.contentType != "application/json" ||
		ss.replyTo != "replies" || !ss.hasTTL || ss.ttl != 30*time.Second || !ss.hasScheduled || !ss.scheduled.Equal(at) {
		t.Fatalf("unexpected settings %+v", ss)
	}
}

func TestParseSendSettings_Defaults(t *testing.T) {
	ss, err := parseSendSettings(resolver())
	if err != nil {
		t.Fatal(err)
	}
	if ss.keyMapping != KeyMappingSession || ss.hasTTL || ss.hasScheduled || ss.hasSessionID {
		t.Fatalf("unexpected defaults %+v", ss)
	}
}

func TestParseSendSettings_KeyMappingAndDelay(t *testing.T) {
	for in, want := range map[string]string{"session": KeyMappingSession, "SessionID": KeyMappingSession, "partition": KeyMappingPartitionKey, "none": KeyMappingNone, "": KeyMappingNone} {
		ss, err := parseSendSettings(resolver(OptKeyMapping, in))
		if err != nil || ss.keyMapping != want {
			t.Errorf("KeyMapping %q → %q, %v; want %q", in, ss.keyMapping, err, want)
		}
	}
	ss, err := parseSendSettings(resolver(OptDelaySeconds, 10, OptTimeToLive, 60))
	if err != nil {
		t.Fatal(err)
	}
	if !ss.hasScheduled || time.Until(ss.scheduled) < 9*time.Second || ss.ttl != time.Minute {
		t.Fatalf("delay/ttl not applied: %+v", ss)
	}
	ss, err = parseSendSettings(resolver(OptDelaySeconds, 0))
	if err != nil || ss.hasScheduled {
		t.Fatalf("zero delay should not schedule: %+v %v", ss, err)
	}
}

func TestParseSendSettings_Errors(t *testing.T) {
	cases := map[string]*messaging.OptionsResolver{
		"session type":    resolver(OptSessionID, 1),
		"partition type":  resolver(OptPartitionKey, 1),
		"keymapping type": resolver(OptKeyMapping, 1),
		"keymapping val":  resolver(OptKeyMapping, "bogus"),
		"subject type":    resolver(OptSubject, true),
		"ttl type":        resolver(OptTimeToLive, "1m"),
		"ttl value":       resolver(OptTimeToLive, -time.Second),
		"scheduled type":  resolver(OptScheduledEnqueueTime, "tomorrow"),
		"delay type":      resolver(OptDelaySeconds, "5"),
		"delay negative":  resolver(OptDelaySeconds, -1),
		"delay+schedule":  resolver(OptDelaySeconds, 5, OptScheduledEnqueueTime, time.Now()),
	}
	for name, r := range cases {
		if _, err := parseSendSettings(r); !errors.Is(err, ErrInvalidOption) {
			t.Errorf("%s: expected ErrInvalidOption, got %v", name, err)
		}
	}
}

func TestParseReceiveSettings(t *testing.T) {
	tg := target{namespace: "ns", entity: "topic"}
	rs, err := parseReceiveSettings(resolver(
		OptReceiveMode, "receive_and_delete",
		OptWaitTimeSeconds, 2,
		OptMaxMessages, 20,
		OptBatchSize, 7,
		OptTimeout, int64(9),
		OptSessionID, "abc",
		OptSubscription, "sub",
		messaging.NamedListener, "group",
	), &tg)
	if err != nil {
		t.Fatal(err)
	}
	if rs.mode != azservicebus.ReceiveModeReceiveAndDelete || !rs.hasMode || rs.wait != 2*time.Second ||
		rs.maxMessages != 20 || rs.batchSize != 7 || rs.timeout != 9*time.Second || !rs.hasTimeout ||
		rs.sessionID != "abc" || !rs.sessionful() || rs.listenerName != "group" || tg.subscription != "sub" {
		t.Fatalf("unexpected settings %+v / %+v", rs, tg)
	}

	tg = target{namespace: "ns", entity: "q"}
	rs, err = parseReceiveSettings(resolver(OptMaxMessages, int32(0), OptSubQueue, "deadletter", OptReceiveMode, azservicebus.ReceiveModePeekLock), &tg)
	if err != nil {
		t.Fatal(err)
	}
	if rs.maxMessages != 1 || rs.batchSize != 1 || tg.subQueue != azservicebus.SubQueueDeadLetter || rs.mode != azservicebus.ReceiveModePeekLock {
		t.Fatalf("unexpected settings %+v / %+v", rs, tg)
	}
	for in, want := range map[string]azservicebus.SubQueue{"transfer": azservicebus.SubQueueTransfer, "none": 0, "DLQ": azservicebus.SubQueueDeadLetter} {
		tg := target{namespace: "ns", entity: "q", subQueue: azservicebus.SubQueueDeadLetter}
		if _, err := parseReceiveSettings(resolver(OptSubQueue, in), &tg); err != nil || tg.subQueue != want {
			t.Errorf("SubQueue %q → %v, %v", in, tg.subQueue, err)
		}
	}

	tg = target{namespace: "ns", entity: "q"}
	rs, err = parseReceiveSettings(resolver(OptBatchSize, 100000, OptAcceptNextSession, true), &tg)
	if err != nil {
		t.Fatal(err)
	}
	if rs.batchSize != maxReceiveMessages || rs.maxMessages != defaultBatchSize || !rs.acceptNextSession || rs.wait != defaultWaitTime || rs.mode != azservicebus.ReceiveModePeekLock {
		t.Fatalf("unexpected settings %+v", rs)
	}
}

func TestParseReceiveSettings_Errors(t *testing.T) {
	cases := map[string]*messaging.OptionsResolver{
		"mode type":          resolver(OptReceiveMode, 3.5),
		"mode string":        resolver(OptReceiveMode, "peek"),
		"mode enum":          resolver(OptReceiveMode, azservicebus.ReceiveMode(42)),
		"wait type":          resolver(OptWaitTimeSeconds, "1"),
		"wait zero":          resolver(OptWaitTimeSeconds, 0),
		"max type":           resolver(OptMaxMessages, 1.0),
		"batch type":         resolver(OptBatchSize, "x"),
		"timeout type":       resolver(OptTimeout, time.Second),
		"timeout zero":       resolver(OptTimeout, 0),
		"session type":       resolver(OptSessionID, 5),
		"next type":          resolver(OptAcceptNextSession, "yes"),
		"session+next":       resolver(OptSessionID, "a", OptAcceptNextSession, true),
		"subscription type":  resolver(OptSubscription, 1),
		"subqueue type":      resolver(OptSubQueue, 1),
		"subqueue value":     resolver(OptSubQueue, "poison"),
		"subqueue+session":   resolver(OptSubQueue, "deadletter", OptAcceptNextSession, true),
		"subscription clash": resolver(OptSubscription, "other"),
	}
	for name, r := range cases {
		tg := target{namespace: "ns", entity: "t", subscription: "sub"}
		if _, err := parseReceiveSettings(r, &tg); !errors.Is(err, ErrInvalidOption) {
			t.Errorf("%s: expected ErrInvalidOption, got %v", name, err)
		}
	}
}

func TestParseBrokerOptions_Receive(t *testing.T) {
	bo, err := parseBrokerOptions(nil, false)
	if err != nil || bo.needsVerification() {
		t.Fatalf("nil resolver: %+v %v", bo, err)
	}
	bo, err = parseBrokerOptions(resolver(
		messaging.DeliveryGuaranteeOpt, messaging.AtMostOnce,
		messaging.ConsumerModeOpt, messaging.ConsumerPull,
		messaging.AckTimeoutOpt, 30*time.Second,
		messaging.MaxDeliveryAttemptsOpt, 5,
		messaging.DeadLetterOpt, "$deadletterqueue",
	), false)
	if err != nil {
		t.Fatal(err)
	}
	if !bo.hasReceiveMode || bo.receiveMode != azservicebus.ReceiveModeReceiveAndDelete || !bo.hasAckTimeout ||
		bo.ackTimeout != 30*time.Second || bo.maxDeliveryAttempts != 5 || bo.deadLetter != "$deadletterqueue" || !bo.needsVerification() {
		t.Fatalf("unexpected %+v", bo)
	}
	bo, err = parseBrokerOptions(resolver(messaging.DeliveryGuaranteeOpt, messaging.AtLeastOnce, messaging.DeadLetterOpt, "orders/$DeadLetterQueue"), false)
	if err != nil || bo.receiveMode != azservicebus.ReceiveModePeekLock || !bo.hasReceiveMode || bo.needsVerification() {
		t.Fatalf("at-least-once + builtin DLQ: %+v %v", bo, err)
	}

	invalid := map[string]*messaging.OptionsResolver{
		"guarantee type":  resolver(messaging.DeliveryGuaranteeOpt, "at-least-once"),
		"guarantee value": resolver(messaging.DeliveryGuaranteeOpt, messaging.DeliveryGuarantee("twice")),
		"mode type":       resolver(messaging.ConsumerModeOpt, "pull"),
		"mode value":      resolver(messaging.ConsumerModeOpt, messaging.ConsumerMode("drip")),
		"ack type":        resolver(messaging.AckTimeoutOpt, 30),
		"ack zero":        resolver(messaging.AckTimeoutOpt, time.Duration(0)),
		"mda type":        resolver(messaging.MaxDeliveryAttemptsOpt, "5"),
		"mda zero":        resolver(messaging.MaxDeliveryAttemptsOpt, 0),
		"dl type":         resolver(messaging.DeadLetterOpt, 1),
		"dl empty":        resolver(messaging.DeadLetterOpt, ""),
	}
	for name, r := range invalid {
		if _, err := parseBrokerOptions(r, false); !errors.Is(err, ErrInvalidOption) {
			t.Errorf("%s: expected ErrInvalidOption, got %v", name, err)
		}
	}
	unsupported := map[string]*messaging.OptionsResolver{
		"exactly once": resolver(messaging.DeliveryGuaranteeOpt, messaging.ExactlyOnce),
		"push":         resolver(messaging.ConsumerModeOpt, messaging.ConsumerPush),
		"ack too long": resolver(messaging.AckTimeoutOpt, 10*time.Minute),
	}
	for name, r := range unsupported {
		if _, err := parseBrokerOptions(r, false); !errors.Is(err, ErrUnsupportedOption) {
			t.Errorf("%s: expected ErrUnsupportedOption, got %v", name, err)
		}
	}
}

func TestParseBrokerOptions_Send(t *testing.T) {
	bo, err := parseBrokerOptions(resolver(messaging.DeliveryGuaranteeOpt, messaging.ExactlyOnce), true)
	if err != nil || !bo.exactlyOnce || !bo.needsVerification() {
		t.Fatalf("exactly-once send: %+v %v", bo, err)
	}
	for _, g := range []messaging.DeliveryGuarantee{messaging.AtLeastOnce, messaging.AtMostOnce} {
		bo, err := parseBrokerOptions(resolver(messaging.DeliveryGuaranteeOpt, g), true)
		if err != nil || bo.hasReceiveMode {
			t.Fatalf("%s send: %+v %v", g, bo, err)
		}
	}
	rejected := map[string]*messaging.OptionsResolver{
		"consumer mode": resolver(messaging.ConsumerModeOpt, messaging.ConsumerPull),
		"ack timeout":   resolver(messaging.AckTimeoutOpt, time.Second),
		"mda":           resolver(messaging.MaxDeliveryAttemptsOpt, 3),
		"dead letter":   resolver(messaging.DeadLetterOpt, "dlq"),
	}
	for name, r := range rejected {
		if _, err := parseBrokerOptions(r, true); !errors.Is(err, ErrUnsupportedOption) {
			t.Errorf("%s: expected ErrUnsupportedOption, got %v", name, err)
		}
	}
}

func TestBrokerOptionsCheck(t *testing.T) {
	tg := target{namespace: "ns", entity: "orders"}
	full := &entityInfo{
		maxDeliveryCount: 10, hasMaxDeliveryCount: true,
		lockDuration: time.Minute, hasLockDuration: true,
		forwardDeadLetteredTo:      "sb://ns.servicebus.windows.net/Orders-DLQ",
		requiresDuplicateDetection: true,
	}
	ok := []*brokerOptions{
		{},
		{exactlyOnce: true},
		{maxDeliveryAttempts: 10, hasMaxDeliveryAttempts: true},
		{ackTimeout: 30 * time.Second, hasAckTimeout: true},
		{deadLetter: "orders-dlq"},
		{deadLetter: "sb://ns.servicebus.windows.net/orders-dlq"},
		{deadLetter: "$deadletterqueue"},
	}
	for i, bo := range ok {
		if err := bo.check(full, tg); err != nil {
			t.Errorf("case %d: unexpected error %v", i, err)
		}
	}
	bad := []struct {
		bo   *brokerOptions
		info *entityInfo
	}{
		{&brokerOptions{exactlyOnce: true}, &entityInfo{}},
		{&brokerOptions{maxDeliveryAttempts: 11, hasMaxDeliveryAttempts: true}, full},
		{&brokerOptions{maxDeliveryAttempts: 1, hasMaxDeliveryAttempts: true}, &entityInfo{}},
		{&brokerOptions{ackTimeout: 2 * time.Minute, hasAckTimeout: true}, full},
		{&brokerOptions{deadLetter: "other"}, full},
		{&brokerOptions{deadLetter: "other"}, &entityInfo{}},
	}
	for i, c := range bad {
		if err := c.bo.check(c.info, tg); !errors.Is(err, ErrEntityMismatch) {
			t.Errorf("bad case %d: expected ErrEntityMismatch, got %v", i, err)
		}
	}
}

func TestBrokerOptionsVerify(t *testing.T) {
	_, fa := withFakes(t)
	u := mustURL(t, "servicebus://ns/orders")
	tg, _ := parseTarget(u)

	// No verification needed → admin not called.
	if err := (&brokerOptions{deadLetter: "$deadletterqueue"}).verify(t.Context(), u, tg, false); err != nil || fa.calls != 0 {
		t.Fatalf("unexpected verify: %v calls=%d", err, fa.calls)
	}
	bo := &brokerOptions{maxDeliveryAttempts: 3, hasMaxDeliveryAttempts: true}
	// Entity missing.
	if err := bo.verify(t.Context(), u, tg, false); !errors.Is(err, ErrEntityMismatch) {
		t.Fatalf("missing entity: %v", err)
	}
	// Admin error.
	fa.err = errors.New("forbidden")
	if err := bo.verify(t.Context(), u, tg, false); err == nil || errors.Is(err, ErrEntityMismatch) {
		t.Fatalf("admin error: %v", err)
	}
	fa.err = nil
	fa.info = &entityInfo{maxDeliveryCount: 5, hasMaxDeliveryCount: true}
	if err := bo.verify(t.Context(), u, tg, false); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// connectAdmin failure.
	connectAdmin = func(*url.URL, target) (sbAdmin, error) { return nil, errors.New("no admin") }
	if err := bo.verify(t.Context(), u, tg, false); err == nil {
		t.Fatal("expected connectAdmin error")
	}
}

func TestForwardTargetMatches(t *testing.T) {
	cases := []struct {
		actual, want string
		match        bool
	}{
		{"", "x", false},
		{"dlq", "dlq", true},
		{"sb://ns.servicebus.windows.net/dlq", "dlq", true},
		{"https://ns.servicebus.windows.net/DLQ/", "dlq", true},
		{"sb://ns.servicebus.windows.net/dlq", "/dlq", true},
		{"sb://ns.servicebus.windows.net/dlq", "other", false},
	}
	for _, c := range cases {
		if got := forwardTargetMatches(c.actual, c.want); got != c.match {
			t.Errorf("forwardTargetMatches(%q,%q)=%v want %v", c.actual, c.want, got, c.match)
		}
	}
}
