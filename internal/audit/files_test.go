package audit

import (
	"bufio"
	"compress/gzip"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppend(t *testing.T) {
	dir := t.TempDir()
	f := NewFiles(dir, 14)
	// 日本時間の日付でファイルを選ぶ（UTC の 10 月 27 日 16 時は日本時間の 28 日 1 時）。
	at := time.Date(2026, 10, 27, 16, 0, 0, 0, time.UTC)
	if err := f.Append(Entry{ID: 7, Time: at, ActorEmail: "d@example.com", ActorRole: "system_admin", TenantID: 10,
		Action: "subscriber.vecgen.create", Target: "001010000000001", Detail: map[string]any{"state": "active"},
		TraceID: "t-1", Remote: "192.0.2.1"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Append(Entry{Time: at, ActorEmail: "c@example.com", Action: "tenant.create", Target: "x"}); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, filepath.Join(dir, "audit-20261028.jsonl"))
	if len(lines) != 2 {
		t.Fatalf("lines = %v", lines)
	}
	l := lines[0]
	if l["time"] != "2026-10-28T01:00:00+09:00" || l["id"] != float64(7) || l["operator"] != "d@example.com" ||
		l["role"] != "system_admin" || l["tenantId"] != float64(10) || l["action"] != "subscriber.vecgen.create" ||
		l["target"] != "001010000000001" || l["traceId"] != "t-1" || l["remote"] != "192.0.2.1" {
		t.Errorf("line = %v", l)
	}
	if d, _ := l["detail"].(map[string]any); d["state"] != "active" {
		t.Errorf("detail = %v", l["detail"])
	}
	// 空の項目は書かない。
	for _, k := range []string{"id", "tenantId", "detail", "traceId", "remote", "jobId"} {
		if _, ok := lines[1][k]; ok {
			t.Errorf("empty %s is written: %v", k, lines[1])
		}
	}
}

func TestCheckWritableDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audit")
	if err := CheckWritableDir(dir); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("left files: %v", ents)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckWritableDir(file); err == nil {
		t.Error("CheckWritableDir on a file: want error")
	}
}
func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		r = zr
	}
	var out []map[string]any
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

func TestMaintain(t *testing.T) {
	dir := t.TempDir()
	f := NewFiles(dir, 14)
	now := time.Date(2026, 10, 28, 3, 0, 0, 0, jst)
	write := func(name, content string, mtime time.Time) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	write("audit-20261028.jsonl", `{"a":1}`+"\n", now)                       // 今日: そのまま
	write("audit-20261027.jsonl", `{"a":2}`+"\n", now.Add(-time.Hour))       // 昨日: 圧縮する
	write("audit-20261026.jsonl", `{"a":3}`+"\n", now.Add(-5*time.Minute))   // 書いたばかり: まだ圧縮しない
	write("audit-20261014.jsonl.gz", "old", now.Add(-14*24*time.Hour))       // 14 日前: 残す
	write("audit-20261013.jsonl.gz", "older", now.Add(-15*24*time.Hour))     // 15 日前: 消す
	write("audit-20261001.jsonl", `{"a":4}`+"\n", now.Add(-27*24*time.Hour)) // 古い圧縮前: 消す
	write("other.txt", "x", now.Add(-30*24*time.Hour))                       // 関係のないファイル: 触らない

	if err := f.Maintain(now); err != nil {
		t.Fatal(err)
	}
	var names []string
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		names = append(names, e.Name())
	}
	want := []string{"audit-20261014.jsonl.gz", "audit-20261026.jsonl", "audit-20261027.jsonl.gz", "audit-20261028.jsonl", "other.txt"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("files = %v, want %v", names, want)
	}
	if lines := readLines(t, filepath.Join(dir, "audit-20261027.jsonl.gz")); len(lines) != 1 || lines[0]["a"] != float64(2) {
		t.Errorf("compressed content = %v", lines)
	}
	if err := NewFiles(filepath.Join(dir, "none"), 14).Maintain(now); err == nil {
		t.Error("Maintain on a missing directory: want error")
	}
}
