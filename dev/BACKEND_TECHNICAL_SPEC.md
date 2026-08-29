# SpiritVPN backend technical specification

Status: **superseded; do not implement**.

This document describes an obsolete device/entry-assignment architecture. The
normative backend v1 design is
[`BACKEND_DOMAIN_AGREEMENTS.md`](BACKEND_DOMAIN_AGREEMENTS.md). This file is kept
only as historical context.

The historical proposal below described a different version-1 backend and is
non-normative. Its historical node protocol was described by
[`contracts/nodeagent/v1/node_agent.proto`](../../contracts/nodeagent/v1/node_agent.proto)
and [NODE_AGENT_CONTRACT.md](NODE_AGENT_CONTRACT.md). The currently deployed
direct-Xray compatibility path is documented separately in
[BACKEND_INTEGRATION.md](BACKEND_INTEGRATION.md).

The key words **MUST**, **MUST NOT**, **SHOULD**, and **MAY** express requirement
strength. Requirements apply to v1 unless a section explicitly says otherwise.

## 1. Purpose and success criteria

The backend is the authoritative service for:

- customer VPN access and logical devices;
- VLESS credentials and entry-node assignments;
- desired Xray user state;
- durable agent operations and reconciliation;
- accumulated traffic and quota periods;
- privacy-reduced source-IP activity;
- customer/admin VPN APIs and audit history.

The implementation is successful when:

1. a device can be provisioned, used, rotated, suspended, resumed, migrated, and
   revoked without a routine Xray restart;
2. retries and duplicate messages never create duplicate devices, operations, or
   usage;
3. an agent or network outage does not lose central desired state;
4. a backend outage does not stop existing VPN traffic and does not lose agent
   data while the node outbox has capacity;
5. Xray restart recovery does not depend on backend availability;
6. quota enforcement is predictable within the documented near-real-time window;
7. source-IP observations never become a claim of physical-device identity;
8. no browsing destinations are ingested by the backend;
9. every destructive or security-relevant action is attributable in the audit log;
10. backup restore and all acceptance scenarios in section 25 pass.

## 2. Scope

### 2.1 Included

- customer and logical-device VPN lifecycle;
- asynchronous access provisioning;
- one active credential per logical device in the normal case;
- entry-node selection and explicit migration;
- client-profile generation;
- per-user or per-subscription byte quotas;
- traffic and source-IP activity ingestion;
- node health, inventory comparison, and repair;
- operator APIs required to inspect and recover the control plane;
- PostgreSQL migrations, workers, metrics, logs, and audit events.

### 2.2 External integrations

The backend MAY receive identity, subscription, or payment entitlement changes
from another product service. Regardless of where those decisions originate, this
backend owns their durable effect on VPN access.

The external identity/payment implementation is not specified here. Its adapter
MUST provide authenticated, idempotent entitlement commands and stable
`customer_id` values.

### 2.3 Explicit non-goals

Version 1 does not promise:

- exact financial-grade byte billing across abrupt Xray process failure;
- identification of a physical device from an IP address;
- device attestation or hardware-bound credentials;
- immediate termination of an already-established Xray connection;
- automatic failover of a live customer session between entry nodes;
- a message broker, long-poll desired-list protocol, or bidirectional gRPC stream;
- destination, DNS, SNI, URL, or browsing-history collection;
- backend full-text search over operational logs;
- Elasticsearch/OpenSearch as a required component;
- high availability for an individual VPN entry node.

## 3. System boundary and authority

```text
Customer/admin API
        |
        v
Backend API + workers <----> PostgreSQL on control-1
        |
        | unary gRPC over WireGuard + mTLS
        v
Node agent + SQLite ----> Xray loopback API

Node operational logs ----> Alloy ----> Loki ----> Grafana
```

| State | Authority | Durable location |
|---|---|---|
| Customer, device, entitlement | Backend | PostgreSQL |
| Credential and assignment | Backend | PostgreSQL |
| Quota period and totals | Backend | PostgreSQL |
| Pending node mutation | Backend | PostgreSQL |
| Applied-node cache | Agent | Node SQLite |
| Undelivered usage/activity | Agent | Node SQLite |
| Runtime users and live counters | Xray | Process memory |
| Operational logs | Observability stack | Loki |

The backend:

- MUST NOT call Xray directly after target cutover;
- MUST NOT connect to node SQLite;
- MUST NOT derive business state from Loki queries;
- MUST treat PostgreSQL as the only central desired-state authority.

The agent:

- MUST NOT connect to central PostgreSQL;
- MUST NOT decide subscription or quota policy;
- MUST execute only its configured customer namespace and inbound.

## 4. Deployment ownership

This infrastructure repository owns:

- provisioning PostgreSQL on `control-1`;
- database storage, network policy, service account, backup, and restore tooling;
- node-agent deployment, management networking, mTLS material, and firewall;
- Alloy, Loki, Grafana, and infrastructure alerts.

The backend repository owns:

- all application schema migrations;
- migration compatibility with released application versions;
- application backup-consistency requirements;
- API and worker processes;
- generated protobuf client code;
- OpenAPI and application-level tests.

Ansible MUST NOT create or mutate application tables. The backend MUST run
migrations as a deployment prerequisite under a PostgreSQL advisory lock. Readiness
MUST remain false when the schema is older or newer than the supported range.

The current staging role embeds a PostgreSQL container beside the API. That is a
compatibility deployment, not the target database boundary defined here.

### 4.1 Runtime configuration

The backend validates configuration before serving traffic. Required configuration
groups include:

```text
PostgreSQL DSN/pool/timeouts
customer and operator authentication trust
credential-encryption active and decrypt-only key versions
node manifest/roster source
agent CA, client certificate, private key, and expected identities
poll, inventory, RPC, retry, and lease intervals
usage/activity response and database batch limits
quota policy and period boundary
retention policy
HTTP bind, trusted proxy, CORS, and rate limits
log level and metrics endpoint
```

