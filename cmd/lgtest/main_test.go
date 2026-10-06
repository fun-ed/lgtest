package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestHelpAndList(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"list", "--provider", "psychz"}} {
		var out, diag bytes.Buffer
		if err := execute(context.Background(), args, strings.NewReader(""), &out, &diag); err != nil {
			t.Fatal(err)
		}
		if out.Len() == 0 {
			t.Fatal("empty CLI output")
		}
	}
}
func TestInvalidSelectionsNeverDownload(t *testing.T) {
	cases := [][]string{
		{"run"},
		{"run", "--region", "missing"},
		{"run", "--region", "psychz:tw", "--file", "9TB"},
		{"run", "--region", "psychz:tw", "--mode", "unknown"},
		{"run", "--region", "psychz:tw", "--mode", "direct", "--proxy", "http://localhost:8080"},
		{"run", "--region", "psychz:tw", "--mode", "http", "--proxy", "http://user:example@localhost:8080"},
		{"run", "--family", "9"},
		{"run", "--runs", "0"},
		{"refresh"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, diag bytes.Buffer
			if err := execute(context.Background(), args, strings.NewReader(""), &out, &diag); err == nil {
				t.Fatal("accepted invalid selection")
			}
		})
	}
}
func TestInteractiveRejectsOutOfRange(t *testing.T) {
	var out, diag bytes.Buffer
	err := execute(context.Background(), []string{"run", "--interactive"}, strings.NewReader("99999\n"), &out, &diag)
	if err == nil || !strings.Contains(diag.String(), "Select region") {
		t.Fatal("missing interactive validation")
	}
}

func TestAutoFamilyCreatesUnpinnedClientForAllModes(t *testing.T) {
	for _, mode := range []string{"direct", "http", "socks5"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var out, diag bytes.Buffer
			err := execute(ctx, []string{"run", "--region", "psychz:tw", "--mode", mode}, strings.NewReader(""), &out, &diag)
			if err == nil || !strings.Contains(out.String(), `"failed_runs":1`) {
				t.Fatalf("auto family did not reach the cancelled measurement: %v; %s", err, out.String())
			}
		})
	}
}
