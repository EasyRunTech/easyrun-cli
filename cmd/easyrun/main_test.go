package main

import (
	"bytes"
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
		{nil, 0, "Pemakaian", ""},
		{[]string{"ngawur"}, 2, "", "perintah tidak dikenal: ngawur"},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if got := run(c.args, &out, &errb); got != c.code {
			t.Errorf("%v: kode %d, ingin %d", c.args, got, c.code)
		}
		if !strings.Contains(out.String(), c.stdout) {
			t.Errorf("%v: stdout %q tak memuat %q", c.args, out.String(), c.stdout)
		}
		if !strings.Contains(errb.String(), c.stderr) {
			t.Errorf("%v: stderr %q tak memuat %q", c.args, errb.String(), c.stderr)
		}
	}
}
