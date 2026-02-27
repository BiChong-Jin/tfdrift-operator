# tfdrift-operator — Concepts & Q&A Summary

## 1. What the Operator Does

The tfdrift-operator detects **Terraform drift** in Kubernetes resources. It compares the live state of Deployments and Services against an expected baseline hash, and emits a warning event when they diverge.

**Key design choices:**

- No custom CRDs — uses annotations and labels only
- Read-only — detects drift but does not revert it
- Opt-in — only watches resources labeled `tfdrift.jin.dev/enabled=true`

---

## 2. Architecture Overview

```
cmd/main.go
  └── controller-runtime Manager
        ├── DeploymentReconciler
        └── ServiceReconciler

internal/drift/hash.go
  ├── HashDeployment()
  └── HashService()
```

How Resources Are Watched

1. Registration — SetupWithManager

ctrl.NewControllerManagedBy(mgr).
For(&appsv1.Deployment{}).
Complete(r)

.For(&appsv1.Deployment{}) tells controller-runtime: "watch all Deployment objects cluster-wide and send any
create/update/delete events to my reconciler."

The same pattern exists in service_controller.go for Services.

---

2. The Watch Mechanism — Informers

Under the hood, controller-runtime uses a Kubernetes Informer (from client-go). Here's what that means:

```
Kubernetes API Server
│
│ LIST (initial full sync)
│ WATCH (streaming connection, long-lived HTTP)
▼
Informer / Cache
(in-memory store of all Deployments/Services)
│
│ on ADD / UPDATE / DELETE event
▼
Work Queue
(rate-limited, deduplicating)
│
│ dequeues {namespace/name}
▼
Reconcile(ctx, req)
```

- On startup: does a LIST to populate the local cache with all existing resources
- Then opens a long-lived WATCH stream to the API server — the API server pushes change events in real time
- Events are fed into a work queue — if the same object changes 5 times quickly, it gets deduplicated into 1
  reconcile call
- The reconciler only receives a {namespace/name} — it then does a fresh r.Get() to read current state from the
  cache

---

3. The Opt-in Filter (inside Reconcile, not at the watch level)

The operator does not filter at the watch level. It watches all Deployments/Services, then checks the label inside
Reconcile:

// deployment_controller.go:59
if dep.Labels == nil || dep.Labels[drift.LabelEnabled] != "true" {
return ctrl.Result{}, nil // skip — not opted in
}

This means every Deployment change triggers a reconcile call, but unmonitored resources are dropped immediately.
This is a common pattern — filtering at the informer level is possible but adds complexity.

---

---

## 3. The Reconcile Loop

**"Reconcile"** means to make two things agree — like reconciling a bank statement. Here it means: compare live state to expected state, and flag the difference.

Flow on every event:

```
1. Fetch object from cache
2. Check opt-in label          → skip if not enabled
3. Read expected hash          → skip if no baseline set
4. Compute live hash
5. Compare hashes
     match   → patch drifted=false
     no match → patch drifted=true, stamp drifted-at, emit Warning event
6. Patch annotations back onto the object
```

**Key property:** the reconciler is stateless and idempotent — it only looks at current state and acts. Running it 100 times on an unchanged object always produces the same result.

---

## 4. How Resources Are Watched

### SetupWithManager

```go
ctrl.NewControllerManagedBy(mgr).
    For(&appsv1.Deployment{}).
    Complete(r)
```

`.For()` registers the controller to watch all Deployments cluster-wide.

### Informer / Cache (under the hood)

```
API Server
    │
    │  LIST (initial full sync on startup)
    │  WATCH (long-lived streaming HTTP connection)
    ▼
Informer / Cache  (in-memory, inside the operator process)
    │
    │  on ADD / UPDATE / DELETE
    ▼
Work Queue  (rate-limited, deduplicating)
    │
    ▼
Reconcile(ctx, req)
```

- The cache is populated once via LIST on startup
- A long-lived WATCH stream pushes changes in real time
- Rapid changes to the same object are deduplicated into one reconcile call
- `r.Get()` inside Reconcile reads from the **cache**, not the API server

---

## 5. Cache vs Network Speed

|                      | Latency           |
| -------------------- | ----------------- |
| Memory (cache)       | ~100 nanoseconds  |
| Network (API server) | ~1–5 milliseconds |

**~10,000–50,000x faster.** Without the cache, every reconcile would hit the API server directly, putting heavy load on the most critical cluster component.

---

## 6. Where the Cache Lives

The informer/cache is a **Go struct in the operator pod's heap memory** — not a sidecar, not a cluster component, just RAM inside your process.

```
tfdrift-operator pod
  └── Go process
        └── controller-runtime Manager
              └── Shared Cache (in-process)
                    ├── Deployment informer
                    └── Service informer
```

If the pod restarts, the cache is lost and rebuilt from a fresh LIST.

---

## 7. Shared Informers (within one process)

controller-runtime shares informers **within a single process**. One informer per resource type, regardless of how many controllers watch it.

```
tfdrift-operator pod
├── Deployment informer → 1 WATCH stream  (Deployments)
└── Service informer    → 1 WATCH stream  (Services)
```

**Total: 2 WATCH streams**, one per unique resource type.

If two controllers inside the same process both watched Deployments, they would still share one stream — the count would not increase.

**Rule:** watch stream count = number of unique resource types watched within one operator process.

---

## 8. Operator Sprawl — Too Many Operators

Each operator process opens its own independent WATCH streams. Across different operator pods there is **no sharing**.

```
10 operators × 3 resource types each = 30 WATCH streams to the API server
```

At scale this becomes a real problem ("operator sprawl"):

- Every event fans out to all watchers simultaneously
- API server CPU/memory climbs
- Eventually the API server starts throttling requests

**Mitigations:**

- Consolidate related controllers into one binary
- Only watch resource types you actually need
- Use label selectors on watches to filter at the API server level
- Tune client-go rate limiters
