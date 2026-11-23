package servicebus

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus/admin"
)

// The interfaces below are the subset of the azservicebus SDK surface the
// provider relies on. They exist so tests can inject fakes without a real
// namespace or emulator. The production adapters wrap the SDK types.

// messageBatch is satisfied by *azservicebus.MessageBatch.
type messageBatch interface {
	AddMessage(m *azservicebus.Message, options *azservicebus.AddMessageOptions) error
	NumMessages() int32
}

// sbSender sends to a queue or topic.
type sbSender interface {
	SendMessage(ctx context.Context, msg *azservicebus.Message, options *azservicebus.SendMessageOptions) error
	NewMessageBatch(ctx context.Context) (messageBatch, error)
	SendMessageBatch(ctx context.Context, batch messageBatch) error
	Close(ctx context.Context) error
}

// sbReceiver is satisfied by both *azservicebus.Receiver and
// *azservicebus.SessionReceiver.
type sbReceiver interface {
	ReceiveMessages(ctx context.Context, maxMessages int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error)
	CompleteMessage(ctx context.Context, message *azservicebus.ReceivedMessage, options *azservicebus.CompleteMessageOptions) error
	AbandonMessage(ctx context.Context, message *azservicebus.ReceivedMessage, options *azservicebus.AbandonMessageOptions) error
	DeadLetterMessage(ctx context.Context, message *azservicebus.ReceivedMessage, options *azservicebus.DeadLetterOptions) error
	DeferMessage(ctx context.Context, message *azservicebus.ReceivedMessage, options *azservicebus.DeferMessageOptions) error
	Close(ctx context.Context) error
}

// sbClient is a connection to one Service Bus namespace.
type sbClient interface {
	NewSender(entity string) (sbSender, error)
	NewReceiver(t target, mode azservicebus.ReceiveMode) (sbReceiver, error)
	// AcceptSession locks the given session, or the next available
	// session when sessionID is empty.
	AcceptSession(ctx context.Context, t target, sessionID string, mode azservicebus.ReceiveMode) (sbReceiver, error)
	Close(ctx context.Context) error
}

// entityInfo is the server-side entity configuration the broker-option
// verifiers need.
type entityInfo struct {
	maxDeliveryCount           int32
	hasMaxDeliveryCount        bool
	forwardDeadLetteredTo      string
	lockDuration               time.Duration
	hasLockDuration            bool
	requiresDuplicateDetection bool
	requiresSession            bool
}

// sbAdmin fetches entity configuration from the management plane.
type sbAdmin interface {
	// EntityInfo returns the configuration of the receive source of t
	// (queue or subscription) or, when forSend is true, of the send
	// destination (queue or topic). It returns (nil, nil) when the entity
	// does not exist.
	EntityInfo(ctx context.Context, t target, forSend bool) (*entityInfo, error)
}

