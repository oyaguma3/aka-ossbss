# 運用ガイド

> **段階 3-5 で書き直すまで、本書は PoC の作りのままです。** MVP セットの段階 3-1 で、相手を aka-provisioner 2.0.0（`/admin/v2`）にし、加入者の台帳を足し、RADIUS クライアント・操作の記録・所属の付け替えと点検・Ki/OPc の表示の画面をなくしました（[設計概要](design-overview.md) §19.2 の「段階 3-1 で決めたこと」）。そのため、本書のうち接続先の URL、aka-provisioner の管理クライアントの登録（ピンのファイルと `SIGHUP` になった）、それらの画面の手順は今の作りと違います。監査ログの日ごとのファイル（`audit/`）と、管理面の Fluent Bit への重ね合わせ（`compose.fluent-bit.yaml`）も本書にはまだありません。

aka-ossbss（以下「本BFF」）の導入と運用の手順。設計は [設計概要（PoC。今の作り）](poc/design-overview.md)、画面は [画面仕様](screen-spec.md) を参照。

本書の手順は、2026-10-11 に検証機（simwifi。Debian 13）で、aka-provisioner（以下「provisioner」）・本PoC・aka-vecgen と同じホストに導入して確かめた（設計概要 §17.6）。

## 1. 構成

```
[ブラウザ] ─HTTPS─> Cloudflare（Access: メールのワンタイム PIN） ─Tunnel─> cloudflared ─HTTP─> 本BFF ──mTLS──> provisioner ──> 本PoC
                                                                    （同じ compose。ホストの受信のポートは開けない）  │              └─> aka-vecgen
                                                                                                                 PostgreSQL（本BFF 専用）
```

| サービス（compose） | 内容 |
|---|---|
| `aka-ossbss` | 本BFF。`:8446` で待ち受ける（HTTP）が、ホストにポートを公開しない。要求は同じ compose の cloudflared からだけ届く |
| `postgres` | 本BFF 専用の PostgreSQL 18。テナント・アカウント・所属の記録・監査ログを持つ。ポートを公開しない |
| `cloudflared` | Cloudflare Tunnel のコネクター。外向きに Cloudflare へ接続し、公開ホスト名への要求を本BFF に届ける |

- 利用者は Cloudflare Access でメールのワンタイム PIN でログインする。本BFF は Access が付ける JWT を要求のたびに検証し、メールアドレスで本BFF のアカウントを引いて、役割とテナントを決める（設計概要 §5）。パスワードは持たない。
- ログインできる人（メールアドレス）は Access のポリシーで、できること（役割・テナント）は本BFF のアカウントで決める。利用者を加えるときは両方に登録する（5 章）。
- 加入者の鍵・認可ポリシー・停止の状態の正本は下流（本PoC、aka-vecgen）にある。本BFF の DB にあるのは、テナント・アカウント・所属の記録・監査ログだけである。
- provisioner は本BFF を全権の管理クライアントとして扱う。テナントと権限の境界は本BFF だけが担う。

| ファイル | 内容 |
|---|---|
| `.env` | 設定（11 章）。`chmod 600` |
| `certs/provisioner-client.pem` | provisioner に提示するクライアント証明書と秘密鍵（2.5） |
| `certs/provisioner-server.pem` | provisioner のサーバー証明書（信頼する証明書として使う。2.5） |
| `secrets/tunnel-token` | Cloudflare Tunnel のトークン（秘密の値。2.6） |

`certs/` と `secrets/` は `.gitignore` で除外している。本BFF と cloudflared のコンテナは UID 65532 で動くので、これらのファイルはこの UID で読めるようにする。

## 2. 導入（provisioner と同じホスト）

### 2.1 前提

