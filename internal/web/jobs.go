package web

import (
	"context"
	"encoding/csv"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/bulk"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// 一括投入と一括の物理削除の画面（設計概要 §10・§15。段階 3-2 の X5〜X7・X10）。見るのは運用担当とシステム管理者、登録・投入・中断は
// システム管理者。ファイルを受けたら全行を確かめ（誤りが 1 行でもあれば何も残さない）、暗号化して検証済みのジョブにする。
// システム管理者が件数を見て確定すると待ち行列に入る。平文のファイルはメモリーの中だけで扱い、ディスクに書かない。

const (
	// maxUploadBytes はアップロードの要求の大きさの上限（ファイルと一律の設定）。
	maxUploadBytes = bulk.MaxFileBytes + maxFormBytes
	// jobsPerPage はジョブの一覧の件数。
	jobsPerPage = 50
	// jobRowsOnPage は詳細に出す成功以外の行の数（全部は CSV で取り出す）。
	jobRowsOnPage = 200
	// jobErrorsOnPage は誤りの行の表示の数。
	jobErrorsOnPage = 100
	// jobDeletedOnPage は確認の画面に出す削除済みの IMSI の数。
	jobDeletedOnPage = 100
	// jobRecheckLimit は、ファイルの検証のときに読み直す印の立った IMSI の数の上限（画面の 1 回の要求の中で行う）。
	jobRecheckLimit = 50
	// jobCSVBatch は成功以外の行の CSV を読む単位。
	jobCSVBatch = 1000
)

var jobKindLabels = map[bulk.Kind]string{
	bulk.VecgenCreate: "vecgen 側への登録",
	bulk.RadiusCreate: "radius 側への登録",
	bulk.VecgenDelete: "vecgen 側からの物理削除",
	bulk.RadiusDelete: "radius 側からの物理削除",
}

// jobKindLabel はジョブの種類の名前を返す。
func jobKindLabel(kind string) string {
	if l, ok := jobKindLabels[bulk.Kind(kind)]; ok {
		return l
	}
	return kind
}

var jobStateLabels = map[string]string{
	db.JobValidated:   "確定を待っています",
	db.JobQueued:      "待ち",
	db.JobRunning:     "実行中",
	db.JobCompleted:   "完了",
	db.JobInterrupted: "中断",
	db.JobCanceled:    "取り消し",
}

// jobStateLabel はジョブの状態の名前を返す。
func jobStateLabel(state string) string {
	if l, ok := jobStateLabels[state]; ok {
		return l
	}
	return state
}

var jobRowLabels = map[string]string{
	db.RowExists:      "既にある",
	db.RowAbsent:      "ない",
	db.RowFailed:      "失敗",
	db.RowUnprocessed: "未処理",
	db.RowExcluded:    "除いた",
}

// jobRowLabel は行の結果の名前を返す。
func jobRowLabel(result string) string {
	if l, ok := jobRowLabels[result]; ok {
		return l
	}
	return result
}

// jobPercent はジョブの進み具合の率（0〜100）を返す。
func jobPercent(j db.Job) int {
	if j.Total == 0 {
		return 0
	}
	return (j.Done() + j.Unprocessed) * 100 / j.Total
}

// ---- 一覧 ----

type jobListData struct {
	Jobs []db.Job
	// CanManage は登録のボタンを出すか。Active は定期の読み込みをするか。
	CanManage, Active bool
}

func (h *Handler) jobList(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.JobView) {
		return
	}
	ctx, cancel := dbContext(r)
	jobs, err := h.db.ListJobs(ctx, jobsPerPage)
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	d := jobListData{Jobs: jobs, CanManage: can(r, authz.JobManage), Active: slices.ContainsFunc(jobs, db.Job.Active)}
	h.renderPart(w, r, http.StatusOK, "jobs", "job-table", "一括投入", d)
}

// jobSummary はダッシュボードのジョブの欄を返す（定期の読み込み）。
func (h *Handler) jobSummary(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.JobView) {
		return
	}
	ctx, cancel := dbContext(r)
	jobs, err := h.db.ActiveJobs(ctx)
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	h.renderBlock(w, r, http.StatusOK, "home", "jobs-summary", jobs)
}

// ---- 登録 ----

