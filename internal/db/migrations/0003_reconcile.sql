-- 定期の処理の記録と、台帳と下流の突き合わせ（設計概要 §9・§16。段階 3-3 の Y2・Y7）。

-- 定期の処理の実行の記録。短い間隔の処理は、何かしたときと失敗のときだけ書く。
CREATE TABLE scheduled_runs (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name        text        NOT NULL,
    started_at  timestamptz NOT NULL,
    finished_at timestamptz,
    status      text        NOT NULL CHECK (status IN ('running', 'ok', 'failed', 'interrupted')),
    detail      text        NOT NULL DEFAULT ''
);
CREATE INDEX scheduled_runs_name ON scheduled_runs (name, started_at DESC);
CREATE INDEX scheduled_runs_started ON scheduled_runs (started_at);

-- 突き合わせの実行。held は、直す数が上限を超えたので何も直していないこと（システム管理者の承認を待つ）。
CREATE TABLE reconcile_runs (
    id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    trigger         text        NOT NULL CHECK (trigger IN ('schedule', 'manual', 'approved')),
    requested_by    text        NOT NULL,
    approve_reason  text        NOT NULL DEFAULT '',
    state           text        NOT NULL DEFAULT 'running'
                                CHECK (state IN ('running', 'completed', 'held', 'failed', 'interrupted')),
    -- 読んだ行の数。
    ledger_rows     integer     NOT NULL DEFAULT 0,
    vecgen_rows     integer     NOT NULL DEFAULT 0,
    radius_rows     integer     NOT NULL DEFAULT 0,
    -- 自動で直す対象の数、直した数、見つけたずれの総数（行として残すのは 10,000 件まで）。
    fixable_count   integer     NOT NULL DEFAULT 0,
    fixed_count     integer     NOT NULL DEFAULT 0,
    findings_total  integer     NOT NULL DEFAULT 0,
    error           text        NOT NULL DEFAULT '',
    started_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz
);
CREATE INDEX reconcile_runs_started ON reconcile_runs (started_at);

-- 見つけたずれ。kind: ledger_only（台帳にあって下流にない）、state_diff（状態が違う）、source_diff（取得先が違う）、
-- side_absent（行は台帳にあり、その側だけ「なし」）、downstream_only（IMSI が台帳にない）、deleted_present（台帳で削除済み）、
-- rule_tenant（ルールのテナントが所属と違う）。
-- status: fixed（直した）、held（保留で直していない）、pending（システム管理者の対応を待つ）、info（報告だけ）、
-- imported（取り込んだ）、deleted（下流から削除した）、gone（対応のときには解消していた）。
CREATE TABLE reconcile_findings (
    id             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id         bigint      NOT NULL REFERENCES reconcile_runs (id) ON DELETE CASCADE,
    imsi           text        NOT NULL,
    kind           text        NOT NULL CHECK (kind IN ('ledger_only', 'state_diff', 'source_diff', 'side_absent',
                                                        'downstream_only', 'deleted_present', 'rule_tenant')),
    side           text        NOT NULL CHECK (side IN ('vecgen', 'radius')),
    ledger_val     text        NOT NULL DEFAULT '',
    downstream_val text        NOT NULL DEFAULT '',
    -- 所属のテナント（台帳にないものは、取り込みの初期値の候補。分からなければ 0）。
    tenant_id      bigint      NOT NULL DEFAULT 0,
    status         text        NOT NULL CHECK (status IN ('fixed', 'held', 'pending', 'info', 'imported', 'deleted', 'gone')),
    resolved_by    text        NOT NULL DEFAULT '',
    resolved_at    timestamptz
);
CREATE INDEX reconcile_findings_run ON reconcile_findings (run_id, id);
CREATE INDEX reconcile_findings_imsi ON reconcile_findings (run_id, imsi);
