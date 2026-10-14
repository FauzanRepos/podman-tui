package registry

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConnectingEndpointCannotPublishAfterSelectionChanges(t *testing.T) {
	UnsetConnection()
	t.Cleanup(UnsetConnection)
	started, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	delayed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-resume
		w.Header().Set("Libpod-API-Version", "6.1.1")
		w.Write([]byte("OK"))
	}))
	defer delayed.Close()
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Libpod-API-Version", "6.1.1")
		w.Write([]byte("OK"))
	}))
	defer ready.Close()
	a := Connection{Name: "a", URI: strings.Replace(delayed.URL, "http://", "tcp://", 1)}
	b := Connection{Name: "b", URI: strings.Replace(ready.URL, "http://", "tcp://", 1)}
	SetConnection(a)
	result := make(chan error, 1)
	go func() { _, err := GetConnection(); result <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		close(resume)
		t.Fatal("connection handshake did not start")
	}
	SetConnection(b)
	close(resume)
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("old endpoint published after host switch")
		}
	case <-time.After(time.Second):
		t.Fatal("stale handshake did not finish")
	}
	ctx, err := GetConnection()
	if err != nil {
		t.Fatal(err)
	}
	SetConnectionStatus(ConnectionStatusConnected)
	cached, err := GetCachedConnection(b)
	if err != nil || cached != ctx {
		t.Fatal("new endpoint binding unavailable", err)
	}
	if _, err = GetCachedConnection(a); err == nil {
		t.Fatal("old endpoint accepted")
	}
	CancelContext()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("published transport not canceled")
	}
	if _, err = GetCachedConnection(b); err == nil {
		t.Fatal("canceled transport remained cached")
	}
	if !ConnectionIsSet() {
		t.Fatal("transport cancellation erased selected connection")
	}
}