// connectClient creates a data-plane client for the namespace in u. It is a
// package-level var so tests can inject a fake.
var connectClient = func(u *url.URL, t target) (sbClient, error) {
	cfg := resolveConfig(u)
	opts := &azservicebus.ClientOptions{CustomEndpoint: cfg.Endpoint}
	var (
		c   *azservicebus.Client
		err error
	)
	if cfg.ConnectionString != "" {
		c, err = azservicebus.NewClientFromConnectionString(cfg.ConnectionString, opts)
	} else {
		cred, cerr := cfg.Credential()
		if cerr != nil {
			return nil, fmt.Errorf("servicebus: failed to build credential: %w", cerr)
		}
		c, err = azservicebus.NewClient(fullyQualifiedNamespace(t.namespace, cfg), cred, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("servicebus: failed to create client for namespace %q: %w", t.namespace, err)
	}
	return &clientAdapter{c: c}, nil
}

// connectAdmin creates a management-plane client for the namespace in u.
// It is a package-level var so tests can inject a fake.
var connectAdmin = func(u *url.URL, t target) (sbAdmin, error) {
	cfg := resolveConfig(u)
	opts := &admin.ClientOptions{ClientOptions: cfg.ClientOpts()}
	var (
		c   *admin.Client
		err error
	)
	if cfg.ConnectionString != "" {
		c, err = admin.NewClientFromConnectionString(cfg.ConnectionString, opts)
	} else {
		cred, cerr := cfg.Credential()
		if cerr != nil {
			return nil, fmt.Errorf("servicebus: failed to build credential: %w", cerr)
		}
		c, err = admin.NewClient(fullyQualifiedNamespace(t.namespace, cfg), cred, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("servicebus: failed to create admin client for namespace %q: %w", t.namespace, err)
	}
	return &adminAdapter{c: c}, nil
}

// --- production adapters -------------------------------------------------

type clientAdapter struct{ c *azservicebus.Client }

func (a *clientAdapter) NewSender(entity string) (sbSender, error) {
	s, err := a.c.NewSender(entity, nil)
	if err != nil {
		return nil, err
	}
	return &senderAdapter{s: s}, nil
}

func (a *clientAdapter) NewReceiver(t target, mode azservicebus.ReceiveMode) (sbReceiver, error) {
	opts := &azservicebus.ReceiverOptions{ReceiveMode: mode, SubQueue: t.subQueue}
	if t.subscription != "" {
		return a.c.NewReceiverForSubscription(t.entity, t.subscription, opts)
	}
	return a.c.NewReceiverForQueue(t.entity, opts)
}

func (a *clientAdapter) AcceptSession(ctx context.Context, t target, sessionID string, mode azservicebus.ReceiveMode) (sbReceiver, error) {
	opts := &azservicebus.SessionReceiverOptions{ReceiveMode: mode}
	switch {
	case t.subscription != "" && sessionID != "":
		return a.c.AcceptSessionForSubscription(ctx, t.entity, t.subscription, sessionID, opts)
	case t.subscription != "":
		return a.c.AcceptNextSessionForSubscription(ctx, t.entity, t.subscription, opts)
	case sessionID != "":
		return a.c.AcceptSessionForQueue(ctx, t.entity, sessionID, opts)
	default:
		return a.c.AcceptNextSessionForQueue(ctx, t.entity, opts)
	}
}

func (a *clientAdapter) Close(ctx context.Context) error { return a.c.Close(ctx) }

type senderAdapter struct{ s *azservicebus.Sender }

func (a *senderAdapter) SendMessage(ctx context.Context, msg *azservicebus.Message, options *azservicebus.SendMessageOptions) error {
	return a.s.SendMessage(ctx, msg, options)
}

func (a *senderAdapter) NewMessageBatch(ctx context.Context) (messageBatch, error) {
	return a.s.NewMessageBatch(ctx, nil)
}

func (a *senderAdapter) SendMessageBatch(ctx context.Context, batch messageBatch) error {
	b, ok := batch.(*azservicebus.MessageBatch)
	if !ok {
		return fmt.Errorf("servicebus: unexpected batch type %T", batch)
	}
	return a.s.SendMessageBatch(ctx, b, nil)
}

func (a *senderAdapter) Close(ctx context.Context) error { return a.s.Close(ctx) }

type adminAdapter struct{ c *admin.Client }

func (a *adminAdapter) EntityInfo(ctx context.Context, t target, forSend bool) (*entityInfo, error) {
	switch {
	case forSend:
		// Send destination is the queue or topic named by the path.
		if q, err := a.c.GetQueue(ctx, t.entity, nil); err != nil {
			return nil, err
		} else if q != nil {
			return queueInfo(q.QueueProperties), nil
		}
		tp, err := a.c.GetTopic(ctx, t.entity, nil)
		if err != nil || tp == nil {
			return nil, err
		}
		return &entityInfo{requiresDuplicateDetection: deref(tp.RequiresDuplicateDetection)}, nil
	case t.subscription != "":
		s, err := a.c.GetSubscription(ctx, t.entity, t.subscription, nil)
		if err != nil || s == nil {
			return nil, err
		}
		p := s.SubscriptionProperties
		info := &entityInfo{
			forwardDeadLetteredTo: deref(p.ForwardDeadLetteredMessagesTo),
			requiresSession:       deref(p.RequiresSession),
		}
		if p.MaxDeliveryCount != nil {
			info.maxDeliveryCount, info.hasMaxDeliveryCount = *p.MaxDeliveryCount, true
		}
		if p.LockDuration != nil {
			if d, err := parseISODuration(*p.LockDuration); err == nil {
				info.lockDuration, info.hasLockDuration = d, true
			}
		}
		return info, nil
	default:
		q, err := a.c.GetQueue(ctx, t.entity, nil)
		if err != nil || q == nil {
			return nil, err
		}
		return queueInfo(q.QueueProperties), nil
	}
}

func queueInfo(p admin.QueueProperties) *entityInfo {
	info := &entityInfo{
		forwardDeadLetteredTo:      deref(p.ForwardDeadLetteredMessagesTo),
		requiresDuplicateDetection: deref(p.RequiresDuplicateDetection),
		requiresSession:            deref(p.RequiresSession),
	}
	if p.MaxDeliveryCount != nil {
		info.maxDeliveryCount, info.hasMaxDeliveryCount = *p.MaxDeliveryCount, true
	}
	if p.LockDuration != nil {
		if d, err := parseISODuration(*p.LockDuration); err == nil {
			info.lockDuration, info.hasLockDuration = d, true
		}
	}
	return info
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// parseISODuration parses the ISO-8601 durations returned by the Service
// Bus management plane (e.g. "PT1M", "PT30S", "P1DT2H", "PT0.5S").
func parseISODuration(s string) (time.Duration, error) {
	orig := s
	if !strings.HasPrefix(s, "P") {
		return 0, fmt.Errorf("servicebus: invalid ISO-8601 duration %q", orig)
	}
	s = s[1:]
	var total time.Duration
	inTime := false
	num := ""
	for _, r := range s {
		switch {
		case r == 'T':
			inTime = true
		case (r >= '0' && r <= '9') || r == '.':
			num += string(r)
		default:
			if num == "" {
				return 0, fmt.Errorf("servicebus: invalid ISO-8601 duration %q", orig)
			}
			var unit time.Duration
			switch {
			case r == 'D' && !inTime:
				unit = 24 * time.Hour
			case r == 'W' && !inTime:
				unit = 7 * 24 * time.Hour
			case r == 'H' && inTime:
				unit = time.Hour
			case r == 'M' && inTime:
				unit = time.Minute
			case r == 'S' && inTime:
				unit = time.Second
			default:
				return 0, fmt.Errorf("servicebus: unsupported ISO-8601 duration component %q in %q", r, orig)
			}
			d, err := time.ParseDuration(num + "s")
			if err != nil {
				return 0, fmt.Errorf("servicebus: invalid ISO-8601 duration %q: %w", orig, err)
			}
			total += time.Duration(float64(unit) * d.Seconds())
			num = ""
		}
	}
	if num != "" {
		return 0, fmt.Errorf("servicebus: invalid ISO-8601 duration %q", orig)
	}
	return total, nil
}
