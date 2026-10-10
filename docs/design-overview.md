# eapaka-ossbss-sample 設計概要

- 状態: 初版（2026-10-10）。§15 の確認事項はすべて推奨案で合意した。実装は §16 の順に進める（まだ着手していない）。
- 対象: 外部 OSS/BSS のサンプル実装としての統合BFF（コマンド名 `eapaka-ossbss`。以下「本BFF」）。
- 関連:
  - eapaka-node-provisioner（以下「provisioner」）: `docs/design-overview.md`、`docs/openapi/provisioner-api.yaml`、`docs/operation-guide.md`
  - 本PoC（eapaka-radius-server-poc）: `docs/D-02_Valkey_データ設計仕様書_r*.md`（認可ポリシー）、`docs/D-09_Auth_Server詳細設計書_r*.md` §8（認可）、`docs/D-13_Provisioning_API詳細設計書_r*.md`
  - 手本にする実装: web-gui-for-eapaka-radius（以下「既存の BFF」。provisioner 経由の画面、`internal/provapi`・`internal/pvapi`）

## 1. 位置づけ

EAP-AKA の加入者を運用する事業者の、外部 OSS/BSS（運用・業務支援システム）に当たるもののサンプル実装。provisioner の API を使って、本PoC（RADIUS サーバー）と aka-only-server（認証ベクターの払い出し）の加入者・RADIUSクライアント・認可ポリシーを操作する。既存の BFF との違いは、次の 3 つである。

- **テナント**: 加入者と RADIUSクライアントをテナント（法人の顧客など）に属させ、テナントの利用者は自分のテナントのものだけを見て操作する。テナントに属さないものも扱う。
- **認証**: 公開 URL を Cloudflare Access（メールのワンタイム PIN）で守り、本BFF は Access が付ける JWT を検証して利用者を識別する。パスワードは持たない。
- **一時停止**: 加入者を登録したまま使えなくする（停止）・戻す（再開）。そのために本PoCと provisioner に停止の印を加える（§8）。

```
[ブラウザ] ─HTTPS─> Cloudflare（Access: メール認証） ─Tunnel─> cloudflared ─HTTP─> 本BFF ──mTLS──> provisioner ──> 本PoC
                                                         （同じホスト。受信のポートは開けない）  │                 └─> aka-only-server
                                                                                              PostgreSQL（本BFF 専用）
```

- 本BFF は provisioner の管理クライアントで、provisioner は管理クライアントを全権として扱う（テナントや権限の概念を持たない）。**テナントと権限の境界は本BFF だけが担う。**
- 加入者のデータ（鍵、認可ポリシー、停止の状態）の正本は下流（本PoC、aka-only-server）にある。本BFF の DB に持つのは、テナント・利用者・所属・監査ログだけである（§7、§11）。
- 画面は既存の BFF と同じく Go + htmx で、見た目は daisyUI（Tailwind CSS）にする。

## 2. 範囲

| 区分 | 内容 |
|---|---|
| 対象 | テナントの管理、利用者（アカウント）の管理、加入者（登録・閲覧・変更・削除・停止・再開）、RADIUSクライアント（登録・閲覧・変更・削除）、認可ポリシーの編集、セッションの閲覧、監査ログ、provisioner の状態と操作の記録 |
| 下流の変更 | 本PoCと provisioner への停止の印の追加（§8） |
| 対象外 | 課金・請求、テナントの自己登録、Cloudflare の設定の自動化、セッションの切断（Disconnect / CoA）、複数ノード、CSV の一括操作 |

## 3. 決定事項

