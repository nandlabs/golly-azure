package servicebus

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"oss.nandlabs.io/golly/messaging"
)

// settleTimeout bounds each settlement call issued from Rsvp.
var settleTimeout = 30 * time.Second

// MessageServiceBus wraps messaging.BaseMessage and adds Service Bus
// specific state: the application properties sent with the message and,
// for received messages, the underlying *azservicebus.ReceivedMessage plus
// the receiver needed to settle it.
type MessageServiceBus struct {
	*messaging.BaseMessage

	provider *Provider

	propsMu    sync.Mutex
	properties map[string]any

	// received is the underlying Service Bus message (nil for outbound).
	received *azservicebus.ReceivedMessage
	// receiver settles the message; nil for outbound messages and for
	// messages received in ReceiveAndDelete mode.
	receiver sbReceiver
	// settled guards against double settlement.
	settled atomic.Bool
	// onSettle is invoked once after the message is settled (used to
	// release per-call session receivers).
	onSettle func()
	// rsvpURL is the source URL used to attribute observer OnAck / OnNack.
	rsvpURL *url.URL
}

var _ messaging.Message = (*MessageServiceBus)(nil)
var _ messaging.Keyed = (*MessageServiceBus)(nil)

func (m *MessageServiceBus) setProp(key string, value any) {
	m.propsMu.Lock()
	if m.properties == nil {
		m.properties = make(map[string]any)
	}
	m.properties[key] = value
	m.propsMu.Unlock()
}

// ApplicationProperties returns a copy of the Service Bus application
// properties carried by the message (the golly headers).
func (m *MessageServiceBus) ApplicationProperties() map[string]any {
	m.propsMu.Lock()
	defer m.propsMu.Unlock()
	if len(m.properties) == 0 {
		return nil
	}
	out := make(map[string]any, len(m.properties))
	for k, v := range m.properties {
		out[k] = v
	}
	return out
}

// SetHeader sets a []byte header (sent as a binary application property).
func (m *MessageServiceBus) SetHeader(key string, value []byte) {
	m.BaseMessage.SetHeader(key, value)
	m.setProp(key, append([]byte(nil), value...))
}

// SetStrHeader sets a string header.
func (m *MessageServiceBus) SetStrHeader(key string, value string) {
	m.BaseMessage.SetStrHeader(key, value)
	m.setProp(key, value)
}

// SetBoolHeader sets a bool header.
func (m *MessageServiceBus) SetBoolHeader(key string, value bool) {
	m.BaseMessage.SetBoolHeader(key, value)
	m.setProp(key, value)
}

// SetIntHeader sets an int header (sent as int64).
func (m *MessageServiceBus) SetIntHeader(key string, value int) {
	m.BaseMessage.SetIntHeader(key, value)
	m.setProp(key, int64(value))
}

// SetInt8Header sets an int8 header.
func (m *MessageServiceBus) SetInt8Header(key string, value int8) {
	m.BaseMessage.SetInt8Header(key, value)
	m.setProp(key, value)
}

// SetInt16Header sets an int16 header.
func (m *MessageServiceBus) SetInt16Header(key string, value int16) {
	m.BaseMessage.SetInt16Header(key, value)
	m.setProp(key, value)
}

// SetInt32Header sets an int32 header.
func (m *MessageServiceBus) SetInt32Header(key string, value int32) {
	m.BaseMessage.SetInt32Header(key, value)
	m.setProp(key, value)
}

// SetInt64Header sets an int64 header.
func (m *MessageServiceBus) SetInt64Header(key string, value int64) {
	m.BaseMessage.SetInt64Header(key, value)
	m.setProp(key, value)
}

// SetFloatHeader sets a float32 header.
func (m *MessageServiceBus) SetFloatHeader(key string, value float32) {
	m.BaseMessage.SetFloatHeader(key, value)
	m.setProp(key, value)
}

// SetFloat64Header sets a float64 header.
func (m *MessageServiceBus) SetFloat64Header(key string, value float64) {
	m.BaseMessage.SetFloat64Header(key, value)
	m.setProp(key, value)
}