- provisioner が、provisioner の運用ガイド 2 章の手順で導入済みであること。provisioner の compose が作る共有ネットワーク `aka-provisioner` に本BFF が参加して、`https://aka-provisioner:9446/admin/v1` で接続する（provisioner の運用ガイド 2.8）。provisioner は管理クライアントが 1 つもないと起動しないので、まだ起動していなければ、2.5 で本BFF を登録してから起動する。別のホストの provisioner につなぐ場合は 3 章も参照。
- Docker Engine と compose プラグイン、git。ビルドの段で画面の CSS を作るために、Tailwind CSS と daisyUI を GitHub などから取得するので、ビルドするホストは外向きに接続できること。
- Cloudflare のアカウントで、公開ホスト名に使うドメインのゾーンが Cloudflare にあり、Zero Trust（Free プランでよい。50 人まで）を有効にしてあること。

以下では、リポジトリを provisioner と同じディレクトリに並べて置く。

```
~/aka-provisioner/   provisioner（導入済み）
~/aka-ossbss/      このリポジトリ
```

### 2.2 Cloudflare 側の準備

Cloudflare の Zero Trust のダッシュボードで行う。画面の名前は 2026-10 時点のもので、変わることがある。

1. **ワンタイム PIN をログイン方法に加える**: 「インテグレーション」→「ID プロバイダー」で One-time PIN を加える（入力したメールアドレスに PIN が届き、それでログインする）。
2. **Tunnel を作る**: 「ネットワーク」→「Tunnels」で、種類 Cloudflared の Tunnel を作る（名前は例 `ossbss`）。コネクターの導入の画面に出るコマンドの、`--token` の後ろの長い文字列が Tunnel のトークンである（2.6 で使う。秘密の値なので、画面に貼り付けたり保存したりしない）。コネクターの導入のコマンドは実行しない（本BFF の compose の cloudflared が接続する）。
3. **公開ホスト名を加える**: Tunnel の「公開ホスト名」（Public Hostname）で、サブドメインとドメイン（例 `ossbss.example.com`）を決め、サービスを種類 `HTTP`、URL `aka-ossbss:8446` にする（compose のネットワークの中の本BFF。ホストの `localhost` ではない）。
4. **Access のアプリを作る**: 「アクセス」→「アプリケーション」で、種類 セルフホスト（self-hosted）のアプリを作る。
   - 公開ホスト名: 3 のホスト名。
   - ログイン方法: One-time PIN。
   - ポリシー: アクション Allow で、ログインを許す人を Include に書く（Emails に利用者のメールアドレス、または Emails ending in にドメイン）。最初は最初のシステム管理者のメールアドレスだけでよい（5 章）。
5. **AUD タグとチームのドメインを控える**: アプリの設定（概要）に出る「アプリケーション オーディエンス（AUD）タグ」と、チームのドメイン（`<チーム名>.cloudflareaccess.com`。「設定」に出る）を控える（2.3 で `.env` に書く。どちらも秘密の値ではない）。

公開ホスト名を作ると、そのホスト名の DNS レコードは Cloudflare が Tunnel に向けて作る。Access のアプリがあれば、ログインしていない要求は Cloudflare で止まり、本BFF には届かない。

### 2.3 取得して `.env` を書く

```bash
cd ~
```

```bash
git clone https://github.com/oyaguma3/aka-ossbss.git
```

```bash
cd ~/aka-ossbss
```

```bash
cp .env.example .env
```

```bash
chmod 600 .env
```

`.env` に次の項目を書く（全項目は 11 章）。

| 項目 | 内容 |
|---|---|
| `OSSBSS_DB_PASSWORD` | 本BFF 専用の PostgreSQL のパスワード。`openssl rand -hex 24` の出力など |
| `OSSBSS_ACCESS_TEAM_DOMAIN` | 2.2 の 5 のチームのドメイン（例 `example.cloudflareaccess.com`） |
| `OSSBSS_ACCESS_AUD` | 2.2 の 5 の AUD タグ |
| `OSSBSS_INITIAL_ADMIN_EMAIL` | 最初のシステム管理者のメールアドレス（Access のポリシーで許したもの）。英小文字・数字・`.` `_` `-` と `@` の 64 文字まで（`+` は使えない。provisioner に `X-Operator-Id` として渡すため） |

