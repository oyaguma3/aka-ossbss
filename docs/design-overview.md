# eapaka-ossbss-sample 設計概要

- 状態: 初版（2026-10-10）に、実装を終えた時点の内容を反映した（2026-10-11）。§16 の手順（下流の変更と本BFF のステップ 1〜6）はすべて完了した。§1〜§16 は今の作りを、§17 はステップごとの決定事項と確かめたことの記録を、§18 は将来の拡張の検討を書く。
- 本BFF の文書: 画面仕様 `docs/screen-spec.md`、運用ガイド `docs/operation-guide.md`（導入・運用の手順）。
- 対象: 外部 OSS/BSS のサンプル実装としての統合BFF（コマンド名 `eapaka-ossbss`。以下「本BFF」）。
- 関連:
  - eapaka-node-provisioner（以下「provisioner」）: `docs/design-overview.md`、`docs/openapi/provisioner-api.yaml`、`docs/operation-guide.md`
  - 本PoC（eapaka-radius-server-poc）: `docs/D-02_Valkey_データ設計仕様書_r*.md`（認可ポリシー）、`docs/D-09_Auth_Server詳細設計書_r*.md` §8（認可）、`docs/D-13_Provisioning_API詳細設計書_r*.md`
  - 手本にした実装: web-gui-for-eapaka-radius（以下「既存の BFF」。provisioner 経由の画面、`internal/provapi`・`internal/pvapi`、テストの偽物）

## 1. 位置づけ

EAP-AKA の加入者を運用する事業者の、外部 OSS/BSS（運用・業務支援システム）に当たるもののサンプル実装。provisioner の API を使って、本PoC（RADIUS サーバー）と aka-only-server（認証ベクターの払い出し）の加入者・RADIUSクライアント・認可ポリシーを操作する。既存の BFF との違いは、次の 3 つである。

- **テナント**: 加入者と RADIUSクライアントをテナント（法人の顧客など）に属させ、テナントの利用者は自分のテナントのものだけを見て操作する。テナントに属さないものも扱う。
- **認証**: 公開 URL を Cloudflare Access（メールのワンタイム PIN）で守り、本BFF は Access が付ける JWT を検証して利用者を識別する。パスワードは持たない。
- **一時停止**: 加入者を登録したまま使えなくする（停止）・戻す（再開）。そのために本PoCと provisioner に停止の印を加えた（§8）。

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
| 下流の変更 | 本PoCと provisioner への停止の印の追加（§8。実施済み） |
| 対象外 | 課金・請求、テナントの自己登録、Cloudflare の設定の自動化、セッションの切断（Disconnect / CoA）、複数ノード、CSV の一括操作（加入者の一括投入は §18.5 で検討した） |

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

- 言語: Go 1.27.3。標準ライブラリを優先する（`net/http` のメソッドつき ServeMux、`html/template`、`embed`、`log/slog`、`encoding/json/v2`、`crypto`）。
- 外部依存（Go）: `github.com/jackc/pgx/v5`（PostgreSQL のドライバー）だけ（§15 の 8）。JWT の検証（RS256）、JWKS の取得、DB のスキーマの移行は自前で書く。
- 画面: htmx は既存の BFF と同じくファイルを同梱する。daisyUI は Tailwind CSS v4 のプラグインとして使い、CSS は Docker のビルドの段で Tailwind のスタンドアロン CLI（Node.js は使わない）で生成して `embed` で同梱する（§15 の 9。`scripts/build-css.sh`）。JavaScript は書かない（daisyUI のコンポーネントは CSS だけで動くものを使い、確認ダイアログは HTML の Invoker Commands で開く。§17.4 の 4）。静的ファイルは内容のハッシュつきの URL で参照する（途中のキャッシュの古い版を使わせない。§17.4）。
- module パス: `github.com/oyaguma3/eapaka-ossbss-sample`。コマンド名は `eapaka-ossbss`。設定は環境変数だけで受け取り、接頭辞は `OSSBSS_` とする。
- 待ち受け: `:8446`（HTTP。cloudflared からだけ受ける。§5.4）。既存のポート（aka-only-server 8443 / 9443、provisioning-api 9444、aka 版 GUI 8444、既存の BFF 8445、provisioner 9446）と重ならない。
- 配備: Docker Compose（本BFF、PostgreSQL、cloudflared。§12）。本BFF のコンテナは distroless の nonroot。
- provisioner のクライアントは、既存の BFF の `internal/provapi` と `internal/pvapi` を元に、1 つのパッケージ `internal/provisioner` にまとめた（共有モジュールにはしない。本BFF の接続先は provisioner だけなので分けない。§17.3 の 1）。
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
- 細部（JWT の受け取りはヘッダーだけ、AUD タグは複数可、時計のずれ 60 秒、JWKS の取り直しの間隔など）は §17.2。

### 5.3 利用者の識別

- JWT のメールアドレス（小文字にしたもの）でアカウントを引く。アカウントがない・無効なら 403「この利用者は登録されていません」（Access は通ったが、本BFF の利用者ではない）。
- サーバー側のセッションは持たない（要求のたびに JWT を検証する）。ログアウトは Access のログアウト（`/cdn-cgi/access/logout`）へのリンクにする。
- CSRF 対策は既存の BFF と同じく `CrossOriginProtection` を使う。
- 最初のシステム管理者は、環境変数 `OSSBSS_INITIAL_ADMIN_EMAIL` で与える（既存の BFF の最初の管理者に当たる。§15 の 12）。起動のたびにシステム管理者・テナントなし・有効に戻し、画面からの削除・降格・無効化は断る（締め出されたときに戻す経路。§17.2 の 3）。
- 接続元のアドレス（アクセスログ、監査ログ）は、Access で動くときは `Cf-Connecting-IP` を使う（§17.2 の 4）。

### 5.4 Cloudflare 側の構成

| 項目 | 内容 |
|---|---|
| ドメイン | 公開ホスト名には Cloudflare に置いた DNS のゾーンが要る（検証では `simauth.link` を使った。§17.2） |
| Tunnel | ダッシュボードで作り、トークンで動かす（remotely-managed）。公開ホスト名を `http://eapaka-ossbss:8446`（compose のネットワークの中の本BFF）に向ける（運用ガイド 2.2） |
| cloudflared | 本BFF と同じ compose のコンテナとして動かす。トークンはファイル `secrets/tunnel-token` で渡す（§17.6 の 1。当初の案の `.env` の `TUNNEL_TOKEN` はやめた） |
| 本BFF の待ち受け | compose のネットワークの中だけ（ホストにポートを公開しない）。ホストの受信は閉じたままでよい（Tunnel は外向きに接続する） |
| Access のアプリ | 公開ホスト名の self-hosted アプリ。ログイン方法はワンタイム PIN、ポリシーで許可するメールアドレス（またはドメイン）を決める。AUD タグとチームのドメインを本BFF の設定に渡す |
| プラン | Zero Trust の Free プラン（50 人まで） |
| provisioner への経路 | 同じホストなら provisioner の共有ネットワーク、別のホストなら VPN（Tailscale 等）。どちらも外向きの接続 |

Cloudflare 側の準備の手順は運用ガイド 2.2。手元の開発と試験は、ドメインがなくても §15 の 10 の開発用の設定で行える（ループバックで待ち受けるバイナリだけ）。

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
- 権限の判定は本BFF だけで行う。画面に出さないだけでなく、要求を受けたときにも必ず判定する（テナントの範囲の確認を含む。`internal/authz` の `Allowed`）。
- A・B が範囲の外（他のテナント、テナントなし）の加入者・RADIUSクライアントを開いたときは、存在しないときと同じ 404 を返す（存在の有無を漏らさない）。範囲の中でも役割にない操作は 403（§17.4 の 2）。
- アカウントの管理: B には自分のテナントのアカウントをすべて見せ（テナント管理者を含む）、操作できるのはテナントユーザーだけ。自分自身と最初のシステム管理者のアカウントは、削除・無効化・役割やテナントの変更を断る（§17.5 の 4・7）。

## 7. テナントと所属

- テナント: 名前（表示用）と識別子を持つ。作成・変更・削除は D だけ。所属するものが残っているテナントは削除できない（先に付け替えるか外す）。
- 所属の記録: 加入者は IMSI、RADIUSクライアントは provisioner（本PoC）が採番する ID ごとに、テナントを 1 つ記録する。記録がなければ「テナントなし」。
- 登録のときの所属: C・D が加入者・RADIUSクライアントを登録するときに、テナント（またはテナントなし）を必ず選ぶ。B が登録した RADIUSクライアントは B のテナントに属する。provisioner の操作が成功してから記録を書く（失敗してもテナントなしとして残るだけで、他のテナントに見えることはない。§17.4 の 3）。
- 付け替え・外す: D だけ。詳細から 1 件ずつ、または一覧でチェックしてまとめて（「テナントなし」の一覧からまとめて割り当てる用途を含む）。provisioner にあるものだけを付け替える（§17.5 の 1）。
- 本BFF の外で作られたもの（本PoCの Admin TUI、aka-only-server の管理 GUI、既存の BFF 等）は、所属の記録がないので「テナントなし」として扱う。
- 本BFF の外で削除されたものは、所属の記録だけが残る。一覧で「下流にない」と表示し、D が点検の画面で見つけてまとめて消す（消す直前に provisioner にないことを確かめ直す。§17.5 の 2）。
- 本BFF で削除したときは、所属の記録も消す。
- IP アドレスの重複: B が登録しようとした IP を他のテナントのクライアントが使っていると、provisioner は「既にある」と返す。その IP が他所で使われていることは分かるが、許容する（決定事項 9）。

### 7.1 テナントの範囲の一覧

provisioner には所属の概念がないので、A・B の一覧は本BFF の所属の記録を元にする。

- 加入者: 所属の記録（IMSI の順）を 1 ページ（20 件）分読み、各 IMSI を provisioner から並行して（同時に 5 件まで）取得する（§15 の 7）。
- RADIUSクライアント: provisioner の一覧（全件）を取り、所属の記録で絞る。
- セッション: provisioner のセッションの一覧（新しい順に 1000 件まで）を取り、所属で絞る（§15 の 7）。
- C・D は provisioner の一覧をそのまま使い、所属（テナント名）を添える。テナントでも絞れる（「テナントなし」の加入者は、provisioner の一覧を 500 件ずつ読んで記録のないものを残し、1 回の表示で 5,000 件まで調べる。§17.4 の 8）。

## 8. 加入者の一時停止（下流の変更）

