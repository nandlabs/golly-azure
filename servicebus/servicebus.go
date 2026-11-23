package servicebus

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"oss.nandlabs.io/golly/messaging"
)

var serviceBusSchemes = []string{ServiceBusScheme}

// listenerBackoff is the pause after a failed listener poll. A var so tests
// can shorten it.
var listenerBackoff = time.Second

// closeTimeout bounds Close calls on SDK links.
const closeTimeout = 10 * time.Second

// listenerEntry tracks a single registered listener so it can be cancelled
// by name via RemoveNamedListener, or as part of a per-source group via
// RemoveListeners.
type listenerEntry struct {
	name   string // empty for unnamed listeners
	cancel context.CancelFunc
	done   chan struct{}
}

// sharedReceiver serialises ReceiveMessages on a cached receiver (the SDK
// allows only one concurrent ReceiveMessages per receiver).
type sharedReceiver struct {
	mu sync.Mutex
	r  sbReceiver
}

// Provider implements the messaging.Provider interface for Azure Service
// Bus queues and topics / subscriptions.
//
// It also satisfies the following capability interfaces:
//   - messaging.ListenerRemover (per-source / named-group listener teardown)
//   - messaging.ProducerCtx (context-aware send)
//   - messaging.ReceiverCtx (context-aware receive / listener teardown)
//   - messaging.ObservableProvider (observer hooks around Send / Receive /
//     Ack / Nack)
//
// Keyed messages (messaging.Keyed) map their routing key to the Service Bus
// SessionID by default (see OptKeyMapping). On session-enabled entities this
// yields FIFO ordering per key and keeps a key on a single partition;
// non-session entities accept and ignore the session id.
//
// One AMQP connection is kept per namespace and one sender per queue /
// topic; they are released by Close.
type Provider struct {
	closed atomic.Bool

	mu        sync.Mutex
	listeners map[string][]*listenerEntry // target.key() → listeners
	clients   map[string]sbClient         // namespace → client
	senders   map[string]sbSender         // namespace/entity → sender
	receivers map[string]*sharedReceiver  // receive key → receiver
	sessions  map[sbReceiver]struct{}     // per-call accept-next-session receivers

	// observer holds an observerHolder so SetObserver can be called
	// concurrently with hook sites.
	observer atomic.Value
}

type observerHolder struct{ obs messaging.Observer }

// Compile-time interface assertions.
var (
	_ messaging.Provider           = (*Provider)(nil)
	_ messaging.ProducerCtx        = (*Provider)(nil)
	_ messaging.ReceiverCtx        = (*Provider)(nil)
	_ messaging.ObservableProvider = (*Provider)(nil)
	_ messaging.ListenerRemover    = (*Provider)(nil)
)

// Id returns the provider id.
func (p *Provider) Id() string { return ServiceBusProviderID }

// Schemes returns the supported URL schemes.
func (p *Provider) Schemes() []string { return serviceBusSchemes }

// Setup re-arms a closed provider. Connections are created lazily, so there
// is nothing else to do.
func (p *Provider) Setup() error {
	p.closed.Store(false)
	return nil
}

// NewMessage creates a new Service Bus message.
func (p *Provider) NewMessage(scheme string, options ...messaging.Option) (messaging.Message, error) {
	baseMsg, err := messaging.NewBaseMessage()
	if err != nil {
		return nil, err
	}
	return &MessageServiceBus{BaseMessage: baseMsg, provider: p}, nil
}

// SetObserver installs (or clears, with nil) the metrics / tracing observer.
func (p *Provider) SetObserver(obs messaging.Observer) {
	p.observer.Store(observerHolder{obs: obs})
}

func (p *Provider) getObserver() messaging.Observer {
	if v := p.observer.Load(); v != nil {
		if h := v.(observerHolder); h.obs != nil {
			return h.obs
		}
	}
	return noopObserver{}
}

type noopObserver struct{}

