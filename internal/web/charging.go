package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/charging"
	"github.com/oyaguma3/aka-ossbss/internal/db"
)

// 課金レコードの画面（課金レコード §8.3・§8.4。段階 3-4 の Z15）。見るのとダウンロードは運用担当とシステム管理者、
// 公開鍵の登録・作り直し・今すぐ取り込む・締めるはシステム管理者。ダウンロードは毎回監査ログに残す。

const (
	// chargingPeriodsPerPage は月の一覧の件数（13 か月と少し）。chargingKeysPerPage は鍵の履歴の件数。
	chargingPeriodsPerPage = 15
	chargingKeysPerPage    = 5
	// chargingFailuresPerPage は取り込めなかった日の件数。
	chargingFailuresPerPage = 31
	// chargingDownloadTimeout はファイルを DB から読む上限時間（ファイルは大きいと数十 MB）。
	chargingDownloadTimeout = 30 * time.Second
	// maxExternalIDLen は外部の番号の文字数の上限（DB の CHECK 制約と同じ）。
	maxExternalIDLen = 100
)

func chargingStateLabel(s string) string {
	switch s {
	case db.PeriodComputed:
		return "ファイルなし（保留）"
	case db.PeriodFiled:
		return "ファイルあり"
	}
	return s
}

// readExternalID はフォームの外部の番号を読む（前後の空白を除く。空は未設定）。正しくなければ文を返す。
func readExternalID(r *http.Request) (string, string) {
	v := strings.TrimSpace(r.PostForm.Get("external"))
	if utf8.RuneCountInString(v) > maxExternalIDLen || strings.ContainsFunc(v, unicode.IsControl) {
		return v, fmt.Sprintf("外部の番号は %d 文字までで、制御文字は使えません。", maxExternalIDLen)
	}
	return v, ""
}

// ---- ダッシュボード ----

// chargingSummary はダッシュボードの課金レコードの欄の値。
type chargingSummary struct {
	Latest  *db.ChargingPeriod
	Pending int
	Warning string
}

// loadKeyState は今の鍵の状態を読む。
func (h *Handler) loadKeyState(ctx context.Context) (charging.KeyState, []db.ChargingPeriod, error) {
	pending, err := h.db.PendingChargingPeriods(ctx)
	if err != nil {
		return charging.KeyState{}, nil, err
	}
	k, ok, err := h.db.CurrentPGPKey(ctx)
	if err != nil {
		return charging.KeyState{}, nil, err
	}
	if !ok {
		return charging.EvaluateKey(time.Now(), nil, len(pending)), pending, nil
	}
	return charging.EvaluateKey(time.Now(), &k, len(pending)), pending, nil
}

// loadChargingSummary はダッシュボードの欄の値を読む（読めなければ nil）。
func (h *Handler) loadChargingSummary(ctx context.Context) *chargingSummary {
	state, pending, err := h.loadKeyState(ctx)
	if err != nil {
		h.log.Warn("home: charging key", "error", err)
		return nil
	}
	periods, err := h.db.ChargingPeriods(ctx, 1)
	if err != nil {
		h.log.Warn("home: charging periods", "error", err)
		return nil
	}
	s := &chargingSummary{Pending: len(pending), Warning: state.Warning}
	if len(periods) > 0 {
		s.Latest = &periods[0]
	}
	return s
}

// ---- 一覧 ----

// chargingPeriodRow は月の一覧の 1 行（版の新しい順のファイルつき）。
type chargingPeriodRow struct {
	db.ChargingPeriod
	Files []db.ChargingFile
}

type chargingData struct {
	Periods []chargingPeriodRow
	Key     charging.KeyState
	Keys    []db.PGPKey
	// Failures は読み切っていない日。Schedules は課金レコードの定期の処理の最後の記録。
	Failures  []db.UsageImportFailure
	Schedules []db.ScheduledRun
	// CanManage は操作のボタンを出すか。Busy は実行が動いているか（定期の読み込みをする）。
	CanManage, Busy bool
	Message, Error  string
	// Armored は登録できなかった鍵の入力（直して送り直せるように残す。公開鍵なので秘密の値ではない）。
	Armored string
}

func (h *Handler) chargingList(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.ChargingView) {
		return
	}
	d := chargingData{}
	switch r.URL.Query().Get("started") {
	case "import":
		d.Message = "利用の集計の取り込みを始めました。"
	case "close":
		d.Message = "締めを始めました。締める月がなければ、取り込みとファイルの生成だけを行います。"
	case "rebuild":
		d.Message = "作り直しを始めました。"
	case "key":
		d.Message = "公開鍵を登録しました。ファイルのない月があれば、続けて作ります。"
	}
	h.renderCharging(w, r, http.StatusOK, d)
}