### 8.1 方針（案ア。2026-10-10 決定）

本PoCの認可ポリシーに停止の印を加え、auth-server は停止中の加入者の認証を拒否する。provisioner はそれを中継・統合する。本BFF は provisioner の API で停止・再開する。

- 認可ポリシーは鍵の置き場所によらず本PoCにあるので、本PoCに鍵を置く加入者にも aka-only-server に置く加入者にも同じように効く。
- 停止の状態は正本（本PoC）にあり、Admin TUI からも見える。
- 接続中のセッションはすぐには切れない（Disconnect / CoA は扱わない）。効くのは次の認証（再認証、Session-Timeout の後）から。画面とドキュメントでそのことを伝える。
- 認可ポリシーがない加入者は停止できない（もともと認証は拒否される）。

### 8.2 本PoCの変更（実施済み。Provisioning API 0.4.0、main `dbd97f5`）

| 対象 | 変更 |
|---|---|
| データ（D-02） | `policy:{IMSI}` の Hash にフィールド `status` を加える（`active` / `suspended`。ないときは `active`）。既存の書き込み（Provisioning API の PUT、Admin TUI の保存と CSV の取り込み）は `default` と `rules` だけを HSET するので、`status` は消えない |
| auth-server（D-09 §8） | 認可ポリシーを読む位置（Challenge の検証の後）で、ルールの評価の前に `status` を見る。`suspended` なら拒否し、event_id `AUTH_SUBSCRIBER_SUSPENDED`（WARN）を出す（§15 の 2） |
| Provisioning API（D-13、OpenAPI） | 認可ポリシーの応答に `status` を加える。変更は専用の `PUT /policies/{imsi}/status`（本文 `{"status": "suspended"}`）で行い、監査ログ（`policy.suspend` / `policy.resume`）に残す。認可ポリシーの PUT（全体の置き換え）では `status` を変えない（§15 の 1） |
| Admin TUI | 認可ポリシーの一覧と編集の画面に状態を出し、停止・再開できるようにする。CSV は末尾に任意の列 `status` を加える（なければ `active`。§15 の 3） |
| ドキュメント | D-02、D-09、D-13、D-04（ログ仕様）、O-01 / O-02（操作・ポリシーの手順）、T-02 / T-03（試験）、ドキュメント一覧を改版する |

### 8.3 provisioner の変更（実施済み。API 0.3.0、`e96390d`）

| 対象 | 変更 |
|---|---|
| 加入者の統合リソース | `status`（認可ポリシーの `status`。ポリシーがなければ省略）を加える |
| 停止・再開 | `PUT /policies/{imsi}/status` を中継する（同じ IMSI のロックを取り、未完了の操作があれば 409。`Idempotency-Key` を扱う）。加入者の操作としての別の口（`/subscribers/{imsi}/suspend` 等）は作らない（§15 の 4） |
| 入力の検証 | 加入者の作成・変更の本文の `policy` は今のまま（`status` は受け付けない。作成は常に `active`） |
| API 仕様 | 0.3.0 に上げる |

既存の BFF（web-gui-for-eapaka-radius）にも、停止の状態の表示と停止・再開を加えた（`ca0bc5d`。§15 の 5）。aka 版 GUI は変えていない。

## 9. 画面（概要）

画面の詳細は画面仕様（`docs/screen-spec.md`）。

| 画面 | 内容 | 使える役割 |
|---|---|---|
| ダッシュボード | 範囲の加入者数・停止中の数・RADIUSクライアント数（A・B は自分のテナント、C・D はすべてとテナントの数。停止中の数は後から読み込む）。C・D には provisioner と下流の状態、未完了の操作の件数も | 全員 |
| 加入者の一覧・詳細 | 役割に応じた項目（§6.2）。停止・再開のボタン（確認ダイアログつき） | 全員 |
| 加入者の登録・変更・削除 | 所属のテナントの選択を含む | C・D（変更の項目は §6.2） |
| 認可ポリシーの編集 | 既存の BFF と同じ作り（ルールの追加・削除・並べ替え） | B・C・D |
| RADIUSクライアント | 一覧・登録・詳細・変更・削除・共有シークレットの表示 | 全員（操作は §6.2） |
| セッション | テナントの範囲のアクティブセッション | 全員 |
| テナント | 一覧・作成・変更・削除、所属の付け替え、下流にない所属の記録の点検 | D |
| アカウント | 一覧・作成・変更・無効化・削除 | B（自分のテナントの A）・D |
| 操作の記録 | provisioner の未完了の操作 | C・D（やり直し・閉じるは D） |
| 監査ログ | 本BFF の監査ログ（テナントの範囲）。D には provisioner と下流の監査ログも | B・C・D |

- daisyUI のコンポーネント（ナビゲーション、テーブル、カード、バッジ、統計、モーダル（`<dialog>`）、タブ、開閉（collapse）、テーマ）で組む。テーマはブラウザの設定（ライト / ダーク）に従う。ナビゲーションの項目は役割で出し分ける。
- 状態の表記は「接続中」「切断」、バージョンは「バージョン」と書く（§17.6）。
- 停止中の加入者は一覧と詳細でバッジ（例「停止中」）を出す。

## 10. provisioner との連携

- 本BFF は provisioner の管理クライアントとして、クライアント証明書を provisioner の `PROVISIONER_ADMIN_CLIENTS` に登録する（既存の BFF と同じ。`gen-client-cert` を持つ）。
- 操作者（`X-Operator-Id`）には、利用者のメールアドレスを渡す（§15 の 6）。provisioner と下流の監査ログには「操作者＝メールアドレス、管理クライアント＝本BFF の識別名」が残る。
- トレースID は要求ごとに採番して渡す（既存の BFF と同じ）。本BFF の監査ログにも残す。
- provisioner への書き込みのフォーム（加入者の登録・変更・削除、停止・再開、認可ポリシーの保存、RADIUSクライアントの登録・変更・削除、操作の記録のやり直し・閉じる）には `Idempotency-Key` を持たせる（§17.4 の 7）。provisioner に届かなかったときは同じキーを残し、断られたときは新しいキーにする。
- 1 回の呼び出しの上限は 60 秒、HTTP サーバーの書き出しの上限はそれより 30 秒長くする（90 秒以上。既存の BFF の §12.6 で分かったこと）。
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
- バックアップは `pg_dump` / `pg_restore`（運用ガイド 8 章）。監査ログは自動では消さない（古い行を消す手順は運用ガイド 7 章。§17.6 の 4）。
- スキーマは `0001_init.sql` のまま（ステップ 2〜6 で変えていない）。

## 12. 配置（Docker Compose）

| サービス | 内容 |
|---|---|
| `eapaka-ossbss` | 本BFF。ポートは公開しない。provisioner と同じホストなら provisioner の共有ネットワーク（`eapaka-provisioner`）にも参加する。停止の上限（10 秒）が Docker の既定の猶予と同じなので、`stop_grace_period` を 20 秒にする（PoC セットのステップ 4。2026-10-12） |
| `postgres` | 本BFF 専用。ポートは公開しない |
| `cloudflared` | Tunnel のコネクター。トークンはファイルで渡す。ヘルスチェックは付けない（イメージにシェルがない）。本BFF が healthy になってから起動する |

- 重ねる compose ファイルで、provisioner が同一ホストか別ホストかを選ぶ（既存の BFF と同じ作り。同一ホストは `compose.eapaka-provisioner.yaml` を重ねる）。
- 証明書（`certs/`）と Tunnel のトークン（`secrets/tunnel-token`）はファイルで渡し、コンテナの UID 65532 で読めるようにする。導入の手順は運用ガイド 2 章。
- 開発用の設定（§15 の 10）は compose では使えない（ループバックで待ち受けるバイナリだけ。§17.2）。compose はいつも Cloudflare Access の設定で動く。

## 13. 監査ログ

- 本BFF の監査ログに、成功した利用者の変更操作（加入者、認可ポリシー、停止・再開、RADIUSクライアント、所属、テナント、アカウント、操作の記録への操作）と、秘密の値の表示を残す。操作者のメールアドレス・役割、対象のテナント、トレースID、接続元を含める。失敗した操作は記録しない（provisioner と下流の監査ログ、アクセスログでトレースID から追える。§17.4 の 6）。操作の名前は画面仕様 §1.7。
- 所属の付け替えとアカウントのテナントの変更は、移動元と移動先のテナントにそれぞれ 1 行ずつ書く（§17.5 の 3）。
- B は自分のテナントが対象のものだけを、C・D はすべてを見る（§15 の 14）。
- provisioner と下流の監査ログ（provisioner の中継）は D だけが見る（テナントで絞れないため）。

## 14. 試験

