package charging

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/oyaguma3/aka-ossbss/internal/db"
)

// 月次のまとめのファイル（課金レコード §7・§8.2。Z13）。CSV を書きながら SHA-256 を計算し、ZIP（読み返さない形）→ PGP → メモリーの順に流す。
// 平文はディスクにも DB にも書かない。

// formatVersion は出力の形の版（列を足したら上げる）。
const formatVersion = "1.0"

// filePage はファイルを作るときの 1 回の読み取りの数。
const filePage = 5000

// ファイルの中の名前と種類。
const (
	fileEvents   = "events.csv"
	fileLines    = "lines.csv"
	fileTenants  = "tenants.csv"
	fileAdj      = "lines_adj.csv"
	fileManifest = "manifest.csv"
)

var (
	eventsHeader = []string{"record_id", "event_time", "event", "side", "tenant_id", "tenant_external_id", "imsi", "line_external_id", "from", "to",
		"operator", "job_id", "trace_id", "reason"}
	linesHeader = []string{"record_id", "period", "imsi", "tenant_id", "tenant_external_id", "line_external_id", "registered_days", "radius_active_days",
		"vecgen_active_days", "radius_state_start", "radius_state_end", "vecgen_state_start", "vecgen_state_end", "first_registered_at", "deleted_at",
		"auth_success_count", "session_count", "session_seconds", "input_octets", "output_octets", "usage_complete"}
	adjHeader     = append(append([]string{}, linesHeader...), "adjusts_record_id", "adjust_reason")
	tenantsHeader = []string{"record_id", "period", "tenant_id", "tenant_external_id", "tenant_name", "created_at", "deleted_at", "existed_days",
		"lines_end", "lines_max", "radius_active_lines_end", "radius_active_lines_max", "aps_end", "groups_end", "brought_cas_end", "placements_end"}
	manifestHeader = []string{"file_name", "kind", "period", "sequence", "version", "record_count", "sha256", "created_at", "format_version"}
)

// FileSource はファイルを作るときに読む口。*db.DB が満たす。
type FileSource interface {
	ChargingEventsPage(ctx context.Context, start, end time.Time, afterID int64, limit int) ([]db.ChargingEventRow, error)
	ChargingLinesPage(ctx context.Context, month time.Time, afterIMSI string, afterTenant int64, limit int) ([]db.ChargingLine, error)
	ChargingAdjPage(ctx context.Context, month time.Time, afterIMSI string, afterTenant int64, limit int) ([]db.ChargingLine, error)
	ChargingTenantsOf(ctx context.Context, month time.Time) ([]db.ChargingTenant, error)
}

// FileName はまとめのファイルの名前を返す（課金レコード §7.1）。
func FileName(p Period, sequence, version int) string {
	return fmt.Sprintf("charging_%s_%06d_v%d.zip.pgp", p.Compact(), sequence, version)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(db.JST).Format(time.RFC3339)
}

func lineFields(recordID, period string, l db.ChargingLine) []string {
	return []string{recordID, period, l.IMSI, strconv.FormatInt(l.TenantID, 10), l.TenantExternalID, l.LineExternalID,
		strconv.Itoa(l.RegisteredDays), strconv.Itoa(l.RadiusActiveDays), strconv.Itoa(l.VecgenActiveDays),
		l.RadiusStart, l.RadiusEnd, l.VecgenStart, l.VecgenEnd, formatTime(l.FirstRegisteredAt), formatTime(l.DeletedAt),
		strconv.FormatInt(l.AuthOK, 10), strconv.FormatInt(l.Sessions, 10), strconv.FormatInt(l.Seconds, 10),
		strconv.FormatInt(l.InOctets, 10), strconv.FormatInt(l.OutOctets, 10), strconv.FormatBool(l.UsageComplete)}
}

func lineRecordID(p Period, imsi string, tenant int64) string {
	return "L-" + p.Compact() + "-" + imsi + "-" + strconv.FormatInt(tenant, 10)
}

// BuildResult は作ったファイルの中身と記録。
type BuildResult struct {
	Data                                         []byte
	ZipSHA256, Manifest                          string
	EventCount, LineCount, TenantCount, AdjCount int
}

// csvFile は ZIP の中の 1 つの CSV を書く。
type csvFile struct {
	name, kind string
	w          *csv.Writer
	hash       io.Writer
	sum        func() string
	count      int
}

// openCSV は ZIP の中に CSV を作る（ヘッダーを書く）。
func openCSV(zw *zip.Writer, name, kind string, header []string, now time.Time) (*csvFile, error) {
	f, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: now})
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	w := csv.NewWriter(io.MultiWriter(f, h))
	w.UseCRLF = true
	if err := w.Write(header); err != nil {
		return nil, err
	}
	return &csvFile{name: name, kind: kind, w: w, sum: func() string { return hex.EncodeToString(h.Sum(nil)) }}, nil
}

