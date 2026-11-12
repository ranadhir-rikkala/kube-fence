# kube-fence design

This document explains how kube-fence decides, why it is built the way it is, and where the current design is knowingly incomplete.

## 1. Goals and non-goals

Goals:

- Stop the specific failure modes of automated agents that write to Kubernetes: duplicated irreversible effects after retries, fights with controllers that own a field, and fast wide-reaching mistakes.
- Work for any client the agent uses (kubectl, client-go, an MCP server, generated scripts).
- Never slow down or break writes from humans and ordinary controllers.
- Keep the decision logic pure and testable.

Non-goals for M1:

- Deciding whether an agent's action is *correct*. kube-fence checks effects, ownership, and volume, not intent quality.
- Replacing RBAC. An agent should still have the narrowest role that works; kube-fence adds stateful checks that RBAC cannot express.
- Exactly-once semantics. See section 5.

## 2. Where to enforce

| Option | Sees all clients | Has state across requests | Knows field ownership | Verdict |
|---|---|---|---|---|
| RBAC | yes | no | no | necessary, not sufficient |
| Proxy in front of an MCP server | only MCP calls | yes | only if it re-reads objects | bypassed by `kubectl` from a shell tool |
| Stateless policy engines (Gatekeeper, Kyverno) | yes | no effect history | can read `managedFields` in rules | no ledger, no time windows |
| **Validating admission webhook** | yes | yes (own store) | yes, via `oldObject.managedFields` | chosen |

A webhook is on the request path for every write, sees the authenticated user, the old and new object, and can return a reason the agent reads back as an error message. The price is that it is on the critical path, which drives the failure-mode choices in section 8.

## 3. Request flow

1. The API server evaluates `matchConditions` (CEL). Only `system:serviceaccount:agents:*` requests reach kube-fence; humans and controllers never hit it.
2. `/mutate` stamps `kube-fence.io/last-agent-write` on creates and non-empty updates. It never denies, uses `failurePolicy: Ignore`, and skips subresources.
3. `/validate` converts the `AdmissionReview` into a `policy.Request` (plain Go maps, no Kubernetes types) and calls `Engine.Evaluate`.
4. `Evaluate`:
   1. Not an agent, or `CONNECT`: allow.
   2. Compute the change set: the full normalized object for `CREATE`, a field-level diff for `UPDATE`, nothing for `DELETE`. An empty `UPDATE` diff is a no-op: allow and do not record.
   3. Derive the intent key.
   4. Under a mutex: check the ledger for the key, check ownership (updates), check limits.
   5. If there are violations and the mode is `enforce`, deny with every violation listed. In `audit`, allow and return the message as an API warning.
   6. Unless the request is a dry run, record the effect. If recording fails in `enforce` mode, deny.

All violations are collected, not just the first, so an agent sees everything it would have to fix.

## 4. Idempotency keys

### Explicit keys

If the object carries `kube-fence.io/intent-id`, the key is `intent:<value>`. This is the Stripe-style contract: one id per distinct action, reused on retries. It is the only way to catch an agent that *recomputes* a relative action on retry ("add two replicas" becoming 3→5 and then 5→7).

Intent ids are ignored on `DELETE`, because a delete carries no new object and an annotation on the stored object belongs to an earlier write.

Tradeoff: the annotation is stored on the object, so a later patch that does not touch annotations carries the old id forward. Within the dedupe window that later write is denied as a duplicate. I accepted that because the alternative (honour an id only when this request changes it) breaks the main use case: after a successful first attempt the stored object already has the id, so the retry would not be recognised. The denial message tells the agent to set a fresh id. A mutating step that strips the id after recording it would remove the trade-off, but mutating webhooks run before validating ones, so it would have to move the dedupe decision into the mutating phase; that is deferred.

### Content keys

Without an explicit id, the key is a SHA-256 (96 bits kept) over:

- operation, group/version/resource, subresource, namespace, name;
- for `CREATE` with `generateName`, the prefix instead of the name;
- the sorted list of `(path, new value)` changes.

Decisions behind that:

- **No requester in the key.** Two agents remediating the same alert should collide.
- **No UID in delete keys.** Deleting StatefulSet pod `db-0` twice is the canonical duplicate, and the second `db-0` has a new UID.
- **`generateName` keyed by prefix.** The API server fills in the random suffix before admission. My first version hashed the name and the kind e2e showed every retried `generateName` create getting a fresh key.
- **Normalization.** `status`, `apiVersion`, `kind`, and all metadata except labels and non-kube-fence annotations are dropped, so `resourceVersion`, `generation`, `managedFields`, and kube-fence's own annotations never make two identical intents look different.
- **New values, not deltas.** Hashing `(path, new value)` means re-asserting the same desired state after someone reverted it is a duplicate within the window. That is deliberate: an agent re-applying an image that Argo just reverted is exactly the controller fight the guard exists to stop.

### Lists

The diff matches list elements by their `name` field when every element has a unique string `name` (containers, env, volumes, ports with names). Other lists are compared as a whole and reported as one changed path. This loses precision for lists keyed by other fields (ports by `containerPort`/`protocol`), which errs toward reporting more change, never less.

## 5. Recording at admission time

The ledger records an effect when the webhook allows it. That is *attempted*, not *committed*: a later validating webhook, a quota check, or an etcd failure can still reject the write. When that happens, a correct retry is denied until the window expires.

The fix in M3 is a two-phase entry:

1. At admission, write the entry as `pending` with the admitted request UID.
2. A small watcher confirms the entry when it observes the object at a `resourceVersion` reached by that write (or observes the delete), and drops pending entries that are never confirmed within a short timeout.
3. Lookups treat `pending` as "unknown": deny with a message that tells the agent to wait and re-read rather than retry blindly.