- 単体テスト: provisioner と DB の偽物と、テスト用の鍵で作った JWT で、権限とテナントの範囲の判定を網羅する（役割 × 操作 × 自テナント / 他テナント / テナントなし。範囲の外は 404、役割にない操作は 403 で、provisioner への書き込みも監査ログもないこと）。主な保護は、外すとテストが失敗することも確かめた（§17.4、§17.5）。
- DB の結合テスト: 接続先を環境変数で指定したときだけ、実際の PostgreSQL で行う（CI ではサービスコンテナ。スキップされたら CI を失敗させる）。
- 契約テスト: provisioner と下流を固定のコミットでビルド・起動して行う（既存の BFF の CI と同じ作り。CI のジョブ「eapaka-node-provisioner との契約テスト」）。
- 脆弱性: 依存と標準ライブラリの既知の脆弱性を、別のワークフローが push と pull request のたびと毎週 govulncheck で調べる。
- 画面: 手元で provisioner と下流をバイナリで起動し、playwright で役割ごとに確かめた（確認ダイアログは Chromium・Firefox・WebKit）。
- 実機: 検証機（simwifi）で、Cloudflare の Tunnel と Access を通した通し（本物の JWT、画面からの登録・停止・再開と eapaka_test での認証）を確かめた（§17.2、§17.4、§17.6）。

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
| 7 | テナントの範囲の一覧の作り | 加入者は 1 ページを 20 件とし、所属の記録の 1 ページ分を provisioner から並行して（同時に 5 件まで）取得する。セッションは、所属の加入者ごとの取得をやめ、provisioner のセッションの一覧（上限 1000 件）を取って所属で絞る。件数が多くなったら、provisioner への一括取得の API の追加を検討する（将来拡張。§18） |
| 8 | Go の外部依存 | `github.com/jackc/pgx/v5` を加える（PostgreSQL を選んだことに伴う）。スキーマの移行のライブラリは使わず、自前で書く |
| 9 | daisyUI の取り込み | Tailwind CSS v4 のスタンドアロン CLI と daisyUI 5 のプラグインのファイルを、版を固定し、チェックサムを確かめて Docker のビルドの段で取得し、CSS を生成する。生成した CSS はリポジトリに入れない（手元の開発では同じ CLI を使う手順を用意する） |
| 10 | ドメインがない間の開発・試験 | 開発用の設定 `OSSBSS_DEV_USER_EMAIL` を設け、JWT の検証の代わりにそのメールアドレスの利用者として動かす。ループバックで待ち受けるときだけ有効にし、起動時に WARN を出す。役割ごとの確認は、DB のアカウントをその利用者に付け替えて行う。「ループバックで待ち受ける」は本BFF の待ち受けアドレス（`OSSBSS_ADDR`）で判定するので、compose（コンテナの中は `:8446`）では使えない（§17.2 の確認事項 1。2026-10-11） |
| 11 | テナント管理者のアカウント管理の範囲 | B は自分のテナントの A を作成・変更・無効化・削除できる。B 自身や他の B は D が管理する。B は A を B に昇格できない |
| 12 | 最初のシステム管理者とアカウントの無効化 | 最初の D は `OSSBSS_INITIAL_ADMIN_EMAIL` で与え、DB に登録がなければ起動時に作る（削除・降格はできない）。アカウントは削除のほかに無効化（ログインできなくする）を持つ |
| 13 | 画面での役割の呼び名 | テナントユーザー（A）、テナント管理者（B）、運用担当（C）、システム管理者（D）とする（「BFF」は利用者に馴染みのない言葉なので画面では使わない） |
| 14 | C の監査ログの閲覧 | C は本BFF の監査ログをすべて見られる。provisioner と下流の監査ログは D だけ |
| 15 | 進め方の順番 | 下流の変更（本PoC → provisioner → 既存の BFF）を先に行い、その後に本BFF を作る（§16） |

## 16. 進め方

すべて完了した（2026-10-11）。

| # | 内容 | 結果 |
|---|---|---|
| 1 | 本PoC: 停止の印（§8.2）。本PoCの流儀（feature → develop → main、ドキュメントの改版）で行う | 完了（main `dbd97f5`） |
| 2 | provisioner: 停止・再開の中継と統合リソースの `status`、API 仕様 0.3.0（§8.3） | 完了（`e96390d`） |
| 3 | 既存の BFF: 停止の状態の表示と停止・再開（§15 の 5） | 完了（`ca0bc5d`） |
| 4.1 | 本BFF: 骨組み（module、設定、HTTP サーバー、PostgreSQL とスキーマの移行、compose、Tailwind / daisyUI のビルド） | 完了（`f8cb885`。§17.1） |
| 4.2 | 本BFF: 認証（JWT の検証、JWKS、開発用の設定）と、アカウント・テナント・権限の判定 | 完了（`8145c6a`。§17.2） |
| 4.3 | 本BFF: provisioner のクライアントと契約テスト、CI | 完了（`5f3d9eb`。§17.3） |
| 4.4 | 本BFF: 画面（加入者・認可ポリシー・停止と再開・RADIUSクライアント・セッション。テナントの範囲） | 完了（`f40f15d`。§17.4） |
| 4.5 | 本BFF: 画面（テナント・アカウント・所属の付け替え・操作の記録・監査ログ・ダッシュボード） | 完了（`9a57be0`。§17.5） |
| 4.6 | 本BFF: compose（cloudflared を含む）・運用ガイド・README、実機での Cloudflare を通した通し | 完了（`ccf182b`、`1313a5e`。§17.6） |

各ステップの終わりに「作ったもの」と「実際に動かして確かめたこと」を報告して確認をもらった。

## 17. 実装の記録

### 17.1 ステップ 1: 骨組み（2026-10-10）

| 対象 | 内容 |
|---|---|
| コマンド | `eapaka-ossbss`。サブコマンド `serve`（既定。起動時に DB のスキーマを移行する）、`migrate`、`healthcheck`（同じコンテナの `/healthz` を確かめる。distroless に curl がないため、compose の healthcheck に使う） |
| 設定 | `internal/config`。`OSSBSS_ADDR`（既定 `:8446`）、`OSSBSS_LOG_LEVEL`、`OSSBSS_DB_ADDR`（既定 `postgres:5432`）、`OSSBSS_DB_NAME` / `OSSBSS_DB_USER`（既定 `ossbss`）、`OSSBSS_DB_PASSWORD`（必須）、`OSSBSS_DB_MAX_CONNS`（既定 10）。誤りはまとめて返す |
| HTTP サーバー | `internal/server`。HTTP（TLS は Cloudflare が終端する）。書き出しの上限は 90 秒（provisioner の呼び出しの上限 60 秒より長くする） |
| DB | `internal/db`。pgx v5（`pgxpool`）。接続文字列にはせず項目ごとに渡す（パスワードの記号のため）。起動時に DB の起動を 30 秒まで待つ |
| スキーマの移行 | `internal/db/migrations/NNNN_名前.sql` を `embed` し、未適用のものを番号順に 1 つのトランザクションで適用する（`schema_migrations`、`pg_advisory_xact_lock` で同時の移行を防ぐ）。DB の版がプログラムより新しければ起動しない。`0001_init.sql` は §11 のテーブルをすべて作る |
| 画面の土台 | `internal/web`。レイアウト（daisyUI の navbar・footer、テーマはブラウザの設定に従う）、トップ（DB の状態）、エラーの画面、`/healthz`、静的ファイル（ETag）、セキュリティのヘッダー（CSP 等。既存の BFF と同じ）、アクセスログ（トレースID。静的ファイルと `/healthz` は debug）、`CrossOriginProtection` |
| CSS | `scripts/build-css.sh`。Tailwind CSS v4.3.3 のスタンドアロン CLI と daisyUI v5.7.43 の `daisyui.mjs` を SHA-256 を確かめて取得し、`internal/web/css/app.input.css` から `internal/web/static/app.css` を作る（生成物と `daisyui.mjs` は `.gitignore`）。Dockerfile のビルドの段でも同じスクリプトを使う（取得したファイルはビルドのキャッシュに置く） |
| compose | `compose.yaml`（本BFF と PostgreSQL 18。ホストにポートを公開しない）、`compose.dev.yaml`（開発用。`OSSBSS_DEV_PUBLISH`、既定 `127.0.0.1:8446` に公開する。ステップ 2 でやめた。§17.2 の 1）。cloudflared はステップ 6 で加えた（§17.6） |
| CI | テスト（gofmt、vet、`go test -race`、PostgreSQL のサービスコンテナでの結合テストとスキップの検出）と、イメージのビルド・CSS の確認・compose の設定の確認 |

- スキーマの値: 役割は `tenant_user`（A）・`tenant_admin`（B）・`operator`（C）・`system_admin`（D）。A・B だけがテナントを持つことを CHECK 制約で守る。メールアドレスは小文字で、`X-Operator-Id` の形式に合うもの（64 文字まで）に限る（§15 の 6）。所属が残るテナントは削除できない（`ON DELETE RESTRICT`）。監査ログの `tenant_id` は外部キーにしない（テナントを消しても記録を残すため）。
- 版の選び方: daisyUI は公開から 2 週間以上たった版にした（v5.7.47 は公開から 10 日だった）。PostgreSQL は 19 が beta のため 18。

### 17.2 ステップ 2: 認証と権限の判定（2026-10-11）

確認事項（2026-10-11 にすべて推奨案で合意）:

| # | 事項 | 決定 |
|---|---|---|
| 1 | 開発用の設定の判定 | 本BFF の待ち受けアドレス（`OSSBSS_ADDR`）がループバック（`127.0.0.1`、`::1`、`localhost`）のときだけ有効にする。compose では使えず、バイナリを直接動かすときだけ使う。Access の設定とは同時に使えない（両方あれば起動しない）。cloudflared と同じネットワークで誤って有効になる経路をなくすため。これに伴い、ステップ 1 の開発用の `compose.dev.yaml` はやめた |
| 2 | JWT の受け取り方 | ヘッダー `Cf-Access-Jwt-Assertion` だけを使う（Cookie の `CF_Authorization` は使わない）。AUD タグはカンマ区切りで複数指定できる |
| 3 | 最初のシステム管理者 | `OSSBSS_INITIAL_ADMIN_EMAIL` のアカウントを、起動のたびにシステム管理者・テナントなし・有効にする（なければ作り、変えられていれば戻して WARN を出す。締め出されたときに元に戻す経路になる）。画面からの削除・降格・無効化は断る（ステップ 5） |
| 4 | 接続元の IP | Access の設定で動くときは `Cf-Connecting-IP` を使う（本BFF はポートを公開せず、要求は cloudflared からだけ届くので、接続元のアドレスは cloudflared のもの）。開発用の設定では接続元のアドレスを使う（ヘッダーは信用しない） |
| 5 | ステップ 2 の DB の操作 | アカウントの参照（テナントの名前つき）と最初のシステム管理者だけ。アカウント・テナントの作成・変更・削除と監査ログの書き込みは、画面と一緒にステップ 5 で作る |
| 6 | 確認の方法 | テストで鍵を作って JWT に署名し、偽物の JWKS で検証する。権限の表は全組み合わせをテストする。開発用の設定で画面を確かめる。ドメイン（`simauth.link`）を取得できたので、ステップ 2 の終わりに Tunnel と Access を通した本物の JWT でも確かめる（検証用の公開ホスト名 `ossbss-dev.simauth.link`。チームは `steep-term-c7da.cloudflareaccess.com`） |

