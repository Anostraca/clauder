//go:build unix

package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseArgs(t *testing.T) {
	cases := []struct {
		args []string
		want options
	}{
		{nil, options{}},
		{[]string{"--resume"}, options{pass: []string{"--resume"}}},
		{[]string{"fix bug"}, options{pass: []string{"fix bug"}}},
		{[]string{"-p", "hi"}, options{pass: []string{"-p", "hi"}}},
		{[]string{"--find", "infra", "--print-path"}, options{find: "infra", hasFind: true, printPath: true}},
		{[]string{"--find=x", "-c"}, options{find: "x", hasFind: true, pass: []string{"-c"}}},
		{[]string{"--favs", "--", "--find", "y"}, options{favs: true, pass: []string{"--find", "y"}}},
		{[]string{"--config", "/c.json", "--settings", "-r"}, options{config: "/c.json", settings: true, pass: []string{"-r"}}},
		{[]string{"--print-path", "--resume", "--favs"}, options{printPath: true, pass: []string{"--resume", "--favs"}}},
		{[]string{"--pull"}, options{pull: true}},
		{[]string{"--pull", "~/x", "--clone-missing", "--jobs", "3"}, options{pull: true, pullPath: "~/x", cloneMissing: true, jobs: 3}},
		{[]string{"--pull", "--jobs=2"}, options{pull: true, jobs: 2}},
		{[]string{"--help"}, options{help: true}},
		{[]string{"--version", "--edit-config"}, options{version: true, editConfig: true}},
	}
	for _, c := range cases {
		got, err := parseArgs(c.args)
		if err != nil {
			t.Errorf("%q: %v", c.args, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q:\n got  %+v\n want %+v", c.args, got, c.want)
		}
	}
	for _, bad := range [][]string{{"--find"}, {"--config"}, {"--jobs", "0"}, {"--jobs", "x"}} {
		if _, err := parseArgs(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestPrintPathStdoutOnly(t *testing.T) {
	cfg, root := fixture(t)
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	code := run([]string{"--config", cfg.file, "--find", "beta", "--print-path"})
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if code != 0 || string(out) != filepath.Join(root, "beta")+"\n" {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
}
