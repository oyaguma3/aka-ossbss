// Package bulk は一括投入と一括の物理削除（設計概要 §10。段階 3-2）の、CSV の読み取りと検証、ジョブの実行を受け持つ。
// 画面（internal/web）がファイルを受けて検証し、確定したジョブをこのパッケージの Runner が対象ごとの待ち行列で動かす。
// 鍵（Ki・OPc）はメモリーの中だけで扱い、誤りの文・ログ・監査ログ・結果の行に入れない。
package bulk

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/oyaguma3/aka-ossbss/internal/db"
)

// Kind はジョブの種類。
type Kind string

const (
	VecgenCreate Kind = "vecgen.create"
	RadiusCreate Kind = "radius.create"
	VecgenDelete Kind = "vecgen.delete"
	RadiusDelete Kind = "radius.delete"
)

// Kinds はジョブの種類（画面の順）。
var Kinds = []Kind{VecgenCreate, RadiusCreate, VecgenDelete, RadiusDelete}

// Valid は知っている種類か。
func (k Kind) Valid() bool { return slices.Contains(Kinds, k) }

// Side は対象の側（待ち行列）。
func (k Kind) Side() db.Side {
	if k == VecgenCreate || k == VecgenDelete {
		return db.SideVecgen
	}
	return db.SideRadius
}

// Create は登録のジョブか（偽なら物理削除）。
func (k Kind) Create() bool { return k == VecgenCreate || k == RadiusCreate }

// Header は CSV のヘッダー（種類ごとに固定。X4）。
func (k Kind) Header() []string {
	if k == VecgenCreate {
		return []string{"imsi", "ki", "opc", "amf", "sqn"}
	}
	return []string{"imsi"}
}

const (
	// MaxRows は 1 回のジョブの行の数の上限。
	MaxRows = 10000
	// MaxFileBytes はファイルの大きさの上限。
	MaxFileBytes = 8 << 20
	// DefaultAMF と DefaultSQN は、vecgen 登録の CSV で空のときの既定（下流の既定と同じ）。
	DefaultAMF = "8000"
	DefaultSQN = "000000000000"
)

// 入力の形式（下流の検証と同じ）。
var (
	imsiPattern = regexp.MustCompile(`^[0-9]{15}$`)
	hex128      = regexp.MustCompile(`^[0-9a-f]{32}$`)
	amfPattern  = regexp.MustCompile(`^[0-9a-f]{4}$`)
	sqnPattern  = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

// Row は CSV の 1 行（16 進は小文字にしたもの）。
type Row struct {
	// Line はファイルの行番号（1 行目はヘッダー）。
	Line              int
	IMSI              string
	Ki, OPc, AMF, SQN string
}

// RowError は 1 行の誤り。Message に入力の値を入れない（列を取り違えた鍵が画面に出ないように）。
type RowError struct {
	// Line はファイルの行番号（0 はファイル全体の誤り）。
	Line int
	// IMSI は形式の正しい IMSI のときだけ入れる。
	IMSI    string
	Message string
}

// utf8BOM は UTF-8 の BOM（Excel の CSV に付く）。
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// ParseCSV はファイルを読み、行を確かめる（X4）。誤りがなければ行を返す。誤りがあれば、分かるだけの誤りを行番号つきで返す。
func ParseCSV(kind Kind, data []byte) ([]Row, []RowError) {
	if len(data) > MaxFileBytes {
		return nil, []RowError{{Message: fmt.Sprintf("ファイルが大きすぎます（%d MiB まで）。", MaxFileBytes>>20)}}
	}
	if !utf8.Valid(data) {
		return nil, []RowError{{Message: "ファイルが UTF-8 ではありません。"}}
	}
	cr := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, utf8BOM)))
	want := kind.Header()
	cr.FieldsPerRecord = len(want)
	cr.ReuseRecord = true

	header, err := cr.Read()
	if err != nil || !slices.Equal(header, want) {
		return nil, []RowError{{Line: 1, Message: "ヘッダーが違います。1 行目を「" + strings.Join(want, ",") + "」にしてください。"}}
	}
	var rows []Row
	var errs []RowError
	seen := make(map[string]int)
	count := 0
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if count++; count > MaxRows {
			return nil, []RowError{{Message: fmt.Sprintf("行が多すぎます（%d 行まで）。", MaxRows)}}
		}
		if pe, ok := errors.AsType[*csv.ParseError](err); ok {
			if errors.Is(pe.Err, csv.ErrFieldCount) {
				errs = append(errs, RowError{Line: pe.StartLine, Message: fmt.Sprintf("列の数が違います（%d 列にしてください）。", len(want))})
				continue
			}
			// 引用符の誤りなどは、その先の行の区切りが分からないので読むのをやめる。
			errs = append(errs, RowError{Line: pe.StartLine, Message: "CSV として読めません（引用符を確かめてください）。これより後ろの行は確かめていません。"})
			break
		}
		if err != nil {
			errs = append(errs, RowError{Message: "ファイルを読めません。"})
			break
		}
		line, _ := cr.FieldPos(0)
		row, rowErrs := parseRow(kind, line, rec)
		if prev, dup := seen[row.IMSI]; dup && row.IMSI != "" {
			rowErrs = append(rowErrs, RowError{Line: line, IMSI: row.IMSI, Message: fmt.Sprintf("%d 行目と同じ IMSI です。", prev)})
		}
		if row.IMSI != "" {
			if _, dup := seen[row.IMSI]; !dup {
				seen[row.IMSI] = line
			}
		}
		errs = append(errs, rowErrs...)
		rows = append(rows, row)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	if len(rows) == 0 {
		return nil, []RowError{{Message: "データの行がありません。"}}
	}
	return rows, nil
}

