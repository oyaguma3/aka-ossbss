-- MVP の起点のスキーマ（設計概要 §14。段階 3-1 の W2）。PoC の 0001_init.sql（所属の記録 subscriber_tenants と
-- client_tenants）は置き換え、引き継がない（PoC のスキーマの DB では起動しない。db.Migrate が断る）。
-- 加入者のデータ（鍵、SQN、状態、認可ポリシー）の正本は下流（aka-vecgen・aka-radius）にあり、台帳はその写し。
-- 秘密の値（Ki / OPc、共有シークレット）は保存しない。
-- 一括投入のジョブ・突き合わせ・利用の集計の表は段階 3-2〜3-4、テナントの slug・ポートと RadSec の表は段階 4 で足す。

-- テナント（法人の顧客など）。作成・変更・削除はシステム管理者だけ。ID は刻印の値になる（設計概要 §6）ので、
-- 削除しても行を残し（deleted_at）、ID を使い回さない。名前は削除していないテナントの中で一意にする。
CREATE TABLE tenants (
    id                   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name                 text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    -- 外部の顧客番号（課金レコードの決定事項 11。編集の画面は段階 3-4）。
    external_customer_id text        NOT NULL DEFAULT '' CHECK (char_length(external_customer_id) <= 100),
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    deleted_at           timestamptz
);
CREATE UNIQUE INDEX tenants_name_live ON tenants (name) WHERE deleted_at IS NULL;

-- 利用者。Cloudflare Access の JWT のメールアドレス（小文字）で識別する。
-- メールアドレスは aka-provisioner に X-Operator-Id として渡すので、その形式（^[A-Za-z0-9._@-]{1,64}$）に限る。
-- 役割: tenant_user（A）、tenant_admin（B）、operator（C）、system_admin（D）。A・B は 1 つのテナントに属し、C・D は属さない。
CREATE TABLE accounts (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      text        NOT NULL UNIQUE CHECK (email ~ '^[a-z0-9._-]+@[a-z0-9._-]+$' AND char_length(email) <= 64),
    role       text        NOT NULL CHECK (role IN ('tenant_user', 'tenant_admin', 'operator', 'system_admin')),
    tenant_id  bigint      REFERENCES tenants (id) ON DELETE RESTRICT,
    disabled   boolean     NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    created_by text        NOT NULL DEFAULT '',
    CONSTRAINT accounts_role_tenant CHECK ((role IN ('tenant_user', 'tenant_admin')) = (tenant_id IS NOT NULL))
);
CREATE INDEX accounts_tenant_id ON accounts (tenant_id);

-- 加入者の台帳（設計概要 §7.1）。IMSI ごとに、所属のテナントと、vecgen 側・radius 側の有無と状態の写しを持つ。
-- aka-provisioner を通した操作が成功したときだけ書き、課金レコードの状態の事実（charging_events）と同じトランザクションで書く。
-- 両側とも物理削除したら deleted_at を付けて残す（登録し直すにはシステム管理者の承認が要る）。
-- 下流の結果が分からなかった操作は check_needed を立てる（両側ともない行は、登録の結果を確かめている間だけある）。
CREATE TABLE subscribers (
    imsi             text        PRIMARY KEY CHECK (imsi ~ '^[0-9]{15}$'),
    tenant_id        bigint      NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    vecgen_present   boolean     NOT NULL DEFAULT false,
    vecgen_state     text        CHECK (vecgen_state IN ('active', 'inactive', 'terminated')),
    radius_present   boolean     NOT NULL DEFAULT false,
    radius_state     text        CHECK (radius_state IN ('active', 'inactive', 'terminated')),
    radius_source_id text,
    -- 回線の外部の番号（任意。課金レコードの決定事項 11。編集の画面は段階 3-4）。
    external_line_id text        NOT NULL DEFAULT '' CHECK (char_length(external_line_id) <= 100),
    registered_at    timestamptz NOT NULL DEFAULT now(),
    registered_by    text        NOT NULL DEFAULT '',
    updated_at       timestamptz NOT NULL DEFAULT now(),
    deleted_at       timestamptz,
    check_needed     boolean     NOT NULL DEFAULT false,
    CONSTRAINT subscribers_vecgen CHECK (vecgen_present = (vecgen_state IS NOT NULL)),
    CONSTRAINT subscribers_radius CHECK (radius_present = (radius_state IS NOT NULL) AND radius_present = (radius_source_id IS NOT NULL)),
    CONSTRAINT subscribers_deleted CHECK (deleted_at IS NULL OR NOT (vecgen_present OR radius_present))
);
-- テナントの範囲の一覧（IMSI の順のページング）と件数に使う。
CREATE INDEX subscribers_tenant_imsi ON subscribers (tenant_id, imsi);
-- 結果を確かめる印の立った行（段階 3-3 の 5 分ごとの確かめ）。
CREATE INDEX subscribers_check_needed ON subscribers (imsi) WHERE check_needed;

-- 課金レコードの状態の事実（課金レコード §5.1・§7.7）。追記だけで、変えない・消さない（13 か月での削除は段階 3-4）。
-- id は events.csv の record_id（E-<id>）になる。tenant_id は対象のテナント（付け替えでは移った先）。
CREATE TABLE charging_events (
    id        bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    time      timestamptz NOT NULL DEFAULT now(),
    event     text        NOT NULL CHECK (event IN ('tenant_created', 'tenant_updated', 'tenant_deleted', 'line_registered',
                                                    'line_state_changed', 'line_tenant_moved', 'line_deleted', 'reconciled')),
    side      text        CHECK (side IN ('vecgen', 'radius')),
    tenant_id bigint      NOT NULL,
    imsi      text,
    from_val  text        NOT NULL DEFAULT '',
    to_val    text        NOT NULL DEFAULT '',
    operator  text        NOT NULL DEFAULT '',
    job_id    text        NOT NULL DEFAULT '',
    trace_id  text        NOT NULL DEFAULT '',
    reason    text        NOT NULL DEFAULT '',
    CONSTRAINT charging_events_line CHECK ((event LIKE 'tenant_%') = (imsi IS NULL)),
    CONSTRAINT charging_events_side CHECK ((event IN ('line_registered', 'line_state_changed', 'line_deleted', 'reconciled')) = (side IS NOT NULL))
);
CREATE INDEX charging_events_time ON charging_events (time);
CREATE INDEX charging_events_imsi ON charging_events (imsi, id) WHERE imsi IS NOT NULL;

-- 監査ログ（設計概要 §14）。tenant_id は対象のテナント（テナントの範囲で見せるため）。13 か月残す（削除は段階 3-3）。
-- テナントを削除しても記録は残すので、外部キーにしない。detail に秘密の値を入れない。job_id は一括投入のジョブ（段階 3-2）。
-- 同じ内容を日ごとの JSONL のファイル（OSSBSS_AUDIT_DIR）にも書く。
CREATE TABLE audit_log (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    time        timestamptz NOT NULL DEFAULT now(),
    actor_email text        NOT NULL DEFAULT '',
    actor_role  text        NOT NULL DEFAULT '',
    tenant_id   bigint,
    action      text        NOT NULL,
    target      text        NOT NULL DEFAULT '',
    detail      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    trace_id    text        NOT NULL DEFAULT '',
    remote      text        NOT NULL DEFAULT '',
    job_id      text        NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_tenant_id ON audit_log (tenant_id, id DESC);
CREATE INDEX audit_log_time ON audit_log (time);
