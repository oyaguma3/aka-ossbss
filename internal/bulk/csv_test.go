package bulk

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/oyaguma3/aka-ossbss/internal/db"
)

const (
	testKi  = "0123456789ABCDEF0123456789abcdef"
	testOPc = "fedcba9876543210FEDCBA9876543210"
)

func TestParseCSVVecgen(t *testing.T) {
	// BOM と CRLF を受け、末尾の空行を無視する。16 進は小文字にし、AMF と SQN は空なら既定。
	data := "\xEF\xBB\xBFimsi,ki,opc,amf,sqn\r\n" +
		"001010000000001," + testKi + "," + testOPc + ",8001,0000000000AB\r\n" +
		"001010000000002," + testKi + "," + testOPc + ",,\r\n\r\n\r\n"
	rows, errs := ParseCSV(VecgenCreate, []byte(data))
	if len(errs) != 0 {
		t.Fatalf("errors = %+v", errs)
	}
	want := []Row{
		{Line: 2, IMSI: "001010000000001", Ki: strings.ToLower(testKi), OPc: strings.ToLower(testOPc), AMF: "8001", SQN: "0000000000ab"},
		{Line: 3, IMSI: "001010000000002", Ki: strings.ToLower(testKi), OPc: strings.ToLower(testOPc), AMF: DefaultAMF, SQN: DefaultSQN},
	}
	if !slices.Equal(rows, want) {
		t.Errorf("rows = %+v", rows)
	}
}

func TestParseCSVErrors(t *testing.T) {
	long := func(n int) string {
		var b strings.Builder
		b.WriteString("imsi\n")
		for i := range n {
			fmt.Fprintf(&b, "%015d\n", i)
		}
		return b.String()
	}
	for _, tt := range []struct {
		name string
		kind Kind
		data string
		line int
		msg  string
	}{
		{"ヘッダーが違う", RadiusCreate, "IMSI\n001010000000001\n", 1, "ヘッダーが違います"},
		{"列の順が違う", VecgenCreate, "imsi,opc,ki,amf,sqn\n", 1, "ヘッダーが違います"},
		{"空", RadiusDelete, "", 1, "ヘッダーが違います"},
		{"行がない", RadiusDelete, "imsi\n\n", 0, "データの行がありません"},
		{"UTF-8 でない", RadiusDelete, "imsi\n\xff\n", 0, "UTF-8"},
		{"列の数", VecgenCreate, "imsi,ki,opc,amf,sqn\n001010000000001," + testKi + "\n", 2, "列の数"},
		{"引用符", RadiusDelete, "imsi\n\"0010\n", 2, "CSV として読めません"},
		{"IMSI の形式", RadiusDelete, "imsi\n1234\n", 2, "IMSI は 15 桁"},
		{"Ki の形式", VecgenCreate, "imsi,ki,opc,amf,sqn\n001010000000001,xyz," + testOPc + ",,\n", 2, "Ki は"},
		{"OPc の形式", VecgenCreate, "imsi,ki,opc,amf,sqn\n001010000000001," + testKi + ",1,,\n", 2, "OPc は"},
		{"AMF の形式", VecgenCreate, "imsi,ki,opc,amf,sqn\n001010000000001," + testKi + "," + testOPc + ",80,\n", 2, "AMF は"},
		{"SQN の形式", VecgenCreate, "imsi,ki,opc,amf,sqn\n001010000000001," + testKi + "," + testOPc + ",,1\n", 2, "SQN は"},
		{"重複", RadiusDelete, "imsi\n001010000000001\n001010000000002\n001010000000001\n", 4, "2 行目と同じ"},
		{"行が多い", RadiusDelete, long(MaxRows + 1), 0, "行が多すぎます"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rows, errs := ParseCSV(tt.kind, []byte(tt.data))
			if rows != nil || len(errs) == 0 {
				t.Fatalf("rows = %d, errors = %+v", len(rows), errs)
			}
			if errs[0].Line != tt.line || !strings.Contains(errs[0].Message, tt.msg) {
				t.Errorf("error = %+v, want line %d, %q", errs[0], tt.line, tt.msg)
			}
		})
	}
	if rows, errs := ParseCSV(RadiusDelete, []byte(long(MaxRows))); len(errs) != 0 || len(rows) != MaxRows {
		t.Errorf("MaxRows rows = %d, errors = %+v", len(rows), errs)
	}
	// 行の誤りが多くても、行の数で断らない。
	var many strings.Builder
	many.WriteString("imsi\n")
	for range MaxRows {
		many.WriteString("1\n")
	}
	if _, errs := ParseCSV(RadiusDelete, []byte(many.String())); len(errs) != MaxRows || !strings.Contains(errs[0].Message, "IMSI は") {
		t.Errorf("many bad rows: %d errors, first %+v", len(errs), errs[0])
	}
	big := make([]byte, MaxFileBytes+1)
	if _, errs := ParseCSV(RadiusDelete, big); len(errs) != 1 || !strings.Contains(errs[0].Message, "大きすぎ") {
		t.Errorf("big file: %+v", errs)
	}
}