| 対象 | 内容 |
|---|---|
| JWT の検証 | `internal/access`。RS256 だけを受け付け（`none` や HS256 は断る）、`kid`、`iss`（`https://<チームのドメイン>`）、`aud`（文字列または配列）、`exp` / `nbf` / `iat`（時計のずれ 60 秒）、`email`（空でない。サービストークンは断る）を確かめる。JWT の長さは 16KB まで |
| JWKS | `https://<チームのドメイン>/cdn-cgi/access/certs` の `keys`（RSA、RS256、2048 ビット以上）を覚えておく。1 時間ごとに取り直し、知らない `kid` でも取り直すが、取り直しは 1 分に 1 回まで（鍵がないときは 5 秒に 1 回まで）。取得できなければ覚えている鍵で続け、鍵がなければ 503。起動時に先に取得する（失敗しても起動は続ける）。本物のチームの JWKS（鍵 2 つ、`Cache-Control: max-age=14400`）を取得・解析できることを確かめた |
| 利用者の識別 | `internal/auth`（Access の JWT か開発用の設定）と、`internal/web` の `authenticate`。メールアドレスを小文字にして DB のアカウントを引き、要求ごとに役割とテナントを決める（サーバー側のセッションは持たない）。JWT がない・検証できなければ 401「Cloudflare Access を通して開いてください。」、アカウントがない・無効なら 403「この利用者（…）は登録されていません。」（Access のログアウトへのリンク「別のメールアドレスでログインし直す」つき）、公開鍵・DB を使えなければ 503。`/healthz` と静的ファイルは対象外 |
| 権限の判定 | `internal/authz`。§6.2 の表をコードの表にし、`Allowed(利用者, 操作, 対象のテナント)` で判定する（A・B は自分のテナントのものだけ、テナントなしは C・D だけ）。アカウントの管理は `CanManageAccount`（D はすべて、B は自分のテナントのテナントユーザーだけ）。メールアドレスの形式（`X-Operator-Id` に渡せるもの）もここ |
| 画面 | ナビゲーションにメールアドレス・役割の呼び名・テナントの名前・ログアウト（Access のログアウト `/cdn-cgi/access/logout`）。開発用の設定では「開発用の設定」のバッジを出し、ログアウトは出さない。トップに利用者と「できること」の表（§6.2 を利用者の役割で絞ったもの） |
| ログ | アクセスログに利用者のメールアドレスと接続元（上の 4）を出す |
| 設定 | `OSSBSS_ACCESS_TEAM_DOMAIN`、`OSSBSS_ACCESS_AUD`、`OSSBSS_INITIAL_ADMIN_EMAIL`（必須）、`OSSBSS_DEV_USER_EMAIL`。Access と開発用の設定のどちらか一方だけを受け付け、どちらもなければ起動しない。compose では Access の設定と最初の管理者が必須 |

本物の Cloudflare Access での確認（2026-10-11、simwifi）:

- 構成: 本BFF のバイナリを Access の設定でループバック（`127.0.0.1:8446`）に起動し、PostgreSQL 18 もループバックだけに公開した。cloudflared（`cloudflare/cloudflared:2026.9.3`、`--network host`、トークンはファイルで渡す）が、公開ホスト名 `ossbss-dev.simauth.link` を `http://localhost:8446` に届ける。simwifi の受信のポートは開けていない。Access のアプリは self-hosted、ログイン方法は One-time PIN（ダッシュボードの「インテグレーション → ID プロバイダー」で追加する）、ポリシーは利用者のメールアドレスだけを許可した。
- ログインしていない要求は、どのパス（`/healthz` を含む）でも Access のログイン画面へ 302 で回され、本BFF に届かなかった。偽の JWT をヘッダーに付けても同じ。
- ワンタイム PIN でログインすると、本BFF が Access の発行した JWT を検証し、利用者（最初のシステム管理者）のダッシュボードを返した（ナビゲーションのメールアドレス・役割、「できること」がすべて「すべて」）。アクセスログの接続元は `Cf-Connecting-IP`（利用者の回線の IPv6 アドレス）になった。
- アカウントを DB で無効にすると、読み直しで 403 と「別のメールアドレスでログインし直す」のボタンが出た（ログに `forbidden user`）。有効に戻すとダッシュボードに戻った。
- ログアウトのリンク（`/cdn-cgi/access/logout`）は Cloudflare が処理して「You successfully logged out.」を出し（本BFF には届かない）、その後に開き直すとログインを求められた。
- AUD タグは秘密の値ではない（ログイン画面への 302 の URL にも `kid` として入っている）。Tunnel のトークンは秘密の値なので、会話や compose のファイルには書かず、ファイルで渡す。

### 17.3 ステップ 3: provisioner のクライアントと契約テスト（2026-10-11）

確認事項（2026-10-11 にすべて推奨案で合意）:

| # | 事項 | 決定 |
|---|---|---|
| 1 | パッケージの分け方 | 1 つのパッケージ `internal/provisioner` にまとめる。既存の BFF は、本PoCに直接つなぐ場合と provisioner 経由の両方のため `provapi` / `pvapi` に分けているが、本BFF の接続先は provisioner だけなので分けない。直接つなぐための部分（本PoCの加入者の操作、接続先の種類の判定）は持たない |
| 2 | 停止・再開の `Idempotency-Key` | 送る（§10 のとおり）。既存の BFF は送らない（本PoCに直接つなぐ場合と揃えるため）が、本BFF は provisioner にだけつなぐ |
| 3 | ステップ 3 で画面に出すもの | トップに provisioner の状態のカード（provisioner と下流 2 つへの接続・版、vector-gateway の AVクライアント、未完了の操作の件数）。運用担当とシステム管理者だけ（§6.2 の「provisioner の状態の閲覧」） |
| 4 | 証明書のファイル名 | `certs/provisioner-client.pem`（本BFF のクライアント証明書と秘密鍵）、`certs/provisioner-server.pem`（provisioner のサーバー証明書） |
| 5 | provisioner の版の扱い | `/status` で停止・再開に対応しているかを判定しない（版の文字列は `dev` のこともある）。加入者の `status` がない応答なら、画面で対応していない旨を出す（ステップ 4） |
| 6 | 確認の方法 | 単体テスト、手元で下流と provisioner をバイナリで起動した契約テスト、`check-provisioner`・トップのカード。simwifi での通し（加入者を作って eapaka_test で認証）は、画面ができるステップ 4 で行う |

| 対象 | 内容 |
|---|---|
| クライアント | `internal/provisioner`。既存の BFF の `provapi` と `pvapi` から作った。mTLS（クライアント証明書の提示、provisioner のサーバー証明書の固定とホスト名の検証）、`X-Operator-Id`（利用者のメールアドレス。書き込みと秘密の値の取得は、なければ送らない）、`X-Trace-ID`、`Idempotency-Key`（書き込みはすべて引数で受け取る。空なら送らない）、ProblemDetails（provisioner の拡張項目 `downstream`、`operationId` 等を含む）、接続できないときの原因の見当（`Diagnose`。本BFF の設定の名前で案内する）。1 回の呼び出しの上限は 60 秒、接続の確立は 10 秒 |
| パスの値 | 要素ごとにエスケープし（`/` もエスケープする）、空・`.`・`..` は送らない（provisioner の `internal/downstream` と同じ作り。既存の BFF は `url.JoinPath` で、要素の中の `/` をエスケープしていなかった） |
| 呼び出し | §10 のすべて: 状態、加入者の統合操作（一覧・取得・作成・変更・削除・Ki / OPc）、操作の記録（一覧・取得・やり直し・閉じる）、認可ポリシー（一覧・取得・PUT・停止と再開・削除）、RADIUSクライアント（一覧・IP での検索・登録・取得・変更・削除・共有シークレット）、セッション、監査ログ 3 種類（provisioner、provisioning-api、aka-only-server） |
| コマンド | `gen-client-cert`（provisioner の `.env` の `PROVISIONER_ADMIN_CLIENTS` に貼る行を出す）、`check-provisioner`（下流 2 つ・AVクライアント・PLMN マップ・未完了の操作を表示。つながらなければ原因の見当を出してエラー）。起動時にも provisioner を確かめてログに出す（つながらなくても起動は続ける） |
| 設定 | `OSSBSS_PROVISIONER_URL`（既定 `https://eapaka-provisioner:9446/admin/v1`）、`OSSBSS_PROVISIONER_CLIENT_CERT`（既定 `/certs/provisioner-client.pem`）、`OSSBSS_PROVISIONER_CLIENT_KEY`、`OSSBSS_PROVISIONER_SERVER_CERT`（既定 `/certs/provisioner-server.pem`）、`OSSBSS_PROVISIONER_TIMEOUT`（既定 60s）。HTTP サーバーの書き出しの上限は、呼び出しの上限＋30 秒（90 秒以上） |
| compose | `certs/` を読み取り専用でマウントする。同一ホスト用に `compose.eapaka-provisioner.yaml`（provisioner の共有ネットワーク `eapaka-provisioner` に本BFF だけが参加する）を重ねる。`.env.example` の既定は同一ホスト |
| 画面 | トップの provisioner の状態のカード（上の 3）。aka-only-server に接続できないときは AVクライアントの行を出さない（確かめられていないため）。provisioner に接続できないときは原因の見当を出す。DB の確認の上限時間は呼び出しごとに取る（provisioner の応答を待つ間に使い切らないように。手元の確認で見つけた） |
| テスト | 単体テスト（偽物の provisioner の mTLS サーバー `provisionertest` で、各操作の要求の中身、操作者の要否、パスのエスケープ、エラー、接続できないとき・サーバー証明書の不一致の原因の見当）。契約テスト（`OSSBSS_TEST_PROVISIONER_*` を与えたときだけ。既存の BFF の契約テストから作り、停止・再開を同じキーで送り直すと最初の応答が返ること、違う内容なら 422 になることを足した）。CI のジョブ「eapaka-node-provisioner との契約テスト」は、provisioner `e96390d`・本PoC `dbd97f5`・aka-only-server `2a6dcf8` を固定のコミットでビルドして相手にする |

### 17.4 ステップ 4: 加入者・認可ポリシー・停止と再開・RADIUSクライアント・セッションの画面（2026-10-11）

確認事項（2026-10-11 にすべて推奨案で合意。8 は「テナントなし」の絞り込みを加えた案で合意）:

| # | 事項 | 決定 |
|---|---|---|
| 1 | 認可ポリシーの画面の置き場所 | 認可ポリシーだけの一覧は作らず、加入者の詳細から編集画面 `/subscribers/{imsi}/policy` を開く。任意の IMSI に認可ポリシーだけを作る機能と、認可ポリシーだけを削除する機能は持たない（§6.2 にない。認証を止めたければ停止を使う）。食い違い `POLICY_MISSING` の加入者は編集画面で作成できる |
| 2 | A・B が範囲外のものを開いたとき | 他のテナントやテナントなしの IMSI・ID には、存在しないときと同じ 404 を返す（存在の有無を漏らさない）。POST でも同じ。範囲の中でも役割にない操作は 403 |
| 3 | 登録・削除と所属の記録の順番 | provisioner の操作を先に行い、成功してから DB に書く（登録は upsert。テナントなしなら記録を消す。削除は記録を消す）。DB への書き込みに失敗するとテナントなしとして残る（画面で警告し、D が付け替える。ステップ 5）。他のテナントに見えてしまうことがない |
| 4 | 確認ダイアログの作り | daisyUI の `<dialog class="modal">` を HTML の Invoker Commands（`<button command="show-modal" commandfor="…">`）で開く。JavaScript を書かず、CSP にも合う。確認のボタンは送るフォームを `form` 属性で指す。ダイアログの中の `<form method="dialog">` はフォームの入れ子にならないよう、送るフォームの外に置く |
| 5 | 停止・再開のボタンの場所 | 加入者の詳細と認可ポリシーの編集画面。一覧はバッジだけ |
| 6 | 本BFF の監査ログ | ステップ 4 で書き込みを作る（成功した変更操作と秘密の値の表示）。閲覧の画面はステップ 5。失敗した操作は記録しない。監査ログの書き込みだけが失敗したときは、画面には成功を出し、ERROR のログを出す |
| 7 | `Idempotency-Key` を付ける範囲 | §10 の操作に加えて、RADIUSクライアントの登録・変更・削除と認可ポリシーの保存にも付ける（provisioner はすべての書き込みで受け付ける） |
| 8 | C・D の一覧の絞り込み | 加入者・RADIUSクライアント・セッションにテナントの絞り込み（「すべて」「テナントなし」と各テナント）。加入者の「テナントなし」は provisioner の一覧を 500 件ずつ読んで所属の記録のないものを残し、1 回の表示で 5,000 件までを調べる（続きは「続きを探す」。provisioner の続きの位置は IMSI なので、調べ終えた IMSI から再開できる）。ステップ 5 で、「テナントなし」の一覧からまとめて割り当てる機能と、下流にない所属の記録をまとめて消す機能（どちらも D）を加える（孤立したデータの対応の省力化） |
| 9 | 実機での確認 | simwifi で provisioner・本PoC・aka-only-server を compose で動かし、本BFF は開発用の設定でループバックに起動して、SSH のポート転送と playwright で操作する。Access を通した画面での停止・再開の 1 往復は、ユーザーのブラウザで行う |

| 対象 | 内容 |
|---|---|
| 画面仕様 | `docs/screen-spec.md` を作った（共通の作法、加入者、認可ポリシー、停止・再開、RADIUSクライアント、セッション）。ステップ 5 の画面はそこに書き足す |
| DB | `internal/db/assignments.go`（テナントの一覧、所属の記録の参照・テナントごとのページと件数・IMSI や ID からの一括の参照・upsert と削除）、`internal/db/audit.go`（監査ログの書き込み。内容は JSONB、テナントなしは NULL）。スキーマは変えていない（`0001` のまま） |
| 範囲の判定 | `internal/web/scope.go`。要求ごとに所属の記録を引いて `authz.Allowed` を通す（`subscriberScope` / `clientScope`）。範囲の外は 404（存在しないときと同じ文）、役割にない操作は 403。provisioner の操作者には利用者のメールアドレスを入れる（`authenticate` で `provisioner.WithOperator`） |
| 加入者 | 一覧（A・B は所属の記録から 20 件ずつ並行して取得、C・D は provisioner の一覧にテナント名を添えて 50 件ずつ、テナントの絞り込み、「テナントなし」の探索）、登録（所属のテナントは「選んでください」から必ず選ぶ、PLMN マップの説明、allow の確認ダイアログ）、詳細（A・B には鍵の置き場所・AMF・SQN・食い違いを出さない）、Ki / OPc の表示、変更（C は Ki / OPc だけ。C が AMF などを送ってきたら 403。D は AMF / SQN / SQN の増加タイプ / 平文HTTP の許可も）、削除（所属の記録も消す）、停止・再開 |
| 認可ポリシー | 既存の BFF の編集画面と同じ作り（ルールの追加・削除・並べ替え、allow の確認、`invalidParams` の対応付け、停止・再開で下書きを残す）。B には鍵の有無を出さない |
| RADIUSクライアント | 一覧（所属で絞る、下流にない所属の記録の行）、登録（B は自分のテナント、C・D は選ぶ）、詳細、変更、共有シークレットの表示、削除（所属の記録も消す） |
| セッション | A・B と C・D のテナントの絞り込みは、provisioner の新しい 1000 件を取って所属で絞り、100 件まで出す。C・D の「すべて」は新しい 100 件。`?imsi=` は範囲を確かめてから provisioner の絞り込みを使う |
| エラーの文 | A・B には操作の記録の ID や設定の名前を出さず、「運用担当に連絡してください」などにする（画面仕様 §1.6）。操作の記録の ID は C・D にだけ文字で出す（リンクはステップ 5） |
| 画面の部品 | ナビゲーション（ダッシュボード・加入者・RADIUSクライアント・セッション）、`partials/common.html`（成功とエラーの文、確認ダイアログ、停止・再開、テナントの選択と絞り込み）。daisyUI 5 の書き方（`fieldset` / `fieldset-legend` / `label`。v4 の `form-control` / `label-text` は使えない） |
| 静的ファイルの URL | 画面は CSS と htmx を内容のハッシュつきの URL（`/static/app.css?v=…`）で参照する（`assetURL`）。Access を通した確認で、ブラウザか Cloudflare のエッジに残っていたステップ 2 の古い CSS が使われ、メニューとダイアログが崩れた（左上に寄った）ため。配信は今までどおり ETag と `no-cache` |
| テスト | provisioner と DB の偽物で、対象を持つすべての操作を役割 × 対象のテナント（自分・他・テナントなし）で網羅（範囲の外は 404、役割にない操作は 403、どちらも provisioner に書き込みを送らず監査ログも書かない）、登録の画面の役割、各画面の表示と操作、二重送信のキー（届かなければ残す）、所属の記録・監査ログの失敗、「テナントなし」の探索の上限と続き。DB の新しい関数は PostgreSQL の結合テスト |

手元での確認（2026-10-11）:

- CI と同じ固定のコミットの provisioner・本PoC・aka-only-server をバイナリで起動し、本BFF（開発用の設定）を playwright-cli で操作した。登録（テナントの選択、allow の確認ダイアログの「やめる」と確認）、詳細、停止・再開（本PoCの Valkey の `status` が変わること）、Ki / OPc の表示、認可ポリシーの編集（ルールの追加・並べ替え・保存。保存しても停止は解けないこと、下書きを持ったまま再開しても保存されないこと）、役割ごとの一覧・詳細（A の範囲の外は 404、A の認可ポリシーの編集は 403、B の RADIUSクライアントの登録は B のテナントに属すること、他のテナントの IP の重複の文、C の「テナントなし」の絞り込みと AMF の 403、D の aka-only-server の加入者の SQN の増加タイプの変更）、削除と所属の記録、監査ログ（トレースID が provisioner の監査ログと一致すること）、provisioner を止めたときの A と D の文、ライト / ダーク。
- 確認ダイアログは Chromium、Firefox 156、WebKit（Safari 26.6 相当）で開けることを確かめた（WebKit は、手元の WSL にシステムのライブラリを入れてから、開く・「やめる」で閉じる・停止・再開を確認）。

simwifi での確認（2026-10-11）:

- 本PoC（`dbd97f5`。vector-gateway を aka-only-server に接続、PLMN `44010` を aka-only-server、RADIUS のポートはループバック）、aka-only-server（`2a6dcf8`）、provisioner（`e96390d`）を compose で同一ホストに導入（provisioner の運用ガイド 2 章の手順）。本BFF は手元でクロスビルドしたバイナリを、開発用の設定でループバックに起動し（PostgreSQL もループバック）、provisioner には `https://127.0.0.1:9446/admin/v1` で接続した。
- 本BFF の画面から RADIUSクライアント（compose のネットワークのゲートウェイ、テナントA）と、置き場所の違う 2 人の加入者（`440200000000001` は本PoC・テナントA、`440100000000001` は aka-only-server・テナントB）を登録し、認可ポリシーのルールを編集して、eapaka_test で両方 Access-Accept。
- テナントユーザー（テナントA）で `440200000000001` を停止すると Access-Reject（テナントB の加入者は Accept のまま。テナントB の加入者の詳細は 404）。テナント管理者（テナントB）で `440100000000001` を停止すると Access-Reject。auth-server に `AUTH_SUBSCRIBER_SUSPENDED`。それぞれ再開すると Access-Accept に戻った。
- セッションの画面は、テナントユーザーには自分のテナントの加入者のものだけ、システム管理者にはすべて（テナント名つき）が出た。
- Cloudflare Access を通した確認: 本BFF をもう 1 つ Access の設定でループバック（`127.0.0.1:8446`。同じ DB）に起動し、cloudflared（`--network host`）で `ossbss-dev.simauth.link` を届けた。ユーザーがブラウザでワンタイム PIN でログインし（アカウントはテナントA のテナントユーザーに付け替えた）、`440200000000001` を停止すると eapaka_test で Access-Reject、再開すると Access-Accept。本BFF の監査ログに `policy.suspend` / `policy.resume`（操作者のメールアドレス・役割、接続元は `Cf-Connecting-IP`）。1 回目は古い CSS でメニューとダイアログが崩れたので、上の「静的ファイルの URL」の対策をして入れ替え、2 回目で直ったことをユーザーが確かめた（メニューが横に並び、ダイアログが中央に出る）。
- 終了後に simwifi のコンテナ・ボリューム・イメージ・作業ファイルを片付けた（Tunnel のトークン `~/aka-work/ossbss-dev/tunnel-token` だけ残す）。

### 17.5 ステップ 5: テナント・アカウント・所属の付け替え・操作の記録・監査ログ・ダッシュボードの画面（2026-10-11）

確認事項（2026-10-11 にすべて推奨案で合意）:

