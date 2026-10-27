package web

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

func TestSessions(t *testing.T) {
	e := newTestEnv()
	start := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	for i, imsi := range []string{imsiOwn, imsiOther, imsiNone, imsiOwn} {
		e.pv.sessions = append(e.pv.sessions, provisioner.Session{ID: fmt.Sprintf("sess-%d", i), IMSI: imsi, NasIP: "192.0.2.1",
			NasIdentifier: "ap-01", StartTime: start, InputOctets: 1536, OutputOctets: 10})
	}
	// テナントユーザー: provisioner の新しい 1000 件を取り、自分のテナントの加入者のものだけ。
	w := get(e.handler(t, "a@example.com"), "/sessions")
	if w.Code != 200 {
		t.Fatalf("sessions = %d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	mustContain(t, body, "sess-0", "sess-3", "2 件", "1.5 KiB", `title="1536 octets"`)
	mustNotContain(t, body, "sess-1", "sess-2", "<th>テナント</th>")
	if e.pv.lastSessions.Limit != sessionsScanned {
		t.Errorf("limit = %d", e.pv.lastSessions.Limit)
	}
	// 運用担当: すべて（新しい 100 件。テナントを添える）、テナントの絞り込み。
	h := e.handler(t, "c@example.com")
	body = get(h, "/sessions").Body.String()
	mustContain(t, body, "sess-0", "sess-1", "sess-2", "テナントB", "テナントなし")
	if e.pv.lastSessions.Limit != sessionsShown {
		t.Errorf("limit = %d", e.pv.lastSessions.Limit)
	}
	body = get(h, "/sessions?tenant=none").Body.String()
	mustContain(t, body, "sess-2")
	mustNotContain(t, body, "sess-0", "sess-1")
	// IMSI で絞る（範囲の中なら provisioner の絞り込みを使う）。
	body = get(e.handler(t, "b@example.com"), "/sessions?imsi="+imsiOwn).Body.String()
	mustContain(t, body, "IMSI <span class=\"font-mono\">"+imsiOwn+"</span> のアクティブセッション 2 件", "すべてのセッションを表示")
	if e.pv.lastSessions.IMSI != imsiOwn {
		t.Errorf("imsi = %q", e.pv.lastSessions.IMSI)
	}
	if w := get(h, "/sessions?imsi=123"); w.Code != 400 || !strings.Contains(w.Body.String(), "IMSI は 15 桁の数字で入力してください。") {
		t.Errorf("bad imsi = %d", w.Code)
	}
	// provisioner のセッションが 1000 件を超えていれば、その旨を出す。
	e.pv.sessions = nil
	for i := range 1001 {
		e.pv.sessions = append(e.pv.sessions, provisioner.Session{ID: fmt.Sprintf("s%d", i), IMSI: imsiOwn})
	}
	body = get(e.handler(t, "a@example.com"), "/sessions").Body.String()
	mustContain(t, body, "provisioner のセッションの新しい 1000 件の中から絞り込んでいます。", "接続開始の新しい 100 件だけを表示しています。", "1000 件")
}