| # | 内容 | 決定日 |
|---|---|---|
| 1 | リポジトリ名は `eapaka-ossbss-sample`（公開リポジトリ `oyaguma3/eapaka-ossbss-sample`） | 2026-10-10 |
| 2 | 画面は Go + htmx。見た目は daisyUI | 2026-10-10 |
| 3 | 認証は Cloudflare Access / Tunnel（公開 URL とメール認証）と、本BFF での JWT の検証。利用者は JWT のメールアドレスで識別し、役割・テナント・権限は本BFF が当てる | 2026-10-10 |
| 4 | DB は PostgreSQL | 2026-10-10 |
| 5 | テナントの所属は、加入者（IMSI）と RADIUSクライアント（ID）ごとに本BFF に記録する（A 方式）。テナントに属さないものも許す | 2026-10-10 |
| 6 | 利用者の役割は 4 つ（§6）。権限は §6.2 の表 | 2026-10-10 |
| 7 | 加入者の停止は、本PoCの認可ポリシーに停止の印を加えて実現する（案ア。§8） | 2026-10-10 |
| 8 | テナント管理者は、自分のテナントのテナントユーザーのアカウントを管理する | 2026-10-10 |
| 9 | 所属の付け替えはシステム管理者だけ。セッションと監査ログはテナントの範囲で見せる。操作の記録のやり直し・閉じるはシステム管理者だけ。テナントの利用者に鍵の置き場所・AMF・SQN・食い違いを見せない（停止の状態は見せる）。IP の重複で他所の使用が分かることは許容する。下流にない所属の記録は表示してシステム管理者が消す。テナントの利用者は 1 人 1 テナント | 2026-10-10 |
| 10 | §15 の確認事項 1〜15 は、すべて推奨案とする | 2026-10-10 |

配置の想定（ユーザーの想定）: 本BFF と cloudflared を同じホストに置き、そのホストの受信を閉じたまま、公開 URL を本BFF にルーティングする。

## 4. 技術方針

既存の BFF と同じ流儀にそろえる。

- 言語: Go 1.27.1。標準ライブラリを優先する（`net/http` のメソッドつき ServeMux、`html/template`、`embed`、`log/slog`、`encoding/json/v2`、`crypto`）。
- 外部依存（Go）: `github.com/jackc/pgx/v5`（PostgreSQL のドライバー）だけ（§15 の 8）。JWT の検証（RS256）、JWKS の取得、DB のスキーマの移行は自前で書く。
- 画面: htmx は既存の BFF と同じくファイルを同梱する。daisyUI は Tailwind CSS v4 のプラグインとして使い、CSS は Docker のビルドの段で Tailwind のスタンドアロン CLI（Node.js は使わない）で生成して `embed` で同梱する（§15 の 9）。JavaScript は書かない（daisyUI のコンポーネントは CSS だけで動くものを使う）。
- module パス: `github.com/oyaguma3/eapaka-ossbss-sample`。コマンド名は `eapaka-ossbss`。設定は環境変数だけで受け取り、接頭辞は `OSSBSS_` とする。
- 待ち受け: `:8446`（HTTP。cloudflared からだけ受ける。§5.4）。既存のポート（aka-only-server 8443 / 9443、provisioning-api 9444、aka 版 GUI 8444、既存の BFF 8445、provisioner 9446）と重ならない。
- 配備: Docker Compose（本BFF、PostgreSQL、cloudflared）。本BFF のコンテナは distroless の nonroot。
- provisioner のクライアントは、既存の BFF の `internal/provapi` と `internal/pvapi` をコピーして手直しする（共有モジュールにはしない）。
- コードのコメントとドキュメントは日本語で書く。ブランチは `main` に直接コミットする（他の GUI と同じ）。

## 5. 認証（Cloudflare Access の JWT）

### 5.1 流れ

1. 利用者が公開 URL（例 `https://ossbss.example.com`）を開くと、Cloudflare Access がメールのワンタイム PIN でログインさせる。誰がログインできるか（メールアドレス、ドメイン）は Access のポリシーで決める。
2. Access は、オリジンへの要求に JWT を付ける（ヘッダー `Cf-Access-Jwt-Assertion`）。cloudflared がそれを本BFF に届ける。
3. 本BFF は要求のたびに JWT を検証し、`email` のアカウントを DB から引いて、役割・テナントを決める。