func (noopObserver) OnSend(*url.URL, messaging.Message, error, time.Duration) {}
func (noopObserver) OnReceive(*url.URL, messaging.Message, error)             {}
func (noopObserver) OnAck(*url.URL, messaging.Message)                        {}
func (noopObserver) OnNack(*url.URL, messaging.Message, bool)                 {}

// --- connection management ---------------------------------------------

func (p *Provider) getClient(u *url.URL, t target) (sbClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[t.namespace]; ok {
		return c, nil
	}
	c, err := connectClient(u, t)
	if err != nil {
		return nil, err
	}
	if p.clients == nil {
		p.clients = make(map[string]sbClient)
	}
	p.clients[t.namespace] = c
	return c, nil
}

func (p *Provider) getSender(u *url.URL, t target) (sbSender, error) {
	client, err := p.getClient(u, t)
	if err != nil {
		return nil, err
	}
	key := t.senderKey()
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.senders[key]; ok {
		return s, nil
	}
	s, err := client.NewSender(t.entity)
	if err != nil {
		return nil, fmt.Errorf("servicebus: failed to create sender for %q: %w", t.entity, err)
	}
	if p.senders == nil {
		p.senders = make(map[string]sbSender)
	}
	p.senders[key] = s
	return s, nil
}

// evictSender drops a sender whose link is unusable so the next send
// creates a fresh one.
func (p *Provider) evictSender(t target, s sbSender, err error) {
	if !isFatal(err) {
		return
	}
	p.mu.Lock()
	if cur, ok := p.senders[t.senderKey()]; ok && cur == s {
		delete(p.senders, t.senderKey())
	}
	p.mu.Unlock()
	closeQuietly(s.Close)
}

func receiverKey(t target, mode azservicebus.ReceiveMode, sessionID string) string {
	k := fmt.Sprintf("%s#mode=%d", t.key(), mode)
	if sessionID != "" {
		k += "#session=" + sessionID
	}
	return k
}

// getSharedReceiver returns the cached receiver for key, creating it with
// create on first use.
func (p *Provider) getSharedReceiver(key string, create func() (sbReceiver, error)) (*sharedReceiver, error) {
	p.mu.Lock()
	if sr, ok := p.receivers[key]; ok {
		p.mu.Unlock()
		return sr, nil
	}
	p.mu.Unlock()
	r, err := create()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if sr, ok := p.receivers[key]; ok {
		// Lost a creation race; keep the existing one.
		closeQuietly(r.Close)
		return sr, nil
	}
	if p.receivers == nil {
		p.receivers = make(map[string]*sharedReceiver)
	}
	sr := &sharedReceiver{r: r}
	p.receivers[key] = sr
	return sr, nil
}

func (p *Provider) evictReceiver(key string, sr *sharedReceiver) {
	p.mu.Lock()
	if cur, ok := p.receivers[key]; ok && cur == sr {
		delete(p.receivers, key)
	}
	p.mu.Unlock()
	closeQuietly(sr.r.Close)
}

func (p *Provider) trackSession(r sbReceiver) {
	p.mu.Lock()
	if p.sessions == nil {
		p.sessions = make(map[sbReceiver]struct{})
	}
	p.sessions[r] = struct{}{}
	p.mu.Unlock()
}

func (p *Provider) releaseSession(r sbReceiver) {
	p.mu.Lock()
	_, ok := p.sessions[r]
	delete(p.sessions, r)
	p.mu.Unlock()
	if ok {
		closeQuietly(r.Close)
	}
}

func closeQuietly(fn func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	if err := fn(ctx); err != nil {
		logger.DebugF("servicebus: close error: %v", err)
	}
}

// isFatal reports whether err indicates the link / connection / lock is
// unusable and the cached sender or receiver should be recreated.
func isFatal(err error) bool {
	var sbErr *azservicebus.Error
	if errors.As(err, &sbErr) {
		switch sbErr.Code {
		case azservicebus.CodeClosed, azservicebus.CodeConnectionLost, azservicebus.CodeLockLost, azservicebus.CodeNotFound, azservicebus.CodeUnauthorizedAccess:
			return true
		}
	}
	return false
}