func (c *csvFile) write(fields []string) error {
	c.count++
	return c.w.Write(fields)
}

func (c *csvFile) close() error {
	c.w.Flush()
	return c.w.Error()
}

// BuildFile は対象月のファイルを、保存した月次の値から作る（外部の番号は今の値。Z12）。
func BuildFile(ctx context.Context, src FileSource, p Period, sequence, version int, key *openpgp.Entity, now time.Time) (BuildResult, error) {
	var out bytes.Buffer
	// ZIP は圧縮済みなので PGP では圧縮しない（Z13）。
	enc, err := openpgp.Encrypt(&out, []*openpgp.Entity{key}, nil, &openpgp.FileHints{IsBinary: true},
		&packet.Config{DefaultCipher: packet.CipherAES256, DefaultHash: crypto.SHA256, DefaultCompressionAlgo: packet.CompressionNone, Time: func() time.Time { return now }})
	if err != nil {
		return BuildResult{}, fmt.Errorf("encrypt: %w", err)
	}
	zipHash := sha256.New()
	zw := zip.NewWriter(io.MultiWriter(enc, zipHash))

	files := []*csvFile{}
	section := func(name, kind string, header []string, fill func(*csvFile) error) (int, error) {
		c, err := openCSV(zw, name, kind, header, now)
		if err != nil {
			return 0, err
		}
		if err := fill(c); err != nil {
			return 0, err
		}
		if err := c.close(); err != nil {
			return 0, err
		}
		files = append(files, c)
		return c.count, nil
	}
	var res BuildResult
	if res.EventCount, err = section(fileEvents, "events", eventsHeader, func(c *csvFile) error { return writeEvents(ctx, src, p, c) }); err != nil {
		return BuildResult{}, fmt.Errorf("events: %w", err)
	}
	if res.LineCount, err = section(fileLines, "lines", linesHeader, func(c *csvFile) error { return writeLines(ctx, src, p, c) }); err != nil {
		return BuildResult{}, fmt.Errorf("lines: %w", err)
	}
	if res.TenantCount, err = section(fileTenants, "tenants", tenantsHeader, func(c *csvFile) error { return writeTenants(ctx, src, p, c) }); err != nil {
		return BuildResult{}, fmt.Errorf("tenants: %w", err)
	}
	if res.AdjCount, err = section(fileAdj, "lines_adj", adjHeader, func(c *csvFile) error { return writeAdj(ctx, src, p, c) }); err != nil {
		return BuildResult{}, fmt.Errorf("lines_adj: %w", err)
	}

	// manifest は最後に書く（CSV のハッシュは書きながら計算した）。
	var manifest strings.Builder
	mw, err := zw.CreateHeader(&zip.FileHeader{Name: fileManifest, Method: zip.Deflate, Modified: now})
	if err != nil {
		return BuildResult{}, fmt.Errorf("manifest: %w", err)
	}
	m := csv.NewWriter(io.MultiWriter(mw, &manifest))
	m.UseCRLF = true
	if err := m.Write(manifestHeader); err != nil {
		return BuildResult{}, fmt.Errorf("manifest: %w", err)
	}
	for _, c := range files {
		if err := m.Write([]string{c.name, c.kind, p.Label(), strconv.Itoa(sequence), strconv.Itoa(version), strconv.Itoa(c.count), c.sum(), formatTime(now), formatVersion}); err != nil {
			return BuildResult{}, fmt.Errorf("manifest: %w", err)
		}
	}
	m.Flush()
	if err := m.Error(); err != nil {
		return BuildResult{}, fmt.Errorf("manifest: %w", err)
	}
	if err := zw.Close(); err != nil {
		return BuildResult{}, fmt.Errorf("zip: %w", err)
	}
	if err := enc.Close(); err != nil {
		return BuildResult{}, fmt.Errorf("encrypt: %w", err)
	}
	res.Data, res.ZipSHA256, res.Manifest = out.Bytes(), hex.EncodeToString(zipHash.Sum(nil)), manifest.String()
	return res, nil
}

