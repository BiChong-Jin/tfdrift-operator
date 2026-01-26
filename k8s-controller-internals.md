# Kubernetes Controller Internals Deep Dive

This document explains three core concepts:
1. The Informer/Cache Mechanism
2. The Manager Abstraction (controller-runtime vs kube-controller-manager)
3. The Relationship Between client-go and controller-runtime

---

## Part 1: The Informer/Cache Mechanism

### The Problem: Why Do We Need Informers?

Imagine you're writing a controller that watches Deployments. The naive approach:

```go
for {
    deployments, _ := client.List(ctx, &appsv1.DeploymentList{})
    for _, d := range deployments.Items {
        // check if something needs to be done
    }
    time.Sleep(5 * time.Second)
}
```

Problems with this approach:
1. **EXPENSIVE**: Every List() hits the API server directly
2. **SLOW**: 5 second polling means 5 second delay to react
3. **SCALES POORLY**: 100 controllers polling = 100x API server load
4. **MISSES EVENTS**: Things can happen between polls

### The Solution: Watch + Local Cache (Informer)

Instead of polling, Kubernetes uses a "watch" mechanism:

```
┌─────────────┐                      ┌─────────────┐
│ API Server  │ ───── watch ──────►  │  Informer   │
│             │  (long-lived HTTP)   │  (in your   │
│             │                      │   process)  │
│             │  "Deployment X added"│             │
│             │  "Deployment Y updated"            │
│             │  "Deployment Z deleted"            │
└─────────────┘                      └─────────────┘
```

The watch is a streaming HTTP connection. API server pushes events as they happen.

### Informer Architecture

An Informer has these components:

```
┌──────────────────────────────────────────────────────────────────┐
│                         INFORMER                                  │
│                                                                   │
│  ┌─────────────┐    ┌─────────────┐    ┌─────────────────────┐  │
│  │  Reflector  │───►│  DeltaFIFO  │───►│   Indexer (Store)   │  │
│  │             │    │   (Queue)   │    │   (Local Cache)     │  │
│  └─────────────┘    └──────┬──────┘    └─────────────────────┘  │
│        │                   │                     │               │
│        │                   ▼                     │               │
│   List+Watch        Event Handlers              Lister           │
│   from API          (your callbacks)         (fast lookups)      │
│                                                                   │
└──────────────────────────────────────────────────────────────────┘
```

Let's break down each component:

1. **REFLECTOR**
   - Connects to API server
   - Does initial LIST to get all existing objects
   - Then WATCHes for changes
   - Pushes events into DeltaFIFO

2. **DELTAFIFO (Queue)**
   - A queue that stores "deltas" (changes)
   - Delta = { Type: Added/Updated/Deleted, Object: the resource }
   - Ensures events are processed in order
   - Deduplicates: if same object updated 5 times quickly, you might only process it once with latest state

3. **INDEXER / STORE (Local Cache)**
   - In-memory copy of all watched resources
   - Indexed for fast lookups (by namespace, name, labels, etc.)
   - Your controller reads from HERE, not from API server

4. **LISTER**
   - Interface to query the local cache
   - `deploymentLister.Get("my-deployment")` reads from memory, NOT API server
   - FAST: microseconds instead of milliseconds

5. **EVENT HANDLERS**
   - Your callbacks: OnAdd, OnUpdate, OnDelete
   - Called when something changes
   - Typically: add the object's key to a workqueue

### How a Watch Actually Works (HTTP Level)

```
GET /apis/apps/v1/deployments?watch=true

Response (chunked, never closes):
{"type":"ADDED","object":{"kind":"Deployment","metadata":{"name":"nginx",...}}}
{"type":"MODIFIED","object":{"kind":"Deployment","metadata":{"name":"nginx",...}}}
{"type":"DELETED","object":{"kind":"Deployment","metadata":{"name":"nginx",...}}}
... (connection stays open, events stream in)
```

If connection drops, Reflector reconnects using "resourceVersion" to resume where it left off (no missed events).

### Workqueue: Why Not Process Directly in Handlers?

Event handlers should be FAST. They just enqueue work:

```go
OnUpdate(old, new) {
    key := namespace + "/" + name
    workqueue.Add(key)  // just add to queue, return immediately
}
```

Why a separate queue?

1. **RATE LIMITING**: If object updates 100 times/second, you don't reconcile 100 times. Queue dedupes.
2. **RETRIES**: If reconcile fails, requeue with backoff
3. **MULTIPLE WORKERS**: Can have N goroutines processing the queue
4. **DECOUPLING**: Handler returns fast, actual work happens async

### The Full Picture

```
┌──────────────────────────────────────────────────────────────────────────┐
│                                                                          │
│   API Server                                                             │
│       │                                                                  │
│       │ watch (streaming HTTP)                                           │
│       ▼                                                                  │
│   Reflector ──► DeltaFIFO ──► Indexer (cache)                           │
│                     │              │                                     │
│                     ▼              │                                     │
│              Event Handlers        │                                     │
│              (OnAdd/Update/Delete) │                                     │
│                     │              │                                     │
│                     ▼              │                                     │
│               Workqueue            │                                     │
│                     │              │                                     │
│                     ▼              ▼                                     │
│               ┌─────────────────────────┐                               │
│               │     YOUR RECONCILE()    │                               │
│               │                         │                               │
│               │  1. Get key from queue  │                               │
│               │  2. Lookup in cache ◄───┘  (fast, local)                │
│               │  3. Compare desired vs actual                           │
│               │  4. Make changes via API server (write only)            │
│               │  5. Return (requeue if error)                           │
│               └─────────────────────────┘                               │
│                                                                          │
└──────────────────────────────────────────────────────────────────────────┘
```

**KEY INSIGHT**: Reads are local (cache), writes go to API server.

### Shared Informer Factory

