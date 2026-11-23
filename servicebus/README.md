# servicebus

Azure Service Bus implementation of the [golly messaging](https://pkg.go.dev/oss.nandlabs.io/golly/messaging) `Provider` interface — queues **and** topics / subscriptions in one provider (the Azure counterpart of golly-aws `sqs` + `sns` and golly-gcp `pubsub`).

---

- [Installation](#installation)
- [Features](#features)
- [Architecture](#architecture)
- [Auto-Registration](#auto-registration)
- [URL Format](#url-format)
- [Configuration](#configuration)
- [Usage](#usage)
- [Message Settlement (Rsvp)](#message-settlement-rsvp)
- [Message Headers & Application Properties](#message-headers--application-properties)
- [Keyed Messages, Sessions & Partition Keys](#keyed-messages-sessions--partition-keys)
- [Options](#options)
- [Broker-Targeted Options](#broker-targeted-options)
- [Error Handling](#error-handling)
- [Thread Safety, Connections & Graceful Shutdown](#thread-safety-connections--graceful-shutdown)
- [Testing](#testing)
- [API Reference](#api-reference)
- [Prerequisites](#prerequisites)

---

## Installation

```bash
go get oss.nandlabs.io/golly-azure/servicebus
```

## Features

- **Send** — send a single message to a queue or topic
- **SendBatch** — send any number of messages, automatically packed into as few Service Bus batches as the namespace's maximum batch size allows
- **Receive / ReceiveBatch** — on-demand receive from a queue or topic subscription with a configurable wait time
- **AddListener** — continuously receive in a background goroutine with error backoff and automatic receiver recovery
- **ListenerRemover** — `RemoveListeners` / `RemoveNamedListener` detach listeners without closing the provider
- **Context-aware** — `SendCtx`, `SendBatchCtx`, `ReceiveCtx`, `ReceiveBatchCtx`, `AddListenerCtx` (`messaging.ProducerCtx` / `messaging.ReceiverCtx`)
- **Observable** — `SetObserver` fires `OnSend` / `OnReceive` / `OnAck` / `OnNack` (`messaging.ObservableProvider`)
- **Settlement** — complete, abandon, dead-letter or defer through `Rsvp`
- **Receive modes** — PeekLock (default) or ReceiveAndDelete
- **Sessions** — `Keyed` routing key → `SessionID`; receive a specific session or the next available one
- **Scheduling & TTL** — `ScheduledEnqueueTime`, `DelaySeconds`, `TimeToLive`
- **Dead-letter queues** — receive from `/$deadletterqueue` sub-queues
- **Broker-targeted options** — `DeliveryGuarantee`, `ConsumerMode`, `AckTimeout`, `MaxDeliveryAttempts`, `DeadLetter`, verified against the entity configuration
- **Config resolution** — `azcfg`: connection string when set, token credential (DefaultAzureCredential by default) otherwise
- **Auto-registration** — blank import registers the provider with the golly messaging manager

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│  Application                                                     │
│                                                                  │
│  import _ "oss.nandlabs.io/golly-azure/servicebus"               │
│                                                                  │
│  mgr := messaging.GetManager()                                   │
│  mgr.Send(url, msg, opts...)                                     │
│  mgr.Receive(url, opts...)                                       │
│  mgr.AddListener(url, fn, opts...)                               │
└─────────────────────────┬────────────────────────────────────────┘
                          │
                          ▼
┌──────────────────────────────────────────────────────────────────┐
│  golly/messaging.Manager                                         │
│  Routes to provider by URL scheme ("servicebus")                 │
└─────────────────────────┬────────────────────────────────────────┘
                          │
                          ▼
┌──────────────────────────────────────────────────────────────────┐
│  servicebus.Provider                                             │
│                                                                  │
│  1. parseTarget(u)        → namespace / entity / subscription    │
│  2. getClient(namespace)  → one cached azservicebus.Client       │
│  3. getSender / receiver  → cached per entity                    │
│  4. SDK call              → SendMessage / ReceiveMessages / ...  │
└─────────────────────────┬────────────────────────────────────────┘
                          │
                          ▼
┌──────────────────────────────────────────────────────────────────┐
│  azcfg.Manager                                                   │
│  namespace → host+path → "servicebus" → "default" → new Config   │
│  ConnectionString ? NewClientFromConnectionString                │
│                   : NewClient(<ns>.servicebus.windows.net, cred) │
└──────────────────────────────────────────────────────────────────┘
```

## Auto-Registration

`init()` in `pkg.go` registers a `Provider` with `messaging.GetManager()`:

```go
import _ "oss.nandlabs.io/golly-azure/servicebus"
```

After this import any `servicebus://` URL passed to the messaging manager routes to this provider.

## URL Format

```
servicebus://<namespace>/<queue>
servicebus://<namespace>/<topic>
servicebus://<namespace>/<topic>?subscription=<subscription>
servicebus://<namespace>/<topic>/subscriptions/<subscription>
servicebus://<namespace>/<queue>/$deadletterqueue
servicebus://<namespace>/<topic>/subscriptions/<subscription>/$deadletterqueue
```

| Component | Maps To                                                                                      |
| --------- | -------------------------------------------------------------------------------------------- |
| Scheme    | `servicebus`                                                                                 |
| Host      | Namespace: a bare name (`myns` → `myns.servicebus.windows.net`) or a fully-qualified host    |
| Path      | Queue or topic name; optionally `/subscriptions/<sub>` and a `$deadletterqueue` suffix       |
| Query     | `subscription=<name>` — the topic subscription to receive from                               |

**Why the namespace is the host:** unlike SQS queue names or Pub/Sub topic names, Service Bus entity names are only unique within a namespace, and credentials / connection strings are scoped to the namespace. Keeping the namespace in the host lets one process talk to several namespaces, and `azcfg` resolves configuration by namespace.

**Send vs receive on topics.** Sends always go to the entity in the path (queue or topic); a `subscription` in the URL is ignored on send, so the same subscription URL can be used for both directions. Receives without a subscription target a queue. Bare namespace names are expanded with the suffix of the configured cloud (`servicebus.usgovcloudapi.net`, `servicebus.chinacloudapi.cn`).

| URL                                              | Send to       | Receive from                          |
| ------------------------------------------------ | ------------- | ------------------------------------- |
| `servicebus://myns/orders`                       | queue orders  | queue orders                          |
| `servicebus://myns/events?subscription=audit`    | topic events  | subscription events/audit             |
| `servicebus://myns/events/subscriptions/audit`   | topic events  | subscription events/audit             |
| `servicebus://myns/orders/$deadletterqueue`      | —             | dead-letter queue of orders           |

## Configuration

Configuration is resolved via [`azcfg`](../azcfg/) with `azcfg.GetConfigOrDefault(u, "servicebus")`:

1. **`url.Host`** — the namespace (e.g. `"myns"`)
2. **`url.Host + "/" + url.Path`**
3. **`"servicebus"`** — package-wide fallback
4. **`"default"`** — process-wide fallback
5. A fresh `azcfg.NewConfig()` (DefaultAzureCredential)

If the resolved config has a `ConnectionString` the client is created with `NewClientFromConnectionString`; otherwise with `NewClient(<fqdn>, cfg.Credential())`. `cfg.Endpoint`, when set, is used as the AMQP `CustomEndpoint` (e.g. a private endpoint or proxy).

### Token credential (recommended)

```go
cfg := azcfg.NewConfig() // DefaultAzureCredential
// cfg.SetManagedIdentity("")                        // system-assigned MI
// cfg.SetClientSecret(tenantID, clientID, secret)   // service principal
azcfg.Manager.Register("myns", cfg)
```

The identity needs the **Azure Service Bus Data Sender** / **Data Receiver** roles (and **Owner**/Manage rights only when using the broker-option verification, which reads entity configuration).

### Connection string / emulator

```go
cfg := azcfg.NewConfig()
cfg.SetConnectionString("Endpoint=sb://localhost;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=SAS_KEY_VALUE;UseDevelopmentEmulator=true;")
azcfg.Manager.Register("localhost", cfg) // URLs: servicebus://localhost/queue.1
```

### Per-namespace configuration

```go
azcfg.Manager.Register("orders-ns", ordersCfg)   // servicebus://orders-ns/...
azcfg.Manager.Register("servicebus", defaultCfg) // everything else
```

## Usage

### Sending

```go
mgr := messaging.GetManager()
u, _ := url.Parse("servicebus://myns/orders")

msg, _ := mgr.NewMessage("servicebus")
msg.SetBodyStr(`{"orderId": 42}`)
msg.SetStrHeader("source", "checkout")

err := mgr.Send(u, msg, messaging.NewOptionsBuilder().
    Add(servicebus.OptContentType, "application/json").
    Add(servicebus.OptSubject, "order.created").
    Add(servicebus.OptTimeToLive, time.Hour).
    Build()...)
```

### Sending a batch

```go
err := mgr.SendBatch(u, msgs) // split into as many AMQP batches as needed
```

### Publishing to a topic and subscribing

```go
topic, _ := url.Parse("servicebus://myns/events")
sub, _ := url.Parse("servicebus://myns/events?subscription=audit")

_ = mgr.Send(topic, msg)
m, err := mgr.Receive(sub)
```

### Receiving

```go
opts := messaging.NewOptionsBuilder().Add(servicebus.OptWaitTimeSeconds, 10).Build()
m, err := mgr.Receive(u, opts...)
if errors.Is(err, servicebus.ErrNoMessages) { /* nothing within 10s */ }
_ = m.Rsvp(true)

batch, err := mgr.ReceiveBatch(u, messaging.NewOptionsBuilder().Add(servicebus.OptBatchSize, 20).Build()...)
```

### Listeners

```go
err := mgr.AddListener(sub, func(m messaging.Message) {
    process(m)
    _ = m.Rsvp(true) // settle inside the callback
}, messaging.NewOptionsBuilder().
    Add(servicebus.OptMaxMessages, 20).
    AddNamedListener("auditors").
    Build()...)

_ = mgr.RemoveNamedListener(sub, "auditors") // or mgr.RemoveListeners(sub)
```

Each listener owns a dedicated receiver that is closed when the listener stops (ctx cancellation, `Timeout`, `RemoveListeners`, `RemoveNamedListener`, `Close`). Receive errors are reported to the observer, logged and retried after a 1s backoff; fatal link errors recreate the receiver.

### Scheduled messages

```go
mgr.Send(u, msg, messaging.Option{Key: servicebus.OptScheduledEnqueueTime, Value: time.Now().Add(time.Hour)})
mgr.Send(u, msg, messaging.Option{Key: servicebus.OptDelaySeconds, Value: 30}) // sqs-style shortcut
```

### Dead-letter queue

```go
dlq, _ := url.Parse("servicebus://myns/orders/$deadletterqueue")
m, _ := mgr.Receive(dlq)
fmt.Println(*m.(*servicebus.MessageServiceBus).ReceivedMessage().DeadLetterReason)
```

## Message Settlement (Rsvp)

Messages are received in **PeekLock** mode by default and must be settled:

| Call                                                        | Service Bus action | Observer           |
| ----------------------------------------------------------- | ------------------ | ------------------ |
| `Rsvp(true)`                                                | Complete           | `OnAck`            |
| `Rsvp(false)`                                               | Abandon (redeliver, DeliveryCount+1) | `OnNack(requeue=true)` |
| `Rsvp(false, Requeue=false)` (`messaging.RequeueOpt`)       | Dead-letter        | `OnNack(requeue=false)` |
| `Rsvp(false, DeadLetterReason=…, DeadLetterDescription=…)`  | Dead-letter with reason | `OnNack(requeue=false)` |
| `Rsvp(false, Defer=true)`                                   | Defer              | `OnNack(requeue=false)` |

This mirrors `sqs` (reject → visible again) and `pubsub` (nack → redeliver). Once `MaxDeliveryCount` is exceeded Service Bus dead-letters abandoned messages automatically. Settlement is idempotent (a second `Rsvp` is a no-op); a failed settlement can be retried. `RsvpCtx` is the context-aware variant. Messages received in **ReceiveAndDelete** mode are already removed, so `Rsvp` is a no-op.

## Message Headers & Application Properties

Headers set on a `*MessageServiceBus` are sent as Service Bus **application properties with their native AMQP types**, and received application properties become golly headers of the matching type:

| Header setter                     | Property type | Read back with      |
| --------------------------------- | ------------- | ------------------- |
| `SetStrHeader`                    | string        | `GetStrHeader`      |
| `SetBoolHeader`                   | bool          | `GetBoolHeader`     |
| `SetIntHeader`                    | **int64**     | `GetInt64Header`    |
| `SetInt8/16/32/64Header`          | int8/16/32/64 | same-width getter   |
| `SetFloatHeader` / `SetFloat64Header` | float32 / float64 | same getter  |
| `SetHeader` ([]byte)              | binary        | `GetHeader`         |

Unsigned properties from other clients widen to int64; `time.Time` properties become RFC 3339 strings; other types are formatted with `%v`. golly's typed getters type-assert, so read headers with the getter matching the stored type. Messages that are not created by this provider (e.g. a plain `BaseMessage`) send no properties, because their headers cannot be enumerated.

Broker metadata is available on received messages: `MessageID()`, `SessionID()`, `SequenceNumber()`, `DeliveryCount()`, `EnqueuedTime()` and the raw `ReceivedMessage()`.

## Keyed Messages, Sessions & Partition Keys

`messaging.Keyed` routing keys map to **`SessionID`** by default. This is the Service Bus primitive that gives ordered (FIFO) delivery per key on session-enabled entities, and on partitioned entities the session id also selects the partition. Non-session entities accept and ignore the session id, so the default is safe everywhere. Change the mapping with `OptKeyMapping`:

| `OptKeyMapping`             | Routing key becomes |
| --------------------------- | ------------------- |
| `"session"` (default)       | `SessionID`         |
| `"partition"`               | `PartitionKey`      |
| `"none"`                    | ignored             |

Explicit `OptSessionID` / `OptPartitionKey` take precedence. If both a session id and a partition key end up set they must be equal (Service Bus rule); otherwise `ErrInvalidOption` is returned before any network I/O. On receive the message's `SessionID` (or else `PartitionKey`) is restored as the routing key.

Session-enabled entities must be received through a session:

```go
// A specific session (the session lock is cached and reused across Receive calls)
mgr.Receive(u, messaging.Option{Key: servicebus.OptSessionID, Value: "tenant-42"})

// The next available session; released once all received messages are settled
mgr.ReceiveBatch(u, messaging.Option{Key: servicebus.OptAcceptNextSession, Value: true})

// Listener that drains sessions one after another
mgr.AddListener(u, fn, messaging.Option{Key: servicebus.OptAcceptNextSession, Value: true})
```

## Options

| Option                    | Type                       | Applies to          | Description |
| ------------------------- | -------------------------- | ------------------- | ----------- |
| `WaitTimeSeconds`         | int                        | Receive*, listener  | Wait for the first message per call / poll (default 5) |
| `BatchSize`               | int                        | ReceiveBatch        | Max messages (default 10, max 5000) |
| `MaxMessages`             | int                        | listener, ReceiveBatch | Max messages per poll (default 10) |
| `Timeout`                 | int seconds                | listener            | Total listener lifetime |
| `ReceiveMode`             | `"PeekLock"`/`"ReceiveAndDelete"` or `azservicebus.ReceiveMode` | receive | Settlement mode |
| `Subscription`            | string                     | receive             | Alternative to `?subscription=` |
| `SubQueue`                | `"deadletter"`/`"transfer"` | receive            | Alternative to `/$deadletterqueue` |
| `SessionID`               | string                     | send, receive       | Explicit session id / session to lock |
| `AcceptNextSession`       | bool                       | receive             | Lock the next available session |
| `NamedListener`           | string                     | listener            | Group name for `RemoveNamedListener` |
| `PartitionKey`            | string                     | send                | `Message.PartitionKey` |
| `KeyMapping`              | `"session"`/`"partition"`/`"none"` | send        | Keyed routing-key mapping |
| `MessageID`               | string                     | Send                | Overrides the default (golly `Id()`); used for duplicate detection |
| `CorrelationID`, `Subject`, `ContentType`, `ReplyTo` | string | send        | System properties |
| `TimeToLive`              | `time.Duration` or int seconds | send            | Message TTL |
| `ScheduledEnqueueTime`    | `time.Time`                | send                | Scheduled delivery |
| `DelaySeconds`            | int                        | send                | Shortcut for now + n seconds |
| `DeadLetterReason`, `DeadLetterDescription` | string   | Rsvp(false)         | Dead-letter instead of abandon |
| `Defer`                   | bool                       | Rsvp(false)         | Defer instead of abandon |

All option keys are exported as `Opt*` constants. Wrong value types or conflicting options return `ErrInvalidOption`.

## Broker-Targeted Options

The golly broker-targeted options are translated as follows. The provider never creates or reconfigures entities — options that depend on entity configuration are **verified** through the management plane (requires Manage rights) and return `ErrEntityMismatch` when unmet, mirroring the `sqs` RedrivePolicy and `pubsub` DeadLetterPolicy checks.

| Option                | Receive side | Send side |
| --------------------- | ------------ | --------- |
| `DeliveryGuarantee`   | `AtLeastOnce` → PeekLock; `AtMostOnce` → ReceiveAndDelete; `ExactlyOnce` → `ErrUnsupportedOption` | `ExactlyOnce` requires `RequiresDuplicateDetection` on the queue/topic; `AtMostOnce` logs a warning |
| `ConsumerMode`        | `Pull` accepted; `Push` → `ErrUnsupportedOption` (the Go SDK is pull-based) | rejected |
| `AckTimeout`          | must be ≤ 5m and ≤ the entity `LockDuration` | rejected |
| `MaxDeliveryAttempts` | entity `MaxDeliveryCount` must be ≥ n | rejected |
| `DeadLetter`          | `"$deadletterqueue"` (built-in, no lookup) or must match `ForwardDeadLetteredMessagesTo` | rejected |

## Error Handling

| Error                  | When |
| ---------------------- | ---- |
| `ErrInvalidURL`        | Missing namespace / entity, unsupported path |
| `ErrInvalidOption`     | Wrong option type, out-of-range or conflicting options |
| `ErrUnsupportedOption` | Broker option Service Bus cannot honour |
| `ErrEntityMismatch`    | Entity configuration does not satisfy a broker option |
| `ErrNoMessages`        | Receive / ReceiveBatch got nothing within `WaitTimeSeconds` |
| `messaging.ErrProviderClosed` | `AddListener` after `Close` (call `Setup` to re-arm) |

SDK errors are wrapped with `%w`, so `var e *azservicebus.Error; errors.As(err, &e)` exposes the SDK `Code`. Context cancellation is returned wrapped (`errors.Is(err, context.Canceled)`).

## Thread Safety, Connections & Graceful Shutdown

- The provider keeps **one AMQP connection per namespace**, one sender per queue/topic and one receiver per receive source + mode (+ session) for `Receive`/`ReceiveBatch`; they are created lazily and are safe for concurrent use (receives on a shared receiver are serialised).
- Fatal link errors (closed, connection lost, lock lost, not found, unauthorized) evict the cached sender/receiver so the next call reconnects.
- `Close()` stops all listeners (waiting for them to exit), then closes receivers, session receivers, senders and connections. `Setup()` re-arms a closed provider.

## Testing

Unit tests run offline against fakes of small internal interfaces over the SDK sender / receiver / client / admin surface:

```bash
GOWORK=off go test -race ./servicebus/...
```

Emulator / live tests are skipped unless `SERVICEBUS_TEST_CONNECTION_STRING` is set. With the [Service Bus emulator](https://learn.microsoft.com/azure/service-bus-messaging/overview-emulator) (default config entities `queue.1`, `topic.1`/`subscription.1`):

```bash
SERVICEBUS_TEST_CONNECTION_STRING='Endpoint=sb://localhost;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=SAS_KEY_VALUE;UseDevelopmentEmulator=true;' \
  GOWORK=off go test -run Emulator ./servicebus/...
```

Override entities with `SERVICEBUS_TEST_QUEUE`, `SERVICEBUS_TEST_TOPIC`, `SERVICEBUS_TEST_SUBSCRIPTION`. A runnable walkthrough lives in [`examples/servicebus`](../examples/servicebus/main.go).

## API Reference

### Provider

| Method | Description |
| ------ | ----------- |
| `Id()`, `Schemes()`, `Setup()`, `NewMessage(scheme, ...)` | Provider basics (`"servicebus-provider"`, `["servicebus"]`) |
| `Send` / `SendCtx` | Send one message |
| `SendBatch` / `SendBatchCtx` | Send many messages in packed batches |
| `Receive` / `ReceiveCtx` | Receive one message |
| `ReceiveBatch` / `ReceiveBatchCtx` | Receive up to `BatchSize` messages |
| `AddListener` / `AddListenerCtx` | Background receive loop |
| `RemoveListeners(u)` / `RemoveNamedListener(u, name)` | Detach listeners |
| `SetObserver(obs)` | Install / clear an observer |
| `Close()` | Stop listeners and release connections |

### MessageServiceBus

Embeds `*messaging.BaseMessage` (body, headers, `Keyed`), plus:

| Method | Description |
| ------ | ----------- |
| `Rsvp(accept, opts...)` / `RsvpCtx` | Settle (complete / abandon / dead-letter / defer) |
| `ApplicationProperties()` | Copy of the properties sent / received |
| `ReceivedMessage()` | Underlying `*azservicebus.ReceivedMessage` |
| `MessageID()`, `SessionID()`, `SequenceNumber()`, `DeliveryCount()`, `EnqueuedTime()` | Broker metadata |

## Prerequisites

- An Azure Service Bus namespace (Standard or Premium for topics), or the Service Bus emulator
- Data-plane roles: **Azure Service Bus Data Sender** and/or **Data Receiver** (or a SAS connection string with Send/Listen)
- Manage rights only for broker-option verification (`MaxDeliveryAttempts`, `AckTimeout`, custom `DeadLetter`, `ExactlyOnce`)
- Session-enabled entities for ordered delivery per key