`COMPOSE_FILE` の既定（`compose.yaml:compose.aka-provisioner.yaml`）は、provisioner と同じホストの構成である。

イメージをビルドする。provisioner の共有ネットワークがまだない段階では、`.env` の `COMPOSE_FILE` に重ねたファイルの外部ネットワークが見つからずに失敗するので、2.5 の証明書の作成までは `-f compose.yaml` を付けて実行する。

```bash
docker compose -f compose.yaml build
```

### 2.4 provisioner のサーバー証明書の SAN を確かめる

同じホストでは、本BFF はホスト名 `aka-provisioner` で provisioner のサーバー証明書を検証する。provisioner の `PROVISIONER_TLS_HOSTS` の既定にこの名前が入っている（provisioner の運用ガイド 2.8）。変えていなければ作業は要らない。

### 2.5 provisioner との証明書のやり取り

本BFF が provisioner に提示するクライアント証明書を作る。証明書と秘密鍵が 1 つの PEM にまとめて標準出力に出る。`-name` の識別名は、provisioner の監査ログの管理クライアント（`mgmtClient`）になる。

```bash
mkdir -p certs
```

```bash
docker compose -f compose.yaml run --rm --no-deps -T aka-ossbss gen-client-cert -name ossbss > certs/provisioner-client.pem 2> gen.txt
```

```bash
cat gen.txt
```

標準エラー（`gen.txt`）の `PROVISIONER_ADMIN_CLIENTS=` の行の値を、provisioner の `.env` の `PROVISIONER_ADMIN_CLIENTS` に書き（他の管理クライアントがあればカンマで区切って加える）、provisioner を起動する（起動済みなら作り直す）。

```bash
cd ~/aka-provisioner
```

```bash
docker compose up -d
```

provisioner のサーバー証明書を、本BFF の `certs/provisioner-server.pem` に取り出す。

```bash
docker compose exec -T aka-provisioner /aka-provisioner server-cert > ~/aka-ossbss/certs/provisioner-server.pem
```

本BFF のコンテナは UID 65532 で動くので、証明書の所有者をこの UID にしてパーミッションを 600 にする。

```bash
cd ~/aka-ossbss
```

```bash
rm gen.txt
```

```bash
sudo chown 65532:65532 certs/*.pem
```

```bash
sudo chmod 600 certs/*.pem
```

以後、`certs/` のファイルを置き換えるときは `sudo` で上書きする。

provisioner に接続できるかを確かめる（DB には接続しない）。下流 2 つ・AVクライアント・PLMN マップ・未完了の操作が出れば、準備はできている。つながらなければ原因の見当が出る。

```bash
docker compose run --rm --no-deps aka-ossbss check-provisioner
```

### 2.6 Tunnel のトークンを置く

2.2 の 2 の Tunnel のトークンを `secrets/tunnel-token` に置く。シェルの履歴に残らないよう、端末で入力して書く（貼り付けた文字は画面に出ない）。

```bash
mkdir -p secrets
```

```bash
(umask 077; read -rs -p 'Tunnel のトークン: ' t; echo; printf '%s' "$t" > secrets/tunnel-token)
```

cloudflared のコンテナも UID 65532 で動くので、所有者をこの UID にする。

```bash
sudo chown 65532:65532 secrets/tunnel-token
```

```bash
sudo chmod 600 secrets/tunnel-token
```

トークンは `.env` に書かない（`docker compose config` の出力やコンテナの環境変数に出るため）。ファイルがないと cloudflared は起動しない。

### 2.7 起動する

```bash
docker compose up -d
```

```bash
docker compose ps
```