// --- send -----------------------------------------------------------------

// Send sends a single message to a queue or topic.
// URL format: servicebus://<namespace>/<queue-or-topic>
//
// Send delegates to SendCtx with context.Background().
func (p *Provider) Send(u *url.URL, msg messaging.Message, options ...messaging.Option) error {
	return p.SendCtx(context.Background(), u, msg, options...)
}

// SendBatch sends messages to a queue or topic, packing them into as few
// Service Bus batches as the namespace's maximum batch size allows.
//
// SendBatch delegates to SendBatchCtx with context.Background().
func (p *Provider) SendBatch(u *url.URL, msgs []messaging.Message, options ...messaging.Option) error {
	return p.SendBatchCtx(context.Background(), u, msgs, options...)
}

// SendCtx is the context-aware variant of Send. ctx is passed to the SDK so
// cancellation and deadlines propagate. Fires the observer's OnSend hook.
func (p *Provider) SendCtx(ctx context.Context, u *url.URL, msg messaging.Message, options ...messaging.Option) (err error) {
	start := time.Now()
	defer func() { p.getObserver().OnSend(u, msg, err, time.Since(start)) }()

	t, ss, err := p.prepareSend(ctx, u, options)
	if err != nil {
		return err
	}
	sbMsg, err := buildMessage(msg, ss, true)
	if err != nil {
		return err
	}
	sender, err := p.getSender(u, t)
	if err != nil {
		return err
	}
	if serr := sender.SendMessage(ctx, sbMsg, nil); serr != nil {
		p.evictSender(t, sender, serr)
		return fmt.Errorf("servicebus: send failed: %w", serr)
	}
	logger.DebugF("Service Bus message sent to %s", t.senderKey())
	return nil
}

// SendBatchCtx is the context-aware variant of SendBatch. The observer's
// OnSend hook fires once per message.
func (p *Provider) SendBatchCtx(ctx context.Context, u *url.URL, msgs []messaging.Message, options ...messaging.Option) error {
	if len(msgs) == 0 {
		return nil
	}
	obs := p.getObserver()
	start := time.Now()
	failAll := func(from int, err error) error {
		for _, m := range msgs[from:] {
			obs.OnSend(u, m, err, time.Since(start))
		}
		return err
	}

	t, ss, err := p.prepareSend(ctx, u, options)
	if err != nil {
		return failAll(0, err)
	}
	sbMsgs := make([]*azservicebus.Message, len(msgs))
	for i, m := range msgs {
		if sbMsgs[i], err = buildMessage(m, ss, false); err != nil {
			return failAll(0, fmt.Errorf("servicebus: message %d: %w", i, err))
		}
	}
	sender, err := p.getSender(u, t)
	if err != nil {
		return failAll(0, err)
	}

	// Pack messages into batches; flush when the next message no longer fits.
	sent := 0
	for sent < len(sbMsgs) {
		batch, berr := sender.NewMessageBatch(ctx)
		if berr != nil {
			p.evictSender(t, sender, berr)
			return failAll(sent, fmt.Errorf("servicebus: create batch failed: %w", berr))
		}
		n := 0
		for sent+n < len(sbMsgs) {
			aerr := batch.AddMessage(sbMsgs[sent+n], nil)
			if errors.Is(aerr, azservicebus.ErrMessageTooLarge) {
				if n == 0 {
					return failAll(sent, fmt.Errorf("servicebus: message %d is too large for a batch: %w", sent, aerr))
				}
				break
			}
			if aerr != nil {
				return failAll(sent, fmt.Errorf("servicebus: add message %d to batch: %w", sent+n, aerr))
			}
			n++
		}
		bstart := time.Now()
		if serr := sender.SendMessageBatch(ctx, batch); serr != nil {
			p.evictSender(t, sender, serr)
			return failAll(sent, fmt.Errorf("servicebus: batch send failed: %w", serr))
		}
		latency := time.Since(bstart)
		for _, m := range msgs[sent : sent+n] {
			obs.OnSend(u, m, nil, latency)
		}
		sent += n
	}
	logger.DebugF("Service Bus batch sent %d messages to %s", len(msgs), t.senderKey())
	return nil
}

