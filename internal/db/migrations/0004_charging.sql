-- 課金レコード（課金レコードの要件と設計。段階 3-4 の Z2）。状態の事実（charging_events）は 0001 にある。

-- 利用の集計（IMSI・テナント・日ごと）。aka-radius から取り込み、13 か月持つ。tenant_id は集計の刻印のテナントで、台帳の所属に
-- 付け替えない（刻印がなければ 0。Z5）。最大 7,800 万行になるので月ごとのパーティションにし、古い月は DROP で消す（Z7）。
-- パーティション（usage_daily_YYYYMM）は取り込みのときに作る。
CREATE TABLE usage_daily (
    date       date   NOT NULL,
    imsi       text   NOT NULL,
    tenant_id  bigint NOT NULL,
    auth_ok    bigint NOT NULL,
    sessions   bigint NOT NULL,
    seconds    bigint NOT NULL,
    in_octets  bigint NOT NULL,
    out_octets bigint NOT NULL,
    PRIMARY KEY (imsi, tenant_id, date)
) PARTITION BY RANGE (date);

-- 日ごとの取り込みの成否。imported_at は最後に読み切った時刻で、一度も読み切っていない日は NULL（Z10 の取り込めなかった日）。
CREATE TABLE usage_import_days (
    date         date        PRIMARY KEY,
    attempted_at timestamptz NOT NULL,
    imported_at  timestamptz,
    last_error   text        NOT NULL DEFAULT ''
);

-- 締めた月。sequence は月次のまとめの通し番号（締めのときにロックの中で最大値 +1 を割り当てる。欠けないように identity は使わない。Z9）。
-- state: computed（計算だけ済み。鍵がないなどでファイルがない）、filed（ファイルがある）。
CREATE TABLE charging_periods (
    period           date        PRIMARY KEY,
    sequence         integer     NOT NULL UNIQUE,
    state            text        NOT NULL DEFAULT 'computed' CHECK (state IN ('computed', 'filed')),
    event_count      integer     NOT NULL DEFAULT 0,
    line_count       integer     NOT NULL DEFAULT 0,
    tenant_count     integer     NOT NULL DEFAULT 0,
    adj_count        integer     NOT NULL DEFAULT 0,
    -- その月の取り込めなかった日の数。
    missing_days     integer     NOT NULL DEFAULT 0,
    closed_at        timestamptz NOT NULL,
    closed_by        text        NOT NULL
);

-- 回線ごとの月次（課金レコード §7.4）。registered_mask は登録されていた日のビット（1 日が最下位。訂正のときの usage_complete の
-- 計算に使う）。open_end は、月末にこの行のテナントに登録されていたか（次の月の月初の状態になる。Z3）。
CREATE TABLE charging_lines (
    period              date     NOT NULL REFERENCES charging_periods (period) ON DELETE CASCADE,
    imsi                text     NOT NULL,
    tenant_id           bigint   NOT NULL,
    registered_mask     integer  NOT NULL,
    registered_days     smallint NOT NULL,
    radius_active_days  smallint NOT NULL,
    vecgen_active_days  smallint NOT NULL,
    radius_state_start  text     NOT NULL,
    radius_state_end    text     NOT NULL,
    vecgen_state_start  text     NOT NULL,
    vecgen_state_end    text     NOT NULL,
    first_registered_at timestamptz,
    deleted_at          timestamptz,
    auth_ok             bigint   NOT NULL,
    sessions            bigint   NOT NULL,
    seconds             bigint   NOT NULL,
    in_octets           bigint   NOT NULL,
    out_octets          bigint   NOT NULL,
    usage_complete      boolean  NOT NULL,
    open_end            boolean  NOT NULL,
    PRIMARY KEY (period, imsi, tenant_id)
);
CREATE INDEX charging_lines_open ON charging_lines (period, imsi) WHERE open_end;