Problem: Multiple controllers might watch the same resource type.
- Deployment controller watches Deployments
- HPA controller ALSO watches Deployments
- Having 2 separate watches = 2x connections, 2x memory for same data

Solution: SharedInformerFactory

```go
factory := informers.NewSharedInformerFactory(client, resyncPeriod)

// Both get the SAME underlying informer
deployInformer1 := factory.Apps().V1().Deployments()
deployInformer2 := factory.Apps().V1().Deployments()  // same instance!

factory.Start(stopCh)  // starts all informers once
```

One watch, one cache, multiple consumers.

---

## Part 2: The Manager Abstraction

There are TWO different things called "manager":

1. **kube-controller-manager** - A Kubernetes COMPONENT (binary/process)
2. **controller-runtime Manager** - A Go LIBRARY construct

Let's understand each:

### kube-controller-manager (The Component)

This is an actual binary that runs as part of the Kubernetes control plane.

Location in a cluster:

```
Control Plane Node
├── /etc/kubernetes/manifests/
│   ├── kube-apiserver.yaml        (static pod)
│   ├── kube-controller-manager.yaml (static pod)  ◄── THIS
│   ├── kube-scheduler.yaml        (static pod)
│   └── etcd.yaml                  (static pod)
```

It's a "static pod" - kubelet runs it directly from a manifest file, not managed by the API server.

What's inside kube-controller-manager:

```
┌─────────────────────────────────────────────────────────────────┐
│                  kube-controller-manager process                │
│                                                                 │
│  ┌─────────────────┐  ┌─────────────────┐  ┌────────────────┐  │
│  │ Deployment      │  │ ReplicaSet      │  │ StatefulSet    │  │
│  │ Controller      │  │ Controller      │  │ Controller     │  │
│  └─────────────────┘  └─────────────────┘  └────────────────┘  │
│                                                                 │
│  ┌─────────────────┐  ┌─────────────────┐  ┌────────────────┐  │
│  │ DaemonSet       │  │ Job             │  │ CronJob        │  │
│  │ Controller      │  │ Controller      │  │ Controller     │  │
│  └─────────────────┘  └─────────────────┘  └────────────────┘  │
│                                                                 │
│  ┌─────────────────┐  ┌─────────────────┐  ┌────────────────┐  │
│  │ Node            │  │ Service         │  │ Endpoints      │  │
│  │ Controller      │  │ Controller      │  │ Controller     │  │
│  └─────────────────┘  └─────────────────┘  └────────────────┘  │
│                                                                 │
│  ┌─────────────────┐  ┌─────────────────┐  ┌────────────────┐  │
│  │ Namespace       │  │ ServiceAccount  │  │ PV/PVC         │  │
│  │ Controller      │  │ Controller      │  │ Controller     │  │
│  └─────────────────┘  └─────────────────┘  └────────────────┘  │
│                                                                 │
│  ... and ~30 more controllers ...                              │
│                                                                 │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │           Shared Informer Factory                        │   │
│  │  (one cache shared by all controllers above)             │   │
│  └─────────────────────────────────────────────────────────┘   │
│                                                                 │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │           Leader Election (one for entire process)       │   │
│  └─────────────────────────────────────────────────────────┘   │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

Key characteristics:
- ONE process with MANY controllers
- ONE leader election for ALL controllers
- ONE shared informer factory
- Part of Kubernetes itself (in kubernetes/kubernetes repo)
- Tightly coupled to Kubernetes release cycle

### controller-runtime Manager (The Library)

This is a Go struct/interface that YOU use to build custom controllers.

```go
import ctrl "sigs.k8s.io/controller-runtime"

mgr, err := ctrl.NewManager(config, ctrl.Options{
    // your options
})
```

What the Manager provides:

```
┌─────────────────────────────────────────────────────────────────┐
│              controller-runtime Manager                         │
│                                                                 │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │  Cluster     - connection to API server                  │   │
│  │  Client      - read/write kubernetes resources           │   │
│  │  Cache       - shared informer cache (like factory)      │   │
│  │  Scheme      - type registry (what Go types exist)       │   │
│  └─────────────────────────────────────────────────────────┘   │
│                                                                 │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │  Leader Election  - HA support                           │   │
│  │  Health Probes    - /healthz, /readyz                    │   │
│  │  Metrics Server   - prometheus metrics                   │   │
│  │  Webhook Server   - admission webhooks                   │   │
│  └─────────────────────────────────────────────────────────┘   │
│                                                                 │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │  Runnables   - things the manager starts/stops           │   │
│  │    └── Your Controllers                                  │   │
│  │    └── Your Webhooks                                     │   │
│  │    └── Custom runnables                                  │   │
│  └─────────────────────────────────────────────────────────┘   │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

The Manager is a CONVENIENCE WRAPPER that:
1. Creates and manages the shared cache
2. Creates a client that reads from cache, writes to API server
3. Handles leader election
4. Manages controller lifecycle (start/stop)
5. Provides health endpoints

### Comparison

| kube-controller-manager | controller-runtime Manager |
|-------------------------|---------------------------|
| A binary/process | A Go struct in your code |
| Runs ~40 controllers | Runs YOUR controllers (1, 2, or more) |
| Part of control plane | Runs as regular workload |
| One per cluster | One per operator deployment |
| Uses client-go directly | Uses controller-runtime abstractions |
| Kubernetes release cycle | Your release cycle |

### What Happens in Your main.go

```go
// 1. Create the manager (this sets up cache, client, etc.)
mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
    Scheme:                 scheme,
    HealthProbeBindAddress: ":8081",
    LeaderElection:         true,
    LeaderElectionID:       "0a560047.jin.dev",
})

// 2. Register your controllers with the manager
// This tells the manager "hey, I have a controller, manage it"
if err = (&controller.DeploymentReconciler{
    Client: mgr.GetClient(),
}).SetupWithManager(mgr); err != nil {
    // handle error
}

// 3. Start the manager
// This starts: cache, controllers, health probes, leader election
if err := mgr.Start(ctx); err != nil {
    // handle error
}
```

