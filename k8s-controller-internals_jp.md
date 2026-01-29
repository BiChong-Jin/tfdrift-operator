# Kubernetes Controller 内部構造の詳細解説

このドキュメントでは、3つのコアコンセプトについて説明します：
1. Informer/Cache メカニズム
2. Manager 抽象化（controller-runtime vs kube-controller-manager）
3. client-go と controller-runtime の関係

---

## パート 1: Informer/Cache メカニズム

### 問題: なぜ Informer が必要なのか？

Deployment を監視するコントローラーを書くことを想像してください。素朴なアプローチは次のようになります：

```go
for {
    deployments, _ := client.List(ctx, &appsv1.DeploymentList{})
    for _, d := range deployments.Items {
        // check if something needs to be done
    }
    time.Sleep(5 * time.Second)
}
```

このアプローチの問題点：
1. **コストが高い**: 毎回の List() が API サーバーに直接アクセスする
2. **遅い**: 5秒のポーリングは反応に5秒の遅延を意味する
3. **スケールしない**: 100のコントローラーがポーリング = API サーバーへの負荷が100倍
4. **イベントの取りこぼし**: ポーリングの間に起きたことを見逃す可能性がある

### 解決策: Watch + ローカルキャッシュ（Informer）

ポーリングの代わりに、Kubernetes は "watch" メカニズムを使用します：

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

watch はストリーミング HTTP 接続です。API サーバーはイベントが発生するとすぐにプッシュします。

### Informer アーキテクチャ

Informer は以下のコンポーネントで構成されています：

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

各コンポーネントを詳しく見ていきましょう：

1. **REFLECTOR**
   - API サーバーに接続する
   - 最初に LIST を実行して既存のすべてのオブジェクトを取得する
   - その後、変更を WATCH する
   - イベントを DeltaFIFO にプッシュする

2. **DELTAFIFO（キュー）**
   - 「デルタ」（変更）を格納するキュー
   - Delta = { Type: Added/Updated/Deleted, Object: リソース }
   - イベントが順序通りに処理されることを保証する
   - 重複排除: 同じオブジェクトが素早く5回更新された場合、最新の状態で1回だけ処理される可能性がある

3. **INDEXER / STORE（ローカルキャッシュ）**
   - 監視対象のすべてのリソースのインメモリコピー
   - 高速な検索のためにインデックス化されている（Namespace、名前、ラベルなど）
   - コントローラーは API サーバーからではなく、ここから読み取る

4. **LISTER**
   - ローカルキャッシュにクエリするためのインターフェース
   - `deploymentLister.Get("my-deployment")` はメモリから読み取り、API サーバーからではない
   - 高速: ミリ秒ではなくマイクロ秒

5. **EVENT HANDLERS**
   - コールバック: OnAdd, OnUpdate, OnDelete
   - 何かが変更されたときに呼び出される
   - 通常: オブジェクトのキーをワークキューに追加する

### Watch が HTTP レベルで実際にどう動作するか

```
GET /apis/apps/v1/deployments?watch=true

Response (chunked, never closes):
{"type":"ADDED","object":{"kind":"Deployment","metadata":{"name":"nginx",...}}}
{"type":"MODIFIED","object":{"kind":"Deployment","metadata":{"name":"nginx",...}}}
{"type":"DELETED","object":{"kind":"Deployment","metadata":{"name":"nginx",...}}}
... (connection stays open, events stream in)
```

接続が切断された場合、Reflector は "resourceVersion" を使用して再接続し、中断した箇所から再開します（イベントの取りこぼしなし）。

### Workqueue: なぜハンドラーで直接処理しないのか？

イベントハンドラーは高速であるべきです。作業をエンキューするだけにとどめます：

```go
OnUpdate(old, new) {
    key := namespace + "/" + name
    workqueue.Add(key)  // just add to queue, return immediately
}
```

なぜ別のキューが必要なのか？

1. **レート制限**: オブジェクトが毎秒100回更新されても、100回 reconcile しない。キューが重複を排除する
2. **リトライ**: reconcile が失敗した場合、バックオフ付きで再キューイングする
3. **複数ワーカー**: N個のゴルーチンがキューを処理できる
4. **疎結合**: ハンドラーは即座に返り、実際の処理は非同期で行われる