### 5.2 JWT の検証

- 署名: RS256。公開鍵はチームの JWKS（`https://<チーム名>.cloudflareaccess.com/cdn-cgi/access/certs`）から取得してキャッシュする。未知の `kid` が来たら取り直す（鍵の入れ替えに追随する。取り直しの間隔に下限を設ける）。
- 確かめる項目: `alg`（RS256 だけ）、`kid`、`iss`（`https://<チーム名>.cloudflareaccess.com`）、`aud`（Access のアプリの AUD タグを含む）、`exp` / `nbf` / `iat`（時計のずれの余裕つき）、`email`（空でない）。
- 設定: `OSSBSS_ACCESS_TEAM_DOMAIN`（例 `example.cloudflareaccess.com`）、`OSSBSS_ACCESS_AUD`。
- JWT がない・検証できない要求は 401 にする（画面には「Cloudflare Access を通して開いてください」と出す）。ヘッダー `Cf-Access-Authenticated-User-Email` は使わない（検証できないため）。
- JWKS を取得できないときは、キャッシュの鍵で検証を続ける。キャッシュもなければ 503。

### 5.3 利用者の識別

- JWT のメールアドレス（小文字にしたもの）でアカウントを引く。アカウントがない・無効なら 403「この利用者は登録されていません」（Access は通ったが、本BFF の利用者ではない）。
- サーバー側のセッションは持たない（要求のたびに JWT を検証する）。ログアウトは Access のログアウト（`/cdn-cgi/access/logout`）へのリンクにする。
- CSRF 対策は既存の BFF と同じく `CrossOriginProtection` を使う。
- 最初のシステム管理者は、環境変数 `OSSBSS_INITIAL_ADMIN_EMAIL` で与える（既存の BFF の最初の管理者に当たる。§15 の 12）。

### 5.4 Cloudflare 側の構成

| 項目 | 内容 |
|---|---|
| ドメイン | 公開ホスト名には Cloudflare に置いた DNS のゾーンが要る（未取得。これが前提） |
| Tunnel | ダッシュボードで作り、トークンで動かす（remotely-managed）。公開ホスト名を `http://eapaka-ossbss:8446`（compose のネットワークの中の本BFF）に向ける |
| cloudflared | 本BFF と同じ compose のコンテナとして動かす。トークンは `.env` の `TUNNEL_TOKEN` |
| 本BFF の待ち受け | compose のネットワークの中だけ（ホストにポートを公開しない）。ホストの受信は閉じたままでよい（Tunnel は外向きに接続する） |
| Access のアプリ | 公開ホスト名の self-hosted アプリ。ログイン方法はワンタイム PIN、ポリシーで許可するメールアドレス（またはドメイン）を決める。AUD タグとチームのドメインを本BFF の設定に渡す |
| プラン | Zero Trust の Free プラン（50 人まで） |
| provisioner への経路 | 同じホストなら provisioner の共有ネットワーク、別のホストなら VPN（Tailscale 等）。どちらも外向きの接続 |

ドメインがない間の開発は、§15 の 10 の開発用の設定で行う。

## 6. 利用者と権限

### 6.1 役割

| 役割 | 呼び名（画面） | 所属 | 内容 |
|---|---|---|---|
| A | テナントユーザー | 1 つのテナント | 自分のテナントの加入者と RADIUSクライアントを見る。加入者の停止・再開をする |
| B | テナント管理者 | 1 つのテナント | A の権限に加え、自分のテナントの認可ポリシーの変更、RADIUSクライアントの管理、テナントユーザーのアカウントの管理をする |
| C | 運用担当 | なし（全テナントとテナントなし） | 加入者の登録・削除、Ki / OPc と認可ポリシーの変更など、日常の運用をする |
| D | システム管理者 | なし（全テナントとテナントなし） | すべての操作。テナントとアカウントの管理、所属の付け替え、AMF / SQN の変更、操作の記録への対応 |