// jobForm はジョブの登録のフォーム。
type jobForm struct {
	Kind  bulk.Kind
	Kinds []bulk.Kind
	// 登録のジョブの一律の設定（X5）。
	Tenant  string
	Tenants tenantInfo
	State   string
	// vecgen 登録。
	SQNType    string
	Clients    []int64
	AllClients []provisioner.VecgenClient
	// radius 登録。
	Source  string
	Sources []provisioner.Source
	Rules   []ruleForm
	// Errors は項目ごとの誤り、Error は全体の誤り、LoadError は選択肢を読めなかったときの文。
	Errors           fieldErrors
	Error, LoadError string
	// FileErrors はファイルの誤り（jobErrorsOnPage 件まで）、FileErrorTotal はその総数。
	FileErrors     []bulk.RowError
	FileErrorTotal int
}

// Create は登録のジョブか。
func (f jobForm) Create() bool { return f.Kind.Create() }

// Header は CSV のヘッダーの文字列。
func (f jobForm) Header() string { return strings.Join(f.Kind.Header(), ",") }

// KindLabel は種類の名前。
func (f jobForm) KindLabel(k bulk.Kind) string { return jobKindLabels[k] }

// MoreFileErrors は画面に出していない誤りの数。
func (f jobForm) MoreFileErrors() int { return f.FileErrorTotal - len(f.FileErrors) }

func (h *Handler) jobNew(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.JobManage) {
		return
	}
	f := jobForm{Kind: bulk.Kind(r.URL.Query().Get("kind")), State: db.StateActive, SQNType: "inc32", Errors: fieldErrors{},
		Rules: []ruleForm{{NASID: "*", SSIDs: "*", Errors: fieldErrors{}}}}
	if !f.Kind.Valid() {
		f.Kind = bulk.VecgenCreate
	}
	h.renderJobForm(w, r, http.StatusOK, f)
}

// loadJobChoices はフォームの選択肢を読む（テナント、許可クライアント、取得先）。テナントを読めなければ false。
func (h *Handler) loadJobChoices(w http.ResponseWriter, r *http.Request, f *jobForm) bool {
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return false
	}
	f.Tenants = t
	ctx, cancel := context.WithTimeout(r.Context(), fetchTimeout)
	defer cancel()
	switch f.Kind {
	case bulk.VecgenCreate:
		f.AllClients, err = h.pv.VecgenClients(ctx)
	case bulk.RadiusCreate:
		f.Sources, err = h.pv.RadiusSources(ctx)
	}
	if err != nil {
		h.log.Warn("job form: load choices", "kind", f.Kind, "error", err)
		f.LoadError = "選択肢を aka-provisioner から読めませんでした: " + h.apiError(r, err, f.Kind.Side()).Message
	}
	return true
}

func (h *Handler) renderJobForm(w http.ResponseWriter, r *http.Request, status int, f jobForm) {
	if f.Tenants.names == nil && !h.loadJobChoices(w, r, &f) {
		return
	}
	f.Kinds = bulk.Kinds
	if f.Errors == nil {
		f.Errors = fieldErrors{}
	}
	numberRules(f.Rules)
	h.renderPart(w, r, status, "job_new", "job-form", "一括投入の登録", f)
}

// readRuleForms はフォームのルールを読む（同じ名前の項目の並び）。読めなければ false。
func readRuleForms(r *http.Request) ([]ruleForm, bool) {
	pf := r.PostForm
	nas, ssids, vlans, timeouts := pf["nas_id"], pf["ssids"], pf["vlan_id"], pf["session_timeout"]
	if len(ssids) != len(nas) || len(vlans) != len(nas) || len(timeouts) != len(nas) || len(nas) > maxRules+1 {
		return nil, false
	}
	rules := make([]ruleForm, 0, len(nas))
	for i := range nas {
		rules = append(rules, ruleForm{NASID: nas[i], SSIDs: ssids[i], VLANID: vlans[i], SessionTimeout: timeouts[i], Errors: fieldErrors{}})
	}
	return rules, true
}

// jobRules はルールの追加・削除・並べ替えを行ったルールの部分を返す（ファイルの選択を残すため、フォーム全体は差し替えない）。
func (h *Handler) jobRules(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.JobManage) {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	rules, ok := readRuleForms(r)
	if !ok {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。画面を開き直してください。")
		return
	}
	f := jobForm{Kind: bulk.RadiusCreate, Rules: applyRuleOp(rules, r.PostForm.Get("op"), 0), Errors: fieldErrors{}}
	if len(f.Rules) > maxRules {
		f.Rules, f.Errors["rules"] = f.Rules[:maxRules], fmt.Sprintf("ルールは %d 件までです。", maxRules)
	}
	numberRules(f.Rules)
	h.renderBlock(w, r, http.StatusOK, "job_new", "job-rules", f)
}