`aka-ossbss` と `postgres` が `healthy`、`cloudflared` が `Up` になる（cloudflared は本BFF が healthy になってから起動する）。ホストに公開したポートがないこと（`PORTS` の欄に `0.0.0.0:` や `127.0.0.1:` がないこと）も確かめる。本BFF の起動ログに、スキーマの移行（`applied` は今回適用した版）、最初のシステム管理者の作成、Cloudflare Access の公開鍵の取得、provisioner の確認が出る。

```bash
docker compose logs aka-ossbss
```

```
{"level":"INFO","msg":"starting","version":"dev"}
{"level":"INFO","msg":"database","addr":"postgres:5432","name":"ossbss","schema_version":1,"applied":[1]}
{"level":"INFO","msg":"initial system admin","email":"admin@example.com","result":"created"}
{"level":"INFO","msg":"cloudflare access","team_domain":"example.cloudflareaccess.com","auds":1,"keys":2}
{"level":"INFO","msg":"provisioner client","url":"https://aka-provisioner:9446/admin/v1","client_cert_fingerprint":"19c856f7...",...}
{"level":"INFO","msg":"provisioner is available","server_version":"dev","prov_reachable":true,"aka_configured":true,"aka_reachable":true}
{"level":"INFO","msg":"listening","addr":"[::]:8446"}
```

cloudflared のログに、Cloudflare への接続（通常 4 本）が出る。

```bash
docker compose logs cloudflared | grep 'Registered tunnel connection'
```

Zero Trust のダッシュボードの Tunnel の一覧でも、状態が「正常」（Healthy）になる。

### 2.8 最初のログイン

ブラウザで公開ホスト名（例 `https://ossbss.example.com/`）を開き、最初のシステム管理者のメールアドレスでワンタイム PIN のログインをする。ダッシュボードに、役割「システム管理者」と provisioner の状態（各ノードが「接続中」）が出れば導入は終わりである。

続けて、テナント（「テナント」）と利用者のアカウント（「アカウント」。5 章）を作る。本BFF の外（本PoCの Admin TUI など）で登録済みの加入者・RADIUSクライアントは「テナントなし」になっているので、必要なら一覧からまとめてテナントに割り当てる（6 章）。

## 3. 別のホストの provisioner につなぐ場合

provisioner が別のホストにある場合は、VPN（Tailscale、WireGuard 等）越しに接続する。

1. provisioner 側で、管理API を VPN 側のアドレスに公開し、そのアドレスをサーバー証明書の SAN に入れる（provisioner の運用ガイド 3.3・3.4。`PROVISIONER_PUBLISH`、`PROVISIONER_TLS_HOSTS`）。
2. 本BFF の `.env` で、`COMPOSE_FILE=compose.yaml` にし（共有ネットワークに参加しない）、`OSSBSS_PROVISIONER_URL` を VPN 側のアドレスにする（例 `https://100.64.0.30:9446/admin/v1`）。
3. 2.5 の証明書のやり取りを行う（サーバー証明書は、SAN を作り直した後のものを取り出す）。

本BFF のコンテナから VPN 側のアドレスに届くこと（ホストが VPN に参加していれば、通常は届く）を、2.5 の `check-provisioner` で確かめる。

## 4. 公開範囲と安全性

- 本BFF・PostgreSQL・cloudflared は、どれもホストにポートを公開しない。ホストの受信のポートは開けなくてよい（cloudflared は外向きに接続する）。
- 公開ホスト名への要求は、Cloudflare Access のログインを通ったものだけが Tunnel を通って本BFF に届く。本BFF は、さらに要求のたびに JWT（署名、`iss`、`aud`、有効期限）を検証し、検証できなければ 401 を返す（`/healthz` と静的ファイルを除く）。同じホストの Docker ネットワーク（compose の既定のネットワーク、`aka-provisioner`）から本BFF に直接届いても、JWT がなければ画面は使えない。
- 接続元のアドレス（アクセスログ、監査ログ）は、cloudflared が付ける `Cf-Connecting-IP` を使う。本BFF に届くのは cloudflared からだけという前提なので、同じ Docker ネットワークのコンテナからの要求ではこの値を偽れる（記録の値だけで、権限には影響しない）。
- ログインできる人は Access のポリシーで絞る。本BFF に登録のないメールアドレスでログインした人には 403 を返す（何も操作できない）。
- ログアウトは画面の「ログアウト」（Access のログアウト `/cdn-cgi/access/logout`）。ログインの有効期間は Access のアプリの設定（セッションの期間）で決まる。
- 開発用の設定（`OSSBSS_DEV_USER_EMAIL`）は compose では使えない（ループバックで待ち受けるバイナリだけ。設計概要 §17.2）。