呼び名は §15 の 13 で決めた（相談のときの整理では、C を「BFFユーザー」、D を「BFF管理者」と呼んでいた）。

### 6.2 権限

A・B が扱えるのは、自分のテナントに属する加入者・RADIUSクライアントだけである。テナントに属さないものは C・D だけが扱う。

| 操作 | A | B | C | D |
|---|---|---|---|---|
| 加入者の登録（Ki / OPc / AMF / SQN を入力。所属のテナントを選ぶ） | × | × | ○ | ○ |
| 加入者の閲覧 | IMSI・認可ポリシー・停止の状態 | IMSI・認可ポリシー・停止の状態 | ○（Ki / OPc の表示を除く） | ○ |
| 加入者の Ki / OPc の表示 | × | × | ○ | ○ |
| 加入者の変更: 認可ポリシー | × | ○ | ○ | ○ |
| 加入者の変更: Ki / OPc | × | × | ○ | ○ |
| 加入者の変更: AMF / SQN（aka-only-server の加入者は SQN の増加タイプ・平文HTTP の許可も） | × | × | × | ○ |
| 加入者の削除 | × | × | ○ | ○ |
| 加入者の停止・再開 | ○ | ○ | ○ | ○ |
| RADIUSクライアントの登録 | × | ○（自分のテナントに属する） | ○ | ○ |
| RADIUSクライアントの閲覧 | ○（共有シークレットを除く） | ○ | ○ | ○ |
| RADIUSクライアントの変更・削除 | × | ○ | ○ | ○ |
| RADIUSクライアントの共有シークレットの表示 | × | ○ | ○ | ○ |
| 所属のテナントの付け替え・外す | × | × | × | ○ |
| セッションの閲覧 | 自分のテナントの加入者 | 自分のテナントの加入者 | ○ | ○ |
| 監査ログの閲覧 | × | 自分のテナントの分 | ○（§15 の 14） | ○ |
| provisioner の状態・操作の記録の閲覧 | × | × | ○ | ○ |
| 操作の記録のやり直し・閉じる | × | × | × | ○ |
| テナントの作成・変更・削除 | × | × | × | ○ |
| アカウントの管理（作成・閲覧・変更・削除） | × | 自分のテナントの A | × | ○（すべて） |
| 下流にない所属の記録の削除 | × | × | × | ○ |

- 「C の閲覧（Ki / OPc の表示を除く）」は、鍵の置き場所・AMF・SQN・食い違いを見られ、Ki / OPc はボタンを押したときだけ表示する（既存の BFF と同じ）、という意味である。
- A・B の画面には、鍵の置き場所・AMF・SQN・2 つのノードの食い違い（`issues`）を出さない。停止の状態は出す。
- 権限の判定は本BFF だけで行う。画面に出さないだけでなく、要求を受けたときにも必ず判定する（テナントの範囲の確認を含む）。

## 7. テナントと所属

- テナント: 名前（表示用）と識別子を持つ。作成・変更・削除は D だけ。所属するものが残っているテナントは削除できない（先に付け替えるか外す）。
- 所属の記録: 加入者は IMSI、RADIUSクライアントは provisioner（本PoC）が採番する ID ごとに、テナントを 1 つ記録する。記録がなければ「テナントなし」。
- 登録のときの所属: C・D が加入者・RADIUSクライアントを登録するときに、テナント（またはテナントなし）を選ぶ。B が登録した RADIUSクライアントは B のテナントに属する。
- 付け替え・外す: D だけ。
- 本BFF の外で作られたもの（本PoCの Admin TUI、aka-only-server の管理 GUI、既存の BFF 等）は、所属の記録がないので「テナントなし」として扱う。
- 本BFF の外で削除されたものは、所属の記録だけが残る。一覧で「下流にない」と表示し、D が記録を消す。
- 本BFF で削除したときは、所属の記録も消す。
- IP アドレスの重複: B が登録しようとした IP を他のテナントのクライアントが使っていると、provisioner は「既にある」と返す。その IP が他所で使われていることは分かるが、許容する（決定事項 9）。

