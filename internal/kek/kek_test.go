package kek

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kek")
	k, err := GenerateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	l, err := Load(path)
	if err != nil || l.ID() != k.ID() || len(k.ID()) != 16 {
		t.Fatalf("Load = %v, %v; id %q want %q", l, err, l.ID(), k.ID())
	}
	if _, err := GenerateFile(path); err == nil {
		t.Error("GenerateFile overwrote an existing file")
	}

	var buf bytes.Buffer
	g, err := Generate(&buf)
	if err != nil || len(strings.TrimSpace(buf.String())) != 64 {
		t.Fatalf("Generate wrote %q, %v", buf.String(), err)
	}
	if err := os.WriteFile(path+"2", buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if l, err := Load(path + "2"); err != nil || l.ID() != g.ID() {
		t.Errorf("Load(generated) = %v, %v", l, err)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"short":   strings.Repeat("ab", 31),
		"not-hex": strings.Repeat("zz", 32),
		"empty":   "",
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Errorf("Load(%s) returned nil error", name)
		}
	}
	if _, err := Load(filepath.Join(dir, "none")); err == nil {
		t.Error("Load(missing) returned nil error")
	}
}

func TestSealOpen(t *testing.T) {
	k1, _ := New(bytes.Repeat([]byte{1}, Size))
	k2, _ := New(bytes.Repeat([]byte{2}, Size))
	plain := []byte("0123456789abcdef0123456789abcdef")

	ct, err := k1.Seal(plain, "aka-ossbss/bulk-job/1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, plain) || len(ct) != 12+len(plain)+16 {
		t.Fatalf("sealed = %x", ct)
	}
	if p, err := k1.Open(ct, "aka-ossbss/bulk-job/1"); err != nil || !bytes.Equal(p, plain) {
		t.Errorf("Open = %x, %v", p, err)
	}
	// 違う KEK・違う AAD（別の行）・壊れた暗号文は復号できない
	if _, err := k2.Open(ct, "aka-ossbss/bulk-job/1"); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Open with another KEK: %v", err)
	}
	if _, err := k1.Open(ct, "aka-ossbss/bulk-job/2"); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Open with another AAD: %v", err)
	}
	ct[len(ct)-1] ^= 1
	if _, err := k1.Open(ct, "aka-ossbss/bulk-job/1"); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Open of a broken ciphertext: %v", err)
	}
	if _, err := k1.Open([]byte("short"), ""); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Open of a short ciphertext: %v", err)
	}
	// 同じ平文でも毎回違う暗号文になる
	a, _ := k1.Seal(plain, "x")
	b, _ := k1.Seal(plain, "x")
	if bytes.Equal(a, b) {
		t.Error("Seal returned the same ciphertext twice")
	}
	if _, err := New(make([]byte, 16)); err == nil {
		t.Error("New accepted a 16-byte key")
	}
}