-- テナントごとの月次（課金レコード §7.5）。live_end は月末に存在していたか（次の月の月初の状態になる）。
-- aps_end などは段階 4 まで 0（Z14）。
CREATE TABLE charging_tenants (
    period                  date    NOT NULL REFERENCES charging_periods (period) ON DELETE CASCADE,
    tenant_id               bigint  NOT NULL,
    tenant_name             text    NOT NULL,
    created_at              timestamptz,
    deleted_at              timestamptz,
    existed_days            smallint NOT NULL,
    lines_end               integer NOT NULL,
    lines_max               integer NOT NULL,
    radius_active_lines_end integer NOT NULL,
    radius_active_lines_max integer NOT NULL,
    aps_end                 integer NOT NULL DEFAULT 0,
    groups_end              integer NOT NULL DEFAULT 0,
    brought_cas_end         integer NOT NULL DEFAULT 0,
    placements_end          integer NOT NULL DEFAULT 0,
    live_end                boolean NOT NULL,
    PRIMARY KEY (period, tenant_id)
);

-- 回線の月次の訂正（課金レコード §7.6）。period はこの訂正を出す月、target_period は置き換える行の月（前の月だけ。Z11）。
CREATE TABLE charging_lines_adj (
    period              date     NOT NULL REFERENCES charging_periods (period) ON DELETE CASCADE,
    target_period       date     NOT NULL,
    imsi                text     NOT NULL,
    tenant_id           bigint   NOT NULL,
    registered_days     smallint NOT NULL,
    radius_active_days  smallint NOT NULL,
    vecgen_active_days  smallint NOT NULL,
    radius_state_start  text     NOT NULL,
    radius_state_end    text     NOT NULL,
    vecgen_state_start  text     NOT NULL,
    vecgen_state_end    text     NOT NULL,
    first_registered_at timestamptz,
    deleted_at          timestamptz,
    auth_ok             bigint   NOT NULL,
    sessions            bigint   NOT NULL,
    seconds             bigint   NOT NULL,
    in_octets           bigint   NOT NULL,
    out_octets          bigint   NOT NULL,
    usage_complete      boolean  NOT NULL,
    reason              text     NOT NULL CHECK (reason IN ('late_usage')),
    PRIMARY KEY (period, imsi, tenant_id)
);

-- 月次のまとめのファイル（版ごと）。data は暗号化した ZIP（平文は DB にもディスクにも置かない）。圧縮できないので TOAST で圧縮を試みない。
CREATE TABLE charging_files (
    id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    period          date        NOT NULL REFERENCES charging_periods (period) ON DELETE CASCADE,
    version         integer     NOT NULL CHECK (version >= 1),
    file_name       text        NOT NULL,
    key_fingerprint text        NOT NULL,
    -- 平文の ZIP の SHA-256、manifest.csv の中身（ファイルごとの件数とハッシュ。秘密の値は含まない）。
    zip_sha256      text        NOT NULL,
    manifest        text        NOT NULL,
    event_count     integer     NOT NULL,
    line_count      integer     NOT NULL,
    tenant_count    integer     NOT NULL,
    adj_count       integer     NOT NULL,
    size            bigint      NOT NULL,
    created_at      timestamptz NOT NULL,
    created_by      text        NOT NULL,
    data            bytea       NOT NULL,
    UNIQUE (period, version)
);
ALTER TABLE charging_files ALTER COLUMN data SET STORAGE EXTERNAL;

-- 課金レコードを暗号化する PGP の公開鍵。今の鍵は最も新しい行で、前の鍵は履歴として残す（Z13）。
CREATE TABLE charging_pgp_keys (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    armored     text        NOT NULL,
    fingerprint text        NOT NULL,
    algorithm   text        NOT NULL,
    -- 期限（無期限は NULL）。
    expires_at  timestamptz,
    created_at  timestamptz NOT NULL,
    created_by  text        NOT NULL
);