// readJobSettings は一律の設定を読んで確かめる（X5）。誤りは f に書く。
func readJobSettings(r *http.Request, f *jobForm) bulk.Settings {
	var s bulk.Settings
	if !f.Create() {
		return s
	}
	pf := r.PostForm
	f.Tenant, f.State = pf.Get("tenant"), pf.Get("state")
	id, chosen, live := readTenantChoice(f.Tenant, f.Tenants)
	f.Errors.check(chosen, "tenant", "所属のテナントを選んでください。")
	f.Errors.check(!chosen || live, "tenant", "選んだテナントはありません。画面を開き直してください。")
	f.Errors.check(f.State == db.StateActive || f.State == db.StateInactive, "state", "登録のときの状態を選んでください。")
	s.TenantID, s.State = id, f.State
	switch f.Kind {
	case bulk.VecgenCreate:
		f.SQNType = pf.Get("sqn_type")
		f.Errors.check(slices.Contains(sqnTypes, f.SQNType), "sqnType", "SQN の方式を選んでください。")
		for _, v := range pf["clients"] {
			cid, err := parseID(v)
			ok := err == nil && slices.ContainsFunc(f.AllClients, func(c provisioner.VecgenClient) bool { return c.ID == cid })
			f.Errors.check(ok, "clients", "許可クライアントの指定が正しくありません。画面を開き直してください。")
			f.Clients = append(f.Clients, cid)
		}
		slices.Sort(f.Clients)
		f.Clients = slices.Compact(f.Clients)
		s.SQNType, s.AllowedClientIDs = f.SQNType, f.Clients
	case bulk.RadiusCreate:
		f.Source = pf.Get("source")
		f.Errors.check(slices.ContainsFunc(f.Sources, func(src provisioner.Source) bool { return src.ID == f.Source }), "source", "取得先を選んでください。")
		s.SourceID = f.Source
		rules, ok := readRuleForms(r)
		if !ok {
			f.Errors["rules"] = "ルールの入力を読み取れませんでした。画面を開き直してください。"
			break
		}
		f.Rules = rules
		if len(rules) == 0 || len(rules) > maxRules {
			f.Errors["rules"] = fmt.Sprintf("ルールは 1〜%d 件にしてください。", maxRules)
			break
		}
		for i := range f.Rules {
			rule, ok := f.Rules[i].toRule(id)
			if !ok {
				f.Errors["rules"] = "ルールの入力を確かめてください。"
			}
			s.Rules = append(s.Rules, rule)
		}
	}
	return s
}

// readJobFile はアップロードされたファイルを読む。平文はメモリーの中だけに置く。
func readJobFile(r *http.Request) (name string, data []byte, msg string) {
	files := r.MultipartForm.File["file"]
	if len(files) != 1 {
		return "", nil, "CSV のファイルを選んでください。"
	}
	fh := files[0]
	if fh.Size > bulk.MaxFileBytes {
		return "", nil, fmt.Sprintf("ファイルが大きすぎます（%d MiB まで）。", bulk.MaxFileBytes>>20)
	}
	src, err := fh.Open()
	if err != nil {
		return "", nil, "ファイルを読めませんでした。"
	}
	defer src.Close()
	data, err = io.ReadAll(src)
	if err != nil {
		return "", nil, "ファイルを読めませんでした。"
	}
	return jobFileName(fh.Filename), data, ""
}

