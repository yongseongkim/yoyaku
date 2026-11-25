package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestOfflineCaseCommands(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{{[]string{"scenarios", "list"}, "normal-booking"}, {[]string{"scenarios", "show", "party-mismatch"}, "직접 확인할 항목"}, {[]string{"scenarios", "json"}, `"schema_version": 1`}, {[]string{"decision", "list"}, "ko-condition-pause"}} {
		t.Run(strings.Join(c.args, "_"), func(t *testing.T) {
			var out bytes.Buffer
			args := append([]string{"--root", "../.."}, c.args...)
			if e := Run(context.Background(), args, &out); e != nil {
				t.Fatal(e)
			}
			if !strings.Contains(out.String(), c.want) {
				t.Fatal(out.String())
			}
		})
	}
}
func TestUnknownCasesDoNotStartModelWork(t *testing.T) {
	for _, args := range [][]string{{"scenarios", "show", "missing"}, {"decision", "run", "missing"}, {"scenarios", "unexpected"}} {
		var out bytes.Buffer
		if Run(context.Background(), append([]string{"--root", "../.."}, args...), &out) == nil {
			t.Fatal("Unknown case accepted", args)
		}
	}
}
