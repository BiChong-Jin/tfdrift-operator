# tfdrift-operator: Interview Q&A / 面接 Q&A

---

## 1. Project Overview / プロジェクト概要

**Q: Can you explain what this project does and what problem it solves?**

This is a Kubernetes operator that automatically detects **Terraform drift** — when the live state of Kubernetes resources (Deployments, Services) diverges from the expected state defined in Terraform. It computes a SHA256 hash of the live resource spec and compares it against a baseline hash stored as an annotation. When drift is detected, it emits a Kubernetes Warning event and updates annotations for audit purposes. It's a non-invasive, event-driven solution that integrates naturally with the Kubernetes ecosystem.

**Q: このプロジェクトは何をするもので、どのような問題を解決しますか？**

これは、Kubernetesリソース（Deployment、Service）のライブ状態がTerraformで定義された期待状態から乖離する **Terraformドリフト** を自動検出するKubernetesオペレーターです。ライブリソースのspecのSHA256ハッシュを計算し、アノテーションに保存されたベースラインハッシュと比較します。ドリフトが検出されると、KubernetesのWarningイベントを発行し、監査用のアノテーションを更新します。Kubernetesエコシステムに自然に統合される、非侵入型のイベント駆動ソリューションです。

---

## 2. Architecture Decisions / アーキテクチャの意思決定

**Q: Why did you use annotations instead of Custom Resource Definitions (CRDs)?**

I chose annotations over CRDs to keep the operator lightweight and simple. CRDs introduce schema management overhead, versioning complexity, and additional RBAC requirements. Since the operator only needs to store a few string values (hashes, timestamps, boolean flags), annotations are the perfect fit. This also means users don't need to install any CRDs — the operator works directly with existing Deployments and Services. The trade-off is that annotations lack schema validation, but the operator itself controls the annotation lifecycle, so this is acceptable.

**Q: なぜCRD（Custom Resource Definition）ではなくアノテーションを使ったのですか？**

オペレーターを軽量かつシンプルに保つために、CRDではなくアノテーションを選択しました。CRDはスキーマ管理のオーバーヘッド、バージョニングの複雑さ、追加のRBAC要件を導入します。オペレーターはいくつかの文字列値（ハッシュ、タイムスタンプ、ブールフラグ）を保存するだけなので、アノテーションが最適です。また、ユーザーはCRDのインストールが不要で、既存のDeploymentやServiceに直接作用します。トレードオフとしてスキーマバリデーションがありませんが、オペレーター自身がアノテーションのライフサイクルを制御するため、許容範囲です。

---

## 3. Reconciliation Loop / Reconciliationループ

**Q: Walk me through the reconciliation flow. What happens when a watched resource changes?**

1. The controller receives a reconciliation request triggered by a Kubernetes watch event.
2. It fetches the resource and checks for the `tfdrift.jin.dev/enabled=true` label — if absent, it skips the resource.
3. It reads the expected baseline hash from the `tfdrift.jin.dev/spec-hash` annotation.
4. It computes the live hash by extracting key spec fields, canonicalizing them (sorting containers by name, env vars by name, ports by number), JSON marshaling, then SHA256 hashing.
5. It compares the two hashes:
   - **Match** → sets `drifted=false`
   - **Mismatch** → sets `drifted=true`, records `drifted-at` timestamp, emits a Warning event
6. Updates annotations via a merge patch (non-destructive to other annotations).

**Q: Reconciliationフローを説明してください。監視対象のリソースが変更されると何が起こりますか？**

1. Kubernetesのwatchイベントによりコントローラーがreconciliationリクエストを受け取ります。
2. リソースを取得し、`tfdrift.jin.dev/enabled=true`ラベルを確認します。存在しなければスキップします。
3. `tfdrift.jin.dev/spec-hash`アノテーションから期待されるベースラインハッシュを読み取ります。
4. 主要なspecフィールドを抽出し、正規化（コンテナ名でソート、環境変数名でソート、ポート番号でソート）、JSONマーシャル、SHA256ハッシュ化してライブハッシュを計算します。
5. 2つのハッシュを比較します：
   - **一致** → `drifted=false`に設定
   - **不一致** → `drifted=true`に設定、`drifted-at`タイムスタンプを記録、Warningイベントを発行
6. マージパッチでアノテーションを更新します（他のアノテーションに影響しません）。

---

## 4. Deterministic Hashing / 決定論的ハッシュ

**Q: How do you ensure the hash is deterministic? What challenges did you face?**