## 5. アカウントの運用

### 5.1 役割

| 役割 | できること（設計概要 §6.2） |
|---|---|
| テナントユーザー | 自分のテナントの加入者・RADIUSクライアント・セッションを見る。加入者の停止・再開 |
| テナント管理者 | テナントユーザーに加え、自分のテナントの認可ポリシーの変更、RADIUSクライアントの管理、テナントユーザーのアカウントの管理、自分のテナントの監査ログ |
| 運用担当 | 全テナントとテナントなしの、加入者の登録・削除、Ki / OPc と認可ポリシーの変更など日常の運用。操作の記録と監査ログの閲覧 |
| システム管理者 | すべて。テナントとアカウントの管理、所属の付け替え、AMF / SQN の変更、操作の記録のやり直し・閉じる、provisioner と下流の監査ログ |

### 5.2 最初のシステム管理者

- `.env` の `OSSBSS_INITIAL_ADMIN_EMAIL` のアカウントは、本BFF の起動のたびにシステム管理者・テナントなし・有効になる（なければ作る）。画面からの削除・降格・無効化はできない。
- 誤って全員を締め出したとき（他のシステム管理者がいない、など）は、このアカウントでログインする。変えられていても、本BFF を起動し直せば戻る。

```bash
docker compose restart aka-ossbss
```

- 最初のシステム管理者を別の人に替えるときは、`.env` を書き換えて `docker compose up -d` で作り直す（前の人のアカウントは普通のシステム管理者として残るので、要らなければ画面から削除する）。

### 5.3 利用者を加える

1. 本BFF の「アカウント」で、メールアドレス・役割・テナントを登録する（テナント管理者は、自分のテナントのテナントユーザーだけを登録できる）。
2. Cloudflare Access のアプリのポリシーで、そのメールアドレス（またはドメイン）のログインを許す（ドメインで許している場合は不要）。

どちらかが欠けるとログインできない（Access で止まるか、本BFF が 403 を返す）。役割・テナントの変更と無効化は次の要求から効く（本BFF はセッションを持たず、要求のたびにアカウントを引く）。

- 自分のアカウントは、削除・無効化・役割やテナントの変更ができない（他のシステム管理者に頼む）。
- 利用者がいなくなったら、本BFF のアカウントを無効化または削除し、Access のポリシーからも外す。

## 6. テナントと所属の運用

- 加入者と RADIUSクライアントは、本BFF の所属の記録でテナントに属する（設計概要 §7）。記録がなければ「テナントなし」で、運用担当とシステム管理者だけが扱える。
- 本BFF の外（本PoCの Admin TUI、aka-vecgen の管理 GUI、既存の BFF など）で登録したものはテナントなしになる。加入者・RADIUSクライアントの一覧で「テナントなし」を選び、チェックしてまとめてテナントに割り当てる（システム管理者）。
- 本BFF の外で削除したものは、所属の記録だけが残る（「下流にない」）。「テナント」→「下流にない所属の記録を点検する」で見つけて消す（システム管理者）。
- テナントは、加入者・RADIUSクライアント・アカウントが属している間は削除できない。先に所属を付け替えるか外し、アカウントを移すか削除する。

## 7. ログと監査ログ