| # | 事項 | 決定 |
|---|---|---|
| 1 | 所属の付け替えの作り | 加入者と RADIUSクライアントの詳細に「所属の変更」（確認ダイアログ）、一覧に D 用の行のチェックボックスと「選んだものの所属を変える」（加入者は 50 件、RADIUSクライアントは 200 件まで）。どの絞り込みでも使え、「テナントなし」の一覧からまとめて割り当てるのにも使う。付け替える前に provisioner にあることを確かめ、ないものは飛ばす |
| 2 | 下流にない所属の記録の点検 | D 用の画面 `/assignments/orphans`。加入者は所属の記録を IMSI の順に読んで provisioner に並行して（同時に 5 件まで）問い合わせ、1 回の表示で 200 件まで調べる（続きは調べ終えた IMSI から）。RADIUSクライアントは全件を突き合わせる。チェックボックスでまとめて消し、消す直前に provisioner にないことを確かめ直す |
| 3 | 付け替えの監査ログ | 移動元と移動先が別のテナントなら、それぞれのテナントに 1 行ずつ書く（テナントなしの側には書かない）。アカウントのテナントの変更も同じ |
| 4 | 自分自身のアカウントの操作 | 自分の削除・無効化・役割やテナントの変更は断る。D が別の D を降格させるのは許す。最初のシステム管理者の保護は決定済みのとおり |
| 5 | ダッシュボードの「停止中の数」 | 画面を出した後に htmx で読み込む（`GET /dashboard/suspended`）。provisioner の認可ポリシーの一覧を 500 件ずつ、10,000 件まで読み、A・B は所属の記録と突き合わせる |
| 6 | 監査ログの絞り込み | C・D はテナント（すべて・テナントなし・各テナント）と操作の種類、B は操作の種類だけ（自分のテナントに固定） |
| 7 | B のアカウントの一覧 | 自分のテナントのアカウントをすべて出し（テナント管理者を含む）、操作できるのはテナントユーザーだけ |
| 8 | 確認の方法 | 手元のバイナリ（provisioner・下流・本BFF）と playwright。simwifi での通しはステップ 6 |

| 対象 | 内容 |
|---|---|
| 画面仕様 | `docs/screen-spec.md` に §6〜§11（ダッシュボード、所属の付け替えと点検、テナント、アカウント、操作の記録、監査ログ）を足した。§1 の共通の作法（ナビゲーション、操作の記録へのリンク、監査ログの操作）も直した |
| DB | `internal/db/tenants.go`（件数つきの一覧、作成・名前の変更・削除。名前の重複は `ErrConflict`、所属やアカウントが残るテナントは `ErrInUse`。RESTRICT の外部キーの SQLSTATE は 23001）、`accounts.go`（一覧・取得・作成・役割とテナントの変更・無効化・削除。変更は画面を開いた時点の役割・テナントを条件にして、他の操作で変わっていたら何もしない）、`assignments.go`（点検用の記録の読み出し）、`audit.go`（監査ログの読み出し。テナント・操作・続きの位置で絞る）。スキーマは変えていない |
| 所属の付け替え | `internal/web/assign.go`。一覧へ戻すときの結果は、文ではなく件数と誤りの種類をクエリで渡し、文はサーバーで作る（任意の文を画面に出させない）。戻り先は一覧のパスだけを受け付ける |
| テナント | `internal/web/tenant.go`。一覧（件数は各一覧の絞り込みへのリンク）、作成、名前の変更、削除（残っている件数を出して断る） |
| アカウント | `internal/web/account.go`。B は自分のテナントのテナントユーザーだけ（他のテナントのアカウントは 404、自分のテナントのテナント管理者は 403）。作成した後に Cloudflare Access のポリシーへの追加を案内する。最初のシステム管理者（`web.Options.InitialAdminEmail`）と自分自身は、変更・削除を断る |
| 操作の記録 | `internal/web/operation.go`。既存の BFF と同じ作りに、IMSI のテナント名を添えた。やり直し・閉じるは D だけで、監査ログの対象のテナントは IMSI の所属 |
| 監査ログ | `internal/web/audit.go`。本BFF の監査ログ（50 件ずつ、「さらに古いものを表示」）と、D の provisioner・provisioning-api・aka-only-server のタブ（ジェネリックな共通の処理） |
| ダッシュボード | `internal/web/dashboard.go`。範囲の件数（A・B は所属の記録、C・D は認可ポリシーの件数とテナントの数）と、後から読み込む停止中の数。未完了の操作があれば先頭に知らせる。「できること」は CSS だけで開閉する collapse |
| ナビゲーション | 役割で項目を出し分ける（`navItems`）。テナント（D）、アカウント（B・D）、操作の記録（C・D）、監査ログ（B・C・D） |
| テスト | ステップ 5 のすべてのルートの役割の確認（許可しない役割は 403 で、provisioner への書き込みも監査ログもない）、アカウント管理（B の範囲、昇格の禁止、最初のシステム管理者・自分自身の保護、他の操作での変更）、テナント、所属の付け替え（両方のテナントへの監査ログ、provisioner にないものを飛ばす、戻り先の検査、クエリの文を出さない）、点検（確かめ直し、200 件ずつ）、操作の記録、監査ログの範囲、ダッシュボード（範囲ごとの停止中の数、上限）。保護を外すとテストが失敗することも確かめた。DB の新しい関数は PostgreSQL の結合テスト |

手元での確認（2026-10-11）:

- CI と同じ固定のコミットの provisioner・本PoC・aka-only-server をバイナリで起動し、本BFF（開発用の設定。開発用の利用者を最初のシステム管理者にした）を playwright-cli で操作した。
- テナントの作成（重複の誤り）、アカウントの作成と変更（役割・テナント。変更前と変更後のテナントに監査ログ）・無効化、最初のシステム管理者で自分の詳細に「変更・削除できません」が出てボタンがないこと。
- 「テナントなし」の一覧で 2 件をチェックしてテナントB にまとめて割り当てた（確認ダイアログ、戻り先の一覧に件数の文、監査ログ）。
- provisioner の API で直接削除した加入者と RADIUSクライアントが点検の画面に出て、まとめて消せた（記録のテナントに監査ログ）。
- provisioner は削除の前に下流 2 つを確かめるので、aka-only-server を止めても未完了の操作はできなかった。そのため provisioner の Valkey に失敗の操作の記録を置いて確かめた。一覧（テナント名つき）、同じ IMSI の操作の 409（D には操作の記録へのリンク、テナント管理者には「運用担当に対応を依頼してください」）、詳細からのやり直し（完了になり、監査ログの対象のテナントは IMSI の所属）。
- テナント管理者の監査ログに自分のテナントのもの（テナントA から移したアカウントの変更を含む）だけが出て、provisioner と下流のタブが出ないこと。D の 3 つのタブ。
- ダッシュボード（D: テナント・加入者・停止中・RADIUSクライアント、provisioner のカード。テナントユーザー: 自分のテナントの件数、ダーク）。

### 17.6 ステップ 6: compose（cloudflared）・運用ガイド・README・実機での確認（2026-10-11）

確認事項（2026-10-11 にすべて推奨案で合意）:

| # | 事項 | 決定 |
|---|---|---|
| 1 | Tunnel のトークンの渡し方 | `.env` の `TUNNEL_TOKEN` ではなく、ファイル `secrets/tunnel-token`（`chmod 600`、所有者は UID 65532）を cloudflared に読み取り専用でマウントし、`--token-file` で渡す（`.env` だと `docker compose config` の出力やコンテナの環境変数に出るため）。ファイルがなければ起動しない（`create_host_path: false`） |
| 2 | 公開ホスト名の向け先 | `http://eapaka-ossbss:8446`（compose のネットワークの中の本BFF）。検証の `ossbss-dev.simauth.link` もこの向け先に変えた（ユーザーがダッシュボードで変更） |
| 3 | cloudflared のヘルスチェック | 付けない（イメージにシェルや curl がない）。状態は `docker compose logs cloudflared` と Cloudflare のダッシュボードで確かめる |
| 4 | 監査ログの保持 | 自動では消さない。運用ガイドに古い行を消す SQL と、先にバックアップを取る手順を書く |
| 5 | 運用ガイドの Cloudflare の手順 | ステップ 2 で行った手順を元に書き、新規作成からの通しは行わない（公開ホスト名の向け先の変更だけを確かめた） |
| 6 | simwifi での確認の分担 | 私: compose での導入（運用ガイドどおり）、ヘルスチェック、ポートを公開しないこと、移行、最初のシステム管理者、`check-provisioner`、JWT なしの要求の 401、バックアップと復元、更新。ユーザー: Access でのログインと画面の操作（テナント・RADIUSクライアント・加入者 2 人の登録、停止・再開、監査ログ、ダッシュボード） |

| 対象 | 内容 |
|---|---|
| compose | `compose.yaml` に `cloudflared`（`cloudflare/cloudflared:2026.9.3`、`tunnel --no-autoupdate run --token-file /etc/cloudflared/token`、本BFF が healthy になってから起動、ポートを公開しない）を加えた |
| 運用ガイド | `docs/operation-guide.md`（構成、導入（Cloudflare の準備、`.env`、provisioner との証明書のやり取り、Tunnel のトークン、起動、最初のログイン）、別ホストの provisioner、公開範囲と安全性、アカウントの運用、テナントと所属の運用、ログと監査ログ、バックアップと復元、更新、障害時の確認、環境変数） |
| README | 機能（画面と役割の表）、導入と運用（運用ガイドへの案内）、開発 |
| CI | compose の設定の検査に、サービスが 3 つ（本BFF・PostgreSQL・cloudflared）であること、トークンをファイルで渡すこと（`--token-file`、`create_host_path: false`、`TUNNEL_TOKEN` がないこと）を加えた |
| 画面の表記 | ユーザーの確認での指摘で、ダッシュボードの状態のバッジを「接続できます」→「接続中」、「接続できません」「provisioner から接続できません」→「切断」（続けて原因の見当）に、「版」→「バージョン」（「スキーマのバージョン」）に変えた（「接続できます」は、まだ接続していないがいつでも接続できる状態とも読めるため） |

simwifi での確認（2026-10-11）:

- 本PoC（`dbd97f5`。vector-gateway を aka-only-server に接続、PLMN `44010` を aka-only-server）・aka-only-server（`2a6dcf8`）・provisioner（`e96390d`）を compose で導入した後、本BFF を運用ガイド 2 章の手順どおりに compose で導入した（作業ツリーを tar で転送）。
- 導入の途中で、provisioner は管理クライアントがないと起動しないことと、本BFF の `COMPOSE_FILE` の外部ネットワーク（`eapaka-provisioner`）がないとビルドと `docker compose run` が失敗することに合わせて、手順を「`-f compose.yaml` でビルドと証明書の作成 → provisioner に登録して起動 → サーバー証明書の取り出し」の順に直した。
- 本BFF・PostgreSQL が healthy、cloudflared が Cloudflare に 4 本接続し、ホストにポートを公開していないこと（`ss` でも 8446 の待ち受けなし）。起動ログの移行・最初のシステム管理者・Access の公開鍵（2 つ）・provisioner の確認。`check-provisioner` で下流 2 つ・AVクライアント・PLMN マップ。
- JWT なしの要求: compose のネットワークと `eapaka-provisioner` のネットワークのコンテナから 401（偽の JWT と `Cf-Access-Authenticated-User-Email` を付けても 401）、`/healthz` は 200。インターネットからはログインしていなければ Access のログイン画面へ 302。
- ユーザーがブラウザで Access を通してログインし（最初のシステム管理者）、テナント・RADIUSクライアント（compose のネットワークのゲートウェイ）・加入者 2 人（本PoC、aka-only-server。既定 allow の確認ダイアログ）を登録し、`440200000000001` を停止すると eapaka_test で Access-Reject（aka-only-server の加入者は Accept）、再開すると Accept。本BFF の監査ログに操作者・役割・テナント・接続元（`Cf-Connecting-IP`）つきで記録された。監査ログ（下流のタブを含む）とダッシュボードの件数も期待どおり。
- バックアップ（`pg_dump -Fc`）の後に変更（テナントの追加、所属の記録の削除）してから復元（`pg_restore --clean --if-exists`）すると元に戻った。バックアップのファイルが 664 になったので、運用ガイドの手順を `umask 077` つきに直した。
- 更新（作業ツリーを転送して `OSSBSS_VERSION=… docker compose up -d --build`）で、本BFF だけが作り直され（移行は不要で `applied` は空）、cloudflared は接続を保った。

## 18. 将来の拡張（大規模化と一括の操作）

2026-10-11 に一括取得の API を検討して記録し（`9b9adcb`）、同日、加入者の一括投入を加えたうえで、目標の規模と運用の条件（§18.1）を置いて整理し直した。記録だけで、実装はしていない。PoC の規模を超えるので、本BFF だけでなく本PoC・aka-only-server・provisioner の作りも対象にする。

### 18.1 前提（2026-10-11 にユーザーが置いた条件）

| 項目 | 値 |
|---|---|
| 加入者の総数 | 200,000 |
| RADIUSクライアント | 1,000 |
| 認可ポリシーのルール | 加入者ごとに平均 1.5 件（大半は 1 件、時々 2 件、まれに 4 件）。認可ポリシーは加入者ごとに 1 つ（`policy:{IMSI}`）なので、その中のルールの件数と読む |
| 一括投入 | CSV で 1 回 10,000 件まで。投入できるのは運用担当（C）とシステム管理者（D） |
| 外部からの操作 | 本PoC の Admin TUI、既存の BFF、aka 版 GUI などからの、加入者・認可ポリシー・RADIUSクライアントの変更を原則禁止する（変更は本BFF → provisioner の経路だけ） |

決まっていないもの（§18.7 の確認事項）: 同時のセッション数、テナントの数とテナントあたりの加入者の最大、10,000 件の投入にかけてよい時間。以下では、セッションは数万件以上になりうるものとして書く。

### 18.2 この規模での今の作りの評価

下流の作り（評価の元になる事実）:

- 本PoC の一覧（加入者・認可ポリシー・RADIUSクライアント・セッション）は、要求のたびに `SCAN MATCH` でキーを集めて並べ替え、その中の 1 ページを返す（`pkg/masterdata/page.go` の `listHashPage`、`session.go` の `List`）。`SCAN` は MATCH の条件によらずキー空間全体をたどる。加入者 20 万件では `sub:` と `policy:` だけで 40 万キーあり、COUNT 100 なので 1 回の一覧で Valkey と約 4,000 往復し、20 万件を並べ替える。RADIUSクライアント（1,000 件）の一覧も、同じだけたどる。ページを重ねて全件を読むと、件数の 2 乗に比例する。
- aka-only-server の加入者の一覧は、IMSI の ZSET の索引を読むので、件数によらない（`internal/store/subscriber.go` の `ListSubscribers`）。
- 本PoC のセッションの一覧は、IMSI を指定しなければ全セッションを読んで並べ替え、新しい順に 1000 件までを返す。IMSI を指定すると索引（`idx:user:{IMSI}`）を使う。
- auth-server・acct-server はキーを直接引き、一覧を使わない（加入者の件数は認証の重さに効かない）。
- provisioner の 1 件の取得は下流に 3 回問い合わせる。一覧は下流の 3 つの一覧（本PoC の加入者・認可ポリシー、aka-only-server の加入者）を合わせる。

| 場面（本BFF） | 今の作り | 20 万件での見込み | 評価 |
|---|---|---|---|
| A・B の加入者の一覧（C・D のテナントの絞り込みも同じ） | 所属の記録を 20 件読み、1 件ずつの取得を 20 回（下流 60 回） | 1 ページの重さは件数によらない | ○（一括取得で軽くなる） |
| C・D の加入者の一覧「すべて」 | provisioner の一覧を 50 件ずつ | 1 ページごとに本PoC の全キーを 2 回たどる | △ |
| C・D の「テナントなし」の探索 | provisioner の一覧を 500 件ずつ、1 回の表示で 5,000 件まで | 1 回の表示で全キーを 20 回たどる。全体を調べるのに 40 回の表示 | × |
| ダッシュボードの停止中の数 | 認可ポリシーの一覧を 500 件ずつ、10,000 件まで | 20 万件の 1/20 しか数えられない | × |
| ダッシュボードの C・D の加入者数・RADIUSクライアント数 | provisioner の一覧の件数 | 表示のたびに全キーをたどる | △ |
| RADIUSクライアントの一覧（全員） | provisioner の全件（500 件ずつ 2 回）を所属で絞る | 表示のたびに全キーを 2 回たどる | △ |
| A・B のセッション（C・D のテナントの絞り込みも同じ） | 新しい 1000 件を取って所属で絞る | セッションが 1000 件を超えると、自分のテナントのセッションが漏れる | × |
| C・D のセッション「すべて」 | 新しい 100 件 | 表示のたびに全セッションを読んで並べ替える | △ |
| 下流にない所属の記録の点検 | 所属の記録を 200 件ずつ、1 件ずつの取得で確かめる | 全体で 1,000 回の表示 | × |
| まとめての付け替え | 50 件まで、1 件ずつの存在確認 | 件数によらない | ○ |
| 加入者の登録 | 1 件ずつ | 10,000 件の投入はできない | × |

○ は問題なし、△ は使えるが待ちが出る、× は正しくないか運用にならない。いずれも時間は計っていない（§18.6 の 1 で計る）。

そのほかの制約:

- 監査ログの Stream は、provisioner（`PROVISIONER_AUDIT_MAX`）・本PoC（`PROVISIONING_API_AUDIT_MAX`）とも既定で約 10,000 件、aka-only-server にも上限がある。10,000 件の投入 1 回で入れ替わる（標準出力の JSON が正本）。
- provisioner は作成のたびに、未完了の操作（`ops:active`）を全件読む。投入の途中で下流が止まり未完了の操作が増えると、1 件ごとに重くなる。
- provisioner での 1 件の作成は、IMSI のロック、操作の記録の保存 3 回、下流への要求 5 回（存在確認の GET 3 本を並行、置き場所への POST、認可ポリシーの PUT）。整合（両方のノードにできるか、元に戻すか）は 1 件ごとに取れるが、複数の件をまとめて元に戻す仕組みはない。
- 1 回の要求の時間: provisioner の書き出し 60 秒、本BFF の provisioner の呼び出し 60 秒・書き出し 90 秒、Cloudflare のプロキシーはオリジンの応答を 100 秒待つと 524 を返す（Free プランでは変えられない）。

### 18.3 外部からの操作の原則禁止で変わること

今の作り（§7）は、本BFF の外で加入者が作られ、消されることを前提にしている（「テナントなし」の探索、下流にない所属の記録の点検、一覧を毎回下流から読む）。変更の経路が本BFF → provisioner だけになれば、本BFF の DB に**加入者の台帳**を持てる。

- `subscriber_tenants` を広げ、IMSI ごとに所属（テナントなしも行として持つ）、鍵の置き場所、停止の状態、登録の日時と操作者を持つ。Ki / OPc、SQN、認可ポリシーの中身は持たない。正本は下流のままで、台帳は一覧・件数・絞り込みのための写しとする。
- 台帳で済むもの: 一覧のページ送りと絞り込み（テナント、テナントなし、停止中）、件数（ダッシュボードの加入者数・停止中の数）、一括投入の前の「既にある」の確認。下流の一覧をたどらなくなる。
- 一覧の表示の項目のうち、SQN・AMF・ルールの件数・食い違いは、ページの分だけ下流から読む（一括取得。§18.4）。
- 台帳と下流のずれは、失敗（provisioner の成功の後の DB の書き込みの失敗、補償の途中の操作）と例外の操作からだけ生じる。定期の突き合わせ（§18.4）で見つけて、D が直す。
- 設計概要 §1 の「本BFF の DB に持つのは、テナント・利用者・所属・監査ログだけ」を改めることになる（台帳を加える）。

原則禁止を守らせる手段:

| 対象 | 手段 |
|---|---|
| 既存の BFF（provisioner 経由） | provisioner の `PROVISIONER_ADMIN_CLIENTS` から外す |
| 既存の BFF（直接接続）など、本PoC の Provisioning API の他の利用者 | `PROVISIONING_API_ADMIN_CLIENTS` を provisioner だけにする |
| aka 版 GUI | aka-only-server の `AKA_ADMIN_CLIENTS` を provisioner だけにする。AVクライアントと AV用サーバー証明書の管理には aka 版 GUI が要る（provisioner は扱わない）ので、その作業の間だけ戻すか、加入者の変更をしない決まりにする |
| 本PoC の Admin TUI | ホストで Valkey を直接操作するので、技術的には止められない。運用の決まりにして、定期の突き合わせで見つける。閲覧と保守（セッションの索引の掃除など）は許す |

障害対応などで例外の操作をしたときは、その後に突き合わせを手で走らせる。

### 18.4 一括取得・一覧の索引・突き合わせ

この規模では、次のものが要る（§18.2 の × と △ を解く）。

