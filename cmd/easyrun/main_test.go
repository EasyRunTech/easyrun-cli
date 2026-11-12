package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	cases := []struct {
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{[]string{"--version"}, 0, "easyrun dev", ""},
		{[]string{"--help"}, 0, "--self-update", ""},
		{[]string{"--help"}, 0, "easyrun ssh <box>", ""},
		{nil, 0, "Pemakaian", ""},
		// Kontrak SSH box §6: perintah yang belum ada → "belum tersedia".
		{[]string{"ngawur"}, 2, "", `perintah "ngawur" belum tersedia`},
		{[]string{"shh"}, 2, "", "Mungkin maksudmu: easyrun ssh"},
		{[]string{"--ngawur"}, 2, "", `opsi "--ngawur" belum tersedia`},
		{[]string{"ssh"}, 2, "", "nama box belum diisi"},
		{[]string{"ssh", "--help"}, 0, "Pemakaian: easyrun ssh <box>", ""},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if got := run(context.Background(), c.args, strings.NewReader(""), &out, &errb); got != c.code {
			t.Errorf("%v: kode %d, ingin %d (stderr %q)", c.args, got, c.code, errb.String())
		}
		if !strings.Contains(out.String(), c.stdout) {
			t.Errorf("%v: stdout %q tak memuat %q", c.args, out.String(), c.stdout)
		}
		if !strings.Contains(errb.String(), c.stderr) {
			t.Errorf("%v: stderr %q tak memuat %q", c.args, errb.String(), c.stderr)
		}
	}
}