func (p *Provider) prepareSend(ctx context.Context, u *url.URL, options []messaging.Option) (target, *sendSettings, error) {
	t, err := parseTarget(u)
	if err != nil {
		return t, nil, err
	}
	r := messaging.NewOptionsResolver(options...)
	bo, err := parseBrokerOptions(r, true)
	if err != nil {
		return t, nil, err
	}
	ss, err := parseSendSettings(r)
	if err != nil {
		return t, nil, err
	}
	if err := bo.verify(ctx, u, t, true); err != nil {
		return t, nil, err
	}
	return t, ss, nil
}

// buildMessage converts a golly message into a Service Bus message.
// single reports whether this is a single send (OptMessageID applies).
func buildMessage(msg messaging.Message, ss *sendSettings, single bool) (*azservicebus.Message, error) {
	out := &azservicebus.Message{Body: msg.ReadBytes()}
	if sbm, ok := msg.(*MessageServiceBus); ok {
		out.ApplicationProperties = sbm.ApplicationProperties()
	}

	msgID := msg.Id()
	if single && ss.messageID != "" {
		msgID = ss.messageID
	}
	if msgID != "" {
		out.MessageID = &msgID
	}

	var sessionID, partitionKey string
	key := messaging.RoutingKeyOf(msg)
	if ss.hasSessionID {
		sessionID = ss.sessionID
	} else if key != "" && ss.keyMapping == KeyMappingSession {
		sessionID = key
	}
	partitionKey = ss.partitionKey
	if partitionKey == "" && key != "" && ss.keyMapping == KeyMappingPartitionKey {
		partitionKey = key
	}
	if sessionID != "" && partitionKey != "" && sessionID != partitionKey {
		return nil, fmt.Errorf("%w: PartitionKey %q must equal SessionID %q for session-aware messages", ErrInvalidOption, partitionKey, sessionID)
	}
	if sessionID != "" {
		out.SessionID = &sessionID
	}
	if partitionKey != "" {
		out.PartitionKey = &partitionKey
	}

	setIf := func(dst **string, v string) {
		if v != "" {
			s := v
			*dst = &s
		}
	}
	setIf(&out.CorrelationID, ss.correlationID)
	setIf(&out.Subject, ss.subject)
	setIf(&out.ContentType, ss.contentType)
	setIf(&out.ReplyTo, ss.replyTo)
	if ss.hasTTL {
		ttl := ss.ttl
		out.TimeToLive = &ttl
	}
	if ss.hasScheduled {
		st := ss.scheduled
		out.ScheduledEnqueueTime = &st
	}
	return out, nil
}

// --- receive --------------------------------------------------------------

// Receive receives a single message.
// URL format: servicebus://<namespace>/<queue> or
// servicebus://<namespace>/<topic>?subscription=<subscription>.
// In PeekLock mode (default) the message is locked; settle it with
// msg.Rsvp(true) (complete) or msg.Rsvp(false) (abandon).
//
// Receive delegates to ReceiveCtx with context.Background().
func (p *Provider) Receive(u *url.URL, options ...messaging.Option) (messaging.Message, error) {
	return p.ReceiveCtx(context.Background(), u, options...)
}

// ReceiveBatch receives up to BatchSize (default 10) messages.
//
// ReceiveBatch delegates to ReceiveBatchCtx with context.Background().
func (p *Provider) ReceiveBatch(u *url.URL, options ...messaging.Option) ([]messaging.Message, error) {
	return p.ReceiveBatchCtx(context.Background(), u, options...)
}

// ReceiveCtx is the context-aware variant of Receive. ctx is composed with
// the WaitTimeSeconds deadline; ErrNoMessages is returned when nothing
// arrived in time.
func (p *Provider) ReceiveCtx(ctx context.Context, u *url.URL, options ...messaging.Option) (messaging.Message, error) {
	msgs, err := p.receive(ctx, u, options, false)
	if err != nil {
		return nil, err
	}
	return msgs[0], nil
}