| # | 内容 | 効果 | 変更 |
|---|---|---|---|
| L1 | 下流の一括取得: 本PoC の `POST /subscribers/lookup`・`POST /policies/lookup`（IMSI を 500 件まで渡し、パイプラインでまとめて読む。Ki / OPc は返さない）、aka-only-server の `POST /subscribers/lookup`、provisioner の `POST /subscribers/lookup`（3 つを合わせて統合した加入者と、見つからなかった IMSI を返す） | 一覧の 1 ページの下流への問い合わせが 60〜150 回から 3 回になる。突き合わせは 20 万件を 400 回で読める | 本PoC、aka-only-server、provisioner、本BFF |
| L2 | 本PoC の一覧の索引: 加入者・認可ポリシー・RADIUSクライアントの ZSET を、`pkg/masterdata` の書き込み（Provisioning API と Admin TUI の共通の部品）で保つ。一覧は ZSET を読む（aka-only-server と同じ作り）。索引がなければ起動時に作る | すべての一覧が件数によらなくなる（RADIUSクライアントの一覧、provisioner の一覧、Admin TUI の閲覧を含む）。台帳の初期化と、突き合わせの逆向き（下流にあって台帳にないもの）が 400 回で全件を読める | 本PoC |
| L3 | セッションの複数 IMSI: 本PoC のセッションの取得で IMSI を 500 件まで渡せるようにする（`idx:user` を使う）。provisioner が中継する | A・B のセッションを、台帳のテナントの IMSI を 500 件ずつ渡して正しく出せる（2 万件のテナントなら 40 回） | 本PoC、provisioner、本BFF |
| L4 | セッションの時刻の索引: セッションの開始時刻の ZSET を auth-server・acct-server が保ち（期限切れのものは読むときに掃除）、新しい順のページを読めるようにする | C・D の「すべて」が、全セッションを読まなくなる | 本PoC（auth-server・acct-server）。影響が大きいので、同時のセッション数しだいで決める |
| R | 定期の突き合わせ（本BFF のジョブ）: 台帳を 500 件ずつ L1 で読み、下流にない・置き場所や停止の状態が違うものを記録する。逆向きは L2 の一覧と台帳を比べる。結果を D の画面に出す（今の「下流にない所属の記録」の点検の画面を置き換える） | ずれと外部の操作を見つける | 本BFF。provisioner §14 の「整合性の定期確認」と役割が重なる |

以前の案のうち、3a（停止中の絞り込み）は台帳で数えるので要らない。案 1（provisioner だけの一括取得）は、下流への問い合わせが減らないので L1 に含めた。

### 18.5 加入者の一括投入（CSV、1 回 10,000 件）

10,000 件は、画面の 1 回の要求では終わらない（1 件の作成が 50 ミリ秒、同時に 5 件でも約 100 秒で、Cloudflare の 100 秒に届く）。処理は**ジョブ**にし、画面は進み具合を htmx の定期の読み込みで出す。1 件の作成にかかる時間は計っていない（同時に 5 件で、1 件 50 ミリ秒なら約 2 分、200 ミリ秒なら約 7 分の見込み）。

仕様（推奨）:

| 事項 | 推奨 |
|---|---|
| 入力の形式 | CSV（UTF-8、ヘッダー行あり）。列は `imsi,ki,opc,amf,sqn,default,rules_json`。`amf`・`sqn` は任意（空なら `8000`・`000000000000`）。`default` は allow / deny。`rules_json` は本PoC の Admin TUI の認可ポリシーの CSV と同じ JSON の配列（`nas_id`・`allowed_ssids`・`vlan_id`・`session_timeout`。空ならルールなし）。ルールは平均 1.5 件・最大 4 件なので 1 行に書ける。1 行は 300 バイト程度、10,000 件で 3MB 程度なので、アップロードの上限はフォームの 1MiB とは別に 8MiB とする |
| 所属のテナント | ファイルごとに 1 つ選ぶ（「選んでください」から必ず選ぶ）。テナントの列は持たない |
| 検証 | 投入の前に全行を確かめる（形式、ファイルの中の IMSI の重複、ルールの形式と件数、10,000 件の上限、PLMN マップでの鍵の置き場所）。1 行でも形式の誤りがあれば何も投入せず、誤りを行番号つきで出す。台帳に既にある IMSI は「既にある」として先に件数を出す |
| 既にある IMSI | 上書きしない。provisioner の 409 も「既にある」とし、所属も変えない（既にある加入者を CSV でテナントに取り込めないようにする。付け替えは D の操作のまま） |
| 実行 | 本BFF のジョブが provisioner の `POST /subscribers` を同時に 5 件まで呼ぶ（行ごとの `Idempotency-Key` は投入の ID と IMSI から作る）。成功のたびに台帳に書く。provisioner や下流に接続できない誤りが 10 件続いたら止める（未完了の操作を増やさない） |
| 止まったときの続き | 同じファイルをもう一度投入する。台帳にある行は「既にある（登録済み）」として飛ばすので、残りだけが投入される |
| 結果 | 件数（登録・既にある・失敗・未処理）と、失敗した行の一覧（行番号、IMSI、理由、操作の記録の ID）。結果は CSV でも取り出せる。Ki / OPc は出さない |
| 同時に走らせる数 | 本BFF 全体で 1 つ（2 つ目は待たせずに断る） |
| 監査ログ | 加入者ごとの `subscriber.create`（内容に投入の ID）と、投入ごとの `subscriber.import`（開始と終了。ファイル名、件数、テナント）。ファイルそのものは保存しない。provisioner・本PoC・aka-only-server の監査ログの Stream の上限は、設定で 100,000 件程度に上げる |
| 秘密の値 | Ki / OPc はジョブのメモリだけに置き、DB に書かない。本BFF を止めると、処理中の投入は「中断」になる（起動時に印を付ける）。続きは同じファイルの投入で行う |
| 扱わないもの | 一括の変更・削除・停止・再開、エクスポート（Ki / OPc をまとめて出すことになる） |

ジョブを置く場所:

| 案 | 内容 | 評価 |
|---|---|---|
| J1. 本BFF の中のジョブ（推奨） | 上の仕様のとおり。provisioner の API は 1 件ずつの作成のまま | API を変えずに作れる。外部の操作を禁止すると本BFF が唯一の管理クライアントなので、provisioner に置く利点（他のクライアントから使える）は薄い。本BFF を止めると中断するが、同じファイルで続けられる |
| J2. provisioner の一括投入オペレーション | `POST /jobs/subscriber-import` で 202 とジョブの ID を返し、`GET /jobs/{id}` で進み具合と行ごとの結果を返す。provisioner §14 の「時間のかかるジョブ」として操作の記録を広げる | provisioner を止めても途中から続けられる。処理を終えるまで Ki / OPc を provisioner の Valkey に置くので、保存の方法（暗号化、保持期間）を決める必要がある。手間は最も大きい |
| J3. provisioner の同期の一括 API | `POST /subscribers/batch`（100 件まで）。J1 の中から呼ぶ | 本BFF と provisioner の間のやり取りが減るだけで、下流への要求の数は変わらない |
| J4. 下流の一括作成 | 本PoC と aka-only-server に一括作成（パイプライン）を加え、provisioner から使う | 10,000 件で下流への要求が 50,000 回から数百回になる。2 つのノードにまたがる一括の補償が難しく、本PoC の D-13 §10（一括操作は扱わない）を見直すことになる。J1 で時間の目標に届かないときだけ検討する |

provisioner の小さな改善として、未完了の操作の IMSI ごとの索引を持てば、投入の途中で未完了の操作が増えても作成が重くならない（任意）。

### 18.6 進め方（推奨）

1. **計測**: 加入者 20 万件（ルール平均 1.5 件。PLMN マップに合わせて本PoC と aka-only-server に分ける）と RADIUSクライアント 1,000 件のデータを、手元か simwifi の本PoC・aka-only-server に作り、今の一覧・1 件の作成・セッションの一覧の時間を計る。§18.2 の評価と、§18.5 の投入の時間を数字で確かめる。
2. **本BFF だけでできること**（下流を変えない）: 台帳（スキーマ `0002`、所属の記録からの移行、登録・削除・停止・再開・付け替えでの更新）、台帳での一覧・件数・停止中の数、一括投入のジョブ（J1）。台帳の初期化は、今の provisioner の一覧で一度だけ全件をたどる（遅いが一度だけ）。この段階でも、C・D の一覧の表示の項目は 1 件ずつの取得で読む（A・B と同じ作り）。
3. **本PoC の改版**（Provisioning API 0.5.0）: L1 の本PoC の部分、L2、L3（同時のセッション数によっては L4）、監査ログの上限の見直し。D-13 §10 の「一括操作は扱わない」は、秘密の値を返さない一括取得を除く、と改める。進め方は本PoC の流儀（feature → develop → main、ドキュメントの改版）。aka-only-server は L1 の部分だけ（管理 API 0.3.0）。
4. **provisioner**（API 0.4.0）: 一括取得の統合、セッションの複数 IMSI の中継、未完了の操作の索引（任意）。
5. **本BFF**: 一覧の表示を一括取得に、セッションを L3 に、突き合わせ（R）を加える。運用ガイドに、外部の操作の禁止（管理クライアントの登録を provisioner と本BFF に絞る手順、例外の操作の後の突き合わせ）を書く。

2 は下流に依存しないので先に進められる。3〜5 は、以前の方針と同じく本PoC → provisioner → 本BFF の順（§15 の 15 と同じ）。

### 18.7 確認事項

| # | 事項 | 推奨 |
|---|---|---|
| 1 | 同時のセッション数の目標 | 決めてもらう。数万件以上なら L4 を入れる。数千件までなら L3 だけでよい |
| 2 | テナントの数と、テナントあたりの加入者の最大 | 決めてもらう。A・B のセッション（L3 の回数）とテナントの一覧の作りに効く |
| 3 | 10,000 件の投入にかけてよい時間 | 30 分以内を目安にする。計測（§18.6 の 1）で J1 が届かなければ J4 を検討する |
| 4 | CSV のルールの書き方 | `rules_json` の列（Admin TUI の認可ポリシーの CSV と同じ JSON）。代わりに、テナントごとに名前をつけたルールの組（型）を本BFF に持ち、CSV では名前で指す案もある（入力の誤りは減るが、管理するものが増える） |
| 5 | 台帳を持つ場所 | 本BFF の DB（テナントと一緒に引ける。外部の操作を禁止すれば、他の管理クライアントはない）。provisioner §14 の台帳（Valkey）は採らない |
| 6 | 突き合わせを持つ場所 | 本BFF（台帳と所属が本BFF にあるため）。provisioner §14 の「整合性の定期確認」は、本BFF の突き合わせで代える |
| 7 | 外部の操作の禁止の範囲 | 加入者・認可ポリシー・RADIUSクライアントの変更を禁止する。Admin TUI の閲覧と保守、aka 版 GUI の AVクライアント・証明書の管理は許す |
| 8 | 作業の区切り | §18.6 の 1・2 を本BFF の 1 つのステップ、3〜5 を下流の改版と合わせたもう 1 つのステップにする |
