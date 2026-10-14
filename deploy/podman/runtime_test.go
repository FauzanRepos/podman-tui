package podman

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/containers/podman-tui/deploy"
	"go.podman.io/podman/v6/pkg/bindings"
)

func TestMutationsRejectUnmanagedAndForeignContainers(t *testing.T) {
	id := strings.Repeat("b", 64)
	app := deploy.Application{ID: strings.Repeat("a", 32)}
	for _, ownership := range []string{"unmanaged", "foreign", "valid"} {
		t.Run(ownership, func(t *testing.T) {
			var mutations atomic.Int32
			labels := map[string]string{}
			if ownership != "unmanaged" {
				labels[deploy.OwnerLabel] = deploy.OwnerValue
				labels[deploy.ReleaseLabel] = strings.Repeat("c", 32)
				labels[deploy.AppLabel] = app.ID
			}
			if ownership == "foreign" {
				labels[deploy.AppLabel] = strings.Repeat("d", 32)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/_ping") {
					w.Header().Set("Libpod-API-Version", "6.1.1")
					w.Header().Set("API-Version", "6.1.1")
					w.Write([]byte("OK"))
					return
				}
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json") {
					json.NewEncoder(w).Encode(map[string]any{"Id": id, "Config": map[string]any{"Labels": labels}, "State": map[string]any{"Running": false}})
					return
				}
				if r.Method == http.MethodPost {
					mutations.Add(1)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				http.NotFound(w, r)
			}))
			defer server.Close()
			ctx, err := bindings.NewConnection(context.Background(), strings.Replace(server.URL, "http://", "tcp://", 1))
			if err != nil {
				t.Fatal(err)
			}
			adapter := New(ctx)
			err = adapter.Start(context.Background(), app, id)
			if ownership == "valid" {
				if err != nil || mutations.Load() != 1 {
					t.Fatal("authorized lifecycle failed", err)
				}
			} else if err == nil || mutations.Load() != 0 {
				t.Fatal("unmanaged/foreign container was mutated", err)
			}
			if err = adapter.Start(context.Background(), app, id[:12]); err == nil {
				t.Fatal("ambiguous partial container ID accepted")
			}
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			if err = adapter.Start(canceled, app, id); !errors.Is(err, context.Canceled) {
				t.Fatal("caller cancellation lost", err)
			}
		})
	}
}

func TestSpecRejectsUnsafeConfigurationBeforeRuntime(t *testing.T) {
	app := deploy.Application{ID: strings.Repeat("a", 32), Name: "web", Image: "docker.io/library/busybox:1.37", Profile: "production", Profiles: map[string]deploy.RuntimeConfig{"production": {Ports: []deploy.Port{{Container: 80}}, Secrets: []deploy.SecretRef{{Name: "credential", Type: "env", Target: "DB_PASSWORD"}}}}}
	release := deploy.Release{ID: strings.Repeat("b", 32), ImageID: strings.Repeat("c", 64)}
	specification, err := containerSpec(app, release)
	if err != nil {
		t.Fatal(err)
	}
	if specification.IsPrivileged() || specification.NoNewPrivileges == nil || !*specification.NoNewPrivileges || specification.PortMappings[0].HostIP != "127.0.0.1" {
		t.Fatal("managed container is not private and unprivileged by default")
	}
	if len(specification.Env) != 0 {
		t.Fatal("secret value materialized into ordinary environment configuration")
	}
	for _, invalid := range []deploy.RuntimeConfig{
		{Ports: []deploy.Port{{Container: 80, IP: "0.0.0.0;bad"}}},
		{Secrets: []deploy.SecretRef{{Name: "credential", Type: "env", Target: "DB_PASSWORD\n"}}},
		{MemoryBytes: -1},
	} {
		app.Profiles[app.Profile] = invalid
		if _, err = containerSpec(app, release); err == nil {
			t.Fatal("unsafe runtime configuration accepted")
		}
	}
}