// renderCharging は画面を返す。定期の読み込み（htmx）には表の部分だけを返す。誤りを出すときは画面全体にする。
func (h *Handler) renderCharging(w http.ResponseWriter, r *http.Request, status int, d chargingData) {
	ctx, cancel := dbContext(r)
	defer cancel()
	periods, err := h.db.ChargingPeriods(ctx, chargingPeriodsPerPage)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	files, err := h.db.ChargingFiles(ctx)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	d.Periods = make([]chargingPeriodRow, len(periods))
	for i, p := range periods {
		d.Periods[i] = chargingPeriodRow{ChargingPeriod: p}
		for _, f := range files {
			if f.Period.Equal(p.Period) {
				d.Periods[i].Files = append(d.Periods[i].Files, f)
			}
		}
	}
	if d.Key, _, err = h.loadKeyState(ctx); err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	if d.Keys, err = h.db.PGPKeys(ctx, chargingKeysPerPage); err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	if d.Failures, err = h.db.UsageImportFailures(ctx, chargingFailuresPerPage); err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	schedules, err := h.db.LatestScheduledRuns(ctx)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	for _, s := range schedules {
		if slices.Contains([]string{charging.JobUsageImport, charging.JobClose, charging.JobRebuild}, s.Name) {
			d.Schedules = append(d.Schedules, s)
		}
	}
	d.CanManage, d.Busy = can(r, authz.ChargingManage), h.charging.Busy()
	if d.Error != "" {
		h.render(w, r, status, "charging", "課金レコード", d)
		return
	}
	h.renderPart(w, r, status, "charging", "charging-table", "課金レコード", d)
}

// chargingStarted は裏の実行を始めた後の共通の処理。
func (h *Handler) chargingStarted(w http.ResponseWriter, r *http.Request, err error, started string) {
	switch {
	case errors.Is(err, charging.ErrRunning):
		h.renderCharging(w, r, http.StatusConflict, chargingData{Error: "取り込み・締め・作り直しのどれかが動いています。終わってからもう一度押してください。"})
	case errors.Is(err, charging.ErrNoKey):
		h.renderCharging(w, r, http.StatusConflict, chargingData{Error: "使える PGP の公開鍵がありません。先に登録してください。"})
	case errors.Is(err, charging.ErrNotClosed):
		h.renderCharging(w, r, http.StatusBadRequest, chargingData{Error: "その月はまだ締めていません。"})
	case err != nil:
		h.dbUnavailable(w, r, err)
	default:
		seeOther(w, r, "/charging?started="+started)
	}
}

func (h *Handler) chargingImport(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.ChargingManage) {
		return
	}
	h.chargingStarted(w, r, h.charging.StartImport(context.WithoutCancel(r.Context()), userFrom(r.Context()).Email), "import")
}

func (h *Handler) chargingClose(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.ChargingManage) {
		return
	}
	h.chargingStarted(w, r, h.charging.StartClose(context.WithoutCancel(r.Context()), userFrom(r.Context()).Email), "close")
}

func (h *Handler) chargingRebuild(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.ChargingManage) {
		return
	}
	month, err := time.ParseInLocation("2006-01", r.PathValue("period"), db.JST)
	if err != nil {
		h.notFound(w, r)
		return
	}
	h.chargingStarted(w, r, h.charging.StartRebuild(context.WithoutCancel(r.Context()), month, userFrom(r.Context()).Email), "rebuild")
}

// chargingKey は PGP の公開鍵を登録する（システム管理者）。
func (h *Handler) chargingKey(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.ChargingManage) {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	armored := strings.TrimSpace(r.PostForm.Get("armored"))
	_, err := h.charging.RegisterKey(context.WithoutCancel(r.Context()), armored, userFrom(r.Context()).Email)
	if ke, ok := errors.AsType[*charging.KeyError](err); ok {
		// 秘密鍵が貼られたときは入力を画面に戻さない。
		d := chargingData{Error: ke.Message}
		if !strings.Contains(armored, "PRIVATE KEY") {
			d.Armored = armored
		}
		h.renderCharging(w, r, http.StatusBadRequest, d)
		return
	}
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	seeOther(w, r, "/charging?started=key")
}

// chargingDownload はファイルをダウンロードする。毎回監査ログに残す（課金レコード §8.4）。
func (h *Handler) chargingDownload(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.ChargingView) {
		return
	}
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		h.notFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), chargingDownloadTimeout)
	defer cancel()
	f, err := h.db.ChargingFileData(ctx, id)
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.renderErrorLink(w, r, http.StatusNotFound, "ファイルが見つかりません。", "/charging", "課金レコードの一覧へ")
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	h.record(r, audit.ActionChargingDownload, f.FileName, authz.NoTenant, map[string]any{"period": f.Period.Format("2006-01"), "version": f.Version,
		"fingerprint": f.KeyFingerprint, "size": f.Size})
	w.Header().Set("Content-Type", "application/pgp-encrypted")
	w.Header().Set("Content-Disposition", `attachment; filename="`+f.FileName+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(f.Data)
}

// subscriberExternal は回線の外部の番号を変える（運用担当・システム管理者）。
func (h *Handler) subscriberExternal(w http.ResponseWriter, r *http.Request) {
	e, ok := h.opStart(w, r, authz.ExternalIDUpdate)
	if !ok {
		return
	}
	value, msg := readExternalID(r)
	if msg != "" {
		h.renderSubscriber(w, r, e.IMSI, http.StatusBadRequest, func(d *subscriberData) { d.Error = msg })
		return
	}
	ctx, cancel := dbContext(r)
	before, err := h.db.SetLineExternalID(ctx, e.IMSI, value)
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	if before != value {
		h.record(r, audit.ActionLineExternal, e.IMSI, e.TenantID, map[string]any{"externalLineId": map[string]any{"from": before, "to": value}})
	}
	h.renderSubscriber(w, r, e.IMSI, http.StatusOK, func(d *subscriberData) { d.Message = "回線の外部の番号を保存しました。" })
}
