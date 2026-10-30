# aka-ossbss

[![CI](https://github.com/oyaguma3/aka-ossbss/actions/workflows/ci.yml/badge.svg)](https://github.com/oyaguma3/aka-ossbss/actions/workflows/ci.yml)
[![govulncheck](https://github.com/oyaguma3/aka-ossbss/actions/workflows/govulncheck.yml/badge.svg)](https://github.com/oyaguma3/aka-ossbss/actions/workflows/govulncheck.yml)

> このリポジトリは、VPS に分けて置いて顧客を収容する **MVP セット**（[aka-mvpset](https://github.com/oyaguma3/aka-mvpset)）の、運用者とテナントの利用者が使う画面と管理面の業務の処理を受け持つノードです。PoC セット（[eapaka-pocset](https://github.com/oyaguma3/eapaka-pocset) の `pocset-v1.0.1`）の [eapaka-ossbss-sample](https://github.com/oyaguma3/eapaka-ossbss-sample) の履歴を引き継いで作りました（もとのコミット `d970d48`）。
>
> MVP のための改修は、[設計概要](docs/design-overview.md)のとおりに MVP セットの段階 3・4 で行っています。段階 3-1（加入者の台帳、[aka-provisioner](https://github.com/oyaguma3/aka-provisioner) 2.0.0 の型つきのクライアント、加入者の画面、認可ポリシーの編集、セッション）と 3-2（一括投入と一括の物理削除）を終えました（設計概要 §19.2）。突き合わせ（3-3）、課金レコード（3-4）、RadSec の受け口の管理（段階 4）はこれからです。[画面仕様](docs/screen-spec.md)と[運用ガイド](docs/operation-guide.md)は段階 3-5 で書き直すまで PoC の作りのままです。

運用者とテナントの利用者が使う画面と、管理面の業務の処理（加入者の台帳など）を受け持つノード（コマンド名 `aka-ossbss`）です。aka-provisioner 2.0.0 を通して、[aka-vecgen](https://github.com/oyaguma3/aka-vecgen)（vecgen 側: 鍵・SQN・状態）と [aka-radius](https://github.com/oyaguma3/aka-radius)（radius 側: 状態・取得先・認可ポリシーのルール）の加入者を、テナントと役割に基づく権限で別々に操作します。

```
[ブラウザ] ─> Cloudflare Access（メール認証）─ Tunnel ─> aka-ossbss ─mTLS─> aka-provisioner ─> aka-vecgen / aka-radius
                                                            │
                                                       PostgreSQL（テナント・アカウント・台帳・一括投入のジョブ・状態の事実・監査ログ）
```

- すべての加入者（IMSI）はどれか 1 つのテナントに属し、テナントの利用者は自分のテナントのものだけを扱います。
- 加入者の台帳（所属、両側の有無と状態、取得先）は、aka-provisioner を通した操作が成功したときだけ書き、課金レコードの状態の事実と同じトランザクションで書きます。結果が分からない操作は印を付け、その場で下流を読み直して台帳を直します。
- 認証は Cloudflare Access / Tunnel（メールのワンタイム PIN）で行い、aka-ossbss は Access の JWT を検証して利用者を識別します。
- 画面は Go + htmx + daisyUI です。

## 機能

| 画面 | 内容 | 使える役割 |
|---|---|---|
| ダッシュボード | 台帳の件数（全体・食い違い・結果を確かめているもの・側と状態ごと）、一括投入のジョブと aka-provisioner と下流の状態（運用担当・システム管理者） | 全員 |
| 加入者 | 台帳の一覧と絞り込み、vecgen 側と radius 側を並べた詳細、状態の変更（側ごと・両方）、解約からの戻し、物理削除、vecgen 側の変更、取得先の変更、所属の付け替え | 全員（操作は役割による） |
| 加入者の登録 | vecgen 側（鍵の入力）、radius 側（取得先と最初のルール）、削除済みの IMSI の登録し直しの承認 | 運用担当（radius 側）・システム管理者 |
| 認可ポリシー | ルールの編集（テナントは所属のテナント） | テナント管理者・運用担当・システム管理者 |
| セッション | テナント（刻印のテナント）か IMSI で絞った aka-radius のセッション | 全員 |
| 一括投入 | CSV での登録と物理削除のジョブ（全行を確かめてから確定、対象ごとの待ち行列、進み具合、中断、成功以外の行の CSV） | 運用担当（閲覧）・システム管理者 |
| テナント | 作成・名前の変更・削除 | システム管理者 |
| アカウント | 作成・役割とテナントの変更・無効化・削除 | テナント管理者（自分のテナントのテナントユーザー）・システム管理者 |
| 監査ログ | aka-ossbss の監査ログ（テナントの範囲）、aka-provisioner・aka-vecgen・aka-radius の監査ログ | テナント管理者（aka-ossbss の分）・運用担当・システム管理者 |

役割はテナントユーザー・テナント管理者・運用担当・システム管理者の 4 つです。権限の詳細は [設計概要](docs/design-overview.md) §5 を参照してください。

## 導入と運用

Docker Compose（aka-ossbss、PostgreSQL、cloudflared）で動かします。ログを管理面の Fluent Bit（aka-provisioner の compose が動かす）に送るときは `compose.fluent-bit.yaml` を重ねます。監査ログの日ごとのファイルは `audit/`（UID 65532 で書けるようにする）に書きます。一括投入の待ちのファイルを暗号化する KEK は `secrets/kek` に置きます（`openssl rand -hex 32` か `aka-ossbss gen-kek` で作り、UID 65532 で読めるようにする。なければ起動しません）。ホストの受信のポートは開けません（cloudflared が外向きに Cloudflare へ接続します）。aka-provisioner が導入済みで、Cloudflare に公開ホスト名に使うドメインのゾーンと Zero Trust があることが前提です。

導入（Cloudflare Access / Tunnel の準備、provisioner との証明書のやり取り、起動）、アカウントの運用、バックアップ、更新、障害時の確認は [運用ガイド](docs/operation-guide.md) を参照してください。

## 開発

Go 1.27.3 と Docker（compose プラグイン）を使います。

画面の CSS（`internal/web/static/app.css`）はリポジトリに入れていないので、最初に作ります。Tailwind CSS のスタンドアロン CLI と daisyUI のプラグインを、版を固定して取得し、SHA-256 を確かめてから使います（Node.js は使いません。Linux の x86_64 / arm64 だけ）。取得したファイルは `.cache/css/` に置いて使い回します。テンプレートのクラスを変えたら作り直します。

```bash
sh scripts/build-css.sh
```

テスト。PostgreSQL の結合テストは、接続先を環境変数で与えたときだけ動きます。テストのたびに public スキーマを作り直すので、名前に `test` を含む DB を使います（既定 `ossbss_test`）。

```bash
docker run -d --name ossbss-it-pg -p 127.0.0.1:25432:5432 -e POSTGRES_USER=ossbss -e POSTGRES_PASSWORD=it-pass -e POSTGRES_DB=ossbss_test postgres:18.6
```

```bash
OSSBSS_TEST_DB_ADDR=127.0.0.1:25432 OSSBSS_TEST_DB_PASSWORD=it-pass go test -race ./...
```

手元で画面を確かめるときは、開発用の設定 `OSSBSS_DEV_USER_EMAIL` で、バイナリを直接動かします。JWT を検証せず、そのメールアドレスの利用者として動きます（役割とテナントは DB のアカウントのもの）。他のホストから届かないよう、ループバックで待ち受けるとき（`OSSBSS_ADDR` が `127.0.0.1:…` など）だけ使え、Cloudflare Access の設定とは同時に使えません。compose では使えません。DB は上のテスト用のコンテナに、別の DB を作って使います。

```bash
docker exec ossbss-it-pg createdb -U ossbss ossbss
```

```bash
go build -o aka-ossbss ./cmd/aka-ossbss
```

aka-provisioner に提示するクライアント証明書を作り、標準エラーに出る「識別名=フィンガープリント」の行を aka-provisioner の管理クライアントのピンのファイルに書いて、aka-provisioner に `SIGHUP` を送ります。aka-provisioner のサーバー証明書は、aka-provisioner の `server-cert` で取り出します。

```bash
./aka-ossbss gen-client-cert -name ossbss-dev > provisioner-client.pem
./aka-ossbss gen-kek -out kek
```

```bash
OSSBSS_ADDR=127.0.0.1:8446 OSSBSS_DB_ADDR=127.0.0.1:25432 OSSBSS_DB_PASSWORD=it-pass OSSBSS_INITIAL_ADMIN_EMAIL=admin@example.com OSSBSS_DEV_USER_EMAIL=admin@example.com OSSBSS_PROVISIONER_URL=https://127.0.0.1:9446/admin/v2 OSSBSS_PROVISIONER_CLIENT_CERT=provisioner-client.pem OSSBSS_PROVISIONER_SERVER_CERT=provisioner-server.pem OSSBSS_AUDIT_DIR=./audit OSSBSS_KEK_FILE=./kek ./aka-ossbss
```

aka-provisioner に接続できるかは、同じ環境変数で `./aka-ossbss check-provisioner` を実行して確かめられます。

aka-provisioner との契約テストは、`OSSBSS_TEST_PROVISIONER_URL`（と `_CLIENT_CERT`、`_SERVER_CERT`、`_AV_CLIENT_ID`、`_SOURCE_ID`）を与えたときだけ動きます（`go test -run Integration ./internal/provisioner/`）。CI のジョブ「aka-provisioner との契約テスト」と同じく、aka-provisioner のリポジトリの `scripts/contract.sh up <バイナリのディレクトリ> <作業ディレクトリ>` で、固定のコミットの aka-vecgen・aka-radius・aka-provisioner を手元で起動し、作業ディレクトリの `contract.env` の `PROVISIONER_TEST_*` を `OSSBSS_TEST_PROVISIONER_*` として渡します。上の開発用の設定も、この aka-provisioner（`contract.env` の URL と証明書）につなげば、本物の下流で画面を確かめられます。

依存と標準ライブラリの既知の脆弱性は、`.github/workflows/govulncheck.yml` が push と pull request のたびと、毎週月曜に govulncheck で調べます。

`http://127.0.0.1:8446/` を開きます（PoC のスキーマの DB では起動しません。新しい DB で始めます）。最初のシステム管理者（`OSSBSS_INITIAL_ADMIN_EMAIL`）は起動時に作られます。役割ごとの表示は、DB でこの利用者のアカウントの役割とテナントを付け替えて確かめます（起動し直すと、最初のシステム管理者はシステム管理者に戻ります）。

compose（`compose.yaml`）は本番の構成です（運用ガイド）。開発用の設定は compose では使えません。

## ライセンス

[LICENSE](LICENSE) を参照してください。