Deterministic hashing was one of the trickiest parts. Kubernetes resources contain many auto-generated and volatile fields. My approach:

- **Field selection**: Only hash fields that represent intended state (replicas, containers, images, env vars, ports, resource limits) — not status, metadata timestamps, or auto-injected fields.
- **Canonicalization**: Sort all lists (containers by name, env vars by name, ports by port number then name) and trim whitespace in map values. This prevents ordering differences from causing false positives.
- **Structured fingerprints**: Define Go structs (`DeploymentFingerprint`, `ContainerFingerprint`, etc.) that capture exactly the fields to compare, then JSON marshal them for stable serialization.
- **Deliberate exclusions**: Ignore `ValueFrom` in env vars since it references dynamic sources that change without spec changes.

**Q: ハッシュの決定性をどのように保証していますか？どのような課題がありましたか？**

決定論的ハッシュは最も難しい部分の一つでした。Kubernetesリソースには自動生成される揮発性フィールドが多く含まれています。私のアプローチ：

- **フィールド選択**: 意図された状態を表すフィールドのみハッシュ化（replicas、containers、images、env vars、ports、resource limits）。status、metadataのタイムスタンプ、自動注入フィールドは除外。
- **正規化**: すべてのリストをソート（コンテナ名順、環境変数名順、ポート番号→名前順）し、map値の空白を除去。順序の違いによる偽陽性を防止。
- **構造化フィンガープリント**: Go構造体（`DeploymentFingerprint`、`ContainerFingerprint`など）で比較対象フィールドを厳密に定義し、安定したシリアライズのためJSONマーシャル。
- **意図的な除外**: `ValueFrom`はspec変更なしに変化する動的ソースを参照するため、env varsで無視。

---

## 5. Why Kubebuilder? / なぜKubebuilder？

**Q: Why did you choose Kubebuilder over alternatives like Operator SDK or writing a raw controller?**

Kubebuilder provides a standardized project layout, built-in scaffolding for controllers, RBAC generation, and tight integration with controller-runtime. It follows upstream Kubernetes conventions. Compared to Operator SDK, Kubebuilder is more lightweight and doesn't add another abstraction layer (Operator SDK is actually built on top of Kubebuilder). Compared to writing a raw controller, Kubebuilder handles all the boilerplate — manager setup, signal handling, leader election, health probes, metrics — letting me focus on the business logic.

**Q: なぜOperator SDKや素のコントローラーではなくKubebuilderを選んだのですか？**

Kubebuilderは標準化されたプロジェクトレイアウト、コントローラーの組み込みスキャフォールディング、RBAC生成、controller-runtimeとの密接な統合を提供します。アップストリームのKubernetes規約に従っています。Operator SDKと比較すると、Kubebuilderはより軽量で追加の抽象化レイヤーがありません（Operator SDKは実際にはKubebuilderの上に構築されています）。素のコントローラーと比較すると、Kubebuilderはすべてのボイラープレート（マネージャー設定、シグナル処理、リーダー選出、ヘルスプローブ、メトリクス）を処理し、ビジネスロジックに集中できます。

---

## 6. Security / セキュリティ

**Q: What security measures did you implement?**

Multiple layers of defense:

- **Distroless base image**: No shell, package manager, or unnecessary tools — reduces attack surface.
- **Non-root execution**: Container runs as UID 65532, never as root.
- **Read-only filesystem**: `readOnlyRootFilesystem: true` prevents runtime modifications.
- **Capability drop**: All Linux capabilities are dropped (`capabilities.drop: ALL`).
- **Seccomp profile**: `RuntimeDefault` seccomp profile restricts syscalls.
- **Least-privilege RBAC**: Only `get`, `list`, `watch` permissions on Deployments and Services — no `update` or `delete` on core resources.
- **HTTP/2 disabled**: Prevents HTTP/2-specific vulnerabilities on metrics/webhook servers.
- **Leader election**: Prevents duplicate processing in HA setups.

**Q: どのようなセキュリティ対策を実装しましたか？**

多層防御を実施しています：

- **Distrolessベースイメージ**: シェル、パッケージマネージャー、不要なツールなし。攻撃対象面を削減。
- **非root実行**: コンテナはUID 65532で実行。rootでは実行しない。
- **読み取り専用ファイルシステム**: `readOnlyRootFilesystem: true`でランタイム変更を防止。
- **ケーパビリティ削除**: すべてのLinuxケーパビリティを削除（`capabilities.drop: ALL`）。
- **Seccompプロファイル**: `RuntimeDefault`でsyscallを制限。
- **最小権限RBAC**: DeploymentとServiceに対する`get`、`list`、`watch`権限のみ。コアリソースへの`update`や`delete`はなし。
- **HTTP/2無効化**: メトリクス/webhookサーバーでのHTTP/2固有の脆弱性を防止。
- **リーダー選出**: HA構成での重複処理を防止。