### 7.1 テナントの範囲の一覧

provisioner には所属の概念がないので、A・B の一覧は本BFF の所属の記録を元にする。

- 加入者: 所属の記録（IMSI の順）を 1 ページ分読み、各 IMSI を provisioner から取得する（§15 の 7）。
- RADIUSクライアント: provisioner の一覧（全件）を取り、所属の記録で絞る。
- セッション: provisioner のセッションの一覧（IMSI での絞り込み）を、所属の加入者ごとに取る（§15 の 7）。

## 8. 加入者の一時停止（下流の変更）

### 8.1 方針（案ア。2026-10-10 決定）

本PoCの認可ポリシーに停止の印を加え、auth-server は停止中の加入者の認証を拒否する。provisioner はそれを中継・統合する。本BFF は provisioner の API で停止・再開する。

- 認可ポリシーは鍵の置き場所によらず本PoCにあるので、本PoCに鍵を置く加入者にも aka-only-server に置く加入者にも同じように効く。
- 停止の状態は正本（本PoC）にあり、Admin TUI からも見える。
- 接続中のセッションはすぐには切れない（Disconnect / CoA は扱わない）。効くのは次の認証（再認証、Session-Timeout の後）から。画面とドキュメントでそのことを伝える。
- 認可ポリシーがない加入者は停止できない（もともと認証は拒否される）。

### 8.2 本PoCの変更（案）

| 対象 | 変更 |
|---|---|
| データ（D-02） | `policy:{IMSI}` の Hash にフィールド `status` を加える（`active` / `suspended`。ないときは `active`）。既存の書き込み（Provisioning API の PUT、Admin TUI の保存と CSV の取り込み）は `default` と `rules` だけを HSET するので、`status` は消えない |
| auth-server（D-09 §8） | 認可ポリシーを読む位置（Challenge の検証の後）で、ルールの評価の前に `status` を見る。`suspended` なら拒否し、event_id `AUTH_SUBSCRIBER_SUSPENDED`（WARN）を出す（§15 の 2） |
| Provisioning API（D-13、OpenAPI） | 認可ポリシーの応答に `status` を加える。変更は専用の `PUT /policies/{imsi}/status`（本文 `{"status": "suspended"}`）で行い、監査ログ（`policy.suspend` / `policy.resume`）に残す。認可ポリシーの PUT（全体の置き換え）では `status` を変えない（§15 の 1） |
| Admin TUI | 認可ポリシーの一覧と編集の画面に状態を出し、停止・再開できるようにする。CSV は末尾に任意の列 `status` を加える（なければ `active`。§15 の 3） |
| ドキュメント | D-02、D-09、D-13、D-04（ログ仕様）、O-01 / O-02（操作・ポリシーの手順）、T-02 / T-03（試験）、ドキュメント一覧を改版する |

### 8.3 provisioner の変更（案）

| 対象 | 変更 |
|---|---|
| 加入者の統合リソース | `status`（認可ポリシーの `status`。ポリシーがなければ省略）を加える |
| 停止・再開 | `PUT /policies/{imsi}/status` を中継する（同じ IMSI のロックを取り、未完了の操作があれば 409。`Idempotency-Key` を扱う）。加入者の操作としての別の口（`/subscribers/{imsi}/suspend` 等）は作らない（§15 の 4） |
| 入力の検証 | 加入者の作成・変更の本文の `policy` は今のまま（`status` は受け付けない。作成は常に `active`） |
| API 仕様 | 0.3.0 に上げる |

既存の BFF（web-gui-for-eapaka-radius）と aka 版 GUI への反映は、§15 の 5 で決める。

