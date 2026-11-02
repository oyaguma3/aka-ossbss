-- RadSec の受け口の管理（設計概要 §13。段階 4-3 の OB10）。望む状態の正本は aka-ossbss にあり、中央の整合のループ（段階 4-4）がエッジに送る。
-- 課金レコードの数量を月末の時点で数えるので（S4-12）、AP・グループ・持ち込みの CA・配置の行は物理削除せず、created_at と deleted_at を持つ
-- （13 か月で消す。Y11）。

-- エッジの台帳。ID は aka-provisioner の下流の登録簿の ID と同じ。消した ID は同じ ID で登録し直せる（行を生き返らせる）。
CREATE TABLE edges (
    id            text        PRIMARY KEY CHECK (id ~ '^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$'),
    name          text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    -- テナントの数の上限（概要 §9.1）。
    capacity      integer     NOT NULL CHECK (capacity BETWEEN 1 AND 2000),
    -- 公開のアドレス（DNS の A レコードの値。画面に出すだけで、動作には使わない）。
    public_addr   text        NOT NULL,
    -- 上流（aka-radius）の認証と課金（IP アドレス:ポート。宣言に入れる）。
    auth_upstream text        NOT NULL,
    acct_upstream text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    text        NOT NULL DEFAULT '',
    updated_at    timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz
);

-- テナントの RadSec の設定（有効にしたテナントだけ）。slug は永久に一意（CA の ID と証明書の名前に使う）。
-- ポートはテナントの削除で空く（released_at）。テナントの CA の証明書を持つのは、CA が止まっていても宣言を作れるようにするため。
CREATE TABLE tenant_radsec (
    tenant_id          bigint      PRIMARY KEY REFERENCES tenants (id) ON DELETE RESTRICT,
    slug               text        NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$'),
    port               integer     NOT NULL CHECK (port BETWEEN 20000 AND 21999),
    ca_certificate     text        NOT NULL,
    source_cidrs       text[]      NOT NULL DEFAULT '{}',
    -- 共通のブロックの照合の形（CRL のない持ち込みの CA があるときは必須。エッジの設計概要 §24.3）。
    match_attr         text        NOT NULL DEFAULT '',
    match_pattern      text        NOT NULL DEFAULT '',
    -- revision は望む状態の版、requested_revision は反映を求めた版（OB1）。宣言はいつも送り、apply は求めた版に届いていないときだけ呼ぶ。
    revision           bigint      NOT NULL DEFAULT 1,
    requested_revision bigint      NOT NULL DEFAULT 0,
    -- 証明書を失効させたので、エッジに CRL の取り直しを送る必要がある（OB18）。
    crl_dirty          boolean     NOT NULL DEFAULT false,
    created_at         timestamptz NOT NULL DEFAULT now(),
    created_by         text        NOT NULL DEFAULT '',
    updated_at         timestamptz NOT NULL DEFAULT now(),
    released_at        timestamptz,
    ca_disabled_at     timestamptz
);
CREATE UNIQUE INDEX tenant_radsec_port_live ON tenant_radsec (port) WHERE released_at IS NULL;

-- 配置（テナントをどのエッジのどのスロットに置くか）。deleted_at は望む状態から外した時刻（課金の数量はここまで数える）、
-- removed_at はエッジから消し終えた時刻（容量はここまで数える）。replaces はスロットの移動の途中で、置き換える配置（OB7）。
CREATE TABLE placements (
    id               bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id        bigint      NOT NULL REFERENCES tenant_radsec (tenant_id) ON DELETE RESTRICT,
    edge_id          text        NOT NULL REFERENCES edges (id) ON DELETE RESTRICT,
    slot             text        NOT NULL CHECK (slot IN ('primary', 'secondary', 'tertiary')),
    replaces         bigint      REFERENCES placements (id) ON DELETE SET NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    created_by       text        NOT NULL DEFAULT '',
    deleted_at       timestamptz,
    removed_at       timestamptz,
    -- エッジの状態の写し（中央の整合のループが書く）。
    applied_revision bigint      NOT NULL DEFAULT 0,
    apply_result     text        NOT NULL DEFAULT '',
    apply_error      text        NOT NULL DEFAULT '',
    checked_at       timestamptz
);
CREATE UNIQUE INDEX placements_tenant_slot ON placements (tenant_id, slot) WHERE deleted_at IS NULL AND replaces IS NULL;
CREATE UNIQUE INDEX placements_tenant_edge ON placements (tenant_id, edge_id) WHERE removed_at IS NULL;
CREATE INDEX placements_edge ON placements (edge_id) WHERE removed_at IS NULL;

-- AP とグループ。名前はテナントの中で種類をまたいで一意、NAS-Identifier（<slug>-<名前>）は全体で一意（OB8）。
CREATE TABLE aps (
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id  bigint      NOT NULL REFERENCES tenant_radsec (tenant_id) ON DELETE RESTRICT,
    name       text        NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$' AND name <> 'probe'),
    kind       text        NOT NULL CHECK (kind IN ('ap', 'group')),
    purpose    text        NOT NULL DEFAULT '',
    nas_id     text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    created_by text        NOT NULL DEFAULT '',
    deleted_at timestamptz,
    CONSTRAINT aps_purpose CHECK ((kind = 'group' AND purpose IN ('box', 'unifi', 'extreme', 'aruba', 'mist', 'other')) OR (kind = 'ap' AND purpose = ''))
);
CREATE UNIQUE INDEX aps_tenant_name ON aps (tenant_id, name) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX aps_nas_id ON aps (nas_id) WHERE deleted_at IS NULL;
CREATE INDEX aps_tenant_deleted ON aps (tenant_id, name) WHERE deleted_at IS NOT NULL;

-- AP とグループの証明書。入れ替えの間は 1 つの AP に複数の有効な証明書がある。本体は radsec-ca にある。
CREATE TABLE ap_certs (
    serial        text        PRIMARY KEY,
    ap_id         bigint      NOT NULL REFERENCES aps (id) ON DELETE CASCADE,
    not_after     timestamptz NOT NULL,
    issued_at     timestamptz NOT NULL DEFAULT now(),
    issued_by     text        NOT NULL DEFAULT '',
    revoked_at    timestamptz,
    revoke_reason text        NOT NULL DEFAULT ''
);
CREATE INDEX ap_certs_ap ON ap_certs (ap_id);
CREATE INDEX ap_certs_expiring ON ap_certs (not_after) WHERE revoked_at IS NULL;

-- 持ち込みの CA。正本は radsec-ca で、ここには宣言に要るものだけを持つ。Meraki の API キーは持たない（OB15）。
-- removing_at は外し始めた時刻、removing_revision は外した版（全配置がこの版を反映したら radsec-ca で止める。OB6）。
CREATE TABLE brought_cas (
    ca_id             text        PRIMARY KEY,
    tenant_id         bigint      NOT NULL REFERENCES tenant_radsec (tenant_id) ON DELETE RESTRICT,
    name              text        NOT NULL,
    certificate       text        NOT NULL,
    subject           text        NOT NULL,
    has_crl           boolean     NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    created_by        text        NOT NULL DEFAULT '',
    removing_at       timestamptz,
    removing_revision bigint      NOT NULL DEFAULT 0,
    deleted_at        timestamptz
);
CREATE INDEX brought_cas_tenant ON brought_cas (tenant_id) WHERE deleted_at IS NULL;