Production secrets have no development defaults. They come from protected secret
files, Vault, or another approved secret provider and never appear in command-line
arguments, environment dumps, startup logs, or diagnostics.

Invalid or internally inconsistent configuration fails startup. Security-critical
key rotation supports one active write key and an explicit set of decrypt-only or
accepted prior key IDs during a bounded overlap.

## 5. Terminology and identifiers

| Name | Meaning |
|---|---|
| `customer_id` | Stable internal customer principal |
| `device_id` | Backend-owned logical device, not discovered hardware |
| `credential_id` | Backend record for one VLESS credential version |
| `credential_uuid` | Secret VLESS authentication UUID |
| `accounting_id` | Stable pseudonymous Xray `email` and accounting key |
| `node_id` | Stable entry-node identity |
| `assignment_id` | Historical association of a credential/accounting ID with a node |
| `operation_id` | Globally unique idempotency key for a node mutation |
| `spool_id, sequence` | Agent outbox cursor |
| `quota_period_id` | One immutable quota interval |

Rules:

- IDs generated by the backend MUST use a cryptographically strong UUID or an
  equivalently collision-resistant opaque value.
- `credential_uuid` and `accounting_id` MUST be different.
- `accounting_id` MUST remain stable across credential rotation.
- `accounting_id` MUST be unique among simultaneously or historically relevant
  backend-managed identities and SHOULD use `u-<opaque-id>`.
- Human email, phone, username, and raw customer ID MUST NOT appear in
  `accounting_id`.
- A customer MAY have multiple devices; each device SHOULD have its own
  `accounting_id` and credential.
- Device mapping remains backend-only. The node-agent protocol has no `device_id`.
- All timestamps are UTC. Database columns use `timestamptz`; wire timestamps use
  Unix milliseconds.

One credential can still be copied to multiple physical devices. Therefore the
backend reports:

```text
registered_device_count
active_credential_count
distinct_source_ip_count
```

It MUST NOT report source-IP cardinality as physical-device count.

## 6. Domain state model

### 6.1 Logical device

```text
PROVISIONING -> ACTIVE -> REVOKING -> REVOKED
       |          |
       |          +-> ERROR
       +------------> ERROR
```

`REVOKED` is terminal. Restoring access creates a new credential/device or an
explicitly audited replacement; it does not silently resurrect a deleted secret.

### 6.2 Access blockers

Effective access MUST be derived from a set of independent blockers rather than a
single mutable boolean:

```text
USER_REQUEST
ADMIN
SUBSCRIPTION
QUOTA
SECURITY
MIGRATION
```

Access is desired present only when:

```text
device not revoked
AND credential active
AND node assignment valid
AND no active access blockers
```

Removing one blocker MUST NOT remove any other blocker. For example:

- quota reset removes `QUOTA`, not `ADMIN`;
- subscription renewal removes `SUBSCRIPTION`, not `SECURITY`;
- an admin resume cannot override a still-exceeded quota.

### 6.3 Assignment apply state

Business desired state and node apply state are distinct:

```text
desired_state: PRESENT | ABSENT
apply_state:   PENDING | APPLIED | RETRYING | FAILED | UNKNOWN
```

The API MUST NOT report a device ready until desired `PRESENT` is confirmed by a
successful agent result or a fresh complete inventory.

### 6.4 Agent operation

```text
PENDING -> IN_FLIGHT -> SUCCEEDED
    |           |
    |           +-> RETRY_WAIT -> IN_FLIGHT
    +--------------> SUPERSEDED
                |
                +-> FAILED_PERMANENT
```

An in-flight operation payload is immutable. A newer desired state may supersede
an operation only before dispatch. If an earlier operation may already be running,
the backend waits for its terminal/expired lease and then sends the latest desired
operation.

## 7. Required PostgreSQL model

Names may follow backend language conventions, but the following logical entities
and constraints are mandatory.

### 7.1 Core entities

```text
customers(
  customer_id,
  status,
  created_at,
  updated_at
)

vpn_devices(
  device_id,
  customer_id,
  accounting_id,
  display_name,
  lifecycle_state,
  created_at,
  revoked_at,
  version
)

vpn_credentials(
  credential_id,
  device_id,
  credential_uuid_encrypted,
  key_version,
  status,
  created_at,
  activated_at,
  retired_at
)

vpn_access_blocks(
  block_id,
  customer_id,
  device_id nullable,
  reason,
  source_reference,
  active,
  created_at,
  cleared_at
)

vpn_nodes(
  node_id,
  lifecycle_state,
  agent_endpoint,
  agent_certificate_identity,
  public_endpoint_metadata,
  capacity,
  last_seen_at,
  last_inventory_at,
  health_state
)

vpn_node_assignments(
  assignment_id,
  credential_id,
  node_id,
  assignment_role,
  desired_state,
  apply_state,
  started_at,
  ended_at,
  version
)
```

Constraints:

- one device has at most one normal active credential;
- one active credential has at most one profile-serving `CURRENT` assignment;
- a durable migration saga may temporarily add one
  `MIGRATION_SOURCE`/`MIGRATION_DESTINATION` pair;
- `vpn_devices.accounting_id` is unique and survives all credential versions;
- historical assignment rows are retained while delayed usage/activity may arrive;
- credential secret columns are encrypted at application level or by an approved
  equivalent envelope-encryption mechanism;
- optimistic `version` increments on state transitions.

Indexes MUST cover customer/device ownership lookups, active accounting/assignment
lookups, due operation claims, node leases, cursor uniqueness, period totals, and
retention scans. Large immutable batch/audit tables SHOULD be time-partitionable
without changing application semantics.

### 7.2 Operation/outbox entities