## 9. 画面（概要）

画面の詳細は、確認事項に合意した後に画面仕様（`docs/screen-spec.md`）で決める。

| 画面 | 内容 | 使える役割 |
|---|---|---|
| ダッシュボード | 自分のテナントの加入者数・停止中の数・RADIUSクライアント数。C・D には provisioner と下流の状態、未完了の操作の件数も | 全員 |
| 加入者の一覧・詳細 | 役割に応じた項目（§6.2）。停止・再開のボタン（確認ダイアログつき） | 全員 |
| 加入者の登録・変更・削除 | 所属のテナントの選択を含む | C・D（変更の項目は §6.2） |
| 認可ポリシーの編集 | 既存の BFF と同じ作り（ルールの追加・削除・並べ替え） | B・C・D |
| RADIUSクライアント | 一覧・登録・詳細・変更・削除・共有シークレットの表示 | 全員（操作は §6.2） |
| セッション | テナントの範囲のアクティブセッション | 全員 |
| テナント | 一覧・作成・変更・削除、所属の付け替え | D |
| アカウント | 一覧・作成・変更・無効化・削除 | B（自分のテナントの A）・D |
| 操作の記録 | provisioner の未完了の操作 | C・D（やり直し・閉じるは D） |
| 監査ログ | 本BFF の監査ログ（テナントの範囲）。D には provisioner と下流の監査ログも | B・C・D |

- daisyUI のコンポーネント（ナビゲーション、テーブル、カード、バッジ、モーダル（`<dialog>`）、トースト、テーマ）で組む。テーマはブラウザの設定（ライト / ダーク）に従う。
- 停止中の加入者は一覧と詳細でバッジ（例「停止中」）を出す。

## 10. provisioner との連携

- 本BFF は provisioner の管理クライアントとして、クライアント証明書を provisioner の `PROVISIONER_ADMIN_CLIENTS` に登録する（既存の BFF と同じ。`gen-client-cert` を持つ）。
- 操作者（`X-Operator-Id`）には、利用者のメールアドレスを渡す（§15 の 6）。provisioner と下流の監査ログには「操作者＝メールアドレス、管理クライアント＝本BFF の識別名」が残る。
- トレースID は要求ごとに採番して渡す（既存の BFF と同じ）。本BFF の監査ログにも残す。
- 加入者の登録・変更・削除、停止・再開のフォームには `Idempotency-Key` を持たせる（既存の BFF と同じ）。
- 1 回の呼び出しの上限は 60 秒、HTTP サーバーの書き出しの上限はそれより長くする（既存の BFF の §12.6 で分かったこと）。
- provisioner の API のうち、本BFF が使うのは: 状態、加入者の統合操作、認可ポリシー（中継）、停止・再開（§8.3）、RADIUSクライアント（中継）、セッション（中継）、操作の記録、監査ログ（3 種類）。

## 11. データモデル（PostgreSQL）

| テーブル | 主な列 | 備考 |
|---|---|---|
| `tenants` | `id`、`name`（一意）、`created_at`、`updated_at` | |
| `accounts` | `id`、`email`（一意、小文字）、`role`（A〜D）、`tenant_id`（A・B は必須、C・D は空）、`disabled`、`created_at`、`updated_at`、`created_by` | 役割とテナントの組み合わせを制約で守る |
| `subscriber_tenants` | `imsi`（主キー）、`tenant_id`、`assigned_at`、`assigned_by` | 行がなければテナントなし |
| `client_tenants` | `client_id`（主キー）、`tenant_id`、`assigned_at`、`assigned_by` | 同上 |
| `audit_log` | `id`、`time`、`actor_email`、`actor_role`、`tenant_id`（対象のテナント）、`action`、`target`、`detail`（JSONB。秘密の値は含めない）、`trace_id`、`remote` | テナントの範囲での検索に索引を張る |
| `schema_migrations` | `version`、`applied_at` | スキーマの移行は起動時に自前で行う（SQL を `embed`） |