### 全体像

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

**重要なポイント**: 読み取りはローカル（キャッシュ）、書き込みは API サーバーへ。

### SharedInformerFactory

問題: 複数のコントローラーが同じリソースタイプを監視する場合がある。
- Deployment コントローラーが Deployment を監視する
- HPA コントローラーも Deployment を監視する
- 2つの別々の watch = 2倍の接続、同じデータに対して2倍のメモリ

解決策: SharedInformerFactory

```go
factory := informers.NewSharedInformerFactory(client, resyncPeriod)

// Both get the SAME underlying informer
deployInformer1 := factory.Apps().V1().Deployments()
deployInformer2 := factory.Apps().V1().Deployments()  // same instance!

factory.Start(stopCh)  // starts all informers once
```

1つの watch、1つのキャッシュ、複数のコンシューマー。

---

## パート 2: Manager 抽象化

「Manager」と呼ばれるものは2つあります：

1. **kube-controller-manager** - Kubernetes の コンポーネント（バイナリ/プロセス）
2. **controller-runtime Manager** - Go のライブラリ構造体

それぞれを理解しましょう：

### kube-controller-manager（コンポーネント）

これは Kubernetes コントロールプレーンの一部として動作する実際のバイナリです。

クラスター内の配置場所：

```
Control Plane Node
├── /etc/kubernetes/manifests/
│   ├── kube-apiserver.yaml        (static pod)
│   ├── kube-controller-manager.yaml (static pod)  ◄── これ
│   ├── kube-scheduler.yaml        (static pod)
│   └── etcd.yaml                  (static pod)
```

これは「Static Pod」です - kubelet がマニフェストファイルから直接実行し、API サーバーによって管理されません。

kube-controller-manager の内部：

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
│  ... その他約30のコントローラー ...                              │
│                                                                 │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │           Shared Informer Factory                        │   │
│  │  （上記すべてのコントローラーで共有される1つのキャッシュ）    │   │
│  └─────────────────────────────────────────────────────────┘   │
│                                                                 │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │           Leader Election（プロセス全体で1つ）            │   │
│  └─────────────────────────────────────────────────────────┘   │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

主な特徴：
- 多数のコントローラーを持つ1つのプロセス
- すべてのコントローラーに対して1つの Leader Election
- 1つの SharedInformerFactory
- Kubernetes 自体の一部（kubernetes/kubernetes リポジトリ内）
- Kubernetes のリリースサイクルに密結合

### controller-runtime Manager（ライブラリ）

これはカスタムコントローラーを構築するためにあなたが使用する Go の構造体/インターフェースです。

```go
import ctrl "sigs.k8s.io/controller-runtime"

mgr, err := ctrl.NewManager(config, ctrl.Options{
    // your options
})
```

Manager が提供するもの：

