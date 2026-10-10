package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/certs"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/config"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/provisioner"
)

// notProvisioner は、provisioner でない相手（本PoCの provisioning-api など）につながったときの文。
const notProvisioner = "接続先は provisioner ではないようです（本PoCの provisioning-api など）。OSSBSS_PROVISIONER_URL を確認してください。"

// newProvisioner は provisioner のクライアントを作る（設計概要 §10）。
func newProvisioner(cfg config.Config, log *slog.Logger) (*provisioner.Client, error) {
	return provisioner.New(provisioner.Options{
		BaseURL:        cfg.Provisioner.URL,
		ClientCertFile: cfg.Provisioner.ClientCertFile,
		ClientKeyFile:  cfg.Provisioner.ClientKeyFile,
		ServerCertFile: cfg.Provisioner.ServerCertFile,
		Timeout:        cfg.Provisioner.Timeout,
		Log:            log,
	})
}

// checkAtStartup は、起動時に provisioner を確かめてログに出す。つながらなくても起動は続ける（画面で示す）。
// provisioner の /status は下流 2 つを確かめるので、下流が止まっていると時間がかかることがある。
func checkAtStartup(ctx context.Context, log *slog.Logger, pv *provisioner.Client) {
	st, err := pv.Status(ctx)
	if err != nil {
		log.Warn("provisioner is not available", "error", err, "hint", provisioner.Diagnose(err))
		return
	}
	if !st.IsProvisioner() {
		log.Warn("unexpected server", "url", pv.BaseURL(), "hint", notProvisioner)
		return
	}
	log.Info("provisioner is available", "server_version", st.Version,
		"prov_reachable", st.Downstreams.Prov.Reachable, "aka_configured", st.Downstreams.Aka.Configured,
		"aka_reachable", st.Downstreams.Aka.Reachable)
	for name, d := range map[string]provisioner.DownstreamStatus{"prov": st.Downstreams.Prov, "aka": st.Downstreams.Aka} {
		if d.Configured && !d.Reachable {
			log.Warn("provisioner cannot reach its downstream", "downstream", name, "error", d.Error, "hint", d.Hint)
		}
	}
}

// checkProvisioner は provisioner への接続を確かめ、結果を表示する（導入時の確認に使う）。
// provisioner に接続できない、または provisioner でない相手だった場合はエラーを返す。
// provisioner の先の下流に接続できない場合は、表示はするがエラーにはしない（provisioner の check-downstream で確かめる）。
func checkProvisioner(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	pv, err := newProvisioner(cfg, log)
	if err != nil {
		return err
	}
	fmt.Printf("接続先: %s\n", pv.BaseURL())
	fmt.Println("クライアント証明書のフィンガープリント:", certs.Fingerprint(pv.ClientCertificate()))
	return printProvisionerStatus(ctx, pv)
}

// printProvisionerStatus は provisioner の状態（下流 2 つへの接続、AVクライアント、PLMN マップ）を表示する。
func printProvisionerStatus(ctx context.Context, pv *provisioner.Client) error {
	st, err := pv.Status(ctx)
	if err != nil {
		if hint := provisioner.Diagnose(err); hint != "" {
			fmt.Println(hint)
		}
		return err
	}
	if !st.IsProvisioner() {
		fmt.Println(notProvisioner)
		return errors.New("the server is not a provisioner")
	}
	fmt.Printf("接続できました。provisioner %s\n", st.Version)
	printDownstream("本PoCの Provisioning API", st.Downstreams.Prov)
	if !st.Downstreams.Aka.Configured {
		fmt.Println("  aka-only-server: 設定なし（すべての加入者の鍵を本PoCに置く）")
	} else {
		printDownstream("aka-only-server", st.Downstreams.Aka)
		// aka-only-server に接続できなければ、AVクライアントは確かめられていない（exists が false になる）ので出さない。
		switch av := st.AVClient; {
		case !st.Downstreams.Aka.Reachable:
		case !av.Exists:
			fmt.Printf("  vector-gateway の AVクライアント（ID %d）が aka-only-server にありません。\n", av.ID)
		case !av.Enabled:
			fmt.Printf("  vector-gateway の AVクライアント: ID %d（%s）は無効になっています。\n", av.ID, av.Name)
		default:
			fmt.Printf("  vector-gateway の AVクライアント: ID %d（%s）\n", av.ID, av.Name)
		}
	}
	var entries []string
	for _, e := range st.PLMNMap {
		entries = append(entries, e.PLMN+"="+string(e.KeyStore))
	}
	fmt.Println("  PLMN マップ:", cmp.Or(strings.Join(entries, ","), "（なし。すべての加入者の鍵を本PoCに置く）"))
	if ops := st.Operations; ops != nil && ops.Running+ops.Retrying+ops.Failed > 0 {
		fmt.Printf("  未完了の操作: 実行中 %d、やり直し中 %d、失敗 %d\n", ops.Running, ops.Retrying, ops.Failed)
	}
	return nil
}

func printDownstream(label string, d provisioner.DownstreamStatus) {
	if !d.Reachable {
		fmt.Printf("  %s: provisioner から接続できません（%s）\n", label, d.Error)
		if d.Hint != "" {
			fmt.Println("    " + d.Hint)
		}
		return
	}
	if d.NodeName != "" {
		fmt.Printf("  %s: %s（ノード %s、加入者 %d）\n", label, d.Version, d.NodeName, d.SubscriberCount)
	} else {
		fmt.Printf("  %s: %s（加入者 %d）\n", label, d.Version, d.SubscriberCount)
	}
}