---

## 7. Testing Strategy / テスト戦略

**Q: How did you approach testing for this operator?**

I used a two-tier testing strategy:

- **Unit tests**: Using Ginkgo/Gomega with `envtest`, which spins up a real Kubernetes API server locally. This gives realistic behavior without needing a full cluster. Tests verify reconciliation logic, hash computation, and annotation updates.
- **E2E tests**: Using Kind (Kubernetes-in-Docker) to create a real cluster, build and load the operator image, deploy it, and verify end-to-end behavior including pod readiness and metrics endpoint availability.

If I had more time, I would expand unit test coverage for edge cases like missing annotations, label removal mid-operation, and hash collision scenarios.

**Q: このオペレーターのテストにはどのようなアプローチを取りましたか？**

2層のテスト戦略を採用しました：

- **ユニットテスト**: `envtest`を使ったGinkgo/Gomegaで、ローカルにリアルなKubernetes APIサーバーを起動。完全なクラスターなしでリアルな動作を確認。Reconciliationロジック、ハッシュ計算、アノテーション更新を検証。
- **E2Eテスト**: Kind（Kubernetes-in-Docker）でリアルなクラスターを作成し、オペレーターイメージをビルド・ロードしてデプロイし、Pod準備状態やメトリクスエンドポイントの可用性を含むエンドツーエンドの動作を検証。

時間があれば、アノテーション欠落、操作中のラベル削除、ハッシュ衝突シナリオなどのエッジケースのユニットテストカバレッジを拡大したいです。

---

## 8. Scalability & Performance / スケーラビリティとパフォーマンス

**Q: How does this operator scale? What happens with thousands of resources?**

The operator leverages controller-runtime's **shared informer cache**, which means it maintains an in-memory cache of watched resources rather than querying the API server on every reconciliation. It also uses **label-based filtering** (`tfdrift.jin.dev/enabled=true`), so only opted-in resources trigger reconciliation. The SHA256 computation and JSON marshaling are lightweight operations. For HA, leader election ensures only one instance reconciles at a time, preventing thundering herd issues. If needed, we could add rate limiting or work queue tuning.

**Q: このオペレーターはどのようにスケールしますか？数千のリソースがある場合はどうなりますか？**

オペレーターはcontroller-runtimeの**共有インフォーマーキャッシュ**を活用しており、ReconciliationのたびにAPIサーバーにクエリするのではなく、監視対象リソースのインメモリキャッシュを維持します。また、**ラベルベースのフィルタリング**（`tfdrift.jin.dev/enabled=true`）を使用し、オプトインされたリソースのみがReconciliationをトリガーします。SHA256計算とJSONマーシャルは軽量な操作です。HAのために、リーダー選出により1つのインスタンスのみがReconciliationを実行し、サンダリングハード問題を防止します。必要に応じて、レート制限やワークキューのチューニングを追加できます。

---

## 9. Limitations & Future Improvements / 制限事項と今後の改善

**Q: What are the current limitations and how would you improve this project?**

Current limitations:
- **Resource scope**: Only supports Deployments and Services. Should extend to StatefulSets, DaemonSets, ConfigMaps, etc.
- **No scheduled reconciliation**: Only triggers on resource changes. A periodic recheck would catch edge cases.
- **External hash dependency**: The baseline hash must be set externally (e.g., by a Terraform provider or CI pipeline). A Terraform provider plugin or webhook that auto-computes the hash on apply would improve UX.
- **ValueFrom exclusion**: Env vars using `valueFrom` are ignored, which could miss some drift.
- **Alerting**: Currently only Kubernetes events. Integration with Slack, PagerDuty, or Prometheus alerts would be valuable.

**Q: 現在の制限事項と、このプロジェクトをどのように改善しますか？**