When `mgr.Start()` is called:
1. Waits for leader election (if enabled)
2. Starts all informers (cache.Start)
3. Waits for cache sync
4. Starts all controllers
5. Starts health/metrics servers
6. Blocks until context is cancelled

---

## Part 3: Relationship Between client-go and controller-runtime

### Layer Diagram

```
┌─────────────────────────────────────────────────────────────────┐
│                     YOUR CONTROLLER CODE                        │
│          (DeploymentReconciler, ServiceReconciler)              │
└─────────────────────────────────────────────────────────────────┘
                              │
                              │ uses
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                   CONTROLLER-RUNTIME                            │
│  sigs.k8s.io/controller-runtime                                 │
│                                                                 │
│  Provides:                                                      │
│  - Manager (lifecycle, shared resources)                        │
│  - Reconciler interface (simple Reconcile(ctx, req) pattern)    │
│  - Builder (easy controller setup with For(), Owns(), etc.)     │
│  - Client (unified read/write interface)                        │
│  - Cache (wraps informers with better API)                      │
│  - Source, EventHandler, Predicate (event filtering)            │
└─────────────────────────────────────────────────────────────────┘
                              │
                              │ wraps/uses
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                       CLIENT-GO                                 │
│  k8s.io/client-go                                               │
│                                                                 │
│  Provides:                                                      │
│  - REST client (raw HTTP to API server)                         │
│  - Typed clients (clientset.AppsV1().Deployments())             │
│  - Dynamic client (unstructured access)                         │
│  - Informers, Listers (watch + cache)                           │
│  - Workqueue (rate-limited queue)                               │
│  - Leader election                                              │
│  - Authentication (kubeconfig, in-cluster)                      │
└─────────────────────────────────────────────────────────────────┘
                              │
                              │ wraps/uses
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                       API MACHINERY                             │
│  k8s.io/apimachinery                                            │
│                                                                 │
│  Provides:                                                      │
│  - Core types (ObjectMeta, TypeMeta, etc.)                      │
│  - Scheme (type registry)                                       │
│  - Serialization (JSON, YAML, Protobuf)                         │
│  - API conventions                                              │
└─────────────────────────────────────────────────────────────────┘
```

### Why Does controller-runtime Exist?

Writing a controller with raw client-go is VERBOSE. Here's a comparison:

**WITH CLIENT-GO ONLY** (what built-in controllers do):

```go
// This is ~200 lines simplified, real code is much more

func main() {
    // 1. Create config
    config, _ := clientcmd.BuildConfigFromFlags("", kubeconfig)

    // 2. Create clientset
    clientset, _ := kubernetes.NewForConfig(config)

    // 3. Create informer factory
    factory := informers.NewSharedInformerFactory(clientset, time.Minute)

    // 4. Get specific informer
    deployInformer := factory.Apps().V1().Deployments()

    // 5. Create workqueue
    queue := workqueue.NewRateLimitingQueue(
        workqueue.DefaultControllerRateLimiter())

    // 6. Add event handlers
    deployInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
        AddFunc: func(obj interface{}) {
            key, _ := cache.MetaNamespaceKeyFunc(obj)
            queue.Add(key)
        },
        UpdateFunc: func(old, new interface{}) {
            key, _ := cache.MetaNamespaceKeyFunc(new)
            queue.Add(key)
        },
        DeleteFunc: func(obj interface{}) {
            key, _ := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
            queue.Add(key)
        },
    })

    // 7. Start informers
    stopCh := make(chan struct{})
    factory.Start(stopCh)

    // 8. Wait for cache sync
    factory.WaitForCacheSync(stopCh)

    // 9. Run workers
    for i := 0; i < workerCount; i++ {
        go func() {
            for {
                key, quit := queue.Get()
                if quit {
                    return
                }

                // 10. Process item
                err := reconcile(key.(string), deployInformer.Lister())
                if err != nil {
                    queue.AddRateLimited(key)
                } else {
                    queue.Forget(key)
                }
                queue.Done(key)
            }
        }()
    }

    <-stopCh
}
```

**WITH CONTROLLER-RUNTIME** (what you do):

```go
func main() {
    mgr, _ := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{})

    _ = ctrl.NewControllerManagedBy(mgr).
        For(&appsv1.Deployment{}).
        Complete(&DeploymentReconciler{Client: mgr.GetClient()})

    mgr.Start(ctx)
}

type DeploymentReconciler struct {
    client.Client
}

func (r *DeploymentReconciler) Reconcile(ctx context.Context,
    req ctrl.Request) (ctrl.Result, error) {

    var deploy appsv1.Deployment
    r.Get(ctx, req.NamespacedName, &deploy)
    // your logic here
    return ctrl.Result{}, nil
}
```

Controller-runtime reduces boilerplate from ~200 lines to ~20 lines!

### What controller-runtime Abstracts

**1. RECONCILER INTERFACE**

- client-go: You manage workqueue, process keys, handle retries manually
- controller-runtime: Just implement `Reconcile(ctx, Request) (Result, error)`

The Result tells the framework what to do:
- `Result{}`: success, don't requeue
- `Result{Requeue: true}`: requeue immediately
- `Result{RequeueAfter: 5*time.Minute}`: requeue after duration
- `error`: requeue with backoff

**2. BUILDER PATTERN**

- client-go: Manually wire informers, handlers, predicates
- controller-runtime:

```go
ctrl.NewControllerManagedBy(mgr).
    For(&appsv1.Deployment{}).           // primary watch
    Owns(&appsv1.ReplicaSet{}).          // watch children
    Watches(&corev1.ConfigMap{}, ...).   // custom watch
    WithEventFilter(predicate.Funcs{}). // filter events
    Complete(reconciler)
```

