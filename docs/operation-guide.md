# 運用ガイド

- 対象: aka-ossbss を MVP セットの管理面の VPS で動かす人。
- 関連: [README](../README.md)（機能と設定の一覧）、[設計概要](design-overview.md)（決定事項と段階の記録）、[画面仕様](screen-spec.md)、aka-provisioner の[運用ガイド](https://github.com/oyaguma3/aka-provisioner/blob/main/docs/operation-guide.md)、[課金レコードの要件と設計](https://github.com/oyaguma3/aka-mvpset/blob/main/docs/charging-records.md)（aka-mvpset）

## 1. 構成と置き場所

`docker compose` で 3 つのコンテナを動かす。管理面の VPS に aka-provisioner と並べて置く。

| コンテナ | 内容 |
|---|---|
| `aka-ossbss` | 画面と業務の処理（台帳、一括投入、突き合わせ、課金レコード）。`:8446` で待ち受ける（HTTP）が、ホストにポートを公開しない。UID 65532 で動く（distroless） |
| `postgres` | 専用の PostgreSQL 18。テナント・アカウント・台帳・ジョブ・突き合わせの結果・課金レコード・監査ログを持つ。ポートを公開しない |
| `cloudflared` | Cloudflare Tunnel のコネクター。外向きに Cloudflare へ接続し、公開ホスト名への要求を aka-ossbss に届ける。UID 65532 で動く |

```
[ブラウザ] ─HTTPS─> Cloudflare（Access: メールのワンタイム PIN）─Tunnel─> cloudflared ─HTTP─> aka-ossbss ── PostgreSQL
                                                       （ホストの受信のポートは開けない）        │     KEK（一括投入の待ちのファイル）
                                                                                                └─mTLS（共有の Docker ネットワーク）─> aka-provisioner ─> aka-vecgen / aka-radius
[運用者] ──SSH（運用用の WireGuard と踏み台）──> コマンド（バックアップ、check-provisioner など）
```

- 利用者は Cloudflare Access でメールのワンタイム PIN でログインする。aka-ossbss は Access が付ける JWT を要求のたびに検証し、メールアドレスでアカウントを引いて、役割とテナントを決める。パスワードは持たない。
- ログインできる人（メールアドレス）は Access のポリシーで、できること（役割・テナント）は aka-ossbss のアカウントで決める。利用者を加えるときは両方に登録する（5.3）。
- 加入者の鍵・SQN・状態・認可ポリシーの正本は下流（aka-vecgen、aka-radius）にある。台帳はその写しで、テナントと課金レコードの正本は aka-ossbss にある。
- aka-provisioner と下流は aka-ossbss を全権の管理クライアントとして扱う。テナントと権限の境界は aka-ossbss だけが担う。

ノードの上に置くもの（`compose.yaml` のあるディレクトリ）:

| ファイル | 内容 | 持ち主と権限 |
|---|---|---|
| `.env` | 設定（項目は `.env.example` と README の「設定」） | 運用者、600 |
| `secrets/kek` | 一括投入の待ちのファイルを暗号化する KEK（6.4） | UID 65532、600 |
| `secrets/tunnel-token` | Cloudflare Tunnel のトークン（2.1） | UID 65532、600 |
| `certs/provisioner-client.pem` | aka-provisioner に提示するクライアント証明書と秘密鍵（3 章） | UID 65532、600 |
| `certs/provisioner-server.pem` | aka-provisioner のサーバー証明書（信頼の起点。3 章） | UID 65532、600 |
| `audit/` | 監査ログの日ごとのファイル（10.3） | UID 65532 |
| ボリューム `<プロジェクト名>_postgres-data` | PostgreSQL のデータ | — |

## 2. 導入

ホストに要るのは Docker Engine と compose プラグインだけである（Debian 13 を主に想定する）。VPS の上ではイメージをビルドしない（12 章）。aka-provisioner を先に導入する（aka-provisioner の運用ガイド 2 章）。aka-ossbss の compose は、aka-provisioner の compose が作る共有ネットワーク `aka-provisioner` と Fluent Bit を使う。

Cloudflare のアカウントで、公開ホスト名に使うドメインのゾーンが Cloudflare にあり、Zero Trust（Free プランでよい。50 人まで）を有効にしてあること。

### 2.1 Cloudflare の準備

Zero Trust のダッシュボードで行う。画面の名前は 2026-10 時点のもので、変わることがある。

1. **ワンタイム PIN をログイン方法に加える**: 「インテグレーション」→「ID プロバイダー」で One-time PIN を加える。
2. **Tunnel を作る**: 「ネットワーク」→「Tunnels」で、種類 Cloudflared の Tunnel を作る。コネクターの導入の画面に出るコマンドの、`--token` の後ろの長い文字列が Tunnel のトークンである（秘密の値）。コネクターの導入のコマンドは実行しない（compose の cloudflared が接続する）。
3. **公開ホスト名を加える**: Tunnel の「公開ホスト名」（Public Hostname）で、サブドメインとドメイン（例 `ossbss.example.com`）を決め、サービスを種類 `HTTP`、URL `aka-ossbss:8446` にする（compose のネットワークの中の aka-ossbss。ホストの `localhost` ではない）。DNS レコードは Cloudflare が Tunnel に向けて作る。
4. **Access のアプリを作る**: 「アクセス」→「アプリケーション」で、種類 セルフホスト（self-hosted）のアプリを作る。
   - 公開ホスト名: 3 のホスト名。
   - ログイン方法: One-time PIN。
   - ポリシー: アクション Allow で、ログインを許す人を Include に書く（Emails か Emails ending in）。最初は最初のシステム管理者のメールアドレスだけでよい（5.2）。
5. **AUD タグとチームのドメインを控える**: アプリの設定（概要）に出る「アプリケーション オーディエンス（AUD）タグ」と、チームのドメイン（`<チーム名>.cloudflareaccess.com`。「設定」に出る）を控える（どちらも秘密の値ではない）。

Access のアプリがあれば、ログインしていない要求は Cloudflare で止まり、aka-ossbss には届かない。

### 2.2 ノードの上の準備

1. `.env` を作る。

   ```bash
   cp .env.example .env && chmod 600 .env
   ```

   | 項目 | 内容 |
   |---|---|
   | `OSSBSS_DB_PASSWORD` | PostgreSQL のパスワード（`openssl rand -hex 24` など） |
   | `OSSBSS_ACCESS_TEAM_DOMAIN` | 2.1 の 5 のチームのドメイン |
   | `OSSBSS_ACCESS_AUD` | 2.1 の 5 の AUD タグ |
   | `OSSBSS_INITIAL_ADMIN_EMAIL` | 最初のシステム管理者のメールアドレス。英数字と `.` `_` `-` と `@` の 64 文字まで（`+` は使えない。aka-provisioner に `X-Operator-Id` として渡すため） |

   `COMPOSE_FILE` の既定（`compose.yaml:compose.aka-provisioner.yaml:compose.fluent-bit.yaml`）は、aka-provisioner と同じ VPS の構成である。

2. KEK を作る。**コンテナを一度でも動かす前に作る**（ないと Docker が同じ名前のディレクトリを作る。できてしまったら、そのディレクトリを消してからファイルを作る）。作ったらオフラインにも写しを置く（6.4）。

   ```bash
   mkdir -p secrets certs audit && (umask 077; openssl rand -hex 32 > secrets/kek)
   ```

3. Tunnel のトークンを置く。シェルの履歴に残らないよう、端末で入力して書く（貼り付けた文字は画面に出ない）。トークンは `.env` に書かない（`docker compose config` の出力やコンテナの環境変数に出るため）。

   ```bash
   (umask 077; read -rs -p 'Tunnel のトークン: ' t; echo; printf '%s' "$t" > secrets/tunnel-token)
   ```

4. aka-provisioner とつなぐ（3.1）。
5. コンテナが読めるようにする。

   ```bash
   sudo chown -R 65532:65532 secrets certs audit
   ```

   ```bash
   sudo chmod 600 secrets/kek secrets/tunnel-token certs/*.pem
   ```

### 2.3 起動と最初のログイン

```bash
docker compose up -d
```

```bash
docker compose ps
```

`aka-ossbss` と `postgres` が `healthy`、`cloudflared` が `Up` になる（cloudflared は aka-ossbss が healthy になってから起動する）。`PORTS` の欄に `0.0.0.0:` や `127.0.0.1:` がないことも確かめる。

- aka-ossbss の起動のログに、スキーマの移行（`database`。`applied` は今回適用した版）、最初のシステム管理者（`initial system admin`）、Access の公開鍵（`cloudflare access`）、aka-provisioner の確かめ（`provisioner is available`）、KEK の識別子（`kek`）、待ち受け（`listening`）が出る。
- cloudflared のログに、Cloudflare への接続が出る（`docker compose logs cloudflared | grep 'Registered tunnel connection'`。通常 4 本）。ダッシュボードの Tunnel の一覧でも状態が「正常」（Healthy）になる。

ブラウザで公開ホスト名を開き、最初のシステム管理者のメールアドレスでログインする。ダッシュボードに役割「システム管理者」と aka-provisioner・下流の状態が出れば導入は終わりである。続けて、テナントとアカウントを作り（5 章）、PGP の公開鍵を登録する（8.1）。

次のどれかに当たると起動しない（ログの `error:` の行か `event_id`）。

| ログ | 原因 | 直し方 |
|---|---|---|
| `KEK_ERR` | `secrets/kek` がない・読めない・形が違う | 6.4 |
| `AUDIT_DIR_ERR` | `audit/` に UID 65532 で書けない | 持ち主と権限を直す |
| `load provisioner …` の誤り | `certs/` の証明書がない・読めない | 3.1 |
| `the database has the PoC schema` | PoC のスキーマの DB を使っている | 新しい DB で始める |
| `the database schema … is newer than this program` | DB のほうが新しい版 | 新しい版で動かすか、バックアップから DB も戻す（12 章） |
| `db is not reachable`（続いて終了） | PostgreSQL に届かない | `docker compose ps postgres`、`docker compose logs postgres`、`OSSBSS_DB_PASSWORD` |
| 設定の誤り（`error:` の行だけ） | 環境変数の値の形が違う | ログを見る |

aka-provisioner に届かなくても起動する（ログの `PROVISIONER_UNAVAILABLE`）。Access の公開鍵を取れないときも起動し、画面が 503 になる（13 章）。

## 3. aka-provisioner とつなぐ

### 3.1 最初につなぐ

同じ VPS では、aka-ossbss は共有ネットワーク `aka-provisioner` に入り、`https://aka-provisioner:9446/admin/v2` でつなぐ。

1. クライアント証明書を作る（証明書と秘密鍵が 1 つの PEM に入る。有効日数は `-days`、既定 825 日）。`-name` の識別名は、aka-provisioner の監査ログの `mgmtClient` になる。aka-provisioner のピンのファイルに書く 1 行が標準エラーに出る。

   ```bash
   docker compose run --rm --no-deps -T aka-ossbss gen-client-cert -name ossbss > certs/provisioner-client.pem
   ```

2. 出た 1 行（`ossbss=<フィンガープリント>`）を aka-provisioner の `secrets/admin-clients` に足す。aka-provisioner が動いていれば `SIGHUP` で読み直し、まだなら起動する（aka-provisioner の運用ガイド 4.1）。
3. aka-provisioner のサーバー証明書を取り出して置く。aka-provisioner のディレクトリで:

   ```bash
   docker compose exec -T aka-provisioner /aka-provisioner server-cert > ../aka-ossbss/certs/provisioner-server.pem
   ```

4. つながることを確かめる（DB には接続しない）。`接続できました` と下流の版と加入者の数が出れば、準備はできている。つながらなければ原因の見当が出る。

   ```bash
   docker compose run --rm --no-deps aka-ossbss check-provisioner
   ```

別のホストの aka-provisioner につなぐときは、`.env` の `COMPOSE_FILE` から `compose.aka-provisioner.yaml` を外し、`OSSBSS_PROVISIONER_URL` を aka-provisioner に届くアドレスにする（aka-provisioner のサーバー証明書の SAN に入れる）。

### 3.2 クライアント証明書を入れ替える

1. 新しい証明書を別の名前で作り、出た 1 行を aka-provisioner のピンのファイルに足して読み直す（新旧の両方を受ける。aka-provisioner の運用ガイド 4.2）。

   ```bash
   docker compose run --rm --no-deps -T aka-ossbss gen-client-cert -name ossbss > provisioner-client-new.pem
   ```

2. 一括投入のジョブが動いていないことを確かめ（12 章）、差し替えて起動し直す。aka-ossbss は証明書を起動のときに読む。

   ```bash
   sudo install -o 65532 -g 65532 -m 600 provisioner-client-new.pem certs/provisioner-client.pem && rm provisioner-client-new.pem
   ```

   ```bash
   docker compose restart aka-ossbss
   ```

3. ダッシュボードの aka-provisioner の状態か `check-provisioner` で確かめ、aka-provisioner のピンから古い行を消して読み直す。

### 3.3 サーバー証明書を差し替える

aka-provisioner がサーバー証明書を作り直したら（aka-provisioner の運用ガイド 4.3）、`certs/provisioner-server.pem` を差し替えて起動し直す。差し替えるまでの間、aka-provisioner につながらない。aka-provisioner のディレクトリで:

```bash
docker compose exec -T aka-provisioner /aka-provisioner server-cert | sudo tee ../aka-ossbss/certs/provisioner-server.pem > /dev/null
```

aka-ossbss のディレクトリで:

```bash
docker compose restart aka-ossbss
```


## 4. 公開範囲

- aka-ossbss・PostgreSQL・cloudflared は、どれもホストにポートを公開しない。ホストの受信のポートは開けなくてよい（cloudflared は外向きに接続する）。
- 公開ホスト名への要求は、Cloudflare Access のログインを通ったものだけが Tunnel を通って aka-ossbss に届く。aka-ossbss は、さらに要求のたびに JWT（署名、`iss`、`aud`、有効期限）を検証し、検証できなければ 401 を返す（`/healthz` と静的ファイルを除く）。同じホストの Docker ネットワークから直接届いても、JWT がなければ画面は使えない。
- 接続元のアドレス（アクセスログ、監査ログ）は、cloudflared が付ける `Cf-Connecting-IP` を使う。aka-ossbss に届くのは cloudflared からだけという前提なので、同じ Docker ネットワークのコンテナからの要求ではこの値を偽れる（記録の値だけで、権限には影響しない）。
- ログインできる人は Access のポリシーで絞る。アカウントのないメールアドレスでログインした人には 403 を返す（何も操作できない）。
- ログアウトは画面の「ログアウト」（Access のログアウト）。ログインの有効期間は Access のアプリの設定（セッションの期間）で決まる。
- 開発用の設定（`OSSBSS_DEV_USER_EMAIL`）は compose では使えない（ループバックで待ち受けるバイナリだけ）。

## 5. アカウントとテナント

### 5.1 役割

| 役割 | できること（一覧は設計概要 §5） |
|---|---|
| テナントユーザー（A） | 自分のテナントの加入者とセッションを見る。加入者の状態の変更 |
| テナント管理者（B） | A に加え、自分のテナントの認可ポリシーの変更、テナントユーザーのアカウントの管理、自分のテナントの監査ログ |
| 運用担当（C） | 全テナントの閲覧、radius 側の登録、認可ポリシーと取得先の変更、個別の物理削除、課金レコードのダウンロードと外部の番号の設定、一括投入と突き合わせの閲覧、下流の監査ログ |
| システム管理者（D） | すべて。vecgen 側の登録（鍵）と変更、一括投入、突き合わせの対応、解約からの戻し、削除済みの IMSI の承認、所属の付け替え、PGP の公開鍵、テナントとアカウントの管理 |

### 5.2 最初のシステム管理者

- `OSSBSS_INITIAL_ADMIN_EMAIL` のアカウントは、起動のたびにシステム管理者・テナントなし・有効になる（なければ作る）。画面からの削除・降格・無効化はできない。
- 誤って全員を締め出したときは、このアカウントでログインする。変えられていても、起動し直せば戻る（`docker compose restart aka-ossbss`）。
- 別の人に替えるときは、`.env` を書き換えて `docker compose up -d` で作り直す（前の人のアカウントは普通のシステム管理者として残るので、要らなければ画面から削除する）。

### 5.3 利用者を加える

1. 「アカウント」で、メールアドレス・役割・テナントを登録する（テナント管理者は、自分のテナントのテナントユーザーだけを登録できる）。
2. Cloudflare Access のアプリのポリシーで、そのメールアドレス（またはドメイン）のログインを許す。

どちらかが欠けるとログインできない（Access で止まるか、aka-ossbss が 403 を返す）。役割・テナントの変更と無効化は次の要求から効く（aka-ossbss はセッションを持たず、要求のたびにアカウントを引く）。

- 自分のアカウントは、削除・無効化・役割やテナントの変更ができない（他のシステム管理者に頼む）。
- 利用者がいなくなったら、アカウントを無効化または削除し、Access のポリシーからも外す。

### 5.4 テナント

- すべての加入者（IMSI）はどれか 1 つのテナントに属する（「テナントなし」はない）。外形監視の試験用の SIM も専用のテナントに属させる。
- テナントの ID は刻印の値になる。変わらず、削除しても使い回さない。
- 加入者（削除済みを除く）とアカウントが残っているテナントは削除できない。削除した行は残す（課金レコードの事実と監査ログのため）。
- 外部の顧客番号（テナントの詳細）と回線の外部の番号（加入者の詳細）は、運用担当とシステム管理者が設定する。課金レコードに出る（8 章）。

## 6. 一括投入

「一括投入」（`/jobs`）で、CSV から加入者の登録と物理削除をまとめて行う。登録・投入・中断はシステム管理者、閲覧は運用担当もできる。

### 6.1 ジョブの種類と CSV

CSV は UTF-8、ヘッダー行あり（固定）、10,000 行・8 MiB まで。BOM と CRLF は受ける。16 進は小文字にして送る。

| ジョブ | 一律に選ぶもの | ヘッダー |
|---|---|---|
| vecgen 側の登録 | テナント、状態、許可クライアント、SQN の方式 | `imsi,ki,opc,amf,sqn`（`amf`・`sqn` は空なら既定） |
| radius 側の登録 | テナント、状態、取得先、ルールの組（1〜32 件。テナントは選んだテナント） | `imsi` |
| vecgen 側の物理削除 | — | `imsi` |
| radius 側の物理削除 | — | `imsi` |

新しい回線を入れるときは、両側のジョブを同じテナントで登録する。認証が通るのは、両側とも `active` になってからである。許可クライアントには aka-radius の AV クライアントを選ぶ。

**鍵の入った CSV の扱い**: 作ったファイルは投入の後すぐに消す。平文のファイルをメールやチャットで渡さない。aka-ossbss は KEK で暗号化して DB に置き、完了・中断・取り消しのときに消す。ディスクに平文を書かず、画面・ログ・監査ログ・成功以外の行の CSV に鍵を出さない。

### 6.2 流れ

1. 「ジョブを登録」でファイルと一律の設定を送ると、全行をその場で確かめる（形式、ファイルの中の重複、別のテナントに属する IMSI、結果を確かめている IMSI、削除の対象が Active）。**1 行でも誤りがあれば何もしない**（行番号つきで誤りを出す。CSV を直して送り直す）。
2. 確認の画面で件数を見る。「既にある」（同じ側に登録済み）と「台帳にない」（削除の対象）は、下流を呼ばずに飛ばす。
3. 削除済みの IMSI があれば、件数と IMSI が出る。「承認して含める」（理由が必須。監査ログに残る）、「除いて進める」、「取り消す」から選ぶ。
4. 「投入する」で待ち行列に入れる。確定しない検証済みのジョブは 24 時間で取り消す。
5. 詳細の画面とダッシュボードで進み具合を見る。終わったら、成功以外の行（既にある・ない・失敗・未処理）を CSV で取り出せる。

待ち行列は対象（vecgen・radius）ごとに 1 本で、それぞれ同時に 1 つのジョブを、確定した順に動かす。1 件ずつ aka-provisioner を呼ぶ（同時 `OSSBSS_BULK_CONCURRENCY` 件、既定 5）。

### 6.3 中断と投入し直し

- システム管理者は、待ちと実行中のジョブを中断できる。処理中の行を終えてから止め、残りを未処理として記録する。再開はしない。
- 接続の誤り（届かない・503・504）が 10 件続くと、自動で中断する（`JOB_INTERRUPTED`）。下流を直してから投入し直す。
- **同じファイルを投入し直せば、残りだけが処理される**（台帳に既にある行は下流を呼ばずに飛ばす）。
- aka-ossbss を止めると、動いていたジョブは処理中の行を終えて中断になり、待っていたジョブは起動の後に続けて動く。強制終了のときは、起動のときに中断にする（未処理の行の一覧は残らないが、投入し直せば同じことである）。
- 結果が分からない行（タイムアウトなど）は、読み直して済んでいれば成功、そうでなければ失敗にする。読み直せないときは台帳に印が残り、5 分ごとの確かめで直る（9 章）。その IMSI は印が消えるまで投入できない。

### 6.4 KEK

- `secrets/kek`（32 バイトの乱数の 16 進）は、待ちのファイルの暗号化にだけ使う。作ったらオフラインに写しを置く。DB のバックアップと同じ場所に置かない（両方を手にした人は待ちのファイルを読める）。
- **KEK を失っても、消えるのは待ちのジョブのファイルだけである。** 加入者の鍵は aka-vecgen にある（aka-vecgen の KEK とは別のもの）。新しい KEK を置いて起動し、中断になったジョブを投入し直す。
- 入れ替えのコマンドはない。入れ替えるときは、待ちのジョブがないことを確かめ、新しいファイルを置いて作り直す（`mv` で置き換えたら `docker compose up -d --force-recreate aka-ossbss`。その場で上書きしたら `restart` でよい）。今の KEK は起動のログの `kek` の識別子で分かる。
- 復号できない待ちのジョブは、動かすときに中断にする（`JOB_DECRYPT_ERR`）。

## 7. 突き合わせ

毎日 4 時（`OSSBSS_RECONCILE_AT`）と手動で、台帳と両側の下流の一覧を IMSI の順に比べる。「突き合わせ」（`/reconcile`）で履歴とずれの一覧を見る（運用担当は閲覧、対応はシステム管理者）。画面と一括投入は止めない。

### 7.1 見つけたずれ

下流が正本なので、台帳の側を直す。

| ずれ | 扱い |
|---|---|
| 台帳にあって下流にない | 自動で台帳を直す（その側を「なし」に。両側ともなくなれば削除済み） |
| 状態・取得先が違う | 自動で台帳を直す |
| 行は台帳にあり、その側だけ「なし」なのに下流にある | 自動で台帳を直す |
| IMSI が台帳にない | システム管理者が決める（7.3） |
| 台帳で削除済みなのに下流にある | システム管理者が決める（7.3。取り込みは理由が必須） |
| ルールのテナントが所属と違う | 直さず、報告だけする。認可ポリシーの画面で保存し直すと直る |

自動の修正は監査ログ（`subscriber.reconcile`。定刻の実行の操作者は `system`、トレースID は `reconcile-<番号>`）に残し、状態が変わったものは課金レコードの補いの事実（`reconciled`）を書く。補いの事実は見つけた時刻の日に数え、前の月は直さない。

ずれが出続けるなら、aka-ossbss を通さずに下流を変えている操作がないかを確かめる（ノードの上のコマンド、管理API の直接の呼び出し）。

### 7.2 保留の承認

自動で直す数が `OSSBSS_RECONCILE_MAX_FIXES`（既定 1,000）を超えると、**何も直さずに保留にする**（下流を古いバックアップから戻したときなどに、台帳と課金レコードを大量に書き換えないため。ダッシュボードに警告が出る）。一覧の読み取りが途中で失敗したときも何も直さない（`RECONCILE_FAILED`）。

1. 保留の実行の詳細で、ずれの種類と件数を見る。
2. 心当たりを確かめる（下流を戻していないか、下流の取り違えがないか）。心当たりがなければ、先に下流を調べる。
3. 正しいと判断したら「このまま直す」（理由が必須）。読み直してから直す。

### 7.3 台帳にない IMSI

最新の実行のずれの一覧で、行を選んで対応する（1 回 100 件まで）。実行のときに読み直し、解消していれば「解消済み」になる。

- **取り込む**: テナントを選んで台帳に入れる。ルールのテナントが全部同じならそれが候補になり、違うときはルールを書き換えてから台帳に入れる。削除済みの IMSI は理由が必須（登録し直しと同じ承認）。
- **下流から削除する**: 確認のダイアログの後に、その側から消す。`active` は止めてから消す。

### 7.4 手動の実行

「今すぐ実行する」で動かす（同時に 1 つ）。下流を戻した後、一括投入の失敗の後などに使う。

## 8. 課金レコード

「課金レコード」（`/charging`）で月次のまとめを扱う。閲覧とダウンロードは運用担当とシステム管理者、公開鍵の登録・作り直し・今すぐ取り込む・今すぐ締めるはシステム管理者。作りの正本は `charging-records.md`（aka-mvpset）。

### 8.1 PGP の鍵を作る・入れ替える

秘密鍵は、ファイルを読む外部のシステムの側で作って持つ。**VPS に秘密鍵を置かない。**

```bash
gpg --quick-generate-key 'aka-ossbss charging <charging@example.com>' default default 2y
```

```bash
gpg --armor --export charging@example.com > charging-public.asc
```

- `default default` は Curve25519 の鍵と暗号化の副鍵を作る。登録で受けるのは RSA 3072 ビット以上か Curve25519 で、暗号化に使える有効な鍵。`gpg --quick-generate-key <名前> rsa3072 default` は暗号化の副鍵を作らないので断られる（`gpg --quick-add-key <指紋> rsa3072 encr` で足す）。
- 公開鍵の ASCII の形を画面に貼って登録する。秘密鍵が貼られたら断り、入力を画面に戻さない。
- **入れ替え**: 新しい公開鍵を登録する。次に作るファイルから新しい鍵を使い、前のファイルは古い鍵のまま残す。古いファイルを読み続けるために、古い秘密鍵は捨てない。前の月を新しい鍵で読みたいときは作り直す（8.4）。
- **警告**（画面、ダッシュボード、ログの `CHARGING_KEY_WARN`）: 期限の 30 日前から、期限切れ、鍵がないまま毎月 25 日を過ぎたとき、鍵がなくて保留の月があるとき。

### 8.2 締めとファイル

| 処理 | いつ | 内容 |
|---|---|---|
| 利用の集計の取り込み | 毎日 3 時（`OSSBSS_USAGE_IMPORT_AT`）と締めの前 | aka-radius の前の 7 日分を読み直す。読めない日は画面に出る（`USAGE_IMPORT_ERR`） |
| 締め | 毎日 6 時 | 締めていない過去の月を古い順に締める（ふつうは翌月 1 日の 6 時。止まっていれば動き出した後に）。事実のない月も出す |

- 通し番号は締めのときに続けて割り当てる（欠けがないことで、外部のシステムが取り込み漏れを見つけられる）。
- **鍵がない・期限切れのとき**は、計算の結果を残してファイルを保留にする（平文のファイルは作らない）。鍵を登録するとすぐ作る。
- 締めの失敗（`CHARGING_CLOSE_ERR`・`CHARGING_FILE_ERR`）は、原因を直してから「今すぐ締める」か次の定刻で締め直す。1 つの月の締めは 1 つのトランザクションで、途中で止まれば何も残らない。
- 取り込めなかった日があっても締める。その日に登録されていた回線の行は `usage_complete` が `false` になり、後で読めれば翌月の訂正で直る（8.5）。aka-radius が止まっていた間の利用の欠けは見分けられない。

### 8.3 ダウンロードと確かめ方

画面のファイルの名前（`charging_<YYYYMM>_<通し番号 6 桁>_v<版>.zip.pgp`）からダウンロードする。毎回、監査ログ（`charging.download`）に残る。読むのは秘密鍵のある外部のシステムの側で行う。

```bash
gpg --output charging.zip --decrypt charging_202611_000001_v1.zip.pgp
```

```bash
unzip charging.zip -d charging
```

```bash
sha256sum charging/events.csv charging/lines.csv charging/tenants.csv charging/lines_adj.csv
```

`manifest.csv` の件数と SHA-256 と比べる。aka-mvpset の `verify/charging/verify-file.py` が、この確かめ（CSV の形、`record_id` の形と重複を含む）をまとめて行う。平文の CSV は、取り込んだら消す。

### 8.4 作り直し

ファイルのある月を「作り直す」と、保存した月次の値から今の鍵でファイルを作り、版を上げる（通し番号と `record_id` は同じ。外部の番号は作り直すときの今の値）。古い版も残る。使うのは、鍵を入れ替えたとき、外部の番号を直したとき、ファイルを失くしたとき。

計算し直せるのは、ファイルのない最新の月だけである（古い月を計算し直すと、次の月の月初の状態と食い違うため）。

### 8.5 訂正の読み方

- 出した行は変えない。締めの後に分かった差は、次の月のまとめの `lines_adj.csv` に、**置き換える行の全体**を 1 行で出す（`record_id` が `A-<出す月>-<IMSI>-<テナント>`、`period` が置き換える行の月、`adjusts_record_id` が置き換える元の `record_id`、`adjust_reason` が `late_usage`）。外部のシステムは、`adjusts_record_id` の行を置き換える。
- 訂正が出るのは前の月の利用（`auth_success_count`・`session_count`・`session_seconds`・`input_octets`・`output_octets` と `usage_complete`）だけ。利用の遅れの取り込みのほかに訂正はない。
- 突き合わせの補いの事実は `events.csv` に出て、見つけた時刻の日に数える。前の月を直さない。
- 利用の行は、集計の刻印のテナントのまま出る。登録と違うテナントなら `registered_days` が 0 の行、刻印がなければ `tenant_id` 0 の行になる。判断は外部のシステムに任せる。

## 9. 定期の処理の時刻

aka-ossbss のプロセスの中で動く（日本時間に固定）。結果は「突き合わせ」と「課金レコード」の画面の定期の処理の表に出る。

| 処理（`scheduled_runs` の名前） | いつ | 内容 |
|---|---|---|
| `usage-import` | 毎日 3 時（`OSSBSS_USAGE_IMPORT_AT`） | 利用の集計の取り込み（8.2） |
| `reconcile` | 毎日 4 時（`OSSBSS_RECONCILE_AT`） | 突き合わせ（7 章） |
| `retention` | 毎日 4 時 30 分 | 暦で 13 か月より古い記録の削除（監査ログ、ジョブ、突き合わせ、定期の処理の記録、課金レコードの事実と月と利用の集計） |
| `charging-close` | 毎日 6 時 | 課金レコードの締め（8.2） |
| `check-needed` | 5 分ごと | 結果が分からなかった加入者の読み直し（100 件まで。読めなければその回はやめる） |
| `job-sweep` | 1 時間ごと | 確定しないジョブの取り消し（6.2） |
| `audit-files` | 1 時間ごと | 監査ログのファイルの圧縮と削除（10.3） |

- 止まっている間に過ぎた毎日の処理は、起動の後に何日分でも 1 回だけ動かす（起動の 1 分後に最初の確かめをする）。
- 失敗は自動でやり直さない。次の定刻か手動で動かす（`SCHEDULE_ERR`、画面の結果）。
- 毎日の時刻を変えるときは `.env` を書き換えて `docker compose up -d`。締めと 13 か月の削除の時刻は固定である。突き合わせ（4 時）は取り込み（3 時）の後、締め（6 時）はその両方の後になるようにしておく。

## 10. ログと監査ログ

| 記録 | 内容 | 置き場所 | 持つ期間 |
|---|---|---|---|
| ログ | 要求、定期の処理、誤り | aka-provisioner の `logs_on_host/ossbss.log`（`compose.fluent-bit.yaml`）と `docker compose logs` | ノードで 2 週間（aka-provisioner の運用ガイド 5 章） |
| 監査ログ | 利用者とシステムの変更の操作、課金レコードのダウンロード | PostgreSQL の `audit_log` と `audit/` の日ごとのファイル | DB は 13 か月、ファイルはノードで 14 日 |

Ki・OPc、KEK、Tunnel のトークンはログにも監査ログにも出さない。IMSI はマスクしない（ほかのノードと同じ）。ログを取りに行く監視用の PC の保管とアクセスできる人も、ノードと同じく限る。

### 10.1 ログ

JSON（1 行に 1 つ）で出す。アクセスログには、利用者のメールアドレス、接続元、トレースID が入る。静的ファイルと `/healthz` は `debug`（`OSSBSS_LOG_LEVEL=debug`）。

### 10.2 `event_id` と対応

監視で拾う対象である。

| `event_id` | 内容 | 対応 |
|---|---|---|
| `PROVISIONER_UNAVAILABLE` | aka-provisioner に届かない | `check-provisioner`（13 章） |
| `RECONCILE_DRIFT` | 突き合わせでずれを見つけた（件数） | 画面で見る（7 章）。毎日出るなら下流を直接変えている操作を探す |
| `RECONCILE_FAILED` | 突き合わせが失敗した、一覧を読み切れなかった | 下流の状態を確かめ、手動で実行する（7.4） |
| `SCHEDULE_ERR` | 定期の処理が失敗した、記録を書けない | 画面の定期の処理の表、DB の状態 |
| `USAGE_IMPORT_ERR` | 利用の集計を読めない日があった | aka-radius の状態。読めれば次の回に追いつく（7 日まで） |
| `CHARGING_CLOSE_ERR` | 締めの失敗 | ログの `error`。直してから締め直す（8.2） |
| `CHARGING_FILE_ERR` | ファイルを作れなかった | ログの `error`。鍵の確かめ、作り直し（8.4） |
| `CHARGING_KEY_WARN` | PGP の鍵がない・期限が近い・切れている | 鍵を登録する（8.1） |
| `JOB_INTERRUPTED` | 一括投入のジョブが中断した（自動・手動・停止のとき、どれも。`reason` が理由） | 理由を確かめ、投入し直す（6.3） |
| `JOB_DECRYPT_ERR` | 待ちのジョブのファイルを復号できない | KEK を確かめる（6.4） |
| `JOB_DB_ERR` / `JOB_FILE_ERR` | ジョブの記録を書けない / ファイルを読めない | DB の状態（ディスクの空き） |
| `LEDGER_WRITE_ERR` | 下流は成功したのに台帳を書けなかった（印を立てる） | DB の状態。印は 5 分ごとに読み直して直る |
| `AUDIT_DB_ERR` / `AUDIT_FILE_ERR` | 監査ログを DB / ファイルに書けない（操作は成功のまま） | DB の状態、`audit/` の持ち主と空き |
| `AUDIT_DIR_ERR` / `KEK_ERR` | 起動の失敗（2.3 の表） | 2.3 |

### 10.3 監査ログ

- 画面の「監査ログ」（`/audit`）で見る（テナント管理者は自分のテナントの分だけ）。運用担当とシステム管理者は、aka-provisioner・aka-vecgen・aka-radius の監査ログも同じ画面のタブで見られる。
- 日ごとのファイル `audit/audit-YYYYMMDD.jsonl`（日付は日本時間）。前の日までのファイルは gzip で圧縮し、14 日で消す（1 時間ごと）。監視用の PC が取りに行く。DB の 13 か月の削除は 9 章。
- 操作の名前の一覧は[画面仕様](screen-spec.md)。結果が分からなかった操作も `detail.result` を `unknown` にして残す。

### 10.4 トレースID で記録をたどる

aka-ossbss の監査ログのトレースID は、aka-provisioner と下流のログ・監査ログと同じ値である（aka-provisioner の運用ガイド 7.3）。一括投入の行は `job-<ジョブの番号>`、突き合わせの修正は `reconcile-<実行の番号>`。

## 11. バックアップと戻し

| 守るもの | 取り方 | 失うと |
|---|---|---|
| PostgreSQL（テナント、アカウント、台帳、課金レコードとファイル、監査ログ、ジョブ） | `scripts/backup-db.sh`（止めずに `pg_dump -Fc`） | 課金レコードの正本と監査ログを失う。台帳は下流から取り込み直せるが、テナントへの所属は失う |
| `secrets/kek` | 作ったときにオフラインへ（6.4） | 待ちのジョブのファイルだけ |
| `secrets/tunnel-token` | オフラインへ | Cloudflare のダッシュボードで出し直す |
| `.env`、`certs/` | ファイルの写し（運用者だけが読める場所に） | 作り直して登録し直す（3 章） |
| `audit/` | 監視用の PC が取りに行く | DB に同じ内容がある |

バックアップ:

```bash
mkdir -p backups && chmod 700 backups
```

```bash
scripts/backup-db.sh backups/ossbss-$(date +%Y%m%d-%H%M%S).dump
```

- 動いたまま取れる（`pg_dump` は 1 つのスナップショットを読む）。利用の集計の表は 13 か月で 11〜12 GB の見込みなので、圧縮しても写しは大きく、時間もかかる。置き場所の空きを確かめる（容量と時間は段階 6 で測る）。
- 写しには一括投入の待ちのファイル（暗号化）が入る。KEK と同じ場所に置かない。
- 頻度と保管する期間（RPO・RTO）は、段階 5 の前に決める。

### 11.1 戻し方

今のデータは消える。

```bash
scripts/restore-db.sh backups/ossbss-20261031-120000.dump
```

```bash
docker compose up -d
```

スクリプトは aka-ossbss と cloudflared を止め、DB を作り直してから読み込む。素の `docker compose` を使うので、プロジェクト名や重ね合わせを変えて起動しているときは `COMPOSE_PROJECT_NAME`・`COMPOSE_FILE` を付けて動かす。動いている postgres が見つからなければ何もしない。

### 11.2 戻した後

1. **待ちのジョブを確かめる。** 写しの時点で待っていたジョブは起動の後に動き出す。動いていたジョブは中断になる。要らないものはすぐ中断する（`/jobs`）。
2. **突き合わせを手動で動かす**（7.4）。写しの後の下流の変更と台帳の差を直す。写しの後に登録した IMSI は「台帳にない」になり、テナントを選んで取り込む（7.3）。差が上限を超えれば保留になるので、内容を確かめて承認する（7.2）。
3. **課金レコードを確かめる**（下の注意）。
4. アカウントとテナントの写しの後の変更をやり直す。

**課金レコードの注意**: 写しの後に締めた月は、戻すと「締めていない月」に戻り、次の締めで**同じ通し番号で別の中身のファイル**ができることがある（戻した分の事実がなくなり、突き合わせの補いの事実が増えるため。戻すのを防ぐ仕組みはない）。

- 戻す前に、外部のシステムに渡し済みの月・通し番号・版と、バックアップの日時を控える。
- 写しより後に締めた月があれば、戻した後に締め直された月のファイルをダウンロードして、外部のシステムに「同じ通し番号で置き換える」と伝える（`record_id` は回線と月で決まるので、同じ行は置き換えになる。`events.csv` の `record_id` は事実の番号なので、同じ番号で別の中身になることがある）。
- 渡し済みのファイルの正本は外部のシステムの側にも残るので、戻す前の内容と比べられる。

## 12. 更新

MVP セットの VPS の上ではイメージをビルドしない。イメージは手元でビルドして VPS に送る（送り方は段階 5 の配置の文書で決める）。

1. 更新の前に DB のバックアップを取る（11 章）。
2. 一括投入のジョブが動いていないこと、締めの最中でないことを確かめる（`/jobs`、`/charging`）。止めると、動いているジョブは処理中の行を終えて中断になる（最大 40 秒）。定期の処理は、止まっている間に過ぎた分を起動の後に動かす。
3. `.env.example` の増えた項目を `.env` に足す。
4. イメージを入れ替え、`docker compose up -d` で作り直す。
5. `docker compose ps` で healthy、画面のフッターの版を確かめる。

- DB のスキーマは起動のときに自動で移行する。DB の版が aka-ossbss より新しいと起動しない（古い版に戻すときは、バックアップから DB も戻す）。
- 画面のフッターと起動のログの版は、ビルドのときの `OSSBSS_VERSION`（既定 `dev`）で決まる。

```bash
OSSBSS_VERSION=$(git rev-parse --short HEAD) docker compose build
```

- cloudflared の版は `compose.yaml` で固定している。更新はこのリポジトリの更新に含まれる。

## 13. 障害時の確かめ

| 症状 | 確かめること |
|---|---|
| 公開ホスト名を開くと Cloudflare のエラー（502、1033 など） | Tunnel が aka-ossbss に届いていない。`docker compose ps` で cloudflared が動いているか、`docker compose logs cloudflared`、公開ホスト名のサービスが `http://aka-ossbss:8446` か（`localhost` ではない）、aka-ossbss が healthy か |
| cloudflared が起動しない・再起動を繰り返す | `secrets/tunnel-token` があり、UID 65532 で読めるか（`permission denied`）、空でないか（`requires the ID or name of the tunnel` が出る）、トークンが正しいか |
| 「Cloudflare Access を通して開いてください。」（401） | JWT を検証できない。`OSSBSS_ACCESS_TEAM_DOMAIN` と `OSSBSS_ACCESS_AUD` が Access のアプリのものと同じか。ログの `unauthenticated request` の `reason` |
| 「この利用者（…）は登録されていません。」（403） | そのメールアドレスのアカウントがない、または無効。別のメールアドレスでログインし直すときは、画面のボタン（Access のログアウト）を使う |
| 「Cloudflare Access の公開鍵を取得できません」（503） | コンテナから `https://<チームのドメイン>/cdn-cgi/access/certs` に外向きに接続できるか（DNS、プロキシ） |
| 「データベースに接続できません」（503） | `docker compose ps postgres`、`docker compose logs postgres`、ディスクの空き |
| aka-provisioner に接続できない（ダッシュボード、画面の誤り） | `docker compose run --rm --no-deps aka-ossbss check-provisioner` に原因の見当が出る（ピンにない、サーバー証明書が違う、SAN にない、共有ネットワークに入っていない、止まっている） |
| aka-provisioner から下流に接続できない | ダッシュボードの下流の状態の `hint`、aka-provisioner の運用ガイド 10 章 |
| 「結果を確かめています」が消えない | 下流に届かず読み直せない。下流が戻れば、詳細を開いたときか 5 分ごとの確かめで直る。ダッシュボードの「確かめている」の数を見る |
| 一括投入が中断する | 詳細の中断の理由。接続の誤りが続いた（6.3）、aka-ossbss が止まった、復号できない（6.4） |
| 一括投入の検証で「結果を確かめている IMSI」の誤りが出る | その IMSI を読み直せるようになってから投入し直す |
| 突き合わせが保留のまま | 7.2 |
| 課金レコードのファイルができない | 画面の警告（鍵がない・期限切れ。8.1）、`CHARGING_FILE_ERR` |
| 通し番号に欠けがあるように見える | 締めのトランザクションの中で割り当てるので、欠けは作らない。外部のシステムの取り込み漏れを確かめる。戻しの後なら 11.2 |
| コンテナの起動で `… is not a file`、`secrets/kek` がディレクトリ | 2.2 の 2。ディレクトリを消してファイルを作る |
| コンテナが unhealthy | `docker compose exec aka-ossbss /aka-ossbss healthcheck`、起動のログ（2.3 の表） |
| 監査ログのファイルが増えない | `AUDIT_FILE_ERR`（`audit/` の持ち主と空き） |