// jobFileName はファイルの名前を、監査ログと画面に出す形にする（ディレクトリと制御文字を除き、255 文字まで）。
func jobFileName(name string) string {
	name = path.Base(strings.ReplaceAll(strings.ToValidUTF8(name, ""), `\`, "/"))
	name = strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return -1
		}
		return c
	}, name)
	if runes := []rune(name); len(runes) > 255 {
		name = string(runes[:255])
	}
	if name == "" || name == "." || name == "/" {
		return "upload.csv"
	}
	return name
}

// classifyJobRows は全行を台帳と比べる（台帳は 1 回の問い合わせ。X6）。印の立った IMSI はその場で読み直してから判定する
// （結果が分からなかった行の残るファイルを、下流が戻った後に投入し直せるように）。
func (h *Handler) classifyJobRows(r *http.Request, kind bulk.Kind, tenant int64, rows []bulk.Row) (bulk.Plan, error) {
	imsis := make([]string, 0, len(rows))
	for _, row := range rows {
		imsis = append(imsis, row.IMSI)
	}
	read := func() (bulk.Plan, error) {
		ctx, cancel := dbContext(r)
		defer cancel()
		entries, err := h.db.SubscribersByIMSI(ctx, imsis)
		if err != nil {
			return bulk.Plan{}, err
		}
		return bulk.Classify(kind, tenant, rows, entries), nil
	}
	plan, err := read()
	if err != nil || len(plan.CheckNeeded) == 0 {
		return plan, err
	}
	fixed := false
	for _, imsi := range plan.CheckNeeded[:min(len(plan.CheckNeeded), jobRecheckLimit)] {
		fixed = h.recheck(r, imsi) || fixed
	}
	if !fixed {
		return plan, nil
	}
	return read()
}

// jobCreate はファイルを受けて全行を確かめ、検証済みのジョブを作る（誤りが 1 行でもあれば何も残さない。X6）。
func (h *Handler) jobCreate(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.JobManage) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	// 要求全体の上限より大きいメモリーを許すので、ファイルはディスクに書かれない（X4）。
	if err := r.ParseMultipartForm(maxUploadBytes + 1); err != nil {
		msg := "入力を読み取れませんでした。"
		if _, tooBig := errors.AsType[*http.MaxBytesError](err); tooBig {
			msg = fmt.Sprintf("ファイルが大きすぎます（%d MiB まで）。", bulk.MaxFileBytes>>20)
		}
		h.renderErrorLink(w, r, http.StatusBadRequest, msg, "/jobs/new", "登録の画面へ")
		return
	}
	defer r.MultipartForm.RemoveAll()

	f := jobForm{Kind: bulk.Kind(r.PostForm.Get("kind")), Errors: fieldErrors{}}
	if !f.Kind.Valid() {
		h.renderErrorLink(w, r, http.StatusBadRequest, "ジョブの種類の指定が正しくありません。", "/jobs/new", "登録の画面へ")
		return
	}
	if !h.loadJobChoices(w, r, &f) {
		return
	}
	settings := readJobSettings(r, &f)
	if f.LoadError != "" {
		f.Error = "選択肢を確かめられないので、受け付けられません。"
		h.renderJobForm(w, r, http.StatusServiceUnavailable, f)
		return
	}
	name, data, msg := readJobFile(r)
	f.Errors.check(msg == "", "file", msg)
	if len(f.Errors) > 0 {
		f.Error = "入力を確かめてください。"
		h.renderJobForm(w, r, http.StatusBadRequest, f)
		return
	}
	defer clear(data)

	rows, errs := bulk.ParseCSV(f.Kind, data)
	var plan bulk.Plan
	if len(errs) == 0 {
		var err error
		if plan, err = h.classifyJobRows(r, f.Kind, settings.TenantID, rows); err != nil {
			h.dbUnavailable(w, r, err)
			return
		}
		errs = plan.Errors
	}
	if len(errs) > 0 {
		f.Error = "ファイルに誤りがあるので、何も投入していません。直してからもう一度選んでください。"
		f.FileErrorTotal, f.FileErrors = len(errs), errs[:min(len(errs), jobErrorsOnPage)]
		h.renderJobForm(w, r, http.StatusBadRequest, f)
		return
	}

	raw, err := json.Marshal(settings, json.Deterministic(true))
	if err != nil {
		h.log.Error("job settings", "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "処理に失敗しました。")
		return
	}
	u := userFrom(r.Context())
	ctx, cancel := dbContext(r)
	job, err := h.db.CreateJob(ctx, db.JobCreate{Kind: string(f.Kind), Settings: raw, FileName: name, KEKID: h.kek.ID(), Total: len(rows),
		PlanExists: plan.Exists, PlanAbsent: plan.Absent, DeletedIMSIs: plan.Deleted, CreatedBy: u.Email},
		func(id int64) ([]byte, error) { return h.kek.Seal(data, bulk.AAD(id)) })
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		detail = map[string]any{}
	}
	detail["kind"], detail["file"], detail["total"] = string(f.Kind), name, len(rows)
	detail["exists"], detail["absent"], detail["deleted"] = plan.Exists, plan.Absent, len(plan.Deleted)
	h.recordJob(r, job, settings.TenantID, audit.ActionJobCreate, detail)
	seeOther(w, r, "/jobs/"+job.JobID())
}

// recordJob はジョブの操作を監査ログに書く。
func (h *Handler) recordJob(r *http.Request, job db.Job, tenant int64, action string, detail map[string]any) {
	h.recordEntry(r, db.AuditEntry{TenantID: tenant, Action: action, Target: "job-" + job.JobID(), Detail: detail, JobID: job.JobID()})
}

// ---- 詳細 ----

// jobData はジョブの詳細の画面の値。
type jobData struct {
	Job      db.Job
	Settings bulk.Settings
	Tenants  tenantInfo
	// ToProcess は確定したら下流を呼ぶ見込みの数（削除済みを含む）。
	ToProcess int
	// Deleted は削除済みの IMSI（jobDeletedOnPage 件まで）。
	Deleted []string
	// Rows は成功以外の行（jobRowsOnPage 件まで）。MoreRows は続きがあるか。
	Rows     []db.JobRow
	MoreRows bool
	// CanManage は操作のボタンを出すか。
	CanManage bool
	// Reason は承認の理由の入力。
	Reason         string
	Errors         fieldErrors
	Message, Error string
}

// loadJob は URL のジョブを読む。なければ 404 を返して false。
func (h *Handler) loadJob(w http.ResponseWriter, r *http.Request) (db.Job, bool) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		h.notFound(w, r)
		return db.Job{}, false
	}
	ctx, cancel := dbContext(r)
	defer cancel()
	job, err := h.db.Job(ctx, id)
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.renderErrorLink(w, r, http.StatusNotFound, "ジョブが見つかりません。", "/jobs", "ジョブの一覧へ")
		return db.Job{}, false
	case err != nil:
		h.dbUnavailable(w, r, err)
		return db.Job{}, false
	}
	return job, true
}

// newJobData は詳細の画面の値を作る。
func (h *Handler) newJobData(w http.ResponseWriter, r *http.Request, job db.Job) (jobData, bool) {
	d := jobData{Job: job, CanManage: can(r, authz.JobManage), Errors: fieldErrors{}, ToProcess: job.Total - job.PlanExists - job.PlanAbsent}
	if err := json.Unmarshal(job.Settings, &d.Settings); err != nil {
		h.log.Error("job settings", "job_id", job.ID, "error", err)
	}
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return jobData{}, false
	}
	d.Tenants = t
	ctx, cancel := dbContext(r)
	defer cancel()
	if job.State == db.JobValidated && job.Deleted > 0 {
		imsis, err := h.db.JobDeletedIMSIs(ctx, job.ID)
		if err != nil {
			h.dbUnavailable(w, r, err)
			return jobData{}, false
		}
		d.Deleted = imsis[:min(len(imsis), jobDeletedOnPage)]
	}
	rows, err := h.db.JobRows(ctx, job.ID, 0, jobRowsOnPage+1)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return jobData{}, false
	}
	if len(rows) > jobRowsOnPage {
		rows, d.MoreRows = rows[:jobRowsOnPage], true
	}
	d.Rows = rows
	return d, true
}

func (h *Handler) job(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.JobView) {
		return
	}
	job, ok := h.loadJob(w, r)
	if !ok {
		return
	}
	// 定期の読み込みで状態が変わったら、操作のボタンごと描き直す。
	if isHTMX(r) && r.URL.Query().Get("state") != job.State {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	d, ok := h.newJobData(w, r, job)
	if !ok {
		return
	}
	if r.URL.Query().Get("interrupted") == "1" {
		d.Message = "中断を受け付けました。"
		if job.State == db.JobRunning {
			d.Message = "中断を受け付けました。処理中の行を終えてから止めます。"
		}
	}
	h.renderPart(w, r, http.StatusOK, "job", "job-progress", "ジョブ "+job.JobID(), d)
}

// jobRowsCSV は成功以外の行を CSV で返す（鍵は含まない）。
func (h *Handler) jobRowsCSV(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.JobView) {
		return
	}
	job, ok := h.loadJob(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="job-%d-rows.csv"`, job.ID))
	// Excel で文字化けしないよう BOM を付ける。
	if _, err := w.Write([]byte("\xEF\xBB\xBF")); err != nil {
		return
	}
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"line", "imsi", "result", "unknown", "reason"}); err != nil {
		return
	}
	after := 0
	for {
		ctx, cancel := dbContext(r)
		rows, err := h.db.JobRows(ctx, job.ID, after, jobCSVBatch)
		cancel()
		if err != nil {
			// 途中まで書いたので、状態コードでは知らせられない。接続を切って不完全なことを知らせる。
			h.log.Error("job rows csv", "job_id", job.ID, "error", err)
			panic(http.ErrAbortHandler)
		}
		for _, row := range rows {
			unknown := ""
			if row.Unknown {
				unknown = "結果不明"
			}
			if err := cw.Write([]string{fmt.Sprint(row.Line), row.IMSI, jobRowLabel(row.Result), unknown, row.Reason}); err != nil {
				return
			}
			after = row.Line
		}
		if len(rows) < jobCSVBatch {
			break
		}
	}
	cw.Flush()
}

