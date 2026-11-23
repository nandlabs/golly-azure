package servicebus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus/admin"
	"oss.nandlabs.io/golly-azure/azcfg"
)

func TestParseTarget(t *testing.T) {
	tests := []struct {
		url     string
		want    target
		wantErr bool
	}{
		{url: "servicebus://myns/orders", want: target{namespace: "myns", entity: "orders"}},
		{url: "servicebus://myns.servicebus.windows.net/orders", want: target{namespace: "myns.servicebus.windows.net", entity: "orders"}},
		{url: "servicebus://myns/events?subscription=audit", want: target{namespace: "myns", entity: "events", subscription: "audit"}},
		{url: "servicebus://myns/events/subscriptions/audit", want: target{namespace: "myns", entity: "events", subscription: "audit"}},
		{url: "servicebus://myns/events/Subscriptions/audit?subscription=audit", want: target{namespace: "myns", entity: "events", subscription: "audit"}},
		{url: "servicebus://myns/orders/$deadletterqueue", want: target{namespace: "myns", entity: "orders", subQueue: azservicebus.SubQueueDeadLetter}},
		{url: "servicebus://myns/orders/$TransferDeadLetterQueue", want: target{namespace: "myns", entity: "orders", subQueue: azservicebus.SubQueueTransfer}},
		{url: "servicebus://myns/events/subscriptions/audit/$deadletterqueue", want: target{namespace: "myns", entity: "events", subscription: "audit", subQueue: azservicebus.SubQueueDeadLetter}},
		{url: "servicebus://myns/orders/", want: target{namespace: "myns", entity: "orders"}},
		{url: "servicebus:///orders", wantErr: true},
		{url: "servicebus://myns", wantErr: true},
		{url: "servicebus://myns/", wantErr: true},
		{url: "servicebus://myns/a/b", wantErr: true},
		{url: "servicebus://myns/events/subscriptions/a?subscription=b", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got, err := parseTarget(mustURL(t, tt.url))
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidURL) {
					t.Fatalf("expected ErrInvalidURL, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
	if _, err := parseTarget(nil); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("nil URL: expected ErrInvalidURL, got %v", err)
	}
}

func TestTargetKeys(t *testing.T) {
	q := target{namespace: "ns", entity: "q"}
	s := target{namespace: "ns", entity: "t", subscription: "s"}
	d := target{namespace: "ns", entity: "q", subQueue: azservicebus.SubQueueDeadLetter}
	x := target{namespace: "ns", entity: "q", subQueue: azservicebus.SubQueueTransfer}
	if q.key() != "ns/q" || q.entityPath() != "q" || q.senderKey() != "ns/q" {
		t.Fatalf("queue keys: %s %s %s", q.key(), q.entityPath(), q.senderKey())
	}
	if s.key() != "ns/t/subscriptions/s" || s.entityPath() != "t/subscriptions/s" || s.senderKey() != "ns/t" {
		t.Fatalf("subscription keys: %s %s %s", s.key(), s.entityPath(), s.senderKey())
	}
	if d.key() != "ns/q/$deadletterqueue" || x.key() != "ns/q/$transferdeadletterqueue" {
		t.Fatalf("subqueue keys: %s %s", d.key(), x.key())
	}
	if receiverKey(q, azservicebus.ReceiveModePeekLock, "") == receiverKey(q, azservicebus.ReceiveModeReceiveAndDelete, "") {
		t.Fatal("receiver keys must differ by mode")
	}
	if receiverKey(q, azservicebus.ReceiveModePeekLock, "a") == receiverKey(q, azservicebus.ReceiveModePeekLock, "") {
		t.Fatal("receiver keys must differ by session")
	}
}

func TestFullyQualifiedNamespace(t *testing.T) {
	tests := []struct {
		ns   string
		cfg  *azcfg.Config
		want string
	}{
		{"myns", nil, "myns.servicebus.windows.net"},
		{"myns", azcfg.NewConfig(), "myns.servicebus.windows.net"},
		{"myns.servicebus.windows.net", nil, "myns.servicebus.windows.net"},
		{"localhost", nil, "localhost"},
		{"localhost:5672", nil, "localhost:5672"},
		{"myns", &azcfg.Config{Cloud: cloud.AzureGovernment}, "myns.servicebus.usgovcloudapi.net"},
		{"myns", &azcfg.Config{Cloud: cloud.AzureChina}, "myns.servicebus.chinacloudapi.cn"},
	}
	for _, tt := range tests {
		if got := fullyQualifiedNamespace(tt.ns, tt.cfg); got != tt.want {
			t.Errorf("fullyQualifiedNamespace(%q) = %q, want %q", tt.ns, got, tt.want)
		}
	}
}

func TestParseISODuration(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"PT1M", time.Minute, false},
		{"PT30S", 30 * time.Second, false},
		{"PT0.5S", 500 * time.Millisecond, false},
		{"PT1H2M3S", time.Hour + 2*time.Minute + 3*time.Second, false},
		{"P1DT2H", 26 * time.Hour, false},
		{"P1W", 7 * 24 * time.Hour, false},
		{"P", 0, false},
		{"1M", 0, true},
		{"PTM", 0, true},
		{"PT5", 0, true},
		{"P1H", 0, true},
		{"PT1X", 0, true},
		{"PT1..2S", 0, true},
	}
	for _, tt := range tests {
		got, err := parseISODuration(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseISODuration(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("parseISODuration(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

type staticCred struct{}

func (staticCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "t", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func TestResolveConfigAndConnect(t *testing.T) {
	// Connection-string config registered under the namespace.
	csCfg := azcfg.NewConfig()
	csCfg.SetConnectionString("Endpoint=sb://cfgns.servicebus.windows.net/;SharedAccessKeyName=k;SharedAccessKey=dGVzdA==")
	azcfg.Manager.Register("cfgns", csCfg)
	t.Cleanup(func() { azcfg.Manager.Unregister("cfgns") })

	u := mustURL(t, "servicebus://cfgns/orders")
	if got := resolveConfig(u); got != csCfg {
		t.Fatal("resolveConfig did not return the namespace config")
	}
	tg, _ := parseTarget(u)
	c, err := connectClient(u, tg)
	if err != nil {
		t.Fatalf("connectClient (connection string): %v", err)
	}
	_ = c.Close(context.Background())
	if _, err := connectAdmin(u, tg); err != nil {
		t.Fatalf("connectAdmin (connection string): %v", err)
	}

	// Token credential config.
	tokCfg := azcfg.NewConfig()
	tokCfg.SetCredential(staticCred{})
	azcfg.Manager.Register("tokns", tokCfg)
	t.Cleanup(func() { azcfg.Manager.Unregister("tokns") })
	u2 := mustURL(t, "servicebus://tokns/orders")
	tg2, _ := parseTarget(u2)
	c2, err := connectClient(u2, tg2)
	if err != nil {
		t.Fatalf("connectClient (credential): %v", err)
	}
	_ = c2.Close(context.Background())
	if _, err := connectAdmin(u2, tg2); err != nil {
		t.Fatalf("connectAdmin (credential): %v", err)
	}

	// Invalid connection string / incomplete credential surface errors.
	badCfg := azcfg.NewConfig()
	badCfg.SetConnectionString("NoEndpoint=x")
	azcfg.Manager.Register("badns", badCfg)
	t.Cleanup(func() { azcfg.Manager.Unregister("badns") })
	u3 := mustURL(t, "servicebus://badns/orders")
	tg3, _ := parseTarget(u3)
	if _, err := connectClient(u3, tg3); err == nil {
		t.Fatal("expected error for invalid connection string")
	}
	if _, err := connectAdmin(u3, tg3); err == nil {
		t.Fatal("expected admin error for invalid connection string")
	}

	incCfg := &azcfg.Config{ClientSecret: "secret"}
	azcfg.Manager.Register("incns", incCfg)
	t.Cleanup(func() { azcfg.Manager.Unregister("incns") })
	u4 := mustURL(t, "servicebus://incns/orders")
	tg4, _ := parseTarget(u4)
	if _, err := connectClient(u4, tg4); !errors.Is(err, azcfg.ErrIncompleteClientSecret) {
		t.Fatalf("expected ErrIncompleteClientSecret, got %v", err)
	}
	if _, err := connectAdmin(u4, tg4); !errors.Is(err, azcfg.ErrIncompleteClientSecret) {
		t.Fatalf("expected ErrIncompleteClientSecret from admin, got %v", err)
	}
}

func TestQueueInfoAndDeref(t *testing.T) {
	var nilStr *string
	if deref(nilStr) != "" {
		t.Fatal("deref(nil) should be zero")
	}
	n := int32(7)
	lock := "PT45S"
	b := true
	fwd := "sb://ns.servicebus.windows.net/dlq"
	info := queueInfo(queuePropsForTest(&n, &lock, &b, &fwd))
	if !info.hasMaxDeliveryCount || info.maxDeliveryCount != 7 || !info.hasLockDuration || info.lockDuration != 45*time.Second ||
		!info.requiresDuplicateDetection || !info.requiresSession || info.forwardDeadLetteredTo != fwd {
		t.Fatalf("unexpected info %+v", info)
	}
	empty := queueInfo(queuePropsForTest(nil, nil, nil, nil))
	if empty.hasMaxDeliveryCount || empty.hasLockDuration {
		t.Fatalf("unexpected info %+v", empty)
	}
}

func queuePropsForTest(n *int32, lock *string, b *bool, fwd *string) admin.QueueProperties {
	return admin.QueueProperties{
		MaxDeliveryCount:              n,
		LockDuration:                  lock,
		RequiresDuplicateDetection:    b,
		RequiresSession:               b,
		ForwardDeadLetteredMessagesTo: fwd,
	}
}