```
┌─────────────────────────────────────────────────────────────────┐
│              controller-runtime Manager                         │
│                                                                 │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │  Cluster     - API サーバーへの接続                       │   │
│  │  Client      - Kubernetes リソースの読み書き              │   │
│  │  Cache       - 共有 Informer キャッシュ（Factory と同様）  │   │
│  │  Scheme      - 型レジストリ（どの Go 型が存在するか）      │   │
│  └─────────────────────────────────────────────────────────┘   │
│                                                                 │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │  Leader Election  - 高可用性サポート                      │   │
│  │  Health Probes    - /healthz, /readyz                    │   │
│  │  Metrics Server   - Prometheus メトリクス                 │   │
│  │  Webhook Server   - Admission Webhook                    │   │
│  └─────────────────────────────────────────────────────────┘   │
│                                                                 │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │  Runnables   - Manager が起動/停止するもの                │   │
│  │    └── Your Controllers                                  │   │
│  │    └── Your Webhooks                                     │   │
│  │    └── Custom runnables                                  │   │
│  └─────────────────────────────────────────────────────────┘   │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

Manager は以下を行う便利なラッパーです：
1. 共有キャッシュを作成・管理する
2. キャッシュから読み取り、API サーバーに書き込むクライアントを作成する
3. Leader Election を処理する
4. コントローラーのライフサイクルを管理する（起動/停止）
5. ヘルスエンドポイントを提供する

### 比較

| kube-controller-manager | controller-runtime Manager |
|-------------------------|---------------------------|
| バイナリ/プロセス | コード内の Go 構造体 |
| 約40のコントローラーを実行 | あなたのコントローラー（1つ、2つ、またはそれ以上）を実行 |
| コントロールプレーンの一部 | 通常のワークロードとして実行 |
| クラスターごとに1つ | オペレーターのデプロイメントごとに1つ |
| client-go を直接使用 | controller-runtime の抽象化を使用 |
| Kubernetes のリリースサイクル | あなたのリリースサイクル |

### main.go で何が起きるか

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

`mgr.Start()` が呼び出されると：
1. Leader Election を待機する（有効な場合）
2. すべての Informer を起動する（cache.Start）
3. キャッシュの同期を待機する
4. すべてのコントローラーを起動する
5. ヘルス/メトリクスサーバーを起動する
6. コンテキストがキャンセルされるまでブロックする

---

## パート 3: client-go と controller-runtime の関係

### レイヤー図

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

### なぜ controller-runtime が存在するのか？

生の client-go でコントローラーを書くのは冗長です。以下に比較を示します：

**CLIENT-GO のみの場合**（組み込みコントローラーが行うこと）：

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

**CONTROLLER-RUNTIME の場合**（あなたが行うこと）：

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

controller-runtime はボイラープレートを約200行から約20行に削減します！

### controller-runtime が抽象化するもの

**1. RECONCILER インターフェース**

- client-go: ワークキュー、キーの処理、リトライを手動で管理する
- controller-runtime: `Reconcile(ctx, Request) (Result, error)` を実装するだけ

Result はフレームワークに何をすべきか伝えます：
- `Result{}`: 成功、再キューイングしない
- `Result{Requeue: true}`: 即座に再キューイング
- `Result{RequeueAfter: 5*time.Minute}`: 指定時間後に再キューイング
- `error`: バックオフ付きで再キューイング

**2. Builder パターン**

- client-go: Informer、ハンドラー、Predicate を手動で配線する
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

controller-runtime の client は：
- あらゆる型に対応（コアリソース、カスタム CRD、非構造化データ）
- デフォルトでキャッシュから読み取る（高速）
- API サーバーに直接書き込む
- Scheme を使用して型を理解する

**4. CACHE**

- client-go: SharedInformerFactory、手動での Informer 管理
- controller-runtime: Cache インターフェース、自動的な Informer 作成

`client.Get()` や `client.List()` を呼び出すと、キャッシュから読み取ります。
`client.Create/Update/Delete/Patch()` を呼び出すと、API サーバーにアクセスします。

### controller-runtime が client-go から使用するもの

controller-runtime は client-go を置き換えるのではなく、使用します：

| controller-runtime コンポーネント | client-go から使用するもの |
|------------------------------|---------------------|
| Manager | Leader Election |
| Cache | SharedInformerFactory |
| Client（書き込み） | REST client |
| Client（読み取り） | Informer の Lister |
| Controller | Workqueue |
| Source | Informer Event Handlers |

内部的には、以下を実行すると：

```go
client.Get(ctx, key, &deployment)
```

controller-runtime は：
1. キャッシュ内で Deployment 型の Informer を検索する
2. ローカルキャッシュから取得するために Lister（client-go 由来）を呼び出す
3. 結果を返す

以下を実行すると：

```go
client.Update(ctx, &deployment)
```

controller-runtime は：
1. REST client（client-go 由来）を使用する
2. API サーバーに PUT リクエストを送信する
3. 結果を返す

### まとめの図

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

## パート 4: HTTP ストリーミング Watch - 内部の仕組み

このセクションでは、"watch" メカニズムが HTTP レベルでどのように動作するかを正確に説明します。

### 基本概念

watch は、（閉じるまで）終わることのない HTTP GET リクエストです。

**通常の HTTP リクエスト：**
```
Client: GET /api/v1/pods
Server: 200 OK, here's all pods (JSON array), CONNECTION CLOSED
```

**Watch HTTP リクエスト：**
```
Client: GET /api/v1/pods?watch=true
Server: 200 OK, here's events... (connection stays open forever)
        {"type":"ADDED","object":{...}}
        {"type":"MODIFIED","object":{...}}
        ... (keeps streaming)