// ReceivedMessage returns the underlying Service Bus message, or nil for
// outbound messages.
func (m *MessageServiceBus) ReceivedMessage() *azservicebus.ReceivedMessage {
	return m.received
}

// MessageID returns the Service Bus MessageID of a received message.
func (m *MessageServiceBus) MessageID() string {
	if m.received == nil {
		return ""
	}
	return m.received.MessageID
}

// SessionID returns the session id of a received message ("" if none).
func (m *MessageServiceBus) SessionID() string {
	if m.received == nil {
		return ""
	}
	return deref(m.received.SessionID)
}

// SequenceNumber returns the broker-assigned sequence number of a received
// message (0 if unknown).
func (m *MessageServiceBus) SequenceNumber() int64 {
	if m.received == nil {
		return 0
	}
	return deref(m.received.SequenceNumber)
}

// DeliveryCount returns how many times the message has been delivered.
func (m *MessageServiceBus) DeliveryCount() uint32 {
	if m.received == nil {
		return 0
	}
	return m.received.DeliveryCount
}

// EnqueuedTime returns when the broker accepted the message (zero if
// unknown).
func (m *MessageServiceBus) EnqueuedTime() time.Time {
	if m.received == nil {
		return time.Time{}
	}
	return deref(m.received.EnqueuedTime)
}

// Rsvp settles a received message.
//
//   - Rsvp(true) completes the message (removes it from the queue /
//     subscription).
//   - Rsvp(false) abandons it: the lock is released and the message is
//     redelivered (its DeliveryCount increments and, once MaxDeliveryCount
//     is exceeded, Service Bus dead-letters it).
//   - Rsvp(false, Requeue=false) or Rsvp(false, DeadLetterReason=...) /
//     DeadLetterDescription=... dead-letters the message.
//   - Rsvp(false, Defer=true) defers it.
//
// Rsvp is a no-op for outbound messages, for messages received in
// ReceiveAndDelete mode, and for messages that were already settled.
// Fires the provider observer's OnAck / OnNack hook (requeue=true only for
// abandon).
func (m *MessageServiceBus) Rsvp(accept bool, options ...messaging.Option) error {
	return m.RsvpCtx(context.Background(), accept, options...)
}

// RsvpCtx is the context-aware variant of Rsvp.
func (m *MessageServiceBus) RsvpCtx(ctx context.Context, accept bool, options ...messaging.Option) error {
	if m.received == nil || m.receiver == nil {
		return nil
	}
	if !m.settled.CompareAndSwap(false, true) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()

	var (
		err     error
		requeue bool
	)
	if accept {
		err = m.receiver.CompleteMessage(ctx, m.received, nil)
	} else {
		r := messaging.NewOptionsResolver(options...)
		reason, hasReason, _ := optString(r, OptDeadLetterReason)
		desc, hasDesc, _ := optString(r, OptDeadLetterDescription)
		deferMsg, _, _ := optBool(r, OptDefer)
		requeueOpt, hasRequeue, _ := optBool(r, messaging.RequeueOpt)
		switch {
		case deferMsg:
			err = m.receiver.DeferMessage(ctx, m.received, nil)
		case hasReason || hasDesc || (hasRequeue && !requeueOpt):
			dl := &azservicebus.DeadLetterOptions{}
			if hasReason {
				dl.Reason = &reason
			}
			if hasDesc {
				dl.ErrorDescription = &desc
			}
			err = m.receiver.DeadLetterMessage(ctx, m.received, dl)
		default:
			requeue = true
			err = m.receiver.AbandonMessage(ctx, m.received, nil)
		}
	}
	if err != nil {
		// Allow the caller to retry settlement.
		m.settled.Store(false)
		if accept {
			return fmt.Errorf("servicebus: complete message failed: %w", err)
		}
		return fmt.Errorf("servicebus: reject message failed: %w", err)
	}
	if m.onSettle != nil {
		m.onSettle()
	}
	if m.provider != nil {
		if accept {
			m.provider.getObserver().OnAck(m.rsvpURL, m)
		} else {
			m.provider.getObserver().OnNack(m.rsvpURL, m, requeue)
		}
	}
	return nil
}