// 列を取り違えても、誤りの文に入力の値（鍵かもしれない）を出さない。
func TestParseCSVDoesNotEchoValues(t *testing.T) {
	data := "imsi,ki,opc,amf,sqn\n" + testKi + ",secretvalue1,secretvalue2,secretvalue3,secretvalue4\n"
	_, errs := ParseCSV(VecgenCreate, []byte(data))
	if len(errs) != 5 {
		t.Fatalf("errors = %+v", errs)
	}
	for _, e := range errs {
		if e.IMSI != "" || strings.Contains(e.Message, "secretvalue") || strings.Contains(e.Message, testKi) {
			t.Errorf("error echoes the input: %+v", e)
		}
	}
}

func TestClassify(t *testing.T) {
	const tenant = 10
	entries := map[string]db.Entry{
		"001010000000001": {IMSI: "001010000000001", TenantID: tenant, VecgenPresent: true, VecgenState: db.StateActive},                      // 既にある
		"001010000000002": {IMSI: "001010000000002", TenantID: tenant, RadiusPresent: true, RadiusState: db.StateActive, RadiusSourceID: "v"}, // 片側
		"001010000000003": {IMSI: "001010000000003", TenantID: 20, RadiusPresent: true, RadiusState: db.StateActive, RadiusSourceID: "v"},     // 別のテナント
		"001010000000004": {IMSI: "001010000000004", TenantID: 20, DeletedAt: time1},                                                          // 削除済み（別のテナントでよい）
		"001010000000005": {IMSI: "001010000000005", TenantID: tenant, CheckNeeded: true, VecgenPresent: true, VecgenState: db.StateInactive}, // 印
	}
	rows := func(n ...int) []Row {
		var out []Row
		for i, x := range n {
			out = append(out, Row{Line: i + 2, IMSI: fmt.Sprintf("00101000000000%d", x)})
		}
		return out
	}
	p := Classify(VecgenCreate, tenant, rows(1, 2, 4, 9), entries)
	if len(p.Errors) != 0 || p.Exists != 1 || !slices.Equal(p.Deleted, []string{"001010000000004"}) {
		t.Errorf("create plan = %+v", p)
	}
	p = Classify(VecgenCreate, tenant, rows(3, 5), entries)
	if len(p.Errors) != 2 || !strings.Contains(p.Errors[0].Message, "別のテナント") || !strings.Contains(p.Errors[1].Message, "確かめている") ||
		!slices.Equal(p.CheckNeeded, []string{"001010000000005"}) {
		t.Errorf("create errors = %+v", p)
	}
	// 削除: ない・削除済み・その側にないものは数えるだけ、Active は誤り（台帳の状態で判定する。テナントは問わない）。
	p = Classify(VecgenDelete, 0, rows(1, 2, 4, 9), entries)
	if len(p.Errors) != 1 || p.Errors[0].IMSI != "001010000000001" || !strings.Contains(p.Errors[0].Message, "Active") || p.Absent != 3 {
		t.Errorf("delete plan = %+v", p)
	}
	p = Classify(VecgenDelete, 0, rows(5), entries)
	if len(p.CheckNeeded) != 1 || len(p.Errors) != 1 {
		t.Errorf("delete of a marked row = %+v", p)
	}
}