```

### HTTP チャンク転送エンコーディング

watch は HTTP/1.1 の「チャンク転送エンコーディング」を使用します：

```
GET /api/v1/namespaces/default/pods?watch=true HTTP/1.1
Host: kubernetes.default.svc
Authorization: Bearer <token>

HTTP/1.1 200 OK
Content-Type: application/json
Transfer-Encoding: chunked      ◄── 重要: Content-Length ではなく chunked

{"type":"ADDED","object":{"kind":"Pod","metadata":{"name":"nginx",...}}}
{"type":"MODIFIED","object":{"kind":"Pod","metadata":{"name":"nginx",...}}}
{"type":"DELETED","object":{"kind":"Pod","metadata":{"name":"nginx",...}}}
... (stream continues)
```

チャンクエンコーディングでは：
- サーバーは Content-Length を事前に宣言しない
- サーバーはイベントが発生するたびにデータを「チャンク」で送信する
- 接続は無期限に開いたまま
- クライアントは行ごとに読み取る（各行 = 1つの JSON イベント）

### Watch イベントフォーマット

ストリーム内の各行は WatchEvent JSON オブジェクトです：

```json
{
    "type": "ADDED" | "MODIFIED" | "DELETED" | "BOOKMARK" | "ERROR",
    "object": {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {
            "name": "nginx",
            "namespace": "default",
            "resourceVersion": "12345",  // ◄── 再開に不可欠
            ...
        },
        "spec": {...},
        "status": {...}
    }
}
```

イベントタイプ：
- **ADDED**: 新しいリソースが作成された（または watch 開始時に存在していた）
- **MODIFIED**: リソースが更新された
- **DELETED**: リソースが削除された
- **BOOKMARK**: 定期的なチェックポイント（resourceVersion のみ、完全なオブジェクトなし）
- **ERROR**: 何か問題が発生した（通常は再接続が必要）

### Resource Version - マジックナンバー

すべての Kubernetes オブジェクトは resourceVersion を持っています（etcd のリビジョン番号に由来）。

```yaml
metadata:
  name: nginx
  resourceVersion: "12345"   # ◄── 更新のたびに変わる
```

watch は resourceVersion を2つの目的で使用します：

**1. 開始点**: どこから監視を開始するか

```
# 現在から監視（将来のイベントのみ）
GET /api/v1/pods?watch=true

# 特定のポイントから監視（バージョン 10000 以降のイベントを再生）
GET /api/v1/pods?watch=true&resourceVersion=10000
```

**2. 再開**: 接続が切断された場合、イベントを見逃さずに再接続

```
# 元の watch は resourceVersion 12345 の時点で切断された
# そこから続行して再接続
GET /api/v1/pods?watch=true&resourceVersion=12345
```

### Watch 中に何が起きるか（シーケンス図）

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
      │         ... 時間が経過 ...                │
      │                                          │
      │  {"type":"MODIFIED","object":{pod1,rv:150}}
      │ ◄──────────────────────────────────────── │
      │                                          │
      │  {"type":"BOOKMARK","object":{rv:200}}   │  (定期的)
      │ ◄──────────────────────────────────────── │
      │                                          │
      │         ... 接続が切断 ...               │
      │               X                          │
      │                                          │
      │  GET /api/v1/pods?watch=true             │
      │  &resourceVersion=200                    │  (BOOKMARK から再開)
      │ ────────────────────────────────────────► │
      │                                          │
      │  {"type":"MODIFIED","object":{pod2,rv:201}}
      │ ◄──────────────────────────────────────── │
      │                                          │
```

### Bookmark - 「古すぎる」エラーの防止

問題: 非常に古い resourceVersion で再接続すると、etcd がその履歴をコンパクション（削除）している可能性があります。その場合、次のエラーが返されます：

```json
{"type":"ERROR","object":{"message":"resourceVersion too old"}}
```

解決策: BOOKMARK イベント

```json
{"type":"BOOKMARK","object":{"metadata":{"resourceVersion":"15000"}}}
```

BOOKMARK は定期的に送信されます（約1分ごと）。以下を含みます：
- resourceVersion のみ（完全なオブジェクトなし - 帯域幅を節約）
- 再開に使用できるチェックポイント

Reflector はあらゆるイベント（BOOKMARK を含む）から最新の resourceVersion を保存するため、再接続時には最近のバージョンを使用します。

