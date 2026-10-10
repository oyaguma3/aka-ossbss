-- 初版のスキーマ（設計概要 §11）。加入者のデータ（鍵、認可ポリシー、停止の状態）の正本は下流にあり、
-- ここにはテナント・利用者・所属・監査ログだけを持つ。秘密の値（Ki / OPc、共有シークレット）は保存しない。

-- テナント（法人の顧客など）。作成・変更・削除はシステム管理者だけ。
CREATE TABLE tenants (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       text        NOT NULL UNIQUE CHECK (char_length(name) BETWEEN 1 AND 100),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- 利用者。Cloudflare Access の JWT のメールアドレス（小文字）で識別する。
-- メールアドレスは provisioner に X-Operator-Id として渡すので、その形式（^[A-Za-z0-9._@-]{1,64}$）に限る（設計概要 §15 の 6）。
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

-- 加入者の所属（A 方式。設計概要 §7）。行がなければテナントなし。
CREATE TABLE subscriber_tenants (
    imsi        text        PRIMARY KEY CHECK (imsi ~ '^[0-9]{15}$'),
    tenant_id   bigint      NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    assigned_at timestamptz NOT NULL DEFAULT now(),
    assigned_by text        NOT NULL DEFAULT ''
);
-- テナントの範囲の一覧（IMSI の順のページング）に使う。
CREATE INDEX subscriber_tenants_tenant_imsi ON subscriber_tenants (tenant_id, imsi);

-- RADIUSクライアントの所属。ID は provisioner（本PoC）が採番する。行がなければテナントなし。
CREATE TABLE client_tenants (
    client_id   bigint      PRIMARY KEY CHECK (client_id > 0),
    tenant_id   bigint      NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    assigned_at timestamptz NOT NULL DEFAULT now(),
    assigned_by text        NOT NULL DEFAULT ''
);
CREATE INDEX client_tenants_tenant_client ON client_tenants (tenant_id, client_id);

-- 監査ログ（設計概要 §13）。tenant_id は対象のテナント（テナントの範囲で見せるため）。
-- テナントを削除しても記録は残すので、外部キーにしない。detail に秘密の値を入れない。
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
    remote      text        NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_tenant_id ON audit_log (tenant_id, id DESC);
CREATE INDEX audit_log_time ON audit_log (time);