```text
agent_operations(
  operation_id,
  node_id,
  accounting_id nullable,
  assignment_id nullable,
  operation_type,
  canonical_payload_encrypted,
  payload_digest,
  desired_version,
  status,
  attempt_count,
  next_attempt_at,
  lease_owner,
  lease_expires_at,
  last_error_code,
  created_at,
  completed_at
)

node_ingestion_cursors(
  node_id,
  stream_kind,
  spool_id,
  highest_contiguous_sequence,
  updated_at
)

api_idempotency_keys(
  principal_id,
  idempotency_key,
  route,
  request_digest,
  response_status,
  response_body,
  expires_at
)
```

Constraints:

- `operation_id` is globally unique;
- `(node_id, stream_kind)` is unique for the current acknowledged cursor;
- `(principal_id, route, idempotency_key)` is unique;
- an idempotency key reused with a different digest returns HTTP `409`;
- only one non-terminal mutating operation per node may be leased at a time;
- secrets in operation payloads use the same protection as credential storage.

Completed backend operation records are retained for at least the agent's
idempotency horizon, initially 30 days. API idempotency records are retained for at
least 24 hours and longer than the documented client retry window for the action.

### 7.3 Usage entities

```text
usage_batches(
  node_id,
  spool_id,
  sequence,
  collected_at,
  received_at,
  processing_status
)

usage_batch_items(
  node_id,
  spool_id,
  sequence,
  accounting_id,
  uplink_bytes,
  downlink_bytes,
  mapping_status
)

quota_periods(
  quota_period_id,
  quota_subject,
  starts_at,
  ends_at,
  byte_limit,
  status
)

usage_period_totals(
  quota_period_id,
  quota_subject,
  uplink_bytes,
  downlink_bytes,
  total_bytes,
  updated_at
)
```

Constraints:

- `(node_id, spool_id, sequence)` uniquely identifies one batch;
- `(node_id, spool_id, sequence, accounting_id)` uniquely identifies an item;
- totals change only in the transaction that first inserts a batch;
- byte addition checks overflow and rejects negative/unrepresentable input;
- period rows are not reused for the next period.

### 7.4 Activity entities

```text
activity_batches(
  node_id,
  spool_id,
  sequence,
  bucket_start,
  bucket_end,
  received_at,
  processing_status
)

source_ip_observations(
  node_id,
  spool_id,
  sequence,
  accounting_id,
  ip_token_key_id,
  ip_token,
  address_family,
  prefix_length,
  first_seen_at,
  last_seen_at,
  connection_count,
  mapping_status
)

source_ip_daily_totals(
  customer_id,
  device_id,
  day,
  distinct_ip_count,
  connection_count,
  updated_at
)
```

Raw source IP and destination activity MUST NOT be stored in these tables.

### 7.5 Audit

```text
audit_events(
  audit_id,
  occurred_at,
  actor_type,
  actor_id,
  action,
  target_type,
  target_id,
  request_id,
  outcome,
  sanitized_metadata
)
```

Audit events are append-only to normal application roles. They cover device
creation/revocation, credential viewing/rotation, blockers, quota changes,
node drain/migration, manual reconcile, and privileged reads.

## 8. Transaction and concurrency requirements

The backend MUST use PostgreSQL transactions for every state transition.

The following pairs occur atomically:

- credential/assignment creation and `EnsureUserPresent` outbox insertion;
- access becoming absent and `EnsureUserAbsent` outbox insertion;
- usage batch insertion, total increment, quota evaluation, and quota-block
  operation insertion;
- activity batch insertion and observation aggregation;
- operation result and assignment apply-state update.

Workers MAY scale horizontally. They MUST coordinate through row locks, leases, or
advisory locks. A recommended operation claim is `FOR UPDATE SKIP LOCKED` plus a
lease. A crashed worker's lease must expire and allow safe retry with the same
`operation_id`.

Only one backend replica SHOULD actively poll a particular node at a time. A
short-lived per-node poll lease prevents duplicate load; safety must not depend on
that optimization because batch insertion remains idempotent.

For one device/accounting ID:

- state transitions use a row lock or compare-and-swap `version`;
- stale requests return `409` or a current resource representation;
- queued operations may be coalesced before dispatch;
- completion of an old operation never overwrites a newer desired version;
- if actual state is now stale, a new corrective operation is created.

## 9. Node roster and scheduling

Infrastructure exports the active entry roster and public client metadata. The
target manifest MUST eventually include:

```text
schema_version
node_id
agent_endpoint
agent_certificate_identity
public address and port
REALITY server name
REALITY public password/key
short ID
fingerprint
flow
capacity metadata
```

The backend validates the whole manifest before atomically applying it. Unknown
fields are rejected unless the manifest version explicitly permits them. Missing
or malformed input does not partially update the roster.

Node lifecycle:

```text
ACTIVE       accepts new assignments
DRAINING     keeps existing assignments; accepts no new ones
DISABLED     no scheduling; operator action required
DECOMMISSIONED historical only
```

Scheduling MUST consider:

- node lifecycle;
- recent agent health and non-bootstrap state;
- configured capacity;
- requested region/product policy;
- current assignment count or measured capacity signal.

A transient health failure MUST NOT automatically move all customers. Migration is
an explicit durable saga. Exit topology is not a scheduling input: customers are
assigned to entries, while entry-to-exit routing remains infrastructure-owned.

## 10. Node-agent gRPC integration

The backend is the gRPC client. It:

- connects over WireGuard;
- validates the agent mTLS server identity against `vpn_nodes`;
- presents the approved backend client certificate;
- rejects a response whose `node_id` differs from the authenticated target;
- reuses one HTTP/2 channel per node;
- applies bounded deadlines, message limits, jitter, and exponential backoff.

Required RPC use:

| RPC | Backend behavior |
|---|---|
| `Health` | Optional cheap probe and diagnostics |
| `GetNodeState` | Poll health, independent activity state, usage/activity, and optional complete user inventory |
| `EnsureUserPresent` | Apply one desired-present credential |
| `EnsureUserAbsent` | Apply one desired-absent accounting ID |
| `ReconcileUsers` | Bootstrap or explicitly repair a complete node set |

Result handling:

| Result | Backend action |
|---|---|
| `APPLIED` | Mark operation success and assignment applied |
| `ALREADY_APPLIED` | Treat as success |
| `RETRYABLE_ERROR` | Keep operation and retry same ID |
| `PERMANENT_ERROR` | Mark permanent failure, expose/alert, require new corrected operation |
| Transport/deadline | Outcome unknown; retry same ID |
| Authentication/identity mismatch | Do not retry aggressively; quarantine node and alert |

No caller-supplied `node_id` authorizes a node. `operation_id` and canonical
payload are immutable across retries.

## 11. Device provisioning

### 11.1 Create

The create-device command:

1. authenticates the customer and checks device/product limits;
2. reserves a `device_id`, stable `accounting_id`, and VLESS UUID;
3. selects an eligible entry node;
4. creates credential and assignment with desired `PRESENT`;
5. inserts `EnsureUserPresent`;
6. commits;
7. returns `202 Accepted` with device/operation state.

The client profile becomes downloadable only after apply success. Retrying the
same HTTP idempotency key returns the original device and operation.

If provisioning reaches permanent error, the device is `ERROR`; the credential is
not silently regenerated because a client may already have received references to
the operation. A corrected explicit retry creates a new operation.

### 11.2 Client profile

The profile combines:

- active `credential_uuid`;
- public endpoint metadata for the assigned entry;
- VLESS, TCP, REALITY, and `xtls-rprx-vision` settings.

Profile responses:

- require ownership or privileged authorization;
- use `Cache-Control: no-store`;
- never appear in logs, traces, analytics, or audit metadata;
- return `409 ACCESS_NOT_READY` while apply state is not confirmed;
- return `410` for a revoked device.

### 11.3 Rotate credential

Rotation:

1. locks the device and verifies it is not revoked;
2. creates a new encrypted UUID while retaining `accounting_id`;
3. updates desired credential state and inserts `EnsureUserPresent`;
4. waits for agent verification;
5. retires the old credential version;
6. exposes the new profile.

Normal v1 rotation invalidates the old credential after apply and interrupts that
device until it installs the new profile. For planned overlap, create a separate
replacement device with a distinct accounting ID, activate it, then revoke the old
device.

### 11.4 Suspend and resume

Suspension creates or activates a named access blocker. If effective access changes
to absent, the same transaction inserts `EnsureUserAbsent`.

Resume clears only the requested blocker. `EnsureUserPresent` is created only when
no blockers remain.

### 11.5 Revoke/delete

Revocation:

1. marks the device terminally revoked;
2. makes desired state absent;
3. inserts `EnsureUserAbsent`;
4. retains accounting and assignment history for late batches and audit;
5. hides or cryptographically erases the credential secret after the defined
   recovery window.

DELETE is idempotent. Repeated revocation does not create unbounded duplicate
operations.

### 11.6 Customer closure and entitlement events

Customer suspension/closure calculates every affected device under one durable
command and activates the appropriate blocker. It creates one node operation per
effective assignment and tracks partial completion; one unavailable node does not
roll back already committed central desired state.

Permanent customer deletion first revokes all VPN devices, then applies the
configured PII deletion/anonymization policy. Pseudonymous accounting/assignment
history required for delayed batches, aggregate correctness, fraud/security
records, or audit is retained only for its approved period.

External entitlement events carry a stable source reference and monotonically
comparable source version or event time. Duplicate events are idempotent. An older
expiry event arriving after a newer renewal cannot re-block access.

## 12. Entry-node migration

Migration is a backend saga, never an implicit reaction to one missed health poll.

Normal make-before-break flow:

1. validate destination node;
2. create destination assignment with the same accounting ID/credential;
3. send `EnsureUserPresent` to destination;
4. confirm destination actual state;
5. update current assignment and profile endpoint;
6. send `EnsureUserAbsent` to source;
7. confirm source absence and close old assignment.

During the bounded overlap, usage from both nodes is valid and is aggregated to the
same quota subject. Failure before step 4 leaves the source authoritative. Failure
after step 4 is resumed from durable saga state; it does not start a second
migration.

Emergency break-before-make MAY be used for a compromised or failed node and must
be explicitly audited. Customers may need a refreshed profile when the public
entry endpoint changes.

## 13. Traffic usage ingestion

Normal poll interval is 15 seconds plus jitter.

For each `GetNodeState` response the backend:

1. authenticates and validates node identity;
2. processes usage batches in ascending cursor order;
3. inserts one batch and all items atomically;
4. maps items through retained `(node_id, accounting_id)` history;
5. increments totals only for a newly inserted batch;
6. evaluates quota blockers;
7. commits;
8. remembers the highest contiguous committed cursor;
9. sends that cursor as ACK on a later poll.

ACK rules:

- never ACK before commit;
- never ACK across a gap;
- never reuse an ACK for a different spool ID;
- duplicate batches are successful no-ops;
- a lost ACK causes harmless redelivery.

Unknown accounting IDs are stored with `mapping_status=QUARANTINED`, committed,
ACKed, and alerted. They do not block the node outbox forever. A repair job may map
them later and apply totals exactly once. Invalid or impossible values follow the
same quarantine pattern.

Late usage:

- remains attributable after revocation because history is retained;
- is applied to the period containing agent `collected_at`;
- may update a closed period and creates an audit/metric for late correction;
- is never silently moved into the current period.