// ---- 確定・取り消し・中断 ----

// jobConfirm は検証済みのジョブを待ち行列に入れる。削除済みの IMSI があれば、承認して含める（理由が必須）か除いて進めるかを選ぶ（X7）。
func (h *Handler) jobConfirm(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.JobManage) {
		return
	}
	job, ok := h.loadJob(w, r)
	if !ok {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	choice, reason := r.PostForm.Get("choice"), strings.TrimSpace(r.PostForm.Get("reason"))
	fail := func(status int, msg string, errs fieldErrors) {
		d, ok := h.newJobData(w, r, job)
		if !ok {
			return
		}
		d.Error, d.Reason = msg, reason
		if errs != nil {
			d.Errors = errs
		}
		h.render(w, r, status, "job", "ジョブ "+job.JobID(), d)
	}
	if job.Deleted > 0 {
		switch {
		case choice == db.DeletedExclude:
			reason = ""
		case choice != db.DeletedInclude:
			fail(http.StatusBadRequest, "削除済みの IMSI の扱いを選んでください。", nil)
			return
		case reason == "" || len([]rune(reason)) > maxReasonLen:
			fail(http.StatusBadRequest, "入力を確かめてください。", fieldErrors{"reason": "承認の理由を 500 文字までで入力してください。"})
			return
		}
	} else {
		choice, reason = "", ""
	}
	ctx, cancel := dbContext(r)
	done, err := h.db.ConfirmJob(ctx, job.ID, choice, reason)
	cancel()
	switch {
	case errors.Is(err, db.ErrConflict):
		seeOther(w, r, "/jobs/"+job.JobID())
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	if choice == db.DeletedInclude {
		var s bulk.Settings
		_ = json.Unmarshal(done.Settings, &s)
		h.recordJob(r, done, s.TenantID, audit.ActionJobReregister, map[string]any{"count": done.Deleted, "reason": reason, "file": done.FileName})
	}
	h.jobs.Wake(done.Target)
	seeOther(w, r, "/jobs/"+done.JobID())
}

// jobCancel は検証済みのジョブを取り消す（ファイルを消す）。
func (h *Handler) jobCancel(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.JobManage) {
		return
	}
	job, ok := h.loadJob(w, r)
	if !ok {
		return
	}
	const reason = "システム管理者が取り消しました。"
	ctx, cancel := dbContext(r)
	done, err := h.db.CancelJob(ctx, job.ID, reason)
	cancel()
	switch {
	case errors.Is(err, db.ErrConflict):
		seeOther(w, r, "/jobs/"+job.JobID())
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	var s bulk.Settings
	_ = json.Unmarshal(done.Settings, &s)
	h.recordJob(r, done, s.TenantID, audit.ActionJobCancel, map[string]any{"kind": done.Kind, "file": done.FileName, "reason": reason})
	seeOther(w, r, "/jobs/"+done.JobID())
}

// jobInterrupt は待ちか実行中のジョブを中断する（再開はしない。同じファイルを投入し直せば残りだけが処理される）。
func (h *Handler) jobInterrupt(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.JobManage) {
		return
	}
	job, ok := h.loadJob(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*dbTimeout)
	_, err := h.jobs.Interrupt(ctx, job.ID, userFrom(r.Context()).Email)
	cancel()
	switch {
	case errors.Is(err, db.ErrConflict):
		seeOther(w, r, "/jobs/"+job.JobID())
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	seeOther(w, r, "/jobs/"+job.JobID()+"?interrupted=1")
}
