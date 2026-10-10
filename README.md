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

## 現在の状態

実装中です（[設計概要](docs/design-overview.md) §16 の 4 のステップ 3 まで）。今は、骨組み（設定、HTTP サーバー、PostgreSQL とスキーマの移行、画面の土台、compose、CI）、Cloudflare Access の JWT による利用者の識別と役割・権限の判定、eapaka-node-provisioner のクライアント（契約テストつき）があります。加入者などの画面はこれから加えます。

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

compose（`compose.yaml`）は本番の構成で、Cloudflare Access の設定（`OSSBSS_ACCESS_TEAM_DOMAIN`、`OSSBSS_ACCESS_AUD`）と `OSSBSS_INITIAL_ADMIN_EMAIL` が必須です。本BFF はホストにポートを公開しません。provisioner 用の証明書は `certs/provisioner-client.pem` と `certs/provisioner-server.pem` に置きます。provisioner と同じホストでは `compose.eapaka-provisioner.yaml` を重ねます（`.env.example` の既定）。

本番の構成（cloudflared を含む）と運用の手順は、設計概要 §16 の 4 のステップ 6 で運用ガイドにまとめます。

## ライセンス

[LICENSE](LICENSE) を参照してください。