Clock skew outside the configured tolerance is alerted and quarantined for period
assignment; the batch itself may still be durably accepted.

## 14. Quota model

The quota subject is configurable as customer, subscription, or device. The normal
product model SHOULD aggregate all device accounting IDs under one
customer/subscription quota.

For each immutable period:

```text
total_bytes = uplink_bytes + downlink_bytes
remaining_bytes = max(byte_limit - total_bytes, 0)
```

When a newly committed batch crosses the limit, the same transaction:

1. activates `QUOTA`;
2. creates absent operations for every affected active assignment;
3. records the crossing batch and total;
4. emits an audit event.

Concurrent batches for the same quota subject lock or atomically update one period
total. Exactly one transition activates a previously absent `QUOTA` blocker; repair
or absent operations are deduplicated even when usage arrives from multiple nodes.

Quota enforcement is near-real-time, not instantaneous. Normal overshoot includes:

- up to one agent collection interval;
- up to one backend polling interval;
- RPC/application latency;
- traffic already in flight;
- any control-plane outage.

Opening a new period creates a new ledger and clears only the corresponding quota
blocker. It does not reset Xray, agent SQLite, lifetime history, admin blocks, or
subscription blocks.

Changing a limit is versioned and audited. Lowering it below current total triggers
the same crossing transaction. Raising it clears `QUOTA` only if policy allows
automatic resume.

## 15. Source-IP activity

### 15.1 Meaning

Activity represents successful Xray access observations, not VPN sessions and not
physical devices. Xray may produce multiple access events for one client action.

The backend exposes these metrics with unambiguous names:

```text
distinct_source_ips
connection_observations
first_seen
last_seen
```

### 15.2 Node preprocessing

Only entry-node agents attribute customer activity. Before an event can leave the
node, the agent:

1. parses the pinned Xray access-log format;
2. accepts only exact successful records with a backend-owned accounting ID;
3. discards destination, destination port, DNS, SNI, URL, and payload data;
4. canonicalizes IPv4 as `/32` and IPv6 as the configured prefix, initially `/64`;
5. computes a versioned HMAC token;
6. aggregates a closed time bucket;
7. persists the activity batch in its independent SQLite outbox.

Parser failures increment a metric and retain only a bounded local diagnostic that
does not contain destination/customer secrets. A pinned-Xray fixture test is
required before every Xray upgrade.

### 15.3 Backend ingestion

Activity uses its own `(node_id, spool_id, sequence)` uniqueness and ACK cursor.
Usage ACK progress and activity ACK progress are independent.

The backend:

- validates closed 60-second UTC-aligned bucket bounds, 43-character unpadded
  base64url HMAC-SHA256 tokens, allowed prefixes, and
  `[A-Za-z0-9._-]{1,32}` key IDs;
- maps retained accounting history;
- stores/aggregates new batches transactionally;
- ACKs the highest contiguous committed activity cursor;
- quarantines unknown/malformed observations without blocking the spool;
- never attempts to reverse or enrich the HMAC token into a destination profile.

`ActivityState.enabled=false` is acceptable only for an explicitly configured
rollout stage. `enabled=true, healthy=false`, a stale closed-bucket timestamp, or a
growing backlog raises activity-specific alerts but does not make Xray user control
or usage ingestion unavailable.

Key rotation permits an overlap in which old and new `ip_token_key_id` values are
accepted. Cross-key distinct counts are not assumed comparable unless a controlled
re-key process exists.

The initial design uses one fleet-scoped activity HMAC key per rotation epoch so
the same normalized source can be compared across entry migration. Infrastructure
provisions that key to entry agents; the backend needs only the accepted key IDs
unless a separately approved re-key workflow requires more.

### 15.4 Retention

Raw activity observations use the shortest operationally useful retention,
initially aligned with the seven-day Loki `activity` tenant. Longer-lived daily
counts MAY be retained without IP tokens. Retention values are configuration and
must be documented with the applicable privacy policy.

## 16. Reconciliation

On a complete, fresh inventory response the backend compares:

```text
central desired users <-> agent-reported actual Xray users
```

Repairs:

```text
desired present, actual missing       EnsureUserPresent
desired absent, actual present        EnsureUserAbsent
same accounting ID, wrong UUID/flow   EnsureUserPresent
```

The backend MUST NOT derive removals when:

- `users_complete=false`;
- observation time is absent or stale;
- `needs_bootstrap=true`;
- agent identity is mismatched;
- the item is outside the backend-owned namespace;
- central desired-state calculation failed;
- another relevant operation is already pending.

A repair operation is durable and deduplicated. Reconciliation does not bypass the
normal operation worker.

For bootstrap, the backend sends one complete `ReconcileUsers(complete=true)` set.
An empty complete set is valid only when central desired state was read
successfully and the node truly has no desired customers.

## 17. Required HTTP API behavior

The backend repository MUST publish a versioned OpenAPI document. Exact resource
paths may be extended, but these capabilities and existing health paths are
required.

### 17.1 Health

```text
GET /health
GET /health/advanced
```

`/health` reports process liveness without depending on agents. `/health/advanced`
reports at least database connectivity, migration compatibility, and worker
readiness. Node outages do not make the whole backend process unready.

### 17.2 Customer VPN API

```text
GET    /api/v1/vpn/devices
POST   /api/v1/vpn/devices
GET    /api/v1/vpn/devices/{device_id}
DELETE /api/v1/vpn/devices/{device_id}
POST   /api/v1/vpn/devices/{device_id}/rotate
GET    /api/v1/vpn/devices/{device_id}/profile
GET    /api/v1/vpn/usage
GET    /api/v1/vpn/activity-summary
```

Create, rotate, and destructive commands require `Idempotency-Key`. Asynchronous
responses use `202` and expose current desired/apply/operation state. List endpoints
are bounded and cursor-paginated with a stable sort.

