package cmd

import (
	"strings"
	"testing"
)

func TestApplicationInputCannotHideTrailingContentBeyondLimit(t *testing.T) {
	prefix := `{"name":"web","image":"docker.io/library/busybox:1.37"}`
	exact := prefix + strings.Repeat(" ", (1<<20)-len(prefix))
	a, err := decodeApplication("-", strings.NewReader(exact))
	if err != nil || a.Name != "web" {
		t.Fatal("valid bounded input rejected", err)
	}
	for _, suffix := range []string{" ", `{"name":"hidden"}`} {
		if _, err = decodeApplication("-", strings.NewReader(exact+suffix)); err == nil {
			t.Fatal("oversize application input accepted")
		}
	}
	if _, err = decodeApplication("-", strings.NewReader(prefix+` {}`)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	if _, err = decodeApplication("-", strings.NewReader(`{"name":"web","unexpected":true}`)); err == nil {
		t.Fatal("unknown configuration accepted")
	}
}
