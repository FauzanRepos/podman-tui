package quadlet

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	podmanenv "go.podman.io/podman/v6/pkg/env"
)

func TestInstalledEnvironmentPreservesPodmanConsumerValues(t *testing.T) {
	m := testManager(t)
	a, r := fixture()
	quadletCommands(t, m, a.ID)
	expected := map[string]string{"MESSAGE": "space % $ quote\" slash\\", "EMPTY": "", "TRAILING": " trailing ", "EQUALS": "a=b=c", "QUOTES": "'\"quoted\"'"}
	cfg := a.Config()
	cfg.Env = expected
	a.Profiles[a.Profile] = cfg
	if err := m.Install(context.Background(), a, r, true); err != nil {
		t.Fatal(err)
	}
	unitPath, _, _ := m.names(a.ID)
	unit, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	envPath := ""
	for _, line := range strings.Split(string(unit), "\n") {
		if value, found := strings.CutPrefix(line, "EnvironmentFile="); found {
			envPath, err = strconv.Unquote(value)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if envPath == "" {
		t.Fatal("installed unit has no environment reference")
	}
	actual, err := podmanenv.ParseFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range expected {
		if actual[key] != want {
			t.Fatalf("Podman sees %s=%q, expected %q", key, actual[key], want)
		}
	}
}
