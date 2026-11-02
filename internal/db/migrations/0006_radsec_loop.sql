-- 中央の整合のループ（設計概要 §13。段階 4-4 の OB2・OB4）。エッジに置いたサーバー証明書とヘルスチェック用の証明書の控えと、ループの途中の状態を配置に持つ。
-- 証明書の本体と鍵はエッジと radsec-ca にある。
ALTER TABLE placements
    ADD COLUMN server_serial      text        NOT NULL DEFAULT '',
    ADD COLUMN server_not_after   timestamptz,
    ADD COLUMN probe_serial       text        NOT NULL DEFAULT '',
    ADD COLUMN probe_not_after    timestamptz,
    -- 発行の世代（Idempotency-Key に入れる。置けたときと 4xx で進める）。
    ADD COLUMN cert_generation    bigint      NOT NULL DEFAULT 0,
    -- 期限が近いので証明書を出し直す（毎日の処理が立て、ループが出し直して消す）。
    ADD COLUMN renew_requested    boolean     NOT NULL DEFAULT false,
    -- 証明書を置いたので反映が要る（反映の後に消す）。
    ADD COLUMN cert_apply_needed  boolean     NOT NULL DEFAULT false,
    -- 新しい証明書の反映の後に superseded で失効させる古い証明書。
    ADD COLUMN superseded_serials text[]      NOT NULL DEFAULT '{}',
    -- 反映に失敗した版（同じ版を自動でやり直さない。OB2）と、人が押した「やり直す」。
    ADD COLUMN failed_revision    bigint      NOT NULL DEFAULT 0,
    ADD COLUMN retry_requested    boolean     NOT NULL DEFAULT false;
CREATE INDEX placements_server_expiring ON placements (server_not_after) WHERE deleted_at IS NULL;
CREATE INDEX placements_probe_expiring ON placements (probe_not_after) WHERE deleted_at IS NULL;