現在の制限事項：
- **リソーススコープ**: DeploymentとServiceのみ対応。StatefulSet、DaemonSet、ConfigMapなどに拡張すべき。
- **スケジュールReconciliationなし**: リソース変更時のみトリガー。定期的な再チェックでエッジケースをキャッチ可能。
- **外部ハッシュ依存**: ベースラインハッシュは外部（TerraformプロバイダーやCIパイプライン）で設定が必要。apply時にハッシュを自動計算するTerraformプロバイダープラグインやwebhookがUXを改善。
- **ValueFromの除外**: `valueFrom`を使用する環境変数は無視されるため、一部のドリフトを見逃す可能性。
- **アラート**: 現在はKubernetesイベントのみ。Slack、PagerDuty、Prometheusアラートとの統合が有用。

---

## 10. Kubernetes Internals / Kubernetes内部構造

**Q: Explain how the controller-runtime watch mechanism works under the hood.**

Controller-runtime uses **shared informers** from client-go. When the operator starts, it sets up a watch connection (long-lived HTTP/2 stream) to the Kubernetes API server for each resource type (Deployments, Services). The informer maintains a local cache and a work queue. When a change event arrives (Add/Update/Delete), the event handler enqueues a reconciliation request (just the namespace/name key). The controller's worker goroutines dequeue requests and call the `Reconcile()` function. This is **level-triggered** (not edge-triggered) — meaning reconciliation operates on the current state, not the delta, making it inherently idempotent.

**Q: controller-runtimeのwatch機構は内部的にどのように動作しますか？**

controller-runtimeはclient-goの**共有インフォーマー**を使用します。オペレーター起動時に、各リソースタイプ（Deployment、Service）に対してKubernetes APIサーバーへのwatch接続（長寿命HTTP/2ストリーム）を設定します。インフォーマーはローカルキャッシュとワークキューを維持します。変更イベント（Add/Update/Delete）が到着すると、イベントハンドラーがReconciliationリクエスト（namespace/nameキーのみ）をキューに入れます。コントローラーのワーカーgoroutineがリクエストをデキューし、`Reconcile()`関数を呼び出します。これは**レベルトリガー**（エッジトリガーではない）であり、デルタではなく現在の状態に基づいて動作するため、本質的に冪等です。

---

## 11. Merge Patch Strategy / マージパッチ戦略

**Q: Why did you use merge patch instead of a regular update for annotations?**

A regular update (`client.Update`) sends the entire resource object and requires the latest `resourceVersion`, leading to conflicts if another controller modifies the same resource simultaneously. A **merge patch** (`client.MergeFrom`) only sends the diff — in this case, just the annotation changes. This avoids optimistic locking conflicts and is safe to run alongside other controllers or operators that may also modify the same Deployments/Services. It's the safest, least-invasive way to update metadata.

**Q: アノテーションの更新に通常のupdateではなくマージパッチを使用したのはなぜですか？**

通常のupdate（`client.Update`）はリソースオブジェクト全体を送信し、最新の`resourceVersion`が必要です。別のコントローラーが同時に同じリソースを変更すると競合が発生します。**マージパッチ**（`client.MergeFrom`）は差分のみ（この場合はアノテーションの変更のみ）を送信します。楽観的ロックの競合を回避し、同じDeployment/Serviceを変更する他のコントローラーやオペレーターと安全に共存できます。メタデータを更新する最も安全で非侵入的な方法です。

---

## 12. Real-World Integration / 実運用での統合

**Q: How would this integrate into a real CI/CD pipeline with Terraform?**

The ideal flow:
1. **Terraform applies** a Kubernetes resource with a `tfdrift.jin.dev/spec-hash` annotation containing the hash of the intended spec.
2. A **custom Terraform provider** or **post-apply script** computes the hash using the same algorithm and sets the annotation.
3. The **tfdrift-operator** watches the resource. If someone manually edits the resource (e.g., `kubectl edit`), the live hash changes while the expected hash stays the same.
4. The operator **detects drift** and emits a Kubernetes event.
5. A **monitoring system** (Prometheus + Alertmanager, or a Kubernetes event watcher) picks up the event and alerts the team via Slack/PagerDuty.
6. The team either **re-applies Terraform** to restore the desired state or **updates Terraform code** to match the intentional change.

**Q: 実際のCI/CDパイプラインでTerraformとどのように統合しますか？**