That is the Kubernetes version of "reconcile on unknown outcome": the agent's question "did my call land?" gets answered from the cluster's own state rather than from the agent's memory of a timed-out RPC.

## 6. Ownership guard

### Reading ownership

`metadata.managedFields` lists, per manager, operation, and subresource, the fields that manager set. kube-fence flattens each `fieldsV1` tree into leaf paths (`f:` fields, `k:{"name":...}` list keys as `[name=x]`, other `k:`/`v:`/`i:` keys as the wildcard `[*]`) and reports a conflict when a changed path and an owned path are prefixes of each other.

It reads `oldObject`, not `object`. By the time admission runs on an `UPDATE`, the API server's field manager has already rewritten `managedFields` on the incoming object, and the agent appears as the owner of what it just changed.

### Protected managers

Only listed managers are protected; ownership by arbitrary managers (including the agent itself, `kubectl-client-side-apply`, or `kubectl-create`) is ignored. Defaults:

| Manager | Subresource | Why |
|---|---|---|
| `kube-controller-manager` | `scale` | the HPA writes `spec.replicas` through `/scale` |
| `argocd-controller`, `argocd-application-controller` | any | Argo CD |
| `kustomize-controller`, `helm-controller` | any | Flux |

Scoping `kube-controller-manager` to `scale` matters: the same manager name also owns status and the `deployment.kubernetes.io/revision` annotation, which agents never touch, but a broader match would be noisy for no benefit.

### Overrides

`kube-fence.io/allow-ownership-override` is honoured only if this request sets it or changes its value. An override left on the object from an earlier incident must not disable the guard forever. Because update operations transfer ownership, an overridden field belongs to the agent until the GitOps tool re-syncs; the e2e reproduces that sequence rather than assuming ownership is sticky.

### Limits found in testing

- `/scale` requests carry an `autoscaling/v1 Scale` object without `managedFields`, so ownership is not checked for them. Replica caps and dedupe still apply.
- On kind with Kubernetes v1.37.0, I compared three ways of creating a Deployment and then writing `spec.replicas` through `/scale` with field manager `kube-controller-manager`:

  | Created with | `spec.replicas` in the manifest | Entry `kube-controller-manager Update scale` afterwards |
  |---|---|---|
  | `kubectl create deployment` | yes (1) | present |
  | client-side `kubectl apply` | no | present |
  | server-side apply | yes (1) | present |
  | server-side apply | no | **absent** |

  Server-side applied manifests that omit replicas, which is the usual pattern when an HPA owns scaling, leave nothing in `managedFields` that kube-fence could match. So `managedFields` alone cannot be the HPA signal; M2 resolves `HorizontalPodAutoscaler.scaleTargetRef` directly and treats `spec.replicas` of any HPA target as owned.

## 7. Blast-radius limits

- **Deletes per window.** Counted from the ledger per namespace across all resource types. Namespace limits override defaults field by field, so `prod: {maxDeletes: 1}` keeps the default window.
- **Replica delta per write.** `|new.spec.replicas - old.spec.replicas|` for any object with `spec.replicas`, including `Scale` objects, so `kubectl scale` is covered.

Both are deliberately simple. A cumulative replica budget per window and per-resource-type delete caps would be easy additions once real agent traffic shows which limits matter.

## 8. Failure modes

| Situation | Effect | Rationale |
|---|---|---|
| kube-fence pod down or unreachable | agent writes fail (`failurePolicy: Fail`), all other writes unaffected | `matchConditions` scope the blast radius of fail-closed to agents |
| Mutating endpoint fails | write proceeds without the provenance annotation | provenance is best-effort |
| Pod restart | ledger history lost, dedupe and delete counts reset | in-memory store; fixed by the CRD ledger in M2 |
| Ledger write error | deny in `enforce`, allow in `audit` | an unrecorded allowed effect would defeat the next dedupe check |
| Two concurrent identical requests | the engine mutex serializes check-and-record, the second is denied | correct for one replica only; a shared store needs compare-and-swap |
| Dry run (`--dry-run=server`) | evaluated, not recorded | matches `sideEffects: NoneOnDryRun` |
| Config change | needs a pod restart | no hot reload yet |

## 9. Concurrency and the ledger interface

```go
type Ledger interface {
    Lookup(key string, since time.Time) (Entry, bool)
    Count(f Filter, since time.Time) int
    Record(e Entry) error
}
```

The engine holds its own mutex around lookup, count, and record, so the in-memory store only needs to be safe for concurrent use. A shared backend has to make check-and-record atomic across replicas. The plan for M2 is one `AgentEffect` custom resource per key, created with a deterministic name derived from the key: `create` either succeeds (first writer) or returns `AlreadyExists` (duplicate), so the API server's own uniqueness gives compare-and-swap without a lock service. Delete counts become a label-selected list over a namespace, which is fine at the volumes agents should be allowed anyway.

ConfigMap- and SQLite-backed stores fit the same interface; the ConfigMap version would need `resourceVersion` preconditions on every write and would contend under load.

## 10. Testing

- `pkg/policy`: table-driven tests where each case is a sequence of requests at given clock offsets against a fresh in-memory ledger. They cover identity matching, diff paths, `managedFields` flattening, overlap and wildcard rules, intent keys, config parsing errors, and the engine's interplay of rules (for example one request that is a duplicate, an ownership conflict, and over the replica cap at once).
- `pkg/ledger`: window boundaries, filters, pruning.
- `internal/webhook`: real HTTP round-trips of `AdmissionReview` JSON through `httptest`, including UID echo, 403 results, audit warnings, JSON patch escaping, and malformed input.
- `hack/e2e.sh`: 21 scenarios against a kind cluster with impersonated agent requests. It caught the `generateName` keying bug and surfaced the `managedFields` gap described in section 6.