func writeEvents(ctx context.Context, src FileSource, p Period, c *csvFile) error {
	var after int64
	for {
		rows, err := src.ChargingEventsPage(ctx, p.Start, p.End, after, filePage)
		if err != nil {
			return err
		}
		for _, ev := range rows {
			if err := c.write([]string{"E-" + strconv.FormatInt(ev.ID, 10), formatTime(ev.Time), ev.Event, string(ev.Side), strconv.FormatInt(ev.TenantID, 10), ev.TenantExternalID,
				ev.IMSI, ev.LineExternalID, ev.From, ev.To, ev.Operator, ev.JobID, ev.TraceID, ev.Reason}); err != nil {
				return err
			}
			after = ev.ID
		}
		if len(rows) < filePage {
			return nil
		}
	}
}

func writeLines(ctx context.Context, src FileSource, p Period, c *csvFile) error {
	imsi, tenant := "", int64(0)
	for {
		rows, err := src.ChargingLinesPage(ctx, p.Start, imsi, tenant, filePage)
		if err != nil {
			return err
		}
		for _, l := range rows {
			if err := c.write(lineFields(lineRecordID(p, l.IMSI, l.TenantID), p.Label(), l)); err != nil {
				return err
			}
			imsi, tenant = l.IMSI, l.TenantID
		}
		if len(rows) < filePage {
			return nil
		}
	}
}

// writeAdj は訂正の行を書く。record_id はこの訂正の行の番号（出す月と回線とテナントで決まる）、period は置き換える行の月。
func writeAdj(ctx context.Context, src FileSource, p Period, c *csvFile) error {
	imsi, tenant := "", int64(0)
	for {
		rows, err := src.ChargingAdjPage(ctx, p.Start, imsi, tenant, filePage)
		if err != nil {
			return err
		}
		for _, l := range rows {
			target := NewPeriod(l.TargetPeriod)
			id := "A-" + p.Compact() + "-" + l.IMSI + "-" + strconv.FormatInt(l.TenantID, 10)
			if err := c.write(append(lineFields(id, target.Label(), l), lineRecordID(target, l.IMSI, l.TenantID), "late_usage")); err != nil {
				return err
			}
			imsi, tenant = l.IMSI, l.TenantID
		}
		if len(rows) < filePage {
			return nil
		}
	}
}

func writeTenants(ctx context.Context, src FileSource, p Period, c *csvFile) error {
	rows, err := src.ChargingTenantsOf(ctx, p.Start)
	if err != nil {
		return err
	}
	for _, t := range rows {
		id := strconv.FormatInt(t.TenantID, 10)
		if err := c.write([]string{"T-" + p.Compact() + "-" + id, p.Label(), id, t.ExternalID, t.Name, formatTime(t.CreatedAt), formatTime(t.DeletedAt),
			strconv.Itoa(t.ExistedDays), strconv.Itoa(t.LinesEnd), strconv.Itoa(t.LinesMax), strconv.Itoa(t.RadiusActiveLinesEnd), strconv.Itoa(t.RadiusActiveLinesMax),
			strconv.Itoa(t.APsEnd), strconv.Itoa(t.GroupsEnd), strconv.Itoa(t.BroughtCAsEnd), strconv.Itoa(t.PlacementsEnd)}); err != nil {
			return err
		}
	}
	return nil
}

// ---- PGP の公開鍵 ----

// KeyError は、登録できない鍵であること（文は画面に出す）。
type KeyError struct{ Message string }

func (e *KeyError) Error() string { return e.Message }

// minRSABits は RSA の鍵の長さの下限（課金レコード §8.3）。
const minRSABits = 3072

// KeyInfo は公開鍵の情報。
type KeyInfo struct {
	Fingerprint, Algorithm string
	// ExpiresAt は期限（ゼロ値は無期限）。主鍵と副鍵のうち早いほう。
	ExpiresAt time.Time
}

// ParsePublicKey は ASCII 形式の公開鍵を読み、登録の条件（1 つの鍵、公開鍵だけ、暗号化に使える鍵がある、期限が切れていない、
// RSA 3072 ビット以上か Curve25519）を確かめる。
func ParsePublicKey(armored string, now time.Time) (*openpgp.Entity, KeyInfo, error) {
	list, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armored))
	if err != nil {
		return nil, KeyInfo{}, &KeyError{"公開鍵を読み取れません。ASCII 形式（-----BEGIN PGP PUBLIC KEY BLOCK-----）で貼り付けてください。"}
	}
	// ReadArmoredKeyRing は最初のブロックしか読まないので、ブロックの数も確かめる。
	if len(list) != 1 || strings.Count(armored, "-----BEGIN PGP") != 1 {
		return nil, KeyInfo{}, &KeyError{"鍵は 1 つだけ登録できます。"}
	}
	e := list[0]
	if e.PrivateKey != nil {
		return nil, KeyInfo{}, &KeyError{"秘密鍵は登録できません。公開鍵だけを貼り付けてください。"}
	}
	key, ok := e.EncryptionKey(now)
	if !ok {
		return nil, KeyInfo{}, &KeyError{"暗号化に使える有効な鍵がありません（期限切れ、失効、または暗号化の副鍵がない）。"}
	}
	algo, err := keyAlgorithm(key.PublicKey)
	if err != nil {
		return nil, KeyInfo{}, err
	}
	info := KeyInfo{Fingerprint: strings.ToUpper(hex.EncodeToString(e.PrimaryKey.Fingerprint)), Algorithm: algo}
	if sig, _ := e.PrimarySelfSignature(); sig != nil {
		info.ExpiresAt = expiry(e.PrimaryKey, sig)
	}
	if t := expiry(key.PublicKey, key.SelfSignature); !t.IsZero() && (info.ExpiresAt.IsZero() || t.Before(info.ExpiresAt)) {
		info.ExpiresAt = t
	}
	return e, info, nil
}