- `tenant_id` の外部キーは `ON DELETE RESTRICT`（所属が残るテナントは削除できない）。
- 秘密の値（Ki / OPc、共有シークレット）は保存しない。
- バックアップは `pg_dump`（運用ガイドで手順を決める）。

## 12. 配置（Docker Compose）

| サービス | 内容 |
|---|---|
| `eapaka-ossbss` | 本BFF。ポートは公開しない。provisioner と同じホストなら provisioner の共有ネットワーク（`eapaka-provisioner`）にも参加する |
| `postgres` | 本BFF 専用。ポートは公開しない |
| `cloudflared` | Tunnel のコネクター。トークンで動く |

- 重ねる compose ファイルで、provisioner が同一ホストか別ホストかを選ぶ（既存の BFF と同じ作り）。
- 開発用の設定（§15 の 10）では、本BFF をホストのループバックにだけ公開し、cloudflared を動かさない。

## 13. 監査ログ

- 本BFF の監査ログに、利用者の変更操作（加入者、認可ポリシー、停止・再開、RADIUSクライアント、所属、テナント、アカウント）と、秘密の値の表示を残す。操作者のメールアドレス・役割、対象のテナント、トレースID を含める。
- B は自分のテナントが対象のものだけを見る。
- provisioner と下流の監査ログ（provisioner の中継）は D だけが見る（テナントで絞れないため）。

## 14. 試験

- 単体テスト: provisioner の偽物（既存の BFF の `fakePV` と同じ作り）と、テスト用の鍵で作った JWT で、権限とテナントの範囲の判定を網羅する（役割 × 操作 × 自テナント / 他テナント / テナントなし）。
- DB の結合テスト: 接続先を環境変数で指定したときだけ、実際の PostgreSQL で行う（CI ではサービスコンテナ）。
- 契約テスト: provisioner と下流を固定のコミットでビルド・起動して行う（既存の BFF の CI と同じ作り）。
- 実機: Cloudflare の Tunnel と Access を通した通しの確認は、ドメインを用意できてから行う。

## 15. 確認事項

いずれも 2026-10-10 に推奨案で合意した。

