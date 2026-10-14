package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containers/podman-tui/deploy"
)

func certificateFixture(t *testing.T, domain string, expired bool) (string, string, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	expiry := now.Add(48 * time.Hour)
	if expired {
		expiry = now.Add(-time.Hour)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: domain}, DNSNames: []string{domain}, NotBefore: now.Add(-24 * time.Hour), NotAfter: expiry, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	certificate := filepath.Join(root, "certificate.pem")
	privateKey := filepath.Join(root, "key.pem")
	certBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err = os.WriteFile(certificate, certBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(privateKey, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0600); err != nil {
		t.Fatal(err)
	}
	return certificate, privateKey, certBytes
}

func TestCertificatePairDomainExpiryAndPermissions(t *testing.T) {
	a := deploy.Application{Domains: []string{"web.example.invalid"}}
	certificate, key, _ := certificateFixture(t, a.Domains[0], false)
	certBytes, _ := os.ReadFile(certificate)
	keyBytes, _ := os.ReadFile(key)
	if _, err := validateCertificate(a, certBytes, keyBytes); err != nil {
		t.Fatal(err)
	}
	wrong := a
	wrong.Domains = []string{"other.example.invalid"}
	if _, err := validateCertificate(wrong, certBytes, keyBytes); err == nil {
		t.Fatal("certificate domain mismatch accepted")
	}
	expired, expiredKey, _ := certificateFixture(t, a.Domains[0], true)
	expiredBytes, _ := os.ReadFile(expired)
	expiredKeyBytes, _ := os.ReadFile(expiredKey)
	if _, err := validateCertificate(a, expiredBytes, expiredKeyBytes); err == nil {
		t.Fatal("expired certificate accepted")
	}
	if err := os.Chmod(key, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readCertificateFile(key, true); err == nil {
		t.Fatal("publicly readable private key accepted")
	}
}

func TestHTTPSImportRedirectAndRenewalFailurePreserveRoute(t *testing.T) {
	a, router := testProxyApplication(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "secure-app") }))
	defer backend.Close()
	candidate := deploy.Candidate{Endpoint: backend.URL}
	if _, err := router.Switch(context.Background(), a, candidate); err != nil {
		t.Fatal(err)
	}
	certificate, key, certBytes := certificateFixture(t, a.Domains[0], false)
	cfg := *a.Proxy
	cfg.HTTPSPort = freePort(t)
	a.Proxy = &cfg
	updated, err := NewTLS(router).Import(context.Background(), a, candidate, certificate, key)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(updated.Proxy.PrivateKey)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("imported key storage is not private", err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certBytes)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: a.Domains[0], MinVersion: tls.VersionTLS12}}, Timeout: time.Second}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for {
		request, _ := http.NewRequest(http.MethodGet, "https"+strings.TrimPrefix(BackendAddress("127.0.0.1", updated.Proxy.HTTPSPort), "http"), nil)
		request.Host = a.Domains[0]
		response, e := client.Do(request)
		if e == nil {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode == 200 && string(body) == "secure-app" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("HTTPS import never served the application", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
	redirect := updated
	redirectConfig := *updated.Proxy
	redirectConfig.RedirectHTTPS = true
	redirect.Proxy = &redirectConfig
	if _, err = router.Switch(context.Background(), redirect, candidate); err != nil {
		t.Fatal(err)
	}
	noRedirect := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer noRedirect.CloseIdleConnections()
	deadline = time.Now().Add(3 * time.Second)
	for {
		request, _ := http.NewRequest(http.MethodGet, BackendAddress("127.0.0.1", redirect.Proxy.HTTPPort), nil)
		request.Host = a.Domains[0]
		response, e := noRedirect.Do(request)
		if e == nil {
			response.Body.Close()
			if response.StatusCode == 308 && strings.HasPrefix(response.Header.Get("Location"), "https://"+a.Domains[0]+":") {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("HTTPS redirect not active", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
	manager := NewTLS(router)
	if _, err = manager.Renew(context.Background(), redirect, candidate); err == nil {
		t.Fatal("imported certificate silently accepted as ACME renewable")
	}
	status, err := manager.Inspect(redirect)
	if err != nil || status.ExpiresAt.Before(time.Now()) || status.Renewable {
		t.Fatal("incorrect certificate status", err)
	}
	if _, err = manager.Issue(context.Background(), redirect, candidate, "owner@example.invalid", true, false); err == nil {
		t.Fatal("ACME terms were implicitly accepted")
	}
}

func TestRenewalSchedulerRejectsInjectionAndUsesStableIdentity(t *testing.T) {
	a := deploy.Application{ID: strings.Repeat("a", 32), Name: "web", Image: "busybox", Profile: "production", Profiles: map[string]deploy.RuntimeConfig{"production": {}}}
	for _, connection := range []string{"host\nExecStart=evil", "host\x00bad"} {
		if _, _, err := RenewalUnits("/usr/bin/podman-tui", "/private/state", connection, a); err == nil {
			t.Fatal("unit injection accepted")
		}
	}
	if _, _, err := RenewalUnits("relative", "/private/state", "host", a); err == nil {
		t.Fatal("relative executable accepted")
	}
	before, err := RenewalCron("/usr/bin/podman-tui", "/private/state", "host", a)
	if err != nil {
		t.Fatal(err)
	}
	a.Name = "renamed"
	after, err := RenewalCron("/usr/bin/podman-tui", "/private/state", "host", a)
	if err != nil || before != after {
		t.Fatal("application rename broke renewal identity", err)
	}
}
