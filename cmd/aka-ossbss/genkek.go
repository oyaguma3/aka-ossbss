package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/oyaguma3/aka-ossbss/internal/kek"
)

// genKEK は一括投入の待ちのファイルを暗号化する KEK（32 バイトの乱数を 16 進で書いたもの）を作る（段階 3-2 の X3）。
// -out を省くか - なら標準出力に書く（コンテナの外のファイルに書き出すため）。ファイルは権限 0600 で作り、上書きしない。
func genKEK(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("gen-kek", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "-", "KEK の出力先（- は標準出力）")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *out == "-" {
		k, err := kek.Generate(stdout)
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "kek %s generated\n", k.ID())
		return nil
	}
	k, err := kek.GenerateFile(*out)
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "kek %s written to %s\n", k.ID(), *out)
	return nil
}