**3. CLIENT**

- client-go clientset: `clientset.AppsV1().Deployments("ns").Get(ctx, "name", metav1.GetOptions{})`
- controller-runtime client: `client.Get(ctx, types.NamespacedName{Name: "name", Namespace: "ns"}, &deploy)`

The controller-runtime client:
- Works with ANY type (core, custom CRDs, unstructured)
- Reads from cache by default (fast)
- Writes directly to API server
- Uses scheme to understand types

**4. CACHE**

- client-go: SharedInformerFactory, manual informer management
- controller-runtime: Cache interface, automatic informer creation

When you call `client.Get()` or `client.List()`, it reads from cache.
When you call `client.Create/Update/Delete/Patch()`, it goes to API server.

### What controller-runtime Uses From client-go

Controller-runtime doesn't replace client-go, it USES it:

| controller-runtime component | uses from client-go |
|------------------------------|---------------------|
| Manager | leader election |
| Cache | SharedInformerFactory |
| Client (writes) | REST client |
| Client (reads) | Lister from informers |
| Controller | workqueue |
| Source | Informer event handlers |

Under the hood, when you do:

```go
client.Get(ctx, key, &deployment)
```

Controller-runtime:
1. Looks up the informer for Deployment type in cache
2. Calls the lister (from client-go) to get from local cache
3. Returns the result

When you do:

```go
client.Update(ctx, &deployment)
```

Controller-runtime:
1. Uses the REST client (from client-go)
2. Sends PUT request to API server
3. Returns the result

### Summary Diagram

```
┌─────────────────────────────────────────────────────────────────┐
│                    YOUR RECONCILER                              │
│                                                                 │
│    func Reconcile(ctx, req) (Result, error) {                   │
│        r.Client.Get(...)    // reads from cache                 │
│        r.Client.Update(...) // writes to API server             │
│    }                                                            │
│                                                                 │
└───────────────────────────┬─────────────────────────────────────┘
                            │
                ┌───────────┴───────────┐
                │                       │
                ▼                       ▼
┌───────────────────────┐   ┌───────────────────────┐
│  controller-runtime   │   │  controller-runtime   │
│       CACHE           │   │       CLIENT          │
│                       │   │                       │
│  - wraps informers    │   │  - Get/List → cache   │
│  - automatic setup    │   │  - Create/Update →    │
│  - indexing           │   │       API server      │
└───────────┬───────────┘   └───────────┬───────────┘
            │                           │
            ▼                           ▼
┌───────────────────────┐   ┌───────────────────────┐
│  client-go            │   │  client-go            │
│  SharedInformer       │   │  REST client          │
│  Lister               │   │                       │
└───────────┬───────────┘   └───────────┬───────────┘
            │                           │
            └───────────┬───────────────┘
                        │
                        ▼
            ┌───────────────────────┐
            │     API SERVER        │
            │                       │
            │  - watch connection   │
            │    (for informers)    │
            │  - REST calls         │
            │    (for writes)       │
            └───────────────────────┘
```

---

## Part 4: HTTP Streaming Watch - Under the Hood

This section explains exactly how the "watch" mechanism works at the HTTP level.

### Basic Concept

A watch is just an HTTP GET request that NEVER ENDS (until you close it).

**Normal HTTP request:**
```
Client: GET /api/v1/pods
Server: 200 OK, here's all pods (JSON array), CONNECTION CLOSED
```

**Watch HTTP request:**
```
Client: GET /api/v1/pods?watch=true
Server: 200 OK, here's events... (connection stays open forever)
        {"type":"ADDED","object":{...}}
        {"type":"MODIFIED","object":{...}}
        ... (keeps streaming)
```

### HTTP Chunked Transfer Encoding

The watch uses HTTP/1.1 "chunked transfer encoding":

```
GET /api/v1/namespaces/default/pods?watch=true HTTP/1.1
Host: kubernetes.default.svc
Authorization: Bearer <token>

HTTP/1.1 200 OK
Content-Type: application/json
Transfer-Encoding: chunked      ◄── KEY: chunked, not Content-Length

{"type":"ADDED","object":{"kind":"Pod","metadata":{"name":"nginx",...}}}
{"type":"MODIFIED","object":{"kind":"Pod","metadata":{"name":"nginx",...}}}
{"type":"DELETED","object":{"kind":"Pod","metadata":{"name":"nginx",...}}}
... (stream continues)
```

With chunked encoding:
- Server doesn't declare Content-Length upfront
- Server sends data in "chunks" as events happen
- Connection stays open indefinitely
- Client reads line by line (each line = one JSON event)

### The Watch Event Format

Each line in the stream is a WatchEvent JSON object:

```json
{
    "type": "ADDED" | "MODIFIED" | "DELETED" | "BOOKMARK" | "ERROR",
    "object": {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {
            "name": "nginx",
            "namespace": "default",
            "resourceVersion": "12345",  // ◄── CRITICAL for resuming
            ...
        },
        "spec": {...},
        "status": {...}
    }
}
```

Event types:
- **ADDED**: New resource created (or existed at watch start)
- **MODIFIED**: Resource updated
- **DELETED**: Resource removed
- **BOOKMARK**: Periodic checkpoint (just resourceVersion, no full object)
- **ERROR**: Something went wrong (usually means reconnect needed)

### Resource Version - The Magic Number

