#!/bin/sh
# backup-db.sh で取ったバックアップから PostgreSQL を戻す（運用ガイド 11 章）。今のデータは消える。
# compose.yaml のあるディレクトリで実行する。終わったら docker compose up -d で起動し、ガイドの「戻した後」の手順を行う。
#
#   scripts/restore-db.sh backups/ossbss-20261031-120000.dump
#
# 写しの後に増えた表（新しい版の移行や、利用の集計の月ごとのパーティション）が残らないよう、
# pg_restore --clean ではなく、DB を作り直してから読み込む。
set -eu

in=${1:?usage: scripts/restore-db.sh <バックアップのファイル>}
if [ ! -f "$in" ]; then
	echo "restore-db: $in がない" >&2
	exit 1
fi

# 動いている postgres がなければ何もしない（compose のプロジェクトを取り違えて、別のデータを消さないため）。
# 別のプロジェクト名や重ね合わせで起動しているときは、COMPOSE_PROJECT_NAME と COMPOSE_FILE を付けて動かす
if [ -z "$(docker compose ps -q postgres 2> /dev/null)" ]; then
	echo "restore-db: この compose のプロジェクトで動いている postgres が見つからない（COMPOSE_PROJECT_NAME・COMPOSE_FILE を確かめる）" >&2
	exit 1
fi

docker compose stop aka-ossbss cloudflared

docker compose exec -T postgres psql -U ossbss -d postgres -v ON_ERROR_STOP=1 -c 'DROP DATABASE IF EXISTS ossbss WITH (FORCE)' -c 'CREATE DATABASE ossbss'
docker compose exec -T postgres pg_restore -U ossbss -d ossbss --exit-on-error < "$in"
echo "restore-db: 戻した。docker compose up -d で起動する（運用ガイド 11 章の「戻した後」）"