// keyAlgorithm は鍵の強さを確かめて、種類の名前を返す。
func keyAlgorithm(pk *packet.PublicKey) (string, error) {
	switch pk.PubKeyAlgo {
	case packet.PubKeyAlgoRSA, packet.PubKeyAlgoRSAEncryptOnly:
		n, err := pk.BitLength()
		if err != nil {
			return "", &KeyError{"鍵の長さを読み取れません。"}
		}
		if int(n) < minRSABits {
			return "", &KeyError{fmt.Sprintf("RSA の鍵は %d ビット以上にしてください（この鍵は %d ビットです）。", minRSABits, n)}
		}
		return fmt.Sprintf("RSA %d", n), nil
	case packet.PubKeyAlgoX25519:
		return "Curve25519", nil
	case packet.PubKeyAlgoECDH:
		if c, err := pk.Curve(); err == nil && c == packet.Curve25519 {
			return "Curve25519", nil
		}
	}
	return "", &KeyError{"使える鍵は RSA 3072 ビット以上か Curve25519 です。"}
}

// expiry は鍵の期限を返す（無期限はゼロ値）。
func expiry(pk *packet.PublicKey, sig *packet.Signature) time.Time {
	if sig == nil || sig.KeyLifetimeSecs == nil || *sig.KeyLifetimeSecs == 0 {
		return time.Time{}
	}
	return pk.CreationTime.Add(time.Duration(*sig.KeyLifetimeSecs) * time.Second)
}

// keyWarnDays は期限の警告を出す日数、keyWarnDay は鍵がないときの警告を出し始める日（課金レコード §8.3。Z13）。
const (
	keyWarnDays = 30
	keyWarnDay  = 25
)

// KeyState は今の鍵の状態（画面と監視で警告を出すかの判断に使う）。
type KeyState struct {
	// Key は今の鍵（なければ nil）。Expired は期限切れ（鍵なしと同じ扱い。決定事項 21 の保留）。
	Key     *db.PGPKey
	Expired bool
	// Warning は警告の文（なければ空）。
	Warning string
}

// Usable はファイルを作れるか。
func (s KeyState) Usable() bool { return s.Key != nil && !s.Expired }

// EvaluateKey は今の鍵の状態を判定する。pending はファイルのない締めた月の数。
func EvaluateKey(now time.Time, key *db.PGPKey, pending int) KeyState {
	s := KeyState{Key: key}
	if key != nil && !key.ExpiresAt.IsZero() && !now.Before(key.ExpiresAt) {
		s.Expired = true
	}
	switch {
	case key == nil && pending > 0:
		s.Warning = fmt.Sprintf("PGP の公開鍵が登録されていないので、%d か月分のファイルを保留しています。", pending)
	case key == nil && now.In(db.JST).Day() >= keyWarnDay:
		s.Warning = "PGP の公開鍵が登録されていません。翌月 1 日の締めまでに登録してください。"
	case s.Expired:
		s.Warning = "PGP の公開鍵の期限が切れています。ファイルを作れないので、新しい鍵を登録してください。"
		if pending > 0 {
			s.Warning = fmt.Sprintf("PGP の公開鍵の期限が切れているので、%d か月分のファイルを保留しています。新しい鍵を登録してください。", pending)
		}
	case key != nil && !key.ExpiresAt.IsZero() && key.ExpiresAt.Sub(now) <= keyWarnDays*24*time.Hour:
		s.Warning = fmt.Sprintf("PGP の公開鍵の期限が近づいています（%s まで）。", key.ExpiresAt.In(db.JST).Format(time.DateOnly))
	}
	return s
}

// ErrNoKey は、使える鍵がないこと。
var ErrNoKey = errors.New("no usable PGP public key")