Every Kubernetes object has a resourceVersion (from etcd's revision number).

```yaml
metadata:
  name: nginx
  resourceVersion: "12345"   # ◄── Changes on every update
```

The watch uses resourceVersion for two purposes:

**1. STARTING POINT**: Where to begin watching

```
# Watch from NOW (only future events)
GET /api/v1/pods?watch=true

# Watch from a specific point (replay events since version 10000)
GET /api/v1/pods?watch=true&resourceVersion=10000
```

**2. RESUMING**: If connection drops, reconnect without missing events

```
# Original watch was at resourceVersion 12345 when it died
# Reconnect and continue from there
GET /api/v1/pods?watch=true&resourceVersion=12345
```

### What Happens During a Watch (Sequence Diagram)

```
Client (Reflector)                          API Server
      │                                          │
      │  GET /api/v1/pods?watch=true             │
      │  &resourceVersion=0                       │
      │ ────────────────────────────────────────► │
      │                                          │
      │  HTTP 200 OK                             │
      │  Transfer-Encoding: chunked              │
      │ ◄──────────────────────────────────────── │
      │                                          │
      │  {"type":"ADDED","object":{pod1, rv:100}}│
      │ ◄──────────────────────────────────────── │
      │                                          │
      │  {"type":"ADDED","object":{pod2, rv:101}}│
      │ ◄──────────────────────────────────────── │
      │                                          │
      │         ... time passes ...              │
      │                                          │
      │  {"type":"MODIFIED","object":{pod1,rv:150}}
      │ ◄──────────────────────────────────────── │
      │                                          │
      │  {"type":"BOOKMARK","object":{rv:200}}   │  (periodic)
      │ ◄──────────────────────────────────────── │
      │                                          │
      │         ... connection drops ...         │
      │               X                          │
      │                                          │
      │  GET /api/v1/pods?watch=true             │
      │  &resourceVersion=200                    │  (resume from bookmark)
      │ ────────────────────────────────────────► │
      │                                          │
      │  {"type":"MODIFIED","object":{pod2,rv:201}}
      │ ◄──────────────────────────────────────── │
      │                                          │
```

### Bookmarks - Preventing "Too Old" Errors

Problem: If you reconnect with a very old resourceVersion, etcd may have compacted (deleted) that history. You get:

```json
{"type":"ERROR","object":{"message":"resourceVersion too old"}}
```

Solution: BOOKMARK events

```json
{"type":"BOOKMARK","object":{"metadata":{"resourceVersion":"15000"}}}
```

Bookmarks are sent periodically (every ~1 minute). They contain:
- Only the resourceVersion (no full object - saves bandwidth)
- A checkpoint you can use to resume

The Reflector saves the latest resourceVersion from ANY event (including bookmarks), so when reconnecting, it uses a recent version.

### List Then Watch Pattern

The Reflector doesn't just watch. It does LIST first, then WATCH:

1. LIST all existing resources
   ```
   GET /api/v1/pods
   Response: {"items": [...], "metadata": {"resourceVersion": "5000"}}
   ```

2. WATCH starting from that resourceVersion
   ```
   GET /api/v1/pods?watch=true&resourceVersion=5000
   ```

Why LIST first?
- Watch only gives you CHANGES, not existing state
- LIST gives you current state + resourceVersion to watch from
- Together: complete picture of past + future

This is called "List-Watch" pattern.

### Actual HTTP Bytes on the Wire

Here's what the raw HTTP looks like (simplified):

```
>>> Request:
GET /api/v1/namespaces/default/pods?watch=true&resourceVersion=1000 HTTP/1.1
Host: 10.96.0.1:443
Authorization: Bearer eyJhbGciOiJSUzI1NiIsImtpZCI6...
Accept: application/json, */*

<<< Response:
HTTP/1.1 200 OK
Cache-Control: no-cache, private
Content-Type: application/json
Transfer-Encoding: chunked
Date: Mon, 27 Jan 2025 10:00:00 GMT

{"type":"ADDED","object":{"apiVersion":"v1","kind":"Pod",...}}
{"type":"MODIFIED","object":{"apiVersion":"v1","kind":"Pod",...}}
(connection stays open, more lines appear as events happen)
```

Each JSON object is on its own line (newline-delimited JSON, aka NDJSON).

### Timeouts and Keep-Alive

Watches don't last forever. The API server has timeouts:

```
GET /api/v1/pods?watch=true&timeoutSeconds=300
```

- Default timeout: ~5-10 minutes (server-configured)
- After timeout: Server closes connection gracefully
- Reflector: Automatically reconnects with latest resourceVersion

Also, TCP keep-alive packets prevent network equipment from killing "idle" connections (even though data flows intermittently).

### How Streaming is Implemented (Server + Client Code)

Normal HTTP: Server writes response, closes connection. Done.
Watch HTTP: Server writes, waits, writes more, waits, writes more... never closes.

How? Two key techniques:

1. **SERVER SIDE**: Flush after each event (don't buffer)
2. **CLIENT SIDE**: Read line-by-line in a loop (don't wait for EOF)

#### Server Side (API Server)

**Normal handler:**

```go
func normalHandler(w http.ResponseWriter, r *http.Request) {
    pods := getAllPods()
    json.NewEncoder(w).Encode(pods)
    // Function returns → Go's HTTP server closes connection
}
```

**Watch handler (simplified):**

```go
func watchHandler(w http.ResponseWriter, r *http.Request) {
    // 1. Get the Flusher interface - this is KEY
    flusher, ok := w.(http.Flusher)
    if !ok {
        http.Error(w, "streaming not supported", 500)
        return
    }

    // 2. Set headers for streaming
    w.Header().Set("Content-Type", "application/json")
    w.Header().Set("Transfer-Encoding", "chunked")
    w.WriteHeader(200)

    // 3. Subscribe to etcd watch channel
    eventCh := etcd.Watch(ctx, "/pods/")

    // 4. Loop forever, sending events as they happen
    for {
        select {
        case event := <-eventCh:
            // Convert to watch event JSON
            watchEvent := WatchEvent{
                Type:   event.Type,
                Object: event.Object,
            }

            // Write JSON + newline
            json.NewEncoder(w).Encode(watchEvent)

            // CRITICAL: Flush immediately!
            // Without this, data sits in buffer, client doesn't see it
            flusher.Flush()

        case <-r.Context().Done():
            // Client disconnected
            return
        }
    }
    // Function NEVER returns (until client disconnects or timeout)
}
```

The key differences:
- Uses `http.Flusher` to push data immediately
- Loops forever instead of returning
- Writes incrementally, not all at once

What is `http.Flusher`?

```go
type Flusher interface {
    Flush()  // Send buffered data to client NOW
}
```

Without `Flush()`, Go's HTTP server buffers writes for efficiency.
With `Flush()`, each event goes to the client immediately.

#### Client Side (Reflector in client-go)

**Normal client:**

```go
func normalRequest() {
    resp, _ := http.Get("http://api/pods")
    body, _ := ioutil.ReadAll(resp.Body)  // Waits for ENTIRE response
    resp.Body.Close()
}
```

**Watch client (simplified):**

```go
func watchRequest() {
    resp, _ := http.Get("http://api/pods?watch=true")
    // DON'T use ioutil.ReadAll - that waits for EOF (never comes!)

    // Instead: read line by line
    reader := bufio.NewReader(resp.Body)

    for {
        // ReadBytes blocks until '\n' is received
        // It does NOT wait for connection to close
        line, err := reader.ReadBytes('\n')

        if err == io.EOF {
            // Server closed connection (timeout, error, etc.)
            break
        }
        if err != nil {
            // Handle error
            break
        }

        // Parse the JSON event
        var event WatchEvent
        json.Unmarshal(line, &event)

        // Process the event (add to DeltaFIFO)
        processEvent(event)
    }

    resp.Body.Close()
}
```

The key insight: `reader.ReadBytes('\n')` returns as soon as ONE line is available. It doesn't wait for the connection to close.

#### The Actual client-go Code

In `k8s.io/apimachinery/pkg/watch/streamwatcher.go`:

```go
// StreamWatcher turns any stream into a watch.Interface.
type StreamWatcher struct {
    source   Decoder
    result   chan Event
    // ...
}

// receive reads from the stream and sends events to result channel
func (sw *StreamWatcher) receive() {
    defer close(sw.result)

    for {
        // Decode blocks until one complete event is received
        // (internally uses buffered reading, like ReadBytes)
        action, obj, err := sw.source.Decode()

        if err != nil {
            // Connection closed or error
            return
        }

        // Send event to the result channel
        sw.result <- Event{
            Type:   action,
            Object: obj,
        }
    }
}
```

And the Reflector consumes this:

```go
// In k8s.io/client-go/tools/cache/reflector.go

func (r *Reflector) watchHandler(w watch.Interface, ...) error {
    for {
        select {
        case event, ok := <-w.ResultChan():
            if !ok {
                // Channel closed (connection died)
                return nil
            }

            // Process the event
            switch event.Type {
            case watch.Added:
                r.store.Add(event.Object)
            case watch.Modified:
                r.store.Update(event.Object)
            case watch.Deleted:
                r.store.Delete(event.Object)
            }

        case <-stopCh:
            return nil
        }
    }
}
```

#### Visual: Normal vs Streaming

**NORMAL REQUEST:**

```
Client                              Server
  │                                    │
  │  GET /pods                         │
  │ ──────────────────────────────────►│
  │                                    │ Process...
  │                                    │ Build full response...
  │  200 OK                            │
  │  Content-Length: 5000              │
  │  {"items": [...all pods...]}       │
  │ ◄──────────────────────────────────│
  │                                    │
  │  Connection closed                 │
  │         X                          │
```

**STREAMING WATCH:**

```
Client                              Server
  │                                    │
  │  GET /pods?watch=true              │
  │ ──────────────────────────────────►│
  │                                    │
  │  200 OK                            │
  │  Transfer-Encoding: chunked        │
  │ ◄──────────────────────────────────│
  │                                    │
  │  {"type":"ADDED",...}\n            │
  │ ◄──────────────────────────────────│ flusher.Flush()
  │                                    │
  │     (client processes event)       │
  │                                    │
  │         ... time passes ...        │
  │                                    │
  │  {"type":"MODIFIED",...}\n         │
  │ ◄──────────────────────────────────│ flusher.Flush()
  │                                    │
  │     (client processes event)       │
  │                                    │
  │         ... time passes ...        │
  │                                    │
  │  {"type":"ADDED",...}\n            │  (new pod created)
  │ ◄──────────────────────────────────│ flusher.Flush()
  │                                    │
  │     ... continues forever ...      │
```

#### TCP Level: Why This Works

At TCP level, there's no "complete response". TCP is just a byte stream.

HTTP adds semantics:
- `Content-Length`: "I'm sending exactly N bytes, then we're done"
- `Transfer-Encoding: chunked`: "I'll send chunks, each prefixed with size"
- Chunked terminator: A zero-length chunk means "done"

For watch:
- Server sends chunks but NEVER sends the zero-length terminator
- TCP connection stays open
- Each `flusher.Flush()` pushes current buffer as a chunk
- Client reads chunks as they arrive

### Error Handling

What can go wrong:

1. **CONNECTION RESET**
   - Network blip, pod restart, etc.
   - Reflector: Reconnect with last known resourceVersion

2. **410 GONE (resourceVersion too old)**
   - etcd compacted the history
   - Reflector: Do a fresh LIST, then WATCH from new resourceVersion

3. **500 INTERNAL SERVER ERROR**
   - API server having issues
   - Reflector: Backoff and retry

4. **WATCH CHANNEL CLOSED**
   - Server decided to close (timeout, resource pressure)
   - Reflector: Reconnect

The Reflector handles ALL of these automatically with exponential backoff.

---

## Part 5: DeltaFIFO - Under the Hood

DeltaFIFO is a queue that stores changes (deltas) to objects. It's the bridge between the Reflector (which receives events) and the Indexer (local cache).

### Why DeltaFIFO Exists

Problem: Raw watch events aren't ideal for processing.

Scenario: Pod "nginx" is updated 10 times in 1 second.

**Without DeltaFIFO:**
- 10 events hit your handler
- 10 reconcile calls
- Wasteful, especially if only final state matters

**With DeltaFIFO:**
- 10 events are COMPRESSED into the queue
- When you pop, you get the LATEST state
- Or you can see ALL deltas if you need history

### The Delta Type

A Delta represents a single change:

```go
type Delta struct {
    Type   DeltaType   // Added, Updated, Deleted, Replaced, Sync
    Object interface{} // The object at that point in time
}

type DeltaType string
const (
    Added    DeltaType = "Added"
    Updated  DeltaType = "Updated"
    Deleted  DeltaType = "Deleted"
    Replaced DeltaType = "Replaced"  // From re-list
    Sync     DeltaType = "Sync"      // Periodic resync
)
```

### DeltaFIFO Data Structure

DeltaFIFO is essentially:

```go
type DeltaFIFO struct {
    // Map from object key to list of deltas for that object
    items map[string]Deltas    // key -> []Delta

    // Queue of keys in FIFO order
    queue []string

    // ... synchronization stuff (lock, cond) ...
}

type Deltas []Delta  // A slice of deltas for one object
```

Visual representation:

```
┌──────────────────────────────────────────────────────────────────┐
│                         DeltaFIFO                                │
│                                                                  │
│   queue (FIFO order):                                           │
│   ┌─────────────────────────────────────────────────────────┐   │
│   │ "default/nginx" │ "default/redis" │ "kube-system/coredns" │ │
│   └─────────────────────────────────────────────────────────┘   │
│            │                │                   │               │
│            ▼                ▼                   ▼               │
│   items (map):                                                  │
│   ┌─────────────────────────────────────────────────────────┐   │
│   │ "default/nginx" ──► [Delta{Added,obj}, Delta{Updated,obj}] ││
│   │ "default/redis" ──► [Delta{Added,obj}]                     ││
│   │ "kube-system/coredns" ──► [Delta{Updated,obj}]             ││
│   └─────────────────────────────────────────────────────────┘   │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

**KEY INSIGHT**: Multiple deltas can accumulate for the SAME object.

### How Events Flow Into DeltaFIFO

When Reflector receives a watch event:

```
Reflector                              DeltaFIFO
    │                                      │
    │  obj = Pod{name: "nginx"}            │
    │  event = MODIFIED                    │
    │                                      │
    │  Add(obj)  or  Update(obj)           │
    │ ────────────────────────────────────►│
    │                                      │
    │                           1. Compute key: "default/nginx"
    │                           2. Create Delta{Updated, obj}
    │                           3. Append to items["default/nginx"]
    │                           4. If key not in queue, append to queue
    │                           5. Signal any waiting consumers
    │                                      │
```

The Add/Update/Delete methods on DeltaFIFO:

```go
func (f *DeltaFIFO) Add(obj interface{}) error {
    f.lock.Lock()
    defer f.lock.Unlock()
    return f.queueActionLocked(Added, obj)
}

func (f *DeltaFIFO) queueActionLocked(actionType DeltaType, obj interface{}) error {
    // 1. Get the key for this object
    key, err := f.KeyOf(obj)  // e.g., "default/nginx"

    // 2. Get existing deltas for this key (or empty slice)
    oldDeltas := f.items[key]

    // 3. Append new delta
    newDeltas := append(oldDeltas, Delta{actionType, obj})

    // 4. Deduplicate (compress) if needed
    newDeltas = dedupDeltas(newDeltas)

    // 5. Store back
    f.items[key] = newDeltas

    // 6. Add key to queue if not already present
    if !f.keyInQueue(key) {
        f.queue = append(f.queue, key)
    }

    // 7. Wake up any Pop() calls waiting for data
    f.cond.Broadcast()
    return nil
}
```

### Deduplication (Compression)

DeltaFIFO compresses consecutive deltas to avoid unbounded growth.

Rule: Only the LAST TWO deltas for a key are kept (with some nuance).

Why last two?
- Need to distinguish "Updated" from "Deleted then re-Added"
- Two deltas capture these cases

Example compression:

```
Before: [Added, Updated, Updated, Updated, Updated]
After:  [Updated, Updated]  (or sometimes just [Updated])
```

Special case - Deleted:
```
[Added, Updated, Deleted] → [Deleted]
(Once deleted, history doesn't matter)
```

### Popping From DeltaFIFO

When you call `Pop()`, you get ALL deltas for ONE object:

```go
func (f *DeltaFIFO) Pop(process PopProcessFunc) (interface{}, error) {
    f.lock.Lock()
    defer f.lock.Unlock()

    for len(f.queue) == 0 {
        f.cond.Wait()  // Block until something is enqueued
    }

    // 1. Get first key from queue (FIFO)
    key := f.queue[0]
    f.queue = f.queue[1:]

    // 2. Get all deltas for that key
    deltas := f.items[key]
    delete(f.items, key)

    // 3. Process the deltas (this calls your handler)
    err := process(deltas)

    // 4. If processing failed, can re-add to queue
    if err != nil {
        f.addIfNotPresent(key, deltas)
    }

    return deltas, err
}
```

The process function receives Deltas ([]Delta), usually:

```go
process := func(obj interface{}) error {
    deltas := obj.(Deltas)

    for _, delta := range deltas {
        switch delta.Type {
        case Added:
            // Handle add
            indexer.Add(delta.Object)
        case Updated:
            // Handle update
            indexer.Update(delta.Object)
        case Deleted:
            // Handle delete
            indexer.Delete(delta.Object)
        }
    }
    return nil
}
```

### The Full Flow

```
Watch Event                     DeltaFIFO                      Indexer
     │                              │                             │
     │  ADDED pod/nginx             │                             │
     │ ────────────────────────────►│                             │
     │                              │                             │
     │  MODIFIED pod/nginx          │ items["default/nginx"] =    │
     │ ────────────────────────────►│   [Added, Updated]          │
     │                              │                             │
     │  MODIFIED pod/nginx          │ items["default/nginx"] =    │
     │ ────────────────────────────►│   [Added, Updated, Updated] │
     │                              │   (may compress)            │
     │                              │                             │
     │                              │  Pop() called by controller │
     │                              │ ─────────────────────────►  │
     │                              │                             │
     │                              │  Returns: [Added, Updated]  │
     │                              │  for "default/nginx"        │
     │                              │                             │
     │                              │            indexer.Add(obj) │
     │                              │            indexer.Update(obj)
     │                              │                             │
     │                              │  Handler processes,         │
     │                              │  adds key to workqueue      │
     │                              │                             │
```

### Replace Operation (Re-List)

When Reflector does a full re-LIST (e.g., after reconnect), it calls:

```go
deltaFIFO.Replace(listOfAllObjects, resourceVersion)
```

This is special because:
1. It's not just one object, it's ALL objects
2. Need to detect deletions (objects in cache but not in list)

Replace algorithm:

```go
func (f *DeltaFIFO) Replace(list []interface{}, resourceVersion string) error {
    // 1. Create a set of all keys in the new list
    newKeys := set{}
    for _, obj := range list {
        key := keyOf(obj)
        newKeys.add(key)

        // 2. Queue a "Replaced" delta for each object
        f.queueActionLocked(Replaced, obj)
    }

    // 3. Find objects in our queue/items that are NOT in new list
    //    These were deleted while we were disconnected
    for key := range f.items {
        if !newKeys.has(key) {
            // Object was deleted!
            // Get last known state and queue a Delete delta
            deletedObj := f.knownObjects.GetByKey(key)
            f.queueActionLocked(Deleted, deletedObj)
        }
    }

    return nil
}
```

This is how DeltaFIFO detects deletions that happened while disconnected.

### Resync (Periodic Reconciliation)

Informers can be configured with a "resync period":

```go
informer := cache.NewSharedInformer(lw, &v1.Pod{}, 30*time.Second)
                                                   ^^^^^^^^^^^^
                                                   resync every 30s
```

During resync:
1. DeltaFIFO enqueues a "Sync" delta for EVERY object in the cache
2. This triggers your handler for all objects
3. Useful for: periodic health checks, correcting any missed state

```go
func (f *DeltaFIFO) Resync() error {
    // Get all objects from the Indexer (known state)
    keys := f.knownObjects.ListKeys()

    for _, key := range keys {
        obj, _ := f.knownObjects.GetByKey(key)
        // Queue a Sync delta
        f.queueActionLocked(Sync, obj)
    }
    return nil
}
```

### Thread Safety

DeltaFIFO is heavily concurrent:
- Multiple goroutines may Add/Update/Delete (from different informers)
- One or more goroutines call Pop()

It uses:
- `sync.Mutex` for protecting items/queue
- `sync.Cond` for signaling Pop() when data arrives

```go
type DeltaFIFO struct {
    lock sync.Mutex
    cond sync.Cond   // Initialized with cond.L = &lock

    items map[string]Deltas
    queue []string
}

// Pop blocks until data available
func (f *DeltaFIFO) Pop(...) {
    f.lock.Lock()
    defer f.lock.Unlock()

    for len(f.queue) == 0 {
        f.cond.Wait()  // Releases lock, waits, re-acquires lock
    }
    // ... process ...
}

// Add wakes up waiting Pop calls
func (f *DeltaFIFO) Add(...) {
    f.lock.Lock()
    defer f.lock.Unlock()

    // ... add to queue ...

    f.cond.Broadcast()  // Wake up all waiting Pop calls
}
```

### Why "Delta" and Why "FIFO"?

**DELTA**: Stores changes (deltas), not just current state. Allows handlers to know WHAT happened, not just current state.

**FIFO**: First-In-First-Out ordering by OBJECT KEY. If nginx changes, then redis changes, you process nginx first. But multiple changes to nginx are batched together.

It's NOT strictly FIFO for events - it's FIFO for object keys, with event batching per key.

### Comparison With Simple Queue

**Simple queue:**
```
[Event1, Event2, Event3, Event4, Event5]
- Process each event individually
- If same object updated 5 times, reconcile 5 times
```

**DeltaFIFO:**
```
{
    "nginx": [Delta1, Delta2, Delta3],  // Batched
    "redis": [Delta1]
}
queue: ["nginx", "redis"]
- Pop returns all deltas for one object
- Reconcile once with full context
```

---

## Key Takeaways

1. **INFORMERS**: Watch API server, cache locally, trigger handlers on changes. You read from local cache (fast), write to API server.

2. **KUBE-CONTROLLER-MANAGER**: A Kubernetes binary that runs all built-in controllers in one process. Part of the control plane.

3. **CONTROLLER-RUNTIME MANAGER**: A library construct that manages YOUR controllers. Provides shared cache, client, leader election, etc.

4. **CLIENT-GO**: Low-level Kubernetes client library. Provides informers, workqueue, clients, leader election primitives.

5. **CONTROLLER-RUNTIME**: High-level framework built ON TOP of client-go. Reduces boilerplate, provides nicer APIs, same underlying mechanisms.

6. **YOUR CONTROLLER**: Built with controller-runtime, which uses client-go, completely separate from kube-controller-manager but uses same patterns.