| # | 事項 | 推奨案 |
|---|---|---|
| 1 | 本PoCの停止の印の API の形 | 認可ポリシーの応答に `status`（`active` / `suspended`）を加え、変更は専用の `PUT /policies/{imsi}/status` で行う。認可ポリシーの PUT（B・C も使う全体の置き換え）では変えない。ポリシーの編集で停止がうっかり解けないように、また停止・再開を監査ログで区別できるようにするため |
| 2 | 停止の判定の位置（auth-server） | 今の認可ポリシーの評価の位置（Challenge の検証の後）で、ルールの評価の前に判定する。認証ベクターの取得（SQN が進む）の前に判定する案もあるが、auth-server の流れを大きく変えるので採らない（停止中でも SQN が進むことは許容する） |
| 3 | 本PoCの Admin TUI と CSV | Admin TUI の認可ポリシーの画面に状態を出し、停止・再開できるようにする。CSV の末尾に任意の列 `status` を加える（ない CSV は `active` として取り込み、今の CSV と互換を保つ） |
| 4 | provisioner での停止・再開の口 | `PUT /policies/{imsi}/status` を中継し（ロック、未完了の操作の確認、`Idempotency-Key`）、加入者の統合リソースに `status` を加える。加入者の操作としての別の口は作らない（停止は本PoCの 1 ノードの操作で、補償の要る複数ノードの操作ではないため） |
| 5 | 既存の BFF・aka 版 GUI への反映 | 既存の BFF（web-gui-for-eapaka-radius）は、直接接続・provisioner 経由のどちらでも停止の状態の表示と停止・再開を加える（同じ本PoCを操作する GUI で状態が見えないと混乱するため）。aka 版 GUI は変えない（aka-only-server は変えない） |
| 6 | `X-Operator-Id` に渡す値 | 利用者のメールアドレスをそのまま渡す。`X-Operator-Id` の形式（`^[A-Za-z0-9._@-]{1,64}$`）に合わないメールアドレス（`+` を含む、64 文字を超える等）は、アカウントを登録するときに断る |
| 7 | テナントの範囲の一覧の作り | 加入者は 1 ページを 20 件とし、所属の記録の 1 ページ分を provisioner から並行して（同時に 5 件まで）取得する。セッションは、所属の加入者ごとの取得をやめ、provisioner のセッションの一覧（上限 1000 件）を取って所属で絞る。件数が多くなったら、provisioner への一括取得の API の追加を検討する（将来拡張） |
| 8 | Go の外部依存 | `github.com/jackc/pgx/v5` を加える（PostgreSQL を選んだことに伴う）。スキーマの移行のライブラリは使わず、自前で書く |
| 9 | daisyUI の取り込み | Tailwind CSS v4 のスタンドアロン CLI と daisyUI 5 のプラグインのファイルを、版を固定し、チェックサムを確かめて Docker のビルドの段で取得し、CSS を生成する。生成した CSS はリポジトリに入れない（手元の開発では同じ CLI を使う手順を用意する） |
| 10 | ドメインがない間の開発・試験 | 開発用の設定 `OSSBSS_DEV_USER_EMAIL` を設け、JWT の検証の代わりにそのメールアドレスの利用者として動かす。ループバックで待ち受けるときだけ有効にし、起動時に WARN を出す。役割ごとの確認は、DB のアカウントをその利用者に付け替えて行う |
| 11 | テナント管理者のアカウント管理の範囲 | B は自分のテナントの A を作成・変更・無効化・削除できる。B 自身や他の B は D が管理する。B は A を B に昇格できない |
| 12 | 最初のシステム管理者とアカウントの無効化 | 最初の D は `OSSBSS_INITIAL_ADMIN_EMAIL` で与え、DB に登録がなければ起動時に作る（削除・降格はできない）。アカウントは削除のほかに無効化（ログインできなくする）を持つ |
| 13 | 画面での役割の呼び名 | テナントユーザー（A）、テナント管理者（B）、運用担当（C）、システム管理者（D）とする（「BFF」は利用者に馴染みのない言葉なので画面では使わない） |
| 14 | C の監査ログの閲覧 | C は本BFF の監査ログをすべて見られる。provisioner と下流の監査ログは D だけ |
| 15 | 進め方の順番 | 下流の変更（本PoC → provisioner → 既存の BFF）を先に行い、その後に本BFF を作る（§16） |

## 16. 進め方

1. 本PoC: 停止の印（§8.2）。本PoCの流儀（feature → develop → main、ドキュメントの改版）で行う。
2. provisioner: 停止・再開の中継と統合リソースの `status`、API 仕様 0.3.0（§8.3）。
3. 既存の BFF: 停止の状態の表示と停止・再開（§15 の 5）。
4. 本BFF のステップ:
   1. 骨組み（module、設定、HTTP サーバー、PostgreSQL とスキーマの移行、compose、Tailwind / daisyUI のビルド）
   2. 認証（JWT の検証、JWKS、開発用の設定）と、アカウント・テナント・権限の判定
   3. provisioner のクライアント（既存の BFF から）と契約テスト、CI
   4. 画面: 加入者・認可ポリシー・停止と再開・RADIUSクライアント・セッション（テナントの範囲）
   5. 画面: テナント・アカウント・所属の付け替え・操作の記録・監査ログ・ダッシュボード
   6. compose（cloudflared を含む）・運用ガイド・README、実機での確認（ドメインを用意できてから、Cloudflare を通した通し）

各ステップの終わりに「作ったもの」と「実際に動かして確かめたこと」を報告して確認をもらう。