### 17.3 Operator/internal API

Required capabilities:

- list node health, staleness, outbox lag, and bootstrap state;
- inspect/retry a failed operation without changing its payload;
- create a corrected replacement after permanent error;
- trigger safe complete reconciliation;
- mark a node active/draining/disabled;
- initiate and inspect migration;
- apply/clear access blockers;
- inspect quarantined usage/activity;
- view audit events.

The compatibility endpoint
`GET /internal/v1/vpn/desired-users` remains only until the old node snapshot timer
is retired. It is not part of target agent operation.

### 17.4 HTTP conventions

- Every request has or receives a `request_id`.
- Authentication failure is `401`; authorization/ownership failure is `403` or a
  non-enumerating `404` according to policy.
- Validation is `400`/`422`; state conflict is `409`; rate limit is `429`.
- Errors use a stable machine code and sanitized detail.
- Secrets never appear in error bodies.
- All collection endpoints have explicit limits.
- API timeouts do not roll back an already committed asynchronous command.

Example error shape:

```json
{
  "type": "https://errors.spiritvpn.example/access-not-ready",
  "title": "VPN access is not ready",
  "status": 409,
  "code": "ACCESS_NOT_READY",
  "request_id": "opaque",
  "retryable": true
}
```

## 18. Authentication and authorization

Customer APIs require an authenticated principal. Every device/profile/usage query
enforces ownership at the database query boundary; object IDs alone are never
authorization.

Operator APIs require a distinct privileged role and stronger authentication.
Machine integrations use separate service identities with least privilege.

Authorization roles include at least:

```text
customer
support-readonly
vpn-operator
security-operator
service-entitlement-writer
```

Support-readonly cannot retrieve credential UUIDs or raw activity tokens. Manual
reconcile, node lifecycle, security blocks, and credential access are audited.

Rate limits apply per principal and source to authentication, device creation,
rotation, profile retrieval, and operator mutations. Rate limiting must not make
health endpoints or internal operation completion inconsistent.

## 19. Secrets and privacy

The backend MUST NOT log:

- VLESS UUIDs or client profiles;
- database, mTLS, API, or HMAC keys;
- complete operation payloads;
- raw source IPs in activity processing;
- browsing destinations;
- authorization headers or cookies.

Required controls:

- encryption in transit;
- application/envelope encryption for credential UUIDs;
- key IDs and rotation support;
- `no-store` profile responses;
- structured log redaction tests;
- sanitized audit metadata;
- configurable retention and deletion jobs;
- database roles separated for migration, runtime, and backup where practical.

`accounting_id` is pseudonymous, not anonymous. Access to usage/activity joins is
restricted accordingly.

Node agents are authenticated measurement/control peers, but every returned field
is still validated as untrusted input. An agent cannot directly change customer
entitlements, quotas, or central desired state; actual inventory can only trigger
bounded repair operations. A fully compromised entry node can falsify or omit the
traffic/activity it observes, which mTLS cannot prevent. Detection, quarantine, and
node replacement are the containment strategy for that threat.

## 20. Background jobs

At minimum:

| Job | Default cadence/trigger |
|---|---|
| Agent state poll | 15 s plus per-node jitter |
| Full user inventory | every poll initially; 60 s when scaled |
| Agent operation worker | event-driven/continuous |
| Retry scheduler | continuous, backoff capped at 60 s |
| Reconciliation planner | after fresh inventory |
| Quota period rollover | scheduled at exact period boundary |
| Quarantine reprocessor | periodic and operator-triggered |
| Node staleness evaluator | at least every poll interval |
| Retention cleanup | daily |
| Manifest/roster sync | deployment-triggered and periodic validation |

Jobs are safe to run concurrently across backend replicas. Scheduled work uses a
distributed lock or idempotent unique key. A missed schedule is run after recovery
without creating duplicate periods or operations.

Graceful shutdown:

1. fails readiness;
2. stops claiming new work;
3. finishes or releases leased transactions;
4. closes gRPC channels and database connections;
5. never ACKs uncommitted agent data.

## 21. Retry, timeout, and backpressure

- Unary control RPC default deadline: 5 seconds.
- Full reconcile gets a separately bounded longer deadline.
- Retries use exponential backoff with jitter, capped at 60 seconds.
- Permanent validation/authentication failures are not hot-looped.
- A circuit breaker/backoff is scoped per node, not fleet-wide.
- State polling may continue while one node mutation is pending.
- Backend API writes commit to the durable outbox before returning success/`202`.
- Database connection-pool exhaustion causes bounded failure, not unbounded request
  queues.
- Poll response limits are configured; the backend drains multiple pages of agent
  batches across successive polls.
- A node with growing outbox lag is alerted before disk exhaustion.

## 22. Observability

Application logs are structured and include:

```text
timestamp
level
service
instance
request_id
operation_id when non-sensitive
node_id when relevant
stable error_code
```

They exclude secret payloads and raw activity.

Required metrics include:

```text
backend_http_requests_total
backend_http_request_duration_seconds
backend_db_pool_in_use
backend_agent_poll_success_total
backend_agent_poll_duration_seconds
backend_agent_last_success_timestamp_seconds
backend_agent_operations{status,type,node}
backend_agent_operation_age_seconds
backend_usage_batches_total{result}
backend_usage_ingest_lag_seconds
backend_activity_batches_total{result}
backend_activity_ingest_lag_seconds
backend_quota_blocks_total
backend_reconcile_repairs_total{reason}
backend_quarantined_items{kind,reason}
backend_nodes{state}
```

Do not place customer, device, accounting ID, credential, or IP token in metric
labels.

Alerts cover:

- backend/database unavailable;
- migration incompatibility;
- agent poll stale per node;
- old pending/retrying/permanent operations;
- bootstrap stuck;
- usage/activity lag or quarantine growth;
- quota worker failure;
- certificate expiry/identity mismatch;
- backup failure and restore-test failure.

Operational logs remain in Loki with low-cardinality `node`, `service`, `role`,
`country`, and level metadata. Elasticsearch is not required for v1.

## 23. Backup, restore, and disaster recovery

Infrastructure provides PostgreSQL backup/restore. The backend provides a
consistency and verification procedure.

Backups MUST include:

- application data and migration metadata;
- encrypted credential material and required key-version references;
- operation, usage, quota, activity, and audit records.

Key material needed to decrypt a restored database is backed up separately under
appropriate custody. A database backup without its key versions is considered
unrestorable.

Restore procedure:

1. restore into an isolated PostgreSQL instance;
2. validate checksums/database consistency;
3. start the matching backend version with outbound agent mutations disabled;
4. verify migrations, counts, credential decryption, totals, and audit;
5. compare central desired state to node inventories;
6. explicitly enable workers;
7. reconcile nodes without pruning from stale/incomplete data.

After point-in-time restore, old agent batches and RPC operations may be replayed.
Unique cursors and operation IDs MUST make replay safe.

RPO/RTO values are deployment policy and must be documented before production.
A recurring restore test is mandatory; backup creation alone is insufficient.

## 24. Compatibility and rollout

Database releases follow expand/migrate/contract:

1. add backward-compatible schema;
2. deploy code that can read old/new state;
3. backfill idempotently;
4. switch writers;
5. verify;
6. remove old schema only after rollback window.

Protobuf v1 changes are additive. Removed field numbers are reserved. Breaking
changes require `spiritvpn.nodeagent.v2`.

OpenAPI changes preserve existing response fields/semantics within v1 or introduce
a new endpoint/version.

Target rollout:

1. provision central PostgreSQL and verified backups;
2. deploy backend migrations/API/workers with agent mutations disabled;
3. deploy agents in observe-only mode;
4. compare inventory, usage, and activity fixtures;
5. bootstrap nodes and enable mutations;
6. enable agent-owned counter reset and acknowledged ingestion;
7. make Xray API loopback-only;
8. retire desired-user snapshot/direct-Xray/exporter compatibility paths.

Rollback never enables two independent StatsService reset owners.

## 25. Acceptance scenario matrix

Every row requires an automated integration, E2E, or deterministic fault-injection
test unless marked operational.

### 25.1 Device and API lifecycle

| Scenario | Required outcome |
|---|---|
| Create first device | One device, credential, assignment, and present operation; eventually active |
| Retry create with same idempotency key | Same response/resource; no duplicate |
| Reuse idempotency key with different body | `409`; no state change |
| Concurrent create at device limit | Limit cannot be exceeded |
| Read another customer's device/profile | Denied without revealing existence |
| Profile before apply | `409 ACCESS_NOT_READY` |
| Agent returns already applied | Device becomes active |
| Permanent provisioning error | Device visible as error; alert/operator remediation |
| Rotate active credential | Accounting ID/history retained; old UUID rejected after apply |
| Concurrent rotate and revoke | Serialized; final desired state is revoked |
| Repeat revoke/delete | Idempotent; no operation explosion |
| Suspend for admin | Absent operation created |
| Quota reset while admin blocked | Admin block remains; no present operation |
| Subscription renewal while security blocked | Security block remains |
| Resume with no blockers | Present operation created and eventually verified |
| Request profile after revoke | `410`/denied; secret not returned |
| Customer suspension with many devices/nodes | All desired states become absent; partial agent progress is tracked |
| Duplicate entitlement event | No duplicate blocker or operation |
| Older expiry arrives after newer renewal | Stale source version ignored |
| Permanent customer closure | All devices revoked before retention/anonymization workflow |

### 25.2 Operations and reconciliation

| Scenario | Required outcome |
|---|---|
| RPC response lost after apply | Same operation ID retry; no duplicate mutation |
| Worker crashes with lease | Lease expires; same operation resumes |
| Same operation ID/different payload | Agent permanent error; backend alerts |
| Two backend replicas | At most one mutating operation in flight per node |
| Two replicas poll one node | Poll lease reduces load; duplicate batches remain safe |
| New desired state while old pending | Undispatched old op superseded or latest correction follows safely |
| Agent unavailable | Operation retained with backoff; other nodes unaffected |
| Agent permanent validation error | No hot retry; explicit corrected operation required |
| Complete inventory missing desired user | Deduplicated present repair |
| Complete inventory has undesired managed user | Absent repair |
| Wrong UUID behind accounting ID | Present/replace repair |
| Partial/stale inventory | No prune |
| Bootstrap flag | Full complete reconcile; no inventory-derived prune beforehand |
| Empty complete desired set | Prune only backend-owned namespace after successful central read |
| Unknown infrastructure identity | Never removed by backend |
| Xray restart | Agent local restore; backend later verifies |
| Agent SQLite loss | Bootstrap/reconcile; central desired state preserved |
| Complete reconcile exceeds unary limit | No partial apply; explicit scale/protocol error and alert |

### 25.3 Usage and quota

| Scenario | Required outcome |
|---|---|
| New usage batch | Insert once and increment correct period |
| Duplicate batch before ACK | No second increment |
| ACK lost | Redelivery is harmless |
| Sequence gap | ACK stops before gap |
| New spool ID after agent DB recreation | Independent cursor; no collision |
| Zero-byte item/batch | Accepted without changing totals |
| Unknown accounting ID | Quarantined, ACKed, alerted; spool not blocked |
| Malformed/anomalous value | Quarantined and alerted |
| Usage after revocation | Mapped through history and counted |
| Late prior-period batch | Correct old period updated and marked late |
| Period rollover retried | Exactly one new period |
| Limit crossed by batch | Quota block and absent operations in same transaction |
| Concurrent crossing batches on two nodes | One quota transition; totals include both exactly once |
| Limit lowered below total | Same quota-crossing behavior |
| Quota raised/reset with other blocker | Other blocker preserved |
| Backend/DB unavailable | Agent retains unacknowledged batches |
| Agent disk nearly full | Alert visible; backend prioritizes draining |
| Xray crashes between samples | Documented bounded loss; no fabricated bytes |
| Agent timestamp outside tolerance | Batch durable/quarantined for period mapping and alerted |

