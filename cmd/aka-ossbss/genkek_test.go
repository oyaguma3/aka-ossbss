package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oyaguma3/aka-ossbss/internal/kek"
)

func TestGenKEK(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := genKEK(nil, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); len(got) != 64 {
		t.Errorf("stdout = %q, want 64 hex characters", got)
	}
	if !strings.Contains(errOut.String(), "generated") {
		t.Errorf("stderr = %q", errOut.String())
	}

	path := filepath.Join(t.TempDir(), "kek")
	if err := genKEK([]string{"-out", path}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if _, err := kek.Load(path); err != nil {
		t.Error(err)
	}
	// 上書きしない。
	if err := genKEK([]string{"-out", path}, &out, &errOut); err == nil {
		t.Error("overwrote an existing KEK")
	}
	if err := genKEK([]string{"extra"}, &out, &errOut); err == nil {
		t.Error("accepted an unexpected argument")
	}
}