// ReceiveBatchCtx is the context-aware variant of ReceiveBatch.
func (p *Provider) ReceiveBatchCtx(ctx context.Context, u *url.URL, options ...messaging.Option) ([]messaging.Message, error) {
	return p.receive(ctx, u, options, true)
}

// receivePlan is the resolved receive configuration shared by Receive,
// ReceiveBatch and AddListener.
type receivePlan struct {
	t    target
	rs   *receiveSettings
	mode azservicebus.ReceiveMode
}

func (p *Provider) planReceive(ctx context.Context, u *url.URL, options []messaging.Option) (*receivePlan, error) {
	t, err := parseTarget(u)
	if err != nil {
		return nil, err
	}
	r := messaging.NewOptionsResolver(options...)
	rs, err := parseReceiveSettings(r, &t)
	if err != nil {
		return nil, err
	}
	bo, err := parseBrokerOptions(r, false)
	if err != nil {
		return nil, err
	}
	mode := rs.mode
	if bo.hasReceiveMode {
		if rs.hasMode && rs.mode != bo.receiveMode {
			return nil, fmt.Errorf("%w: %s conflicts with %s", ErrInvalidOption, OptReceiveMode, messaging.DeliveryGuaranteeOpt)
		}
		mode = bo.receiveMode
	}
	if err := bo.verify(ctx, u, t, false); err != nil {
		return nil, err
	}
	return &receivePlan{t: t, rs: rs, mode: mode}, nil
}

func (p *Provider) receive(ctx context.Context, u *url.URL, options []messaging.Option, batch bool) ([]messaging.Message, error) {
	plan, err := p.planReceive(ctx, u, options)
	if err != nil {
		return nil, err
	}
	client, err := p.getClient(u, plan.t)
	if err != nil {
		return nil, err
	}
	max := 1
	if batch {
		max = plan.rs.batchSize
	}
	obs := p.getObserver()

	waitCtx, cancel := context.WithTimeout(ctx, plan.rs.wait)
	defer cancel()

	// Per-call session receiver for AcceptNextSession.
	if plan.rs.acceptNextSession {
		rcv, aerr := client.AcceptSession(waitCtx, plan.t, "", plan.mode)
		if aerr != nil {
			return nil, p.receiveError(ctx, waitCtx, u, aerr)
		}
		p.trackSession(rcv)
		msgs, rerr := rcv.ReceiveMessages(waitCtx, max, nil)
		if rerr != nil || len(msgs) == 0 {
			p.releaseSession(rcv)
			return nil, p.receiveError(ctx, waitCtx, u, rerr)
		}
		var onSettle func()
		if plan.mode == azservicebus.ReceiveModeReceiveAndDelete {
			p.releaseSession(rcv)
		} else {
			var outstanding atomic.Int32
			outstanding.Store(int32(len(msgs)))
			onSettle = func() {
				if outstanding.Add(-1) == 0 {
					p.releaseSession(rcv)
				}
			}
		}
		out := p.wrapReceived(u, msgs, rcv, plan.mode, onSettle)
		for _, m := range out {
			obs.OnReceive(u, m, nil)
		}
		return out, nil
	}

	key := receiverKey(plan.t, plan.mode, plan.rs.sessionID)
	sr, err := p.getSharedReceiver(key, func() (sbReceiver, error) {
		if plan.rs.sessionID != "" {
			return client.AcceptSession(waitCtx, plan.t, plan.rs.sessionID, plan.mode)
		}
		return client.NewReceiver(plan.t, plan.mode)
	})
	if err != nil {
		return nil, p.receiveError(ctx, waitCtx, u, err)
	}
	sr.mu.Lock()
	msgs, err := sr.r.ReceiveMessages(waitCtx, max, nil)
	sr.mu.Unlock()
	if err != nil || len(msgs) == 0 {
		if isFatal(err) {
			p.evictReceiver(key, sr)
		}
		return nil, p.receiveError(ctx, waitCtx, u, err)
	}
	out := p.wrapReceived(u, msgs, sr.r, plan.mode, nil)
	for _, m := range out {
		obs.OnReceive(u, m, nil)
	}
	return out, nil
}

