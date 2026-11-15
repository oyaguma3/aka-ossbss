-- エッジのコンテナのひな形を段階的に届ける（aka-mvpset の段階 6-0 の U6-7・U6-8。radsec-edge の §24.14）。
-- 配置ごとに、エッジが返すひな形の状態の写しを持つ（整合のループが毎周回書く。NULL は分からない）。
ALTER TABLE placements
    ADD COLUMN template_current boolean,
    ADD COLUMN config_current   boolean,
    ADD COLUMN can_rollback     boolean NOT NULL DEFAULT false,
    -- 記録のイメージの ID の短い形（画面に出す）。
    ADD COLUMN template_image   text    NOT NULL DEFAULT '';

-- 反映し直し。運用担当・システム管理者が選んだテナントを、エッジごとに 1 件ずつ今の形・前の形にする。1 件でも失敗したら止まる。
-- 同じエッジで動いている（running）・止まっている（stopped）ものは 1 つまで。
CREATE TABLE edge_rollouts (
    id          bigserial   PRIMARY KEY,
    edge_id     text        NOT NULL REFERENCES edges (id) ON DELETE CASCADE,
    mode        text        NOT NULL CHECK (mode IN ('latest', 'previous')),
    state       text        NOT NULL CHECK (state IN ('running', 'stopped', 'done', 'canceled')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  text        NOT NULL,
    finished_at timestamptz,
    stop_reason text        NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX edge_rollouts_active ON edge_rollouts (edge_id) WHERE state IN ('running', 'stopped');
CREATE INDEX edge_rollouts_edge ON edge_rollouts (edge_id, id DESC);

-- 反映し直しの 1 件（選んだ順）。started_at はエッジが返した反映の始まりの時刻（エッジの時計。結果を読むときに比べる）。
CREATE TABLE edge_rollout_items (
    rollout_id   bigint      NOT NULL REFERENCES edge_rollouts (id) ON DELETE CASCADE,
    seq          integer     NOT NULL,
    -- 外した配置は 13 か月で消す（PurgeOld）。そのときは項目も消える。
    placement_id bigint      NOT NULL REFERENCES placements (id) ON DELETE CASCADE,
    tenant_id    bigint      NOT NULL,
    state        text        NOT NULL CHECK (state IN ('waiting', 'applying', 'ok', 'failed', 'skipped')),
    started_at   timestamptz,
    finished_at  timestamptz,
    error        text        NOT NULL DEFAULT '',
    PRIMARY KEY (rollout_id, seq)
);
