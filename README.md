# eapaka-ossbss-sample

外部 OSS/BSS（運用・業務支援システム）のサンプル実装としての統合BFF（コマンド名 `eapaka-ossbss`）です。[eapaka-node-provisioner](https://github.com/oyaguma3/eapaka-node-provisioner) の API を使って、[EAP-AKA RADIUS PoC](https://github.com/oyaguma3/eapaka-radius-server-poc) と [aka-only-server](https://github.com/oyaguma3/aka-only-server) の加入者・RADIUSクライアント・認可ポリシーを、テナントと役割に基づく権限で操作します。

```
[ブラウザ] ─> Cloudflare Access（メール認証）─ Tunnel ─> 統合BFF ─> eapaka-node-provisioner ─> RADIUS PoC / aka-only-server
                                                          │
                                                     PostgreSQL
```

- 加入者と RADIUSクライアントをテナントに属させ、テナントの利用者は自分のテナントのものだけを扱います（テナントに属さないものも扱えます）。
- 認証は Cloudflare Access / Tunnel（メールのワンタイム PIN）で行い、統合BFF は Access の JWT を検証して利用者を識別します。
- 加入者を登録したまま一時的に使えなくする（停止・再開）ことができます。
- 画面は Go + htmx + daisyUI です。

個人利用・検証用途のサンプルです。

## 機能

| 画面 | 内容 | 使える役割 |
|---|---|---|
| ダッシュボード | 自分の範囲の加入者・停止中・RADIUSクライアントの数、provisioner と下流の状態 | 全員 |
| 加入者 | 一覧・登録・詳細・変更・削除、Ki / OPc の表示、停止・再開、認可ポリシーの編集 | 全員（操作は役割による） |
| RADIUSクライアント | 一覧・登録・詳細・変更・削除、共有シークレットの表示 | 全員（操作は役割による） |
| セッション | テナントの範囲のアクティブセッション | 全員 |
| テナント | 作成・名前の変更・削除、所属の付け替え、下流にない所属の記録の点検 | システム管理者 |
| アカウント | 作成・役割とテナントの変更・無効化・削除 | テナント管理者（自分のテナントのテナントユーザー）・システム管理者 |
| 操作の記録 | provisioner の未完了の操作、やり直し・閉じる | 運用担当・システム管理者 |
| 監査ログ | 本BFF の監査ログ（テナントの範囲）、provisioner と下流の監査ログ | テナント管理者・運用担当・システム管理者 |

役割はテナントユーザー・テナント管理者・運用担当・システム管理者の 4 つです。権限の詳細は [設計概要](docs/design-overview.md) §6、画面の詳細は [画面仕様](docs/screen-spec.md) を参照してください。

## 導入と運用

Docker Compose（統合BFF、PostgreSQL、cloudflared）で動かします。ホストの受信のポートは開けません（cloudflared が外向きに Cloudflare へ接続します）。eapaka-node-provisioner が導入済みで、Cloudflare に公開ホスト名に使うドメインのゾーンと Zero Trust があることが前提です。

導入（Cloudflare Access / Tunnel の準備、provisioner との証明書のやり取り、起動）、アカウントの運用、バックアップ、更新、障害時の確認は [運用ガイド](docs/operation-guide.md) を参照してください。

## 開発

Go 1.27.1 と Docker（compose プラグイン）を使います。

画面の CSS（`internal/web/static/app.css`）はリポジトリに入れていないので、最初に作ります。Tailwind CSS のスタンドアロン CLI と daisyUI のプラグインを、版を固定して取得し、SHA-256 を確かめてから使います（Node.js は使いません。Linux の x86_64 / arm64 だけ）。取得したファイルは `.cache/css/` に置いて使い回します。テンプレートのクラスを変えたら作り直します。

```bash
sh scripts/build-css.sh
```

テスト。PostgreSQL の結合テストは、接続先を環境変数で与えたときだけ動きます。テストのたびに public スキーマを作り直すので、名前に `test` を含む DB を使います（既定 `ossbss_test`）。

```bash
docker run -d --name ossbss-it-pg -p 127.0.0.1:25432:5432 -e POSTGRES_USER=ossbss -e POSTGRES_PASSWORD=it-pass -e POSTGRES_DB=ossbss_test postgres:18
```

```bash
OSSBSS_TEST_DB_ADDR=127.0.0.1:25432 OSSBSS_TEST_DB_PASSWORD=it-pass go test -race ./...
```

手元で画面を確かめるときは、開発用の設定 `OSSBSS_DEV_USER_EMAIL` で、バイナリを直接動かします。JWT を検証せず、そのメールアドレスの利用者として動きます（役割とテナントは DB のアカウントのもの）。他のホストから届かないよう、ループバックで待ち受けるとき（`OSSBSS_ADDR` が `127.0.0.1:…` など）だけ使え、Cloudflare Access の設定とは同時に使えません。compose では使えません。DB は上のテスト用のコンテナに、別の DB を作って使います。

```bash
docker exec ossbss-it-pg createdb -U ossbss ossbss
```

```bash
go build -o eapaka-ossbss ./cmd/eapaka-ossbss
```

provisioner に提示するクライアント証明書を作り、標準エラーに出る `PROVISIONER_ADMIN_CLIENTS=` の行を provisioner の `.env` に登録します。provisioner のサーバー証明書は、provisioner の `server-cert` で取り出します（provisioner の運用ガイド 2.8）。

```bash
./eapaka-ossbss gen-client-cert -name ossbss-dev > provisioner-client.pem
```

```bash
OSSBSS_ADDR=127.0.0.1:8446 OSSBSS_DB_ADDR=127.0.0.1:25432 OSSBSS_DB_PASSWORD=it-pass OSSBSS_INITIAL_ADMIN_EMAIL=admin@example.com OSSBSS_DEV_USER_EMAIL=admin@example.com OSSBSS_PROVISIONER_URL=https://127.0.0.1:9446/admin/v1 OSSBSS_PROVISIONER_CLIENT_CERT=provisioner-client.pem OSSBSS_PROVISIONER_SERVER_CERT=provisioner-server.pem ./eapaka-ossbss
```

provisioner に接続できるかは、同じ環境変数で `./eapaka-ossbss check-provisioner` を実行して確かめられます。provisioner との契約テストは、`OSSBSS_TEST_PROVISIONER_URL`、`OSSBSS_TEST_PROVISIONER_CLIENT_CERT`、`OSSBSS_TEST_PROVISIONER_SERVER_CERT` を与えたときだけ動きます（`go test -run Integration ./internal/provisioner/`。CI のジョブ「eapaka-node-provisioner との契約テスト」と同じ手順で、下流と provisioner を手元で動かせます）。

`http://127.0.0.1:8446/` を開きます。最初のシステム管理者（`OSSBSS_INITIAL_ADMIN_EMAIL`）は起動時に作られます。役割ごとの表示は、DB でこの利用者のアカウントの役割とテナントを付け替えて確かめます（起動し直すと、最初のシステム管理者はシステム管理者に戻ります）。

compose（`compose.yaml`）は本番の構成です（運用ガイド）。開発用の設定は compose では使えません。

## ライセンス

[LICENSE](LICENSE) を参照してください。
