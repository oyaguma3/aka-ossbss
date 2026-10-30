package db

// OpenTestDB は外部の試験パッケージ（db_test）から結合試験の DB を開く口（internal/charging の実行を DB につないで確かめる。
// charging は db を使うので、package db の試験からは使えない）。
var OpenTestDB = openTestDB