// receiveError maps a receive failure (or an empty result when err is nil)
// to the error returned to callers, firing OnReceive for real failures.
func (p *Provider) receiveError(ctx, waitCtx context.Context, u *url.URL, err error) error {
	if ctx.Err() != nil {
		e := fmt.Errorf("servicebus: receive aborted: %w", ctx.Err())
		p.getObserver().OnReceive(u, nil, e)
		return e
	}
	if err == nil || waitCtx.Err() != nil || isTimeout(err) {
		return ErrNoMessages
	}
	e := fmt.Errorf("servicebus: receive failed: %w", err)
	p.getObserver().OnReceive(u, nil, e)
	return e
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var sbErr *azservicebus.Error
	return errors.As(err, &sbErr) && sbErr.Code == azservicebus.CodeTimeout
}

func (p *Provider) wrapReceived(u *url.URL, msgs []*azservicebus.ReceivedMessage, rcv sbReceiver, mode azservicebus.ReceiveMode, onSettle func()) []messaging.Message {
	out := make([]messaging.Message, len(msgs))
	for i, rm := range msgs {
		m := p.toMessage(u, rm)
		if mode == azservicebus.ReceiveModePeekLock {
			m.receiver = rcv
			m.onSettle = onSettle
		}
		out[i] = m
	}
	return out
}

// toMessage converts a received Service Bus message into a MessageServiceBus.
// Application properties become golly headers with their native types;
// SessionID (or else PartitionKey) becomes the Keyed routing key.
func (p *Provider) toMessage(u *url.URL, rm *azservicebus.ReceivedMessage) *MessageServiceBus {
	baseMsg, _ := messaging.NewBaseMessage()
	_, _ = baseMsg.SetBodyBytes(rm.Body)
	m := &MessageServiceBus{BaseMessage: baseMsg, provider: p, received: rm, rsvpURL: u}
	for k, v := range rm.ApplicationProperties {
		setHeaderFromAny(baseMsg, k, v)
		m.setProp(k, v)
	}
	if sid := deref(rm.SessionID); sid != "" {
		baseMsg.SetRoutingKey(sid)
	} else if pk := deref(rm.PartitionKey); pk != "" {
		baseMsg.SetRoutingKey(pk)
	}
	return m
}

// setHeaderFromAny stores an AMQP application property as a golly header of
// the matching type. Unsigned integers widen to int64; time.Time becomes an
// RFC 3339 string; anything else is formatted with %v.
func setHeaderFromAny(bm *messaging.BaseMessage, k string, v any) {
	switch x := v.(type) {
	case string:
		bm.SetStrHeader(k, x)
	case bool:
		bm.SetBoolHeader(k, x)
	case int:
		bm.SetIntHeader(k, x)
	case int8:
		bm.SetInt8Header(k, x)
	case int16:
		bm.SetInt16Header(k, x)
	case int32:
		bm.SetInt32Header(k, x)
	case int64:
		bm.SetInt64Header(k, x)
	case uint8:
		bm.SetInt64Header(k, int64(x))
	case uint16:
		bm.SetInt64Header(k, int64(x))
	case uint32:
		bm.SetInt64Header(k, int64(x))
	case uint64:
		bm.SetInt64Header(k, int64(x))
	case float32:
		bm.SetFloatHeader(k, x)
	case float64:
		bm.SetFloat64Header(k, x)
	case []byte:
		bm.SetHeader(k, x)
	case time.Time:
		bm.SetStrHeader(k, x.Format(time.RFC3339Nano))
	case nil:
		// skip
	default:
		bm.SetStrHeader(k, fmt.Sprintf("%v", x))
	}
}

// --- listeners --------------------------------------------------------------