### 25.4 Activity and privacy

| Scenario | Required outcome |
|---|---|
| Valid entry access log | Sanitized bucket with accounting ID and HMAC IP token |
| Many flows from same IP/bucket | One observation aggregate with count |
| Same credential from two IPs | `distinct_source_ips=2`, not `device_count=2` |
| Multiple devices behind one NAT | Device count from credentials; IP count may be one |
| IPv6 privacy addresses in one `/64` | Normalize according to configured prefix |
| Duplicate activity batch | No double count |
| Activity ACK lost | Harmless redelivery |
| Activity backlog while usage healthy | Independent cursors; usage continues |
| Unknown accounting ID | Quarantine and ACK |
| Unknown HMAC key ID | Quarantine and alert |
| Key rotation overlap | Both approved key IDs accepted |
| Malformed Xray log after upgrade | Parser-error metric/alert; no unsafe raw upload |
| Destination present in input line | Discarded before SQLite/gRPC/backend logs |
| Loki unavailable | Activity/usage/control path continues |
| Activity outbox reaches its reservation | Usage/control storage remains available; activity alert/drop policy applies |

### 25.5 Node and migration

| Scenario | Required outcome |
|---|---|
| Add healthy entry manifest | Validated atomic roster update |
| Malformed manifest | Entire update rejected |
| Node marked draining | No new assignments; existing remain |
| One missed poll | No mass migration |
| Node stale beyond alert threshold | Alert; explicit operator policy decides migration |
| Successful migration | Destination present before source absent |
| Destination provisioning fails | Source remains current |
| Crash mid-migration | Durable saga resumes without duplicate migration |
| Usage during overlap | Both nodes count toward same quota |
| Emergency break-before-make | Explicit audit and predictable customer interruption |
| Node certificate mismatch | Connection rejected, node quarantined, critical alert |
| Certificate expiry approaching | Alert before loss of control |
| Planned mTLS certificate/CA rotation | Trust overlap preserves authenticated control, then old identity is removed |

### 25.6 Backend, database, and recovery

| Scenario | Required outcome |
|---|---|
| PostgreSQL unavailable at startup | Liveness may pass; readiness fails; no worker mutation |
| PostgreSQL fails mid-request before commit | No acknowledged command/state change |
| PostgreSQL fails after commit/client timeout | Idempotency retry returns committed result |
| Backend process crash | Durable operations/batches resume |
| Multiple instances start migrations | One migration lock owner; compatible startup |
| Unsupported schema version | Readiness false; no workers |
| Graceful shutdown | No new leases; no ACK before commit |
| Restore older point in time | Replayed operation/batches deduplicate |
| Backup without encryption keys | Restore test fails explicitly |
| Loki/Prometheus unavailable | VPN control and accounting continue |
| control-1 total loss | Recovery follows documented RPO/RTO and restore procedure |
| Missing/invalid production secret or config | Startup fails before serving requests or claiming work |
| Credential-encryption key rotation | New writes use active key; approved old rows remain decryptable during overlap |

### 25.7 Security

| Scenario | Required outcome |
|---|---|
| Invalid/expired customer auth | `401`; no resource lookup side channel |
| Valid customer requests another owner ID | Denied |
| Support role requests credential | Denied and audited |
| Secret appears in attempted log field | Redaction test fails build |
| Oversized/list-abuse request | Bounded rejection/rate limit |
| Forged node ID in payload | Ignored; certificate/target identity controls |
| Replay of valid mutating HTTP request | Idempotency prevents duplicate effect |
| Reused operation ID with altered request | Rejected |
| Raw IP/destination submitted as activity | Rejected/quarantined; never normalized into business tables |
| Authenticated agent sends oversized/anomalous data | Message/batch bounds and quarantine protect database/service |
| Agent reports false actual inventory | Central desired records remain unchanged; only bounded repair operations result |

## 26. Definition of done

The backend is not production-ready until:

- this specification and generated OpenAPI/protobuf clients agree;
- migrations work from an empty database and every supported prior release;
- all section 25 automated scenarios pass, with operational restore/cert tests
  evidenced separately;
- load tests validate configured fleet/user/event limits and backpressure;
- no reset-based usage exporter competes with the agent;
- Xray management is loopback-only on migrated nodes;
- dashboard/alerts cover every required stale/backlog/error state;
- database and encryption-key restore has been demonstrated;
- security review confirms ownership checks, redaction, and least privilege;
- the compatibility snapshot/direct-Xray path has an explicit removal date.

## 27. Locked implementation decisions

- PostgreSQL is the central authority.
- Backend initiates unary gRPC to agents over WireGuard and mTLS.
- Agents never access central PostgreSQL.
- Backend never accesses Xray directly after cutover.
- Commands are declarative and idempotent by immutable `operation_id`.
- There is at most one mutating operation in flight per node.
- Usage and activity are independent acknowledged outboxes.
- Reconciliation is automatic but pruning requires fresh complete inventory.
- Logical device mapping belongs to backend; IP count is not device count.
- Xray UUID and accounting ID are distinct.
- Operational logs use Alloy/Loki and are not business state.
- Raw IPs and browsing destinations are not centrally collected.
- Elasticsearch/OpenSearch and a message broker are deferred until measured scale
  or query requirements justify them.
