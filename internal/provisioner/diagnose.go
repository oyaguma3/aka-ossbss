package provisioner

import (
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"syscall"
)

// Diagnose は、provisioner に接続できなかったときの原因の見当を、利用者向けの文で返す。
// 見当がつかなければ空文字列を返す（既存の BFF の pvapi.Diagnose と同じ判定で、文面を本BFF の設定の名前にしたもの）。
// provisioner の先の下流（provisioning-api、aka-vecgen）に接続できない場合は、provisioner が応答
// （503 DOWNSTREAM_UNAVAILABLE、/status の downstreams の hint）で示すので、ここでは扱わない。
func Diagnose(err error) string {
	if err == nil {
		return ""
	}
	if _, ok := errors.AsType[x509.HostnameError](err); ok {
		return "provisioner のサーバー証明書に、接続先のホスト名（または IP アドレス）が入っていません。" +
			"provisioner 側の .env の PROVISIONER_TLS_HOSTS に接続先（同一ホストなら aka-provisioner）を入れて" +
			"サーバー証明書を作り直し（provisioner の運用ガイド 3.4）、本BFF にも渡し直してください。"
	}
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
		return "provisioner のサーバー証明書が、設定した証明書（OSSBSS_PROVISIONER_SERVER_CERT）と一致しません。" +
			"provisioner の aka-provisioner server-cert で取り出した証明書を設定してください。"
	}
	if _, ok := errors.AsType[x509.CertificateInvalidError](err); ok {
		return "provisioner のサーバー証明書が有効期間外です。provisioner 側で作り直し、本BFF にも渡し直してください。"
	}
	// provisioner は未登録・有効期間外・証明書なしの接続を TLS のアラートで拒否する。
	if opErr, ok := errors.AsType[*net.OpError](err); ok && opErr.Op == "remote error" &&
		(strings.Contains(opErr.Err.Error(), "bad certificate") || strings.Contains(opErr.Err.Error(), "certificate required")) {
		return "provisioner が本BFF のクライアント証明書を受け付けませんでした。" +
			"provisioner 側の .env の PROVISIONER_ADMIN_CLIENTS に、このクライアント証明書のフィンガープリントが登録されているか" +
			"（登録後に provisioner を作り直したか）、証明書が有効期間内かを確認してください。"
	}
	// TLS 1.3 では、provisioner がクライアント証明書を検証して拒否する前に本BFF はリクエストを送っており、
	// 拒否のアラートより先に接続のリセットが届くことがある。
	if errors.Is(err, syscall.ECONNRESET) {
		return "provisioner が接続を切りました。本BFF のクライアント証明書が受け付けられなかった可能性があります。" +
			"provisioner 側の .env の PROVISIONER_ADMIN_CLIENTS に、このクライアント証明書のフィンガープリントが登録されているか" +
			"（登録後に provisioner を作り直したか）、証明書が有効期間内かを確認してください" +
			"（provisioner のログに admin client certificate rejected が出ていれば、これが原因です）。"
	}
	if dnsErr, ok := errors.AsType[*net.DNSError](err); ok && dnsErr.IsNotFound {
		return "provisioner のホスト名（" + dnsErr.Name + "）を解決できません。provisioner が起動しているか、" +
			"同一ホストの場合は本BFF が共有ネットワーク（aka-provisioner）に参加しているか（compose.aka-provisioner.yaml）確認してください。"
	}
	if opErr, ok := errors.AsType[*net.OpError](err); ok && opErr.Op == "dial" {
		return "provisioner に接続できません。provisioner が起動しているか、接続先の URL とポートが正しいか確認してください。"
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return "provisioner から時間内に応答がありません。接続先のアドレス、VPN、provisioner 側の公開先（PROVISIONER_PUBLISH）を確認してください。"
	}
	return ""
}
