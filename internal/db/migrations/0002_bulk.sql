-- 一括投入と一括の物理削除のジョブ（設計概要 §10・§14。段階 3-2 の X2）。
-- 待ちのファイルは KEK で暗号化して file_enc に置き（AAD は aka-ossbss/bulk-job/<ID>）、完了・中断・取り消しのときに消す。
-- 平文のファイルと鍵（Ki / OPc）は保存しない。ID は台帳の事実と監査ログの job_id（10 進の文字列）になる。
CREATE TABLE bulk_jobs (
    id                  bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind                text        NOT NULL CHECK (kind IN ('vecgen.create', 'radius.create', 'vecgen.delete', 'radius.delete')),
    -- 待ち行列（対象ごとに 1 本）。
    target              text        GENERATED ALWAYS AS (split_part(kind, '.', 1)) STORED,
    -- validated（検証済み）→ queued（待ち）→ running（実行中）→ completed（完了）/ interrupted（中断）。
    -- validated からは canceled（取り消し）、queued からは interrupted にもなる。
    state               text        NOT NULL DEFAULT 'validated'
                                    CHECK (state IN ('validated', 'queued', 'running', 'completed', 'interrupted', 'canceled')),
    -- 一律の設定（テナント、状態、許可クライアント、SQN の方式、取得先、ルールの組）。
    settings            jsonb       NOT NULL DEFAULT '{}'::jsonb,
    file_name           text        NOT NULL CHECK (char_length(file_name) <= 255),
    file_enc            bytea,
    kek_id              text        NOT NULL DEFAULT '',
    -- ファイルの行の数と、検証のときの件数（既にある・台帳にない）。
    total               integer     NOT NULL,
    plan_exists         integer     NOT NULL DEFAULT 0,
    plan_absent         integer     NOT NULL DEFAULT 0,
    -- 検証のときに削除済みだった IMSI と、システム管理者の選択（include: 承認して含める、exclude: 除いて進める）。
    deleted_imsis       text[]      NOT NULL DEFAULT '{}',
    deleted_choice      text        NOT NULL DEFAULT '' CHECK (deleted_choice IN ('', 'include', 'exclude')),
    deleted_reason      text        NOT NULL DEFAULT '',
    -- 実行の結果の件数。
    ok_count            integer     NOT NULL DEFAULT 0,
    exists_count        integer     NOT NULL DEFAULT 0,
    absent_count        integer     NOT NULL DEFAULT 0,
    failed_count        integer     NOT NULL DEFAULT 0,
    unprocessed_count   integer     NOT NULL DEFAULT 0,
    excluded_count      integer     NOT NULL DEFAULT 0,
    -- 実行中のジョブの中断を求めた利用者（空なら求められていない）。
    interrupt_by        text        NOT NULL DEFAULT '',
    -- 中断・取り消しの理由。
    reason              text        NOT NULL DEFAULT '',
    created_by          text        NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    queued_at           timestamptz,
    started_at          timestamptz,
    finished_at         timestamptz,
    CONSTRAINT bulk_jobs_file CHECK (state IN ('validated', 'queued', 'running') OR file_enc IS NULL)
);
CREATE INDEX bulk_jobs_queue ON bulk_jobs (target, queued_at, id) WHERE state = 'queued';
CREATE INDEX bulk_jobs_open ON bulk_jobs (state) WHERE state IN ('validated', 'queued', 'running');

-- ジョブの成功以外の行（既にある・ない・失敗・未処理・除いた）。鍵は持たない。
-- unknown は、下流の結果が分からなかった行（台帳の check_needed を立てた行）。
CREATE TABLE bulk_job_rows (
    job_id  bigint  NOT NULL REFERENCES bulk_jobs (id) ON DELETE CASCADE,
    line    integer NOT NULL,
    imsi    text    NOT NULL,
    result  text    NOT NULL CHECK (result IN ('exists', 'absent', 'failed', 'unprocessed', 'excluded')),
    reason  text    NOT NULL DEFAULT '',
    unknown boolean NOT NULL DEFAULT false,
    PRIMARY KEY (job_id, line)
);