理想的なフロー：
1. **Terraformがapply**で、意図されたspecのハッシュを含む`tfdrift.jin.dev/spec-hash`アノテーション付きのKubernetesリソースを作成。
2. **カスタムTerraformプロバイダー**または**apply後スクリプト**が同じアルゴリズムでハッシュを計算しアノテーションを設定。
3. **tfdrift-operator**がリソースを監視。誰かが手動でリソースを編集（例：`kubectl edit`）するとライブハッシュが変わるが、期待ハッシュは変わらない。
4. オペレーターが**ドリフトを検出**しKubernetesイベントを発行。
5. **監視システム**（Prometheus + Alertmanager、またはKubernetesイベントウォッチャー）がイベントを検知し、Slack/PagerDuty経由でチームにアラート。
6. チームが**Terraformを再apply**して望ましい状態を復元するか、意図的な変更に合わせて**Terraformコードを更新**。

---

## 13. Go-Specific Questions / Go固有の質問

**Q: Why Go for this project? What Go-specific patterns did you use?**

Go is the de facto language for Kubernetes ecosystem tools. controller-runtime, client-go, and the entire Kubernetes API machinery are written in Go. Specific patterns used:

- **Interface satisfaction**: `Reconciler` interface implementation for controllers
- **Context propagation**: `context.Context` passed through the entire call chain for cancellation and timeout
- **Struct embedding**: Using controller-runtime's `client.Client` and `record.EventRecorder` in reconciler structs
- **Error wrapping**: `fmt.Errorf("...: %w", err)` for proper error chain propagation
- **Goroutine-safe design**: controller-runtime manages goroutines; the reconcile function is called concurrently

**Q: なぜこのプロジェクトにGoを選んだのですか？どのようなGo固有のパターンを使用しましたか？**

GoはKubernetesエコシステムツールの事実上の標準言語です。controller-runtime、client-go、Kubernetes API機構全体がGoで書かれています。使用した具体的なパターン：

- **インターフェース充足**: コントローラーの`Reconciler`インターフェース実装
- **コンテキスト伝播**: キャンセルとタイムアウトのため`context.Context`をコールチェーン全体に渡す
- **構造体埋め込み**: Reconciler構造体にcontroller-runtimeの`client.Client`と`record.EventRecorder`を使用
- **エラーラッピング**: 適切なエラーチェーン伝播のための`fmt.Errorf("...: %w", err)`
- **goroutine安全設計**: controller-runtimeがgoroutineを管理し、reconcile関数は並行で呼び出される

---

## 14. What Did You Learn? / 何を学びましたか？

**Q: What was the most challenging part and what did you learn?**

The most challenging part was **designing the deterministic hashing strategy**. Kubernetes resources have many auto-populated fields (default values, injected sidecars, etc.) that can differ between what Terraform sees and what's actually in the cluster. I had to carefully choose which fields to include and ensure canonicalization handled all edge cases.

I learned:
- The importance of **level-triggered over edge-triggered** design in distributed systems
- How **Kubernetes informers and work queues** provide reliable, scalable event processing
- How to think about **non-invasive operator design** — monitoring without modifying
- The value of **merge patches** for safe concurrent metadata updates

**Q: 最も困難だった部分は何で、何を学びましたか？**

最も困難だったのは**決定論的ハッシュ戦略の設計**です。Kubernetesリソースには多くの自動設定フィールド（デフォルト値、注入されたサイドカーなど）があり、Terraformが認識するものとクラスター内の実際の状態が異なる場合があります。含めるフィールドを慎重に選択し、正規化がすべてのエッジケースを処理することを確認する必要がありました。

学んだこと：
- 分散システムにおける**エッジトリガーよりレベルトリガー**設計の重要性
- **Kubernetesインフォーマーとワークキュー**が信頼性の高いスケーラブルなイベント処理を提供する仕組み
- **非侵入型オペレーター設計**の考え方 — 変更せずに監視する
- 安全な並行メタデータ更新のための**マージパッチ**の価値

---

## 15. Behavioral / 行動質問

**Q: Why did you build this project?**

I built this to solve a real operational pain point: in production Kubernetes environments managed by Terraform, manual changes (hotfixes, debugging tweaks) often go undetected, causing the actual state to silently drift from the IaC-defined state. This creates audit gaps and can lead to unexpected behavior during the next Terraform apply. I wanted an automated, always-on solution native to the Kubernetes ecosystem.

**Q: なぜこのプロジェクトを作ったのですか？**

Terraformで管理される本番Kubernetes環境における実際の運用上の課題を解決するために作りました。手動変更（ホットフィックス、デバッグ調整）が検出されないまま放置され、実際の状態がIaCで定義された状態からサイレントにドリフトすることがあります。これは監査ギャップを生み、次のTerraform apply時に予期しない動作を引き起こす可能性があります。Kubernetesエコシステムにネイティブな自動化された常時稼働ソリューションが欲しかったのです。