- 本BFF のログは標準出力に JSON で出る（`docker compose logs aka-ossbss`）。アクセスログには、利用者のメールアドレス、接続元、トレースID が入る。静的ファイルと `/healthz` は debug（`OSSBSS_LOG_LEVEL=debug` で出る）。
- 本BFF の監査ログ（利用者の変更操作と秘密の値の表示）は DB にあり、画面の「監査ログ」で見る（テナント管理者は自分のテナントの分だけ）。トレースID は、provisioner と下流のログ・監査ログと同じ値なので、突き合わせられる。システム管理者は、provisioner・provisioning-api・aka-vecgen の監査ログも同じ画面のタブで見られる。
- 監査ログは自動では消さない。保持する期間を決めて、古いものを消す場合は、先にバックアップ（8 章）を取ってから行う（例: 400 日より前を消す）。

```bash
docker compose exec -T postgres psql -U ossbss -d ossbss -c "DELETE FROM audit_log WHERE time < now() - interval '400 days'"
```

- Docker のコンテナのログは、既定では大きさの上限なく溜まる。Docker デーモンの設定（`/etc/docker/daemon.json` の `log-opts` の `max-size` / `max-file`）で上限を決めておく。

## 8. バックアップと復元

本BFF の DB にあるのは、テナント・アカウント・所属の記録・監査ログである（加入者の鍵や認可ポリシーは下流にあり、それぞれのバックアップの手順に従う）。あわせて `.env`、`certs/`、`secrets/` も控えておく（秘密の値を含むので、置き場所に注意する）。

### 8.1 バックアップ

```bash
cd ~/aka-ossbss
```

```bash
mkdir -p backup && chmod 700 backup
```

バックアップのファイルは本人だけが読めるようにする（`umask 077`）。

```bash
(umask 077; docker compose exec -T postgres pg_dump -U ossbss -d ossbss -Fc > backup/ossbss-$(date +%Y%m%d-%H%M%S).dump)
```

### 8.2 復元

本BFF と cloudflared を止めてから、バックアップを戻す（`--clean --if-exists` で、今のテーブルを消して作り直す）。

```bash
docker compose stop aka-ossbss cloudflared
```

```bash
docker compose exec -T postgres pg_restore -U ossbss -d ossbss --clean --if-exists < backup/ossbss-20261011-120000.dump
```

```bash
docker compose up -d
```

バックアップの後に本BFF で登録・削除した加入者・RADIUSクライアントは、所属の記録が戻らない（テナントなしになる、または下流にない所属の記録が残る）。復元の後に 6 章の手順で点検する。

## 9. 更新

先にバックアップ（8.1）を取る。

```bash
cd ~/aka-ossbss
```

```bash
git pull
```

```bash
OSSBSS_VERSION=$(git rev-parse --short HEAD) docker compose up -d --build
```

- DB のスキーマは、本BFF の起動時に自動で移行する。DB の版が本BFF より新しいと起動しない（古い版に戻すときは、バックアップから DB も戻す）。
- 画面のフッターにバージョン（`OSSBSS_VERSION`）が出る。
- cloudflared の版は `compose.yaml` で固定している。更新はこのリポジトリの更新に含まれる。

## 10. 障害時の確認