### List Then Watch パターン

Reflector は単に watch するだけではありません。まず LIST を行い、その後 WATCH します：

1. 既存のすべてのリソースを LIST する
   ```
   GET /api/v1/pods
   Response: {"items": [...], "metadata": {"resourceVersion": "5000"}}
   ```

2. その resourceVersion から WATCH を開始する
   ```
   GET /api/v1/pods?watch=true&resourceVersion=5000
   ```

なぜ最初に LIST するのか？
- watch は変更のみを提供し、既存の状態は提供しない
- LIST は現在の状態 + watch を開始するための resourceVersion を提供する
- 合わせて: 過去と未来の完全な全体像

これを「List-Watch」パターンと呼びます。

### 実際のワイヤー上の HTTP バイト列

生の HTTP は以下のようになります（簡略化）：

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

各 JSON オブジェクトはそれぞれの行にあります（改行区切り JSON、別名 NDJSON）。

### タイムアウトとキープアライブ

watch は永遠に続くわけではありません。API サーバーにはタイムアウトがあります：

```
GET /api/v1/pods?watch=true&timeoutSeconds=300
```

- デフォルトのタイムアウト: 約5〜10分（サーバー設定による）
- タイムアウト後: サーバーは接続を正常に閉じる
- Reflector: 最新の resourceVersion で自動的に再接続する

また、TCP キープアライブパケットにより、ネットワーク機器が「アイドル」な接続を切断するのを防ぎます（データが断続的に流れていても）。

### ストリーミングの実装方法（サーバー側 + クライアント側のコード）

通常の HTTP: サーバーがレスポンスを書き込み、接続を閉じる。完了。
Watch HTTP: サーバーが書き込み、待機し、さらに書き込み、待機し、さらに書き込む... 決して閉じない。

どうやって？ 2つの重要なテクニック：

1. **サーバー側**: 各イベントの後にフラッシュする（バッファリングしない）
2. **クライアント側**: ループ内で行ごとに読み取る（EOF を待たない）

#### サーバー側（API Server）

**通常のハンドラー：**

```go
func normalHandler(w http.ResponseWriter, r *http.Request) {
    pods := getAllPods()
    json.NewEncoder(w).Encode(pods)
    // Function returns → Go's HTTP server closes connection
}
```

**Watch ハンドラー（簡略化）：**

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

主な違い：
- `http.Flusher` を使用してデータを即座にプッシュする
- 返り値を返す代わりに永久にループする
- 一度にすべてではなく、インクリメンタルに書き込む

`http.Flusher` とは何か？

```go
type Flusher interface {
    Flush()  // Send buffered data to client NOW
}
```

`Flush()` がなければ、Go の HTTP サーバーは効率のために書き込みをバッファリングします。
`Flush()` があれば、各イベントは即座にクライアントに送信されます。

#### クライアント側（client-go の Reflector）

**通常のクライアント：**

```go
func normalRequest() {
    resp, _ := http.Get("http://api/pods")
    body, _ := ioutil.ReadAll(resp.Body)  // Waits for ENTIRE response
    resp.Body.Close()
}
```

**Watch クライアント（簡略化）：**

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

重要な洞察: `reader.ReadBytes('\n')` は1行が利用可能になるとすぐに返します。接続が閉じるのを待ちません。

#### 実際の client-go コード

`k8s.io/apimachinery/pkg/watch/streamwatcher.go` 内：

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

そして Reflector がこれを消費します：

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

#### 図解: 通常リクエスト vs ストリーミング

**通常のリクエスト：**

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

**ストリーミング Watch：**

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
  │     (クライアントがイベントを処理)    │
  │                                    │
  │         ... 時間が経過 ...          │
  │                                    │
  │  {"type":"MODIFIED",...}\n         │
  │ ◄──────────────────────────────────│ flusher.Flush()
  │                                    │
  │     (クライアントがイベントを処理)    │
  │                                    │
  │         ... 時間が経過 ...          │
  │                                    │
  │  {"type":"ADDED",...}\n            │  (新しい Pod が作成された)
  │ ◄──────────────────────────────────│ flusher.Flush()
  │                                    │
  │     ... 永遠に続く ...              │
