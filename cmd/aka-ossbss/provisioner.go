package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/oyaguma3/aka-ossbss/internal/certs"
	"github.com/oyaguma3/aka-ossbss/internal/config"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// notProvisioner は、aka-provisioner 2.0.0 でない相手につながったときの文。
const notProvisioner = "接続先は aka-provisioner 2.0.0 ではないようです。OSSBSS_PROVISIONER_URL（https://…/admin/v2）を確認してください。"

// newProvisioner は aka-provisioner のクライアントを作る（設計概要 §1）。
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

// checkAtStartup は、起動時に aka-provisioner を確かめてログに出す。つながらなくても起動は続ける（画面で示す）。
func checkAtStartup(ctx context.Context, log *slog.Logger, pv *provisioner.Client) {
	st, err := pv.Status(ctx)
	if err != nil {
		log.Warn("provisioner is not available", "event_id", "PROVISIONER_UNAVAILABLE", "error", err, "hint", provisioner.Diagnose(err))
		return
	}
	if !st.IsProvisioner() {
		log.Warn("unexpected server", "event_id", "PROVISIONER_UNAVAILABLE", "url", pv.BaseURL(), "hint", notProvisioner)
		return
	}
	log.Info("provisioner is available", "server_version", st.Version, "valkey_reachable", st.Valkey.Reachable)
	for _, d := range st.Downstreams {
		if !d.Reachable {
			log.Warn("provisioner cannot reach its downstream", "downstream", string(d.Name), "error", d.Error, "hint", d.Hint)
		}
	}
}

// checkProvisioner は aka-provisioner への接続を確かめ、結果を表示する（導入時の確認に使う）。
// aka-provisioner に接続できない、または aka-provisioner 2.0.0 でない相手だった場合はエラーを返す。
// aka-provisioner の先の下流に接続できない場合は、表示はするがエラーにはしない（aka-provisioner の check-downstream で確かめる）。
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

// printProvisionerStatus は aka-provisioner の状態（下流 2 つへの接続）を表示する。
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
		return errors.New("the server is not aka-provisioner 2.0.0")
	}
	fmt.Printf("接続できました。aka-provisioner %s\n", st.Version)
	if !st.Valkey.Reachable {
		fmt.Printf("  aka-provisioner の Valkey に接続できません（%s）\n", st.Valkey.Error)
	}
	for _, d := range st.Downstreams {
		label := cmp.Or(map[provisioner.Downstream]string{provisioner.Vecgen: "aka-vecgen", provisioner.Radius: "aka-radius"}[d.Name], string(d.Name))
		if !d.Reachable {
			fmt.Printf("  %s: aka-provisioner から接続できません（%s）\n", label, cmp.Or(d.Error, "まだ確かめていません"))
			if d.Hint != "" {
				fmt.Println("    " + d.Hint)
			}
			continue
		}
		if n, ok := d.Node(); ok {
			fmt.Printf("  %s: %s（加入者 %d）\n", label, n.Version, n.SubscriberCount)
		} else {
			fmt.Printf("  %s: 接続できます\n", label)
		}
	}
	return nil
}