// parseRow は 1 行を確かめる。row.IMSI は形式が正しいときだけ入れる。
func parseRow(kind Kind, line int, rec []string) (Row, []RowError) {
	row := Row{Line: line}
	var errs []RowError
	bad := func(msg string) { errs = append(errs, RowError{Line: line, Message: msg}) }
	if imsi := strings.TrimSpace(rec[0]); imsiPattern.MatchString(imsi) {
		row.IMSI = imsi
	} else {
		bad("IMSI は 15 桁の数字にしてください。")
	}
	if kind == VecgenCreate {
		row.Ki, row.OPc = normalizeHex(rec[1]), normalizeHex(rec[2])
		row.AMF, row.SQN = normalizeHex(rec[3]), normalizeHex(rec[4])
		if row.AMF == "" {
			row.AMF = DefaultAMF
		}
		if row.SQN == "" {
			row.SQN = DefaultSQN
		}
		if !hex128.MatchString(row.Ki) {
			bad("Ki は 16 進 32 桁にしてください。")
		}
		if !hex128.MatchString(row.OPc) {
			bad("OPc は 16 進 32 桁にしてください。")
		}
		if !amfPattern.MatchString(row.AMF) {
			bad("AMF は 16 進 4 桁にしてください（空なら 8000）。")
		}
		if !sqnPattern.MatchString(row.SQN) {
			bad("SQN は 16 進 12 桁にしてください（空なら 000000000000）。")
		}
	}
	for i := range errs {
		errs[i].IMSI = row.IMSI
	}
	return row, errs
}

func normalizeHex(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

// Plan はファイルの全行を台帳と比べた結果（設計概要 §10.2。X6）。
type Plan struct {
	// Errors は誤り（1 行でもあれば何も投入しない）。
	Errors []RowError
	// Exists は同じ側に既にある数、Absent は削除の対象が台帳にない数（どちらも下流を呼ばずに数える）。
	Exists, Absent int
	// Deleted は登録の対象のうち削除済みの IMSI（昇順）。登録し直すにはシステム管理者の承認が要る。
	Deleted []string
	// CheckNeeded は結果を確かめている IMSI（Errors にも入る）。
	CheckNeeded []string
}

// Classify は全行を台帳の行と比べる。tenant は登録のジョブで選んだテナント。
func Classify(kind Kind, tenant int64, rows []Row, entries map[string]db.Entry) Plan {
	var p Plan
	side := kind.Side()
	fail := func(r Row, msg string) {
		p.Errors = append(p.Errors, RowError{Line: r.Line, IMSI: r.IMSI, Message: msg})
	}
	for _, r := range rows {
		e, ok := entries[r.IMSI]
		if ok && e.CheckNeeded {
			p.CheckNeeded = append(p.CheckNeeded, r.IMSI)
			fail(r, "下流の結果を確かめている IMSI です。結果が確かめられてから投入してください。")
			continue
		}
		if kind.Create() {
			switch {
			case !ok:
			case e.Deleted():
				p.Deleted = append(p.Deleted, r.IMSI)
			case e.TenantID != tenant:
				fail(r, "別のテナントに属する IMSI です。")
			case e.Present(side):
				p.Exists++
			}
			continue
		}
		switch {
		case !ok || e.Deleted() || !e.Present(side):
			p.Absent++
		case e.State(side) == db.StateActive:
			fail(r, "利用中（Active）の加入者は物理削除できません。先に止めるか解約してください。")
		}
	}
	slices.Sort(p.Deleted)
	return p
}