```

#### TCP レベル: なぜこれが動作するのか

TCP レベルでは、「完全なレスポンス」というものは存在しません。TCP は単なるバイトストリームです。

HTTP がセマンティクスを追加します：
- `Content-Length`: 「正確に N バイト送信し、その後完了」
- `Transfer-Encoding: chunked`: 「チャンクを送信し、各チャンクにサイズをプレフィックスとして付ける」
- チャンクターミネーター: ゼロ長のチャンクは「完了」を意味する

watch の場合：
- サーバーはチャンクを送信するが、ゼロ長のターミネーターは決して送信しない
- TCP 接続は開いたまま
- 各 `flusher.Flush()` が現在のバッファをチャンクとしてプッシュする
- クライアントはチャンクが到着するたびに読み取る

### エラーハンドリング

何がうまくいかなくなる可能性があるか：

1. **CONNECTION RESET**
   - ネットワークの瞬断、Pod の再起動など
   - Reflector: 最後に知られている resourceVersion で再接続する

2. **410 GONE（resourceVersion が古すぎる）**
   - etcd が履歴をコンパクションした
   - Reflector: 新しい LIST を実行し、新しい resourceVersion から WATCH する

3. **500 INTERNAL SERVER ERROR**
   - API サーバーに問題がある
   - Reflector: バックオフしてリトライする

4. **WATCH チャンネルが閉じられた**
   - サーバーが閉じることを決定した（タイムアウト、リソース圧迫）
   - Reflector: 再接続する

Reflector は指数バックオフでこれらすべてを自動的に処理します。

---

## パート 5: DeltaFIFO - 内部の仕組み

DeltaFIFO はオブジェクトの変更（デルタ）を格納するキューです。Reflector（イベントを受信する）と Indexer（ローカルキャッシュ）の橋渡しをします。

### なぜ DeltaFIFO が存在するのか

問題: 生の watch イベントは処理に最適ではありません。

シナリオ: Pod "nginx" が1秒間に10回更新される。

**DeltaFIFO なしの場合：**
- 10個のイベントがハンドラーに到達する
- 10回の reconcile 呼び出し
- 無駄が多い、特に最終状態だけが重要な場合

**DeltaFIFO ありの場合：**
- 10個のイベントがキュー内で圧縮される
- Pop すると最新の状態が得られる
- 履歴が必要な場合はすべてのデルタを確認できる

### Delta 型

Delta は単一の変更を表します：

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

### DeltaFIFO データ構造

DeltaFIFO は本質的に以下のようなものです：

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

視覚的な表現：

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

**重要なポイント**: 同じオブジェクトに対して複数のデルタが蓄積される可能性があります。

### イベントが DeltaFIFO にどのように流れるか

Reflector が watch イベントを受信したとき：

```
Reflector                              DeltaFIFO
    │                                      │
    │  obj = Pod{name: "nginx"}            │
    │  event = MODIFIED                    │
    │                                      │
    │  Add(obj)  or  Update(obj)           │
    │ ────────────────────────────────────►│
    │                                      │
    │                           1. キーを計算: "default/nginx"
    │                           2. Delta{Updated, obj} を作成
    │                           3. items["default/nginx"] に追加
    │                           4. キーがキューになければ、キューに追加
    │                           5. 待機中のコンシューマーにシグナルを送信
    │                                      │
```

DeltaFIFO の Add/Update/Delete メソッド：

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

### 重複排除（圧縮）

DeltaFIFO は無制限の成長を避けるために、連続するデルタを圧縮します。

ルール: 1つのキーに対して最後の2つのデルタのみが保持されます（いくつかのニュアンスあり）。

なぜ最後の2つなのか？
- 「Updated」と「Deleted してから再度 Added」を区別する必要がある
- 2つのデルタでこれらのケースをカバーできる

圧縮の例：

```
Before: [Added, Updated, Updated, Updated, Updated]
After:  [Updated, Updated]  (or sometimes just [Updated])
```

特殊なケース - Deleted:
```
[Added, Updated, Deleted] → [Deleted]
(削除された以上、履歴は関係ない)
```

### DeltaFIFO からの Pop

`Pop()` を呼び出すと、1つのオブジェクトのすべてのデルタを取得します：

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

process 関数は Deltas ([]Delta) を受け取り、通常は以下のようになります：

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

### 全体のフロー

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
     │                              │   (圧縮される可能性あり)     │
     │                              │                             │
     │                              │  コントローラーが Pop() 呼出  │
     │                              │ ─────────────────────────►  │
     │                              │                             │
     │                              │  戻り値: [Added, Updated]   │
     │                              │  "default/nginx" に対して   │
     │                              │                             │
     │                              │            indexer.Add(obj) │
     │                              │            indexer.Update(obj)
     │                              │                             │
     │                              │  ハンドラーが処理し、        │
     │                              │  キーをワークキューに追加    │
     │                              │                             │
```