// AddListener registers a listener that continuously receives messages in a
// background goroutine. Supported options: WaitTimeSeconds, MaxMessages,
// Timeout, ReceiveMode, Subscription, SessionID, AcceptNextSession,
// SubQueue, NamedListener and the broker-targeted options.
//
// AddListener delegates to AddListenerCtx with context.Background().
func (p *Provider) AddListener(u *url.URL, listener func(msg messaging.Message), options ...messaging.Option) error {
	return p.AddListenerCtx(context.Background(), u, listener, options...)
}

// AddListenerCtx is the context-aware variant of AddListener. Cancelling
// ctx stops the listener, as do RemoveListeners / RemoveNamedListener and
// Close. The listener owns a dedicated receiver; messages should be
// settled (Rsvp) inside the callback, before the receiver is closed.
//
// With AcceptNextSession the listener locks the next available session,
// drains it until a poll comes back empty, releases it and moves on to the
// next session.
func (p *Provider) AddListenerCtx(ctx context.Context, u *url.URL, listener func(msg messaging.Message), options ...messaging.Option) error {
	if listener == nil {
		return fmt.Errorf("%w: listener func is nil", ErrInvalidOption)
	}
	if p.closed.Load() {
		return messaging.ErrProviderClosed
	}
	plan, err := p.planReceive(ctx, u, options)
	if err != nil {
		return err
	}
	client, err := p.getClient(u, plan.t)
	if err != nil {
		return err
	}
	// Create the non-session receiver up front so configuration errors
	// surface synchronously.
	var rcv sbReceiver
	if !plan.rs.sessionful() {
		if rcv, err = client.NewReceiver(plan.t, plan.mode); err != nil {
			return fmt.Errorf("servicebus: failed to create receiver for %s: %w", plan.t.entityPath(), err)
		}
	}

	var (
		lctx   context.Context
		cancel context.CancelFunc
	)
	if plan.rs.hasTimeout {
		lctx, cancel = context.WithTimeout(ctx, plan.rs.timeout)
	} else {
		lctx, cancel = context.WithCancel(ctx)
	}

	entry := &listenerEntry{name: plan.rs.listenerName, cancel: cancel, done: make(chan struct{})}
	key := plan.t.key()
	p.mu.Lock()
	if p.listeners == nil {
		p.listeners = make(map[string][]*listenerEntry)
	}
	p.listeners[key] = append(p.listeners[key], entry)
	p.mu.Unlock()

	go p.runListener(lctx, u, client, plan, rcv, listener, entry)
	return nil
}

func (p *Provider) runListener(lctx context.Context, u *url.URL, client sbClient, plan *receivePlan, rcv sbReceiver, listener func(messaging.Message), entry *listenerEntry) {
	defer close(entry.done)
	defer entry.cancel()
	defer func() {
		if rcv != nil {
			closeQuietly(rcv.Close)
		}
		p.forgetListener(plan.t.key(), entry)
	}()
	obs := p.getObserver()
	logger.InfoF("Service Bus listener started for %s", plan.t.entityPath())
	defer logger.InfoF("Service Bus listener stopped for %s", plan.t.entityPath())

	backoff := func() bool {
		select {
		case <-lctx.Done():
			return false
		case <-time.After(listenerBackoff):
			return true
		}
	}

	for {
		if p.closed.Load() || lctx.Err() != nil {
			return
		}
		waitCtx, cancel := context.WithTimeout(lctx, plan.rs.wait)

		if rcv == nil {
			var err error
			switch {
			case plan.rs.sessionful():
				rcv, err = client.AcceptSession(waitCtx, plan.t, plan.rs.sessionID, plan.mode)
			default:
				rcv, err = client.NewReceiver(plan.t, plan.mode)
			}
			if err != nil {
				timedOut := waitCtx.Err() != nil || isTimeout(err)
				cancel()
				rcv = nil
				if lctx.Err() != nil {
					return
				}
				if timedOut {
					continue // no session available yet
				}
				obs.OnReceive(u, nil, err)
				logger.ErrorF("Service Bus listener receiver error on %s: %v", plan.t.entityPath(), err)
				if !backoff() {
					return
				}
				continue
			}
			if plan.rs.sessionful() {
				// Session locked: poll it with a fresh wait window.
				cancel()
				continue
			}
		}

		msgs, err := rcv.ReceiveMessages(waitCtx, plan.rs.maxMessages, nil)
		timedOut := waitCtx.Err() != nil
		cancel()
		if err != nil && !timedOut && !isTimeout(err) {
			if lctx.Err() != nil {
				return
			}
			obs.OnReceive(u, nil, err)
			logger.ErrorF("Service Bus listener receive error on %s: %v", plan.t.entityPath(), err)
			if isFatal(err) {
				closeQuietly(rcv.Close)
				rcv = nil
			}
			if !backoff() {
				return
			}
			continue
		}
		if len(msgs) == 0 {
			if plan.rs.acceptNextSession {
				// Session drained: release it and move on to the next.
				closeQuietly(rcv.Close)
				rcv = nil
			}
			continue
		}
		for _, m := range p.wrapReceived(u, msgs, rcv, plan.mode, nil) {
			if p.closed.Load() {
				return
			}
			obs.OnReceive(u, m, nil)
			listener(m)
		}
	}
}