| 症状 | 確かめること |
|---|---|
| 公開ホスト名を開くと Cloudflare のエラー（502、1033 など） | Tunnel が本BFF に届いていない。`docker compose ps` で cloudflared が動いているか、`docker compose logs cloudflared` に接続の失敗がないか、公開ホスト名のサービスが `http://aka-ossbss:8446` か（`localhost` ではない）、本BFF が healthy か |
| cloudflared が起動しない・再起動を繰り返す | `secrets/tunnel-token` があり、UID 65532 で読めるか（`permission denied`）。トークンが正しいか（Cloudflare の認証の失敗がログに出る） |
| 「Cloudflare Access を通して開いてください。」（401） | JWT を検証できない。`.env` の `OSSBSS_ACCESS_TEAM_DOMAIN` と `OSSBSS_ACCESS_AUD` が Access のアプリのものと同じか。本BFF のログの `unauthenticated request` の `reason` に理由が出る |
| 「この利用者（…）は登録されていません。」（403） | 本BFF にそのメールアドレスのアカウントがない、または無効。別のメールアドレスでログインし直すときは、画面のボタン（Access のログアウト）を使う |
| 「Cloudflare Access の公開鍵を取得できません」（503） | 本BFF のコンテナから `https://<チームのドメイン>/cdn-cgi/access/certs` に外向きに接続できるか（DNS、プロキシ） |
| 「データベースに接続できません」（503） | `docker compose ps` で postgres が healthy か。`docker compose logs postgres` |
| provisioner に接続できない（ダッシュボードのカード、画面のエラー） | カードと `docker compose run --rm --no-deps aka-ossbss check-provisioner` に原因の見当が出る（証明書の未登録、サーバー証明書の不一致、共有ネットワーク `aka-provisioner` に参加していない、provisioner が止まっている、など） |
| provisioner から下流に接続できない | ダッシュボードの provisioner のカードの各下流の状態と、provisioner の運用ガイド 10 章 |
| 加入者の登録・変更・削除が「完了していない操作が残っています」で断られる | 「操作の記録」で、その操作をやり直すか閉じる（システム管理者。provisioner の運用ガイド 6 章） |

## 11. 環境変数

`.env` に書く項目（`.env.example` を参照）。

| 変数名 | 既定 | 内容 |
|---|---|---|
| `COMPOSE_FILE` | `compose.yaml:compose.aka-provisioner.yaml` | 重ねる compose ファイル。別のホストの provisioner につなぐ場合は `compose.yaml` だけ（3 章） |
| `PROVISIONER_SHARED_NETWORK` | `aka-provisioner` | provisioner の共有ネットワークの名前（provisioner の `.env` の同名の項目と同じ） |
| `OSSBSS_DB_PASSWORD` | （必須） | 本BFF 専用の PostgreSQL のパスワード |
| `OSSBSS_ACCESS_TEAM_DOMAIN` | （必須） | Cloudflare Access のチームのドメイン（`<チーム名>.cloudflareaccess.com`） |
| `OSSBSS_ACCESS_AUD` | （必須） | Access のアプリの AUD タグ（複数のアプリで使う場合はカンマ区切り） |
| `OSSBSS_INITIAL_ADMIN_EMAIL` | （必須） | 最初のシステム管理者のメールアドレス（5.2） |
| `OSSBSS_PROVISIONER_URL` | `https://aka-provisioner:9446/admin/v1` | provisioner のベース URL（空なら既定） |
| `OSSBSS_PROVISIONER_CLIENT_KEY` | （空） | クライアント証明書の秘密鍵を別のファイルにした場合の、コンテナ内のパス（`/certs/...`） |
| `OSSBSS_PROVISIONER_TIMEOUT` | `60s` | provisioner の 1 回の呼び出しの上限時間 |
| `OSSBSS_LOG_LEVEL` | `info` | ログのレベル（`debug` / `info` / `warn` / `error`） |
| `OSSBSS_VERSION` | `dev` | 画面に出すバージョン（ビルドのときに渡す。9 章） |
| `TZ` | `Asia/Tokyo` | コンテナのタイムゾーン（画面の日時の表示） |

compose が固定で渡す項目: `OSSBSS_ADDR`（`:8446`）、`OSSBSS_DB_ADDR`（`postgres:5432`）、`OSSBSS_DB_NAME` / `OSSBSS_DB_USER`（`ossbss`）、`OSSBSS_PROVISIONER_CLIENT_CERT`（`/certs/provisioner-client.pem`）、`OSSBSS_PROVISIONER_SERVER_CERT`（`/certs/provisioner-server.pem`）。Tunnel のトークンは環境変数でなく `secrets/tunnel-token`（2.6）。
