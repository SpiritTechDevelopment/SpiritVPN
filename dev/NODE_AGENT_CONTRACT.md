# Backend ↔ node-agent contract

Status: **superseded; do not implement**.

This document belongs to the obsolete device/entry-assignment architecture. The
normative backend v1 boundaries and node-agent requirements are defined in
[`BACKEND_DOMAIN_AGREEMENTS.md`](BACKEND_DOMAIN_AGREEMENTS.md). This file is kept
only as historical context.

The historical proposal below described the abandoned `spiritvpn.nodeagent.v1`
control protocol and is non-normative. The current repository still uses direct
Xray API helpers and an
add-only node snapshot for recovery; those remain the operational compatibility
path until the migration in [Transition from the current repository](#transition-from-the-current-repository)
is complete.

The machine-readable RPC schema is
[`contracts/nodeagent/v1/node_agent.proto`](../../contracts/nodeagent/v1/node_agent.proto).

## 1. Goals and non-goals

The design must:

- keep the central backend database authoritative for customers, assignments,
  quotas, and accumulated usage;
- prevent the backend from calling Xray directly;
- add/remove users through a node-local agent without restarting Xray;
- survive backend/network outages without dropping already captured usage;
- restore Xray runtime users after an Xray restart without waiting for the
  backend;
- automatically repair drift between backend desired state, agent local state,
  and Xray actual state;
- expose aggregate per-user uplink/downlink volume;
- deliver privacy-reduced source-IP activity without raw IPs or destinations.

The first version does not provide:

- exact financial-grade byte accounting across abrupt Xray process failure;
- hard termination guarantees for a connection that was already established
  when its user was removed;
- identification of physical devices from access logs or IP addresses;
- high availability for a single entry node;
- a message broker, long-poll channel, or versioned desired-list protocol.

## 2. Authority and state locations

| State | Authoritative owner | Durable location | Rebuild source |
|---|---|---|---|
| Customer and VPN credential | Backend | Central PostgreSQL | Backend backup |
| User-to-entry assignment | Backend | Central PostgreSQL | Backend backup |
| Quota and billing-period totals | Backend | Central PostgreSQL | Backend backup |
| Pending agent operation | Backend | Central PostgreSQL outbox | Backend desired state |
| Applied user cache | Node agent | Local SQLite | Full backend reconciliation |
| Operation idempotency log | Node agent | Local SQLite | Expires only after a safe retention window |
| Undelivered usage | Node agent | Local SQLite outbox | Not rebuildable after Xray reset |
| Undelivered source activity | Node agent | Independent local SQLite outbox | Rebuilt only while local access log remains |
| Runtime users | Xray | Process memory | Agent SQLite |
| Live traffic counters | Xray | Process memory | Not rebuildable after process failure |

The agent never connects to central PostgreSQL. Xray never becomes a desired-state
store. The local SQLite database is a disposable-but-protected recovery cache, not
a second business database.

Deployment ownership is split deliberately:

- this infrastructure repository provisions and protects the PostgreSQL service on
  `control-1`, its database, role, network policy, storage, and backups;
- the backend repository owns PostgreSQL schema migrations and runs them as a
  deployment/startup prerequisite under a migration lock;
- the agent owns and migrates its SQLite schema; Ansible creates only its protected
  directories and service configuration.

Recommended agent paths:

```text
/var/lib/spirit-agent/state.db
/etc/spirit-agent/config.yml
/etc/spirit-agent/tls/
```

## 3. Identity agreement

Each backend-managed Xray user has two different identifiers:

```text
credential_uuid  = secret VLESS authentication credential
accounting_id    = stable pseudonymous Xray "email" and usage key
```

They MUST NOT be the same value. Rotating `credential_uuid` does not change
`accounting_id`, usage history, or quota ownership.

Version 1 accepts the repository's safe accounting-ID character set:

```text
[A-Za-z0-9._@:+-]{1,128}
```

Recommended customer namespace:

```text
u-<opaque-backend-id>
```

Reserved infrastructure/test namespaces include:

```text
svc-entry-*
via-*
e2e-*
```

The backend maps `(node_id, accounting_id)` to its internal user/credential
record. The agent does not send a trusted backend `user_id`.

There is no device entity in this protocol. Its unit of control and accounting is
one credential/accounting ID on one entry node. If the backend models customer
devices, it may issue one credential per device and maintain that mapping centrally;
the agent neither discovers nor invents devices.

## 4. Network and trust boundary

The backend is the gRPC client. Each entry agent serves gRPC on its WireGuard
management address. Connections use:

- the management WireGuard overlay;
- mutual TLS with one identity per entry node;
- a firewall rule that permits the agent port only from the backend;
- one reused HTTP/2 channel per node;
- deadlines, bounded message sizes, and retry backoff.

The backend validates the agent's server certificate and maps that identity to
`node_id`. The agent accepts only the backend client certificate and returns its
locally configured `node_id`; the backend rejects an identity/response mismatch.
A caller-provided `node_id` is never authorization input.

The agent talks to Xray only through:

```text
127.0.0.1:10085
```

After migration, remote access to Xray HandlerService/StatsService is closed.

## 5. RPC surface

`NodeAgentService` exposes:

| RPC | Purpose |
|---|---|
| `GetNodeState` | Health, independent usage/activity outboxes, and optional actual Xray user inventory |
| `EnsureUserPresent` | Idempotently add or repair one backend-owned user |
| `EnsureUserAbsent` | Capture final usage and idempotently remove one backend-owned user |
| `ReconcileUsers` | Complete bootstrap/disaster-recovery reconciliation |
| `Health` | Cheap readiness/liveness check |

All mutating calls carry a globally unique `operation_id`. A retry uses the same
ID. The agent persists the result and returns it for duplicate calls without
performing the mutation twice.

The operation log also stores a canonical request digest. Reusing an
`operation_id` with a different method or payload returns `PERMANENT_ERROR` and
does not mutate local or Xray state. Completed records are retained for at least
30 days and never for less than the backend's maximum retry horizon.
Canonicalization is semantic rather than raw protobuf bytes; in particular,
`ReconcileUsers.users` is validated for duplicate accounting IDs and sorted by
`accounting_id` before hashing.

The backend permits at most one mutating RPC in flight per node. Stats/state
polling may continue concurrently.

### 5.1 Operation result rules

`APPLIED`
: The requested state was changed and verified in Xray.

`ALREADY_APPLIED`
: Xray already matched the request; this is success.

`RETRYABLE_ERROR`
: A transient local/Xray error. The backend retains the operation and retries
  with the same ID.

`PERMANENT_ERROR`
: Invalid/unsupported input. Automatic retry stops and an alert is raised.

Authentication, malformed protobuf, deadline, and transport failures use normal
gRPC status codes. Operation diagnostics are sanitized and never contain
credentials or customer activity.

`Health.ready=true` means SQLite control/usage state is healthy/writable, Xray is
reachable, bootstrap is complete, and usage can be collected safely. It does not
depend on the backend having polled recently. Activity reports an independent
`ActivityState`; activity degradation must not make user control unavailable.

## 6. Local agent database

The implementation uses SQLite in WAL mode. Its logical tables are:

```text
agent_meta(
  usage_spool_id,
  activity_spool_id,
  initialized,
  last_xray_audit_at
)

managed_users(
  accounting_id,
  credential_uuid,
  flow,
  desired_present,
  applied,
  updated_at
)

operations(
  operation_id,
  operation_type,
  request_digest,
  status,
  result,
  created_at,
  updated_at
)

usage_batches(
  spool_id,
  sequence,
  collected_at,
  payload,
  acknowledged
)

activity_buckets(
  bucket_start,
  bucket_end,
  payload,
  closed
)

activity_batches(
  spool_id,
  sequence,
  bucket_start,
  bucket_end,
  payload,
  acknowledged
)
```

The database and directory are mode `0600`/`0700`. They contain VLESS UUIDs and
are sensitive. They do not require backup because the user cache is rebuilt from
the backend, but unacknowledged usage is lost if the disk is lost.

## 7. Backend database responsibilities

The backend owns at least these logical records:

```text
vpn_nodes
vpn_credentials
vpn_node_assignments
agent_operations
node_ingestion_cursors
usage_batches
usage_batch_items
usage_period_totals
activity_batches
source_ip_observations
source_ip_daily_totals
```

User assignment changes and creation of their pending `agent_operation` happen
in one PostgreSQL transaction. RPC execution happens after commit through a
durable worker/outbox.

Assignments are not physically deleted immediately: final usage may arrive after
an `EnsureUserAbsent`, and history must retain the accounting mapping.

Usage batches are unique by:

```text
(node_id, spool_id, sequence)
```

Totals are incremented only when that unique batch is first inserted.

Quota-period rollover is a central-ledger operation: the backend closes the old
period and opens a new total/baseline. It does not reset the agent outbox or issue
an extra Xray reset. A batch is assigned by `collected_at`; consequently the
period-boundary attribution error is bounded by one collection interval. A stricter
billing boundary would require a separate protocol and is outside v1.

When a committed period total reaches a quota, the backend changes the desired
assignment to disabled and enqueues `EnsureUserAbsent` in the same transaction.
Enforcement is intentionally near-real-time rather than instantaneous: normal
overshoot is bounded by the collection interval, backend poll interval, RPC
latency, and traffic already in flight.

## 8. Adding or rotating a user

Backend transaction:

1. create/update the credential and node assignment;
2. set desired state to active/pending;
3. insert an `EnsureUserPresent` operation;
4. commit.

Agent execution:

1. check the durable operation log;
2. validate accounting ID, UUID, flow, and ownership namespace;
3. acquire the node-local mutation/StatsService-reset lock;
4. on credential replacement, collect/reset and durably spool the old
   accounting ID's remaining usage;
5. persist the desired local user before touching Xray;
6. call local HandlerService;
7. verify the actual Xray user;
8. persist the operation result;
9. return success.

If the agent crashes after step 5, its local audit completes the Xray change on
restart. A credential rotation replaces `credential_uuid` while retaining
`accounting_id`.

The backend marks the assignment applied after RPC success. Client access should
not be reported ready until that acknowledgement.

## 9. Removing a user

Backend transaction:

1. set desired state to disabled/pending;
2. insert an `EnsureUserAbsent` operation;
3. commit.

Agent execution, under the same node-local mutation/StatsService-reset lock:

1. check the operation log;
2. collect/reset and persist the user's remaining usage;
3. persist `desired_present=false` (a local tombstone);
4. remove the user through local HandlerService;
5. verify absence;
6. persist the operation result;
7. return success.

If the agent crashes after the tombstone but before Xray removal, local
reconciliation finishes the operation. Infrastructure and unknown users are
never pruned.

Removal is guaranteed to reject a fresh connection after successful verification.
Whether the pinned Xray build terminates an already established connection is a
separate behavior that MUST be proven by an E2E test before claiming hard quota
enforcement.

## 10. Usage collection and delivery

Usage collection is agent-driven, not backend-availability-driven.

Default loop:

```text
Every 15 seconds (with a randomized phase):
  QueryStats(pattern="user>>>", reset=true)
  parse backend-owned counters
  persist batches in SQLite
```

The pinned Xray API returns all matching `Stat` records in one `QueryStats`
response and has no pagination. The agent therefore makes one local bulk query,
then chunks the result for SQLite/gRPC. It does not make two Xray RPCs per user.

Usage reset, credential replacement, and user removal are serialized by one local
lock. This prevents a bulk reset and a final-user reset from racing or assigning
the same bytes twice.

The final-user collection uses the complete stat-key prefix
`user>>><accounting_id>>>traffic>>>` and accepts only exact parsed uplink/downlink
keys; it never queries by a bare substring.

One Xray result becomes one or more ordered batches:

```text
spool_id = random UUID created with the local SQLite spool
sequence = monotonically increasing within spool_id
```

All chunks derived from one Xray reset are inserted in one SQLite transaction
before another reset is allowed. Chunk sequences are contiguous.

`GetNodeState` returns unacknowledged batches in ascending sequence, bounded by
`max_usage_batches`.

The backend:

1. inserts each batch and its items;
2. adds uplink/downlink to the current user period only for a newly inserted batch;
3. evaluates quota;
4. commits;
5. acknowledges the highest contiguous committed sequence on the next
   `GetNodeState`.

The agent deletes/compacts batches only after receiving an acknowledgement for
the matching `spool_id`. A stale or foreign-spool acknowledgement is ignored.
An acknowledgement beyond the highest sequence the agent has emitted is also
ignored and alerted.

If the outbox cannot safely persist another reset result, the agent stops issuing
new reset queries and raises a critical alert. There remains a small unavoidable
loss window between Xray's in-memory reset and the SQLite commit, plus up to one
collection interval on abrupt Xray process failure. This is suitable for quota
accounting, not exact financial byte billing.

## 11. Source-IP activity collection and delivery

Activity is a separate, lower-priority signal. It describes successful connection
observations, not stable VPN sessions or physical devices.

The entry agent consumes the protected local Xray access log using a persisted
inode/offset checkpoint. Before an observation can leave the node, it:

1. parses only the pinned and contract-tested Xray log format;
2. accepts only successful records with an exact backend-managed accounting ID;
3. discards destination address/domain, destination port, DNS, SNI, URL, and
   payload information;
4. canonicalizes IPv4 as `/32` and IPv6 using the configured prefix, initially
   `/64`;
5. computes a versioned HMAC source token;
6. aggregates observations into closed UTC-aligned 60-second buckets.

Version 1 computes:

```text
HMAC-SHA256(
  key,
  "spiritvpn-source-ip-v1\0"
  || family byte (4 or 6)
  || prefix-length byte
  || canonical 4-byte/16-byte network address
)
```

`ip_token` is the unpadded base64url encoding of the 32-byte result (43 ASCII
characters). `ip_token_key_id` matches `[A-Za-z0-9._-]{1,32}` and identifies the
configured key without exposing it. Raw source IP is never persisted in agent
SQLite, gRPC, backend PostgreSQL, or centralized logs.
Infrastructure provisions one fleet-scoped key per rotation epoch to entry agents,
with a bounded old/new overlap; the key itself is never sent in this gRPC protocol.

Each closed bucket becomes one or more immutable `ActivityBatch` messages:

```text
activity_spool_id = UUID for the independent activity spool
sequence          = monotonically increasing within that spool
```

Activity and usage have separate cursors, response limits, transactions, and ACKs.
`GetNodeState` may return both. The backend acknowledges activity only after the
corresponding PostgreSQL transaction commits. Duplicate delivery is harmless
because `(node_id, activity_spool_id, sequence)` is unique centrally.

Malformed/unknown log formats increment a parser-error metric and alert. The agent
does not forward the raw line as a fallback. Every pinned Xray upgrade requires
access-log parser fixture tests.

Activity storage has an independent quota/reservation and MUST NOT consume space
reserved for usage. If activity can no longer be spooled, the agent preserves
already acknowledged semantics, stops advancing its source checkpoint where
possible, increments a dropped/backlog metric, and alerts; usage collection and
user control continue. `ActivityState.healthy=false` reports this degradation to
the backend without changing `Health.ready`.

## 12. Automatic reconciliation

There are two independent repair loops.

### Agent-local repair

Every 30 seconds, and immediately after an Xray restart:

```text
agent managed_users ↔ actual Xray users
```

The agent restores missing managed users and completes tombstoned removals without
waiting for the backend.

### Backend repair

`GetNodeState(include_users=true)` returns the latest complete inventory observed
directly from Xray, `users_complete=true`, and its observation time. If the agent
cannot return the complete inventory within its configured response limit, the RPC
fails with `RESOURCE_EXHAUSTED`; it never labels a partial array complete.

The backend compares:

```text
central desired users ↔ agent-reported actual Xray users
```

It creates durable repair operations:

```text
desired but missing     → EnsureUserPresent
actual but undesired    → EnsureUserAbsent (backend-owned namespace only)
same ID, wrong UUID     → EnsureUserPresent (replace)
```

Before inserting a repair, the backend checks for an existing pending operation
for the same node/accounting ID. An inventory with `users_complete=false`, a stale
observation time, or `needs_bootstrap=true` never authorizes pruning.

For the initial fleet, every 15-second state poll may include users. If inventory
size becomes material, usage remains at 15 seconds while full inventory is
requested every 60 seconds. No protocol change is required.

## 13. Bootstrap and local database loss

A new/corrupt local database starts with:

```text
needs_bootstrap=true
initialized=false
```

In that state the agent does not prune users. The backend calls
`ReconcileUsers(complete=true)` with the full backend-owned set.

The explicit completeness flag prevents an empty/truncated payload from being
interpreted as permission to remove every customer. After atomic local persistence,
the agent applies and verifies the full diff under the mutation/StatsService-reset
lock, captures remaining counters before any removal, then marks itself initialized.

## 14. Failure behavior

| Failure | Required behavior |
|---|---|
| Backend/PostgreSQL unavailable | Agent keeps Xray state and spools usage/activity locally |
| Agent unavailable | Existing Xray traffic continues; backend operations remain pending |
| Xray restarts | Agent restores runtime users from SQLite |
| RPC response is lost | Backend retries the same `operation_id` |
| Agent crashes mid-add | Persisted local desired state is applied on recovery |
| Agent crashes mid-remove | Persisted tombstone is applied on recovery |
| SQLite is lost/corrupt | Agent enters bootstrap mode and refuses prune |
| Usage batch is resent | Backend unique key prevents double counting |
| Activity batch is resent | Independent backend unique key prevents double counting |
| Access-log parser no longer matches | Reject unsafe parsing, increment metric, and alert |
| Activity outbox is full | Preserve usage/control capacity; alert and stop/limit new activity |
| Disk/outbox is full | Stop new reset queries and alert; never discard unacknowledged batches |
| Node remains unreachable | Retain operation, back off, and alert |

## 15. Security and privacy invariants

- Agent and backend mutually authenticate; WireGuard alone is not application
  authentication.
- Agent controls only the configured customer inbound and namespace.
- The Xray management API is loopback-only after cutover.
- UUIDs, TLS keys, operation payloads, and local SQLite content are never logged.
- Usage contains only accounting ID and aggregate uplink/downlink bytes.
- Activity contains accounting ID, time bounds, counters, and a versioned HMAC
  source token; never a raw IP.
- Destination, destination port, DNS, SNI, URL, and browsing activity are not part
  of this contract.
- Error strings returned over RPC are sanitized and length-bounded.

## 16. Default timing and limits

Initial values, subject to load testing:

```text
Agent → Xray usage collection:  15 s
Agent activity bucket:          60 s, UTC-aligned and closed before emission
Agent → Xray user audit:        30 s
Backend → GetNodeState:         15 s + jitter
Full actual-user inventory:     every state poll initially; 60 s when needed
Unary RPC deadline:             5 s (longer for bounded full reconcile)
Operation retry:                exponential backoff, capped at 60 s
Mutating operations per node:   1 in flight
```

All response and batch sizes are bounded. The backend reuses gRPC channels instead
of creating a TLS connection for every poll.

Version 1 assumes one node's complete user inventory and full reconcile request fit
within the configured unary-message limit. Usage and activity are already chunked.
If the user set outgrows that limit, the implementation must introduce a separately
versioned paginated/streaming inventory and bootstrap contract; it must not silently
truncate v1 messages.

## 17. Observability contract

At minimum the agent exports:

```text
spirit_agent_up
spirit_agent_xray_up
spirit_agent_usage_last_collection_timestamp_seconds
spirit_agent_usage_outbox_batches
spirit_agent_usage_outbox_bytes
spirit_agent_activity_last_bucket_timestamp_seconds
spirit_agent_activity_outbox_batches
spirit_agent_activity_outbox_bytes
spirit_agent_activity_parser_errors_total
spirit_agent_activity_dropped_observations_total
spirit_agent_last_backend_poll_timestamp_seconds
spirit_agent_local_reconcile_errors_total
spirit_agent_needs_bootstrap
```

Alerts cover stale collection, growing/full outboxes, access-log parser failures,
activity drops, Xray unavailability, bootstrap stuck, repeated reconcile failure,
and backend node staleness.

Because the agent becomes the sole StatsService reset owner, the current central
`xray-usage-exporter` cannot continue treating raw Xray counters as monotonic.
Operational per-user dashboards must consume agent/backend accumulated counters or
an agent-exported monotonic series.

## 18. Locked decisions

- Backend calls agents over unary gRPC; agents do not call central PostgreSQL.
- Central PostgreSQL is authoritative; node-local SQLite is a recovery cache/outbox.
- There is no desired-list revision or long-poll protocol in v1.
- Commands are declarative (`Ensure*`), durable, ordered per node, and idempotent
  by `operation_id`.
- Reusing an operation ID with a different payload is rejected; StatsService reset
  and user mutations are locally serialized.
- Stats are collected locally in one bulk Xray query, reset, persisted, then pulled
  by the backend with explicit acknowledgement.
- Source activity is sanitized/aggregated on the entry node and delivered through
  an independent acknowledged outbox; raw IP and destination data never leave it.
- `accounting_id` and VLESS credential UUID are distinct.
- The protocol has no device abstraction; optional device mapping belongs to the
  backend.
- Reconciliation accompanies normal state/usage polling and is also performed
  locally by the agent.
- Infrastructure provisions PostgreSQL on `control-1`; the backend owns and runs
  its schema migrations. The agent owns its node-local SQLite schema.
- Normal user changes never restart Xray.
- A broker and bidirectional stream are deferred until fleet scale or network
  topology proves they are needed.

## 19. Transition from the current repository

The current repository still has:

- direct controller/backend access to Xray `:10085`;
- `xray-api.sh` add/remove/stats helpers;
- an add-only `/var/lib/xray/desired-users.json` recovery timer;
- a central non-resetting `xray-usage-exporter`.

Cut over in this order:

1. provision PostgreSQL on `control-1` through a dedicated infrastructure role and
   playbook, including backup/restore verification;
2. implement backend-owned migrations, durable operation/usage tables, and the v1
   gRPC client;
3. deploy the agent in observe-only mode with local SQLite, `reset=false`, and
   access-log parser fixtures;
4. compare agent inventory/usage and sanitized activity with existing Xray helpers
   and controlled test traffic;
5. enable agent mutations and bootstrap each entry from central desired state;
6. make the agent the sole reset owner and move operational usage metrics off the
   central raw-Xray exporter;
7. bind Xray management to loopback and close remote `:10085`;
8. split raw Xray access from operational log shipping and enable only sanitized
   activity delivery;
9. retire the old desired-user fetch/reconcile timers after restart recovery,
   quota/removal, activity-privacy, and redelivery E2E tests pass.

Until step 9, [BACKEND_INTEGRATION.md](BACKEND_INTEGRATION.md) remains the current
operational compatibility contract.