func (p *Provider) forgetListener(key string, entry *listenerEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entries := p.listeners[key]
	for i, e := range entries {
		if e == entry {
			entries = append(entries[:i], entries[i+1:]...)
			break
		}
	}
	if len(entries) == 0 {
		delete(p.listeners, key)
	} else {
		p.listeners[key] = entries
	}
}

// RemoveListeners cancels every listener registered for the URL's receive
// source (namespace + entity [+ subscription]). Idempotent. Implements
// messaging.ListenerRemover.
func (p *Provider) RemoveListeners(u *url.URL) error {
	t, err := parseTarget(u)
	if err != nil {
		return err
	}
	p.mu.Lock()
	entries := p.listeners[t.key()]
	delete(p.listeners, t.key())
	p.mu.Unlock()
	for _, e := range entries {
		e.cancel()
	}
	return nil
}

// RemoveNamedListener cancels listeners registered under the given name
// (messaging.NamedListener option) for the URL. Other listeners on the same
// source keep receiving. Idempotent. Implements messaging.ListenerRemover.
func (p *Provider) RemoveNamedListener(u *url.URL, name string) error {
	t, err := parseTarget(u)
	if err != nil {
		return err
	}
	p.mu.Lock()
	entries := p.listeners[t.key()]
	var kept, removed []*listenerEntry
	for _, e := range entries {
		if e.name == name {
			removed = append(removed, e)
		} else {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		delete(p.listeners, t.key())
	} else {
		p.listeners[t.key()] = kept
	}
	p.mu.Unlock()
	for _, e := range removed {
		e.cancel()
	}
	return nil
}

// Close stops all listeners and closes every cached receiver, sender and
// namespace connection. A closed provider can be re-armed with Setup;
// Send / Receive reconnect lazily.
func (p *Provider) Close() error {
	p.closed.Store(true)
	p.mu.Lock()
	listeners := p.listeners
	receivers := p.receivers
	sessions := p.sessions
	senders := p.senders
	clients := p.clients
	p.listeners, p.receivers, p.sessions, p.senders, p.clients = nil, nil, nil, nil, nil
	p.mu.Unlock()

	for _, entries := range listeners {
		for _, e := range entries {
			e.cancel()
		}
	}
	for _, entries := range listeners {
		for _, e := range entries {
			select {
			case <-e.done:
			case <-time.After(closeTimeout):
			}
		}
	}
	for _, sr := range receivers {
		closeQuietly(sr.r.Close)
	}
	for r := range sessions {
		closeQuietly(r.Close)
	}
	for _, s := range senders {
		closeQuietly(s.Close)
	}
	var firstErr error
	for ns, c := range clients {
		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		if err := c.Close(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("servicebus: close namespace %q: %w", ns, err)
		}
		cancel()
	}
	return firstErr
}