### Replace 操作（再 LIST）

Reflector が完全な再 LIST を行うとき（例: 再接続後）、以下を呼び出します：

```go
deltaFIFO.Replace(listOfAllObjects, resourceVersion)
```

これが特別な理由：
1. 1つのオブジェクトではなく、すべてのオブジェクトである
2. 削除の検出が必要（キャッシュにはあるがリストにないオブジェクト）

Replace アルゴリズム：

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

これが、切断中に発生した削除を DeltaFIFO が検出する方法です。

### Resync（定期的な再調整）

Informer は「resync 期間」を設定できます：

```go
informer := cache.NewSharedInformer(lw, &v1.Pod{}, 30*time.Second)
                                                   ^^^^^^^^^^^^
                                                   30秒ごとに resync
```

resync 中：
1. DeltaFIFO がキャッシュ内のすべてのオブジェクトに対して「Sync」デルタをエンキューする
2. これによりすべてのオブジェクトに対してハンドラーがトリガーされる
3. 用途: 定期的なヘルスチェック、見逃した状態の修正

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

### スレッドセーフティ

DeltaFIFO は高度に並行処理されます：
- 複数のゴルーチンが Add/Update/Delete を実行する可能性がある（異なる Informer から）
- 1つ以上のゴルーチンが Pop() を呼び出す

以下を使用します：
- `sync.Mutex` で items/queue を保護する
- `sync.Cond` でデータが到着したときに Pop() にシグナルを送る

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

### なぜ「Delta」でなぜ「FIFO」なのか？

**DELTA**: 現在の状態だけでなく、変更（デルタ）を格納します。ハンドラーが現在の状態だけでなく、何が起きたかを知ることができます。

**FIFO**: オブジェクトキーによる先入れ先出し順序。nginx が変更され、その後 redis が変更された場合、nginx が先に処理されます。ただし、nginx に対する複数の変更はバッチ処理されます。

これはイベントに対して厳密な FIFO ではありません - オブジェクトキーに対する FIFO であり、キーごとにイベントがバッチ処理されます。

### シンプルなキューとの比較

**シンプルなキュー：**
```
[Event1, Event2, Event3, Event4, Event5]
- 各イベントを個別に処理する
- 同じオブジェクトが5回更新されたら、5回 reconcile する
```

**DeltaFIFO：**
```
{
    "nginx": [Delta1, Delta2, Delta3],  // バッチ処理
    "redis": [Delta1]
}
queue: ["nginx", "redis"]
- Pop は1つのオブジェクトのすべてのデルタを返す
- 完全なコンテキストで1回 reconcile する
```

---

## 重要なポイントまとめ

1. **INFORMERS**: API サーバーを監視し、ローカルにキャッシュし、変更時にハンドラーをトリガーする。ローカルキャッシュから読み取り（高速）、API サーバーに書き込む。

2. **KUBE-CONTROLLER-MANAGER**: すべての組み込みコントローラーを1つのプロセスで実行する Kubernetes バイナリ。コントロールプレーンの一部。

3. **CONTROLLER-RUNTIME MANAGER**: あなたのコントローラーを管理するライブラリ構造体。共有キャッシュ、クライアント、Leader Election などを提供する。

4. **CLIENT-GO**: 低レベルの Kubernetes クライアントライブラリ。Informer、Workqueue、クライアント、Leader Election プリミティブを提供する。

5. **CONTROLLER-RUNTIME**: client-go の上に構築された高レベルフレームワーク。ボイラープレートを削減し、より良い API を提供するが、基盤となるメカニズムは同じ。

6. **あなたのコントローラー**: controller-runtime で構築され、client-go を使用する。kube-controller-manager とは完全に別だが、同じパターンを使用する。
