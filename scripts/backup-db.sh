#!/bin/sh
# PostgreSQL のバックアップを、止めずに取って手元に写す（pg_dump -Fc。運用ガイド 11 章）。
# compose.yaml のあるディレクトリで実行する。写しには台帳・課金レコード・一括投入の待ちのファイル（KEK で暗号化）が入るので、
# 運用者だけが読める場所に置く。KEK（secrets/kek）と同じ場所には置かない。
#
#   scripts/backup-db.sh backups/ossbss-$(date +%Y%m%d-%H%M%S).dump
set -eu

out=${1:?usage: scripts/backup-db.sh <出力のファイル>}

# 動いている postgres がなければ何もしない（compose のプロジェクトを取り違えて、別のデータを取らないため）。
# 別のプロジェクト名や重ね合わせで起動しているときは、COMPOSE_PROJECT_NAME と COMPOSE_FILE を付けて動かす
if [ -z "$(docker compose ps -q postgres 2> /dev/null)" ]; then
	echo "backup-db: この compose のプロジェクトで動いている postgres が見つからない（COMPOSE_PROJECT_NAME・COMPOSE_FILE を確かめる）" >&2
	exit 1
fi

# 本人だけが読めるようにする。途中で失敗したときに半端なファイルを残さない
umask 077
tmp=$out.tmp
trap 'rm -f "$tmp"' EXIT
docker compose exec -T postgres pg_dump -U ossbss -d ossbss -Fc > "$tmp"
mv "$tmp" "$out"
echo "backup-db: $out"
