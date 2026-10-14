package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/containers/podman-tui/deploy"
)

type TLS struct{ Router *Router }

func NewTLS(router *Router) *TLS { return &TLS{Router: router} }

type CertificateStatus struct {
	ExpiresAt   time.Time `json:"expires_at"`
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Staging     bool      `json:"staging"`
	Renewable   bool      `json:"renewable"`
}
type certificateRequest struct {
	Email       string
	Staging     bool
	Domains     []string
	Webroot     string
	RenewalHash string
	Client      string // absolute client path for minimal host-scheduler environments
}

func (t *TLS) directory(a deploy.Application) (string, error) {
	if t.Router == nil || !t.Router.Local {
		return "", errors.New("TLS operations require explicitly authorized local host integration")
	}
	if a.Proxy == nil {
		return "", errors.New("TLS requires a proxy configuration")
	}
	if err := a.Validate(); err != nil {
		return "", err
	}
	if err := safePath(a.Proxy.ManagedDir); err != nil {
		return "", err
	}
	if len(a.ID) != 32 {
		return "", errors.New("TLS requires a managed application identity")
	}
	root := filepath.Join(a.Proxy.ManagedDir, ".certificates", a.ID)
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	for p := root; p != a.Proxy.ManagedDir; p = filepath.Dir(p) {
		stat, err := os.Lstat(p)
		if err != nil || !stat.IsDir() || stat.Mode().Perm()&0077 != 0 {
			return "", errors.New("certificate directories must be private and non-symlink")
		}
	}
	return root, nil
}
func readCertificateFile(file string, key bool) ([]byte, error) {
	stat, err := os.Stat(file)
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > 4<<20 {
		return nil, errors.New("certificate/key must be a bounded regular file")
	}
	if key && stat.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private key permissions must be 0600 or stricter")
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4<<20+1))
	if len(data) > 4<<20 {
		return nil, errors.New("certificate/key exceeds size limit")
	}
	return data, err
}
func leafCertificate(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("invalid PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}
func validateCertificate(a deploy.Application, certificate, key []byte) (*x509.Certificate, error) {
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil {
		return nil, errors.New("certificate and private key do not form a valid pair")
	}
	leaf := pair.Leaf
	if leaf == nil {
		leaf, err = x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return nil, err
		}
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return nil, errors.New("certificate is expired or not yet valid")
	}
	for _, domain := range a.Domains {
		if err = leaf.VerifyHostname(domain); err != nil {
			return nil, errors.New("certificate does not cover every application domain")
		}
	}
	return leaf, nil
}
func readPrivateJSON(file string, value any) error {
	stat, err := os.Lstat(file)
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 || stat.Size() > 1<<20 {
		return errors.New("unsafe certificate state file")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}
func writeJSON(file string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return replace(file, data)
}
func (t *TLS) record(a deploy.Application, success bool, err error, expires time.Time, staging bool) error {
	root, e := t.directory(a)
	if e != nil {
		return e
	}
	var status CertificateStatus
	file := filepath.Join(root, "status.json")
	if e = readPrivateJSON(file, &status); e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	status.LastAttempt = time.Now().UTC()
	status.Staging = staging
	status.LastError = ""
	if success {
		status.LastSuccess = status.LastAttempt
		status.ExpiresAt = expires
	}
	if err != nil {
		status.LastError = deploy.Sanitize(err.Error())
	}
	return writeJSON(file, status)
}

// Import copies a verified pair into a versioned private directory and activates it transactionally.
// The caller persists the returned application only after success; the old version remains available.
func (t *TLS) Import(ctx context.Context, a deploy.Application, c deploy.Candidate, certPath, keyPath string) (deploy.Application, error) {
	root, err := t.directory(a)
	if err != nil {
		return a, err
	}
	cert, err := readCertificateFile(certPath, false)
	if err != nil {
		return a, err
	}
	key, err := readCertificateFile(keyPath, true)
	if err != nil {
		return a, err
	}
	leaf, err := validateCertificate(a, cert, key)
	if err != nil {
		return a, err
	}
	version, err := os.MkdirTemp(root, "version-")
	if err != nil {
		return a, err
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(version)
		}
	}()
	certificate := filepath.Join(version, "fullchain.pem")
	privateKey := filepath.Join(version, "privkey.pem")
	if err = replace(certificate, cert); err != nil {
		return a, err
	}
	if err = replace(privateKey, key); err != nil {
		return a, err
	}
	updated := a
	config := *a.Proxy
	config.Certificate = certificate
	config.PrivateKey = privateKey
	updated.Proxy = &config
	undo, err := t.Router.Switch(ctx, updated, c)
	if err != nil {
		return a, errors.Join(err, t.record(a, false, err, time.Time{}, false))
	}
	if err = t.record(updated, true, nil, leaf.NotAfter, false); err != nil {
		recovery, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if restoreErr := undo(recovery); restoreErr != nil {
			success = true // uncertain live route may still need these certificate files
			return updated, errors.Join(err, restoreErr, deploy.ErrRecoveryRequired)
		}
		return a, err
	}
	success = true
	return updated, nil
}

func renewalHash(root, id string) (string, error) {
	data, err := readCertificateFile(filepath.Join(root, "acme", "renewal", "ptui-"+id+".conf"), false)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
func (t *TLS) Issue(ctx context.Context, a deploy.Application, c deploy.Candidate, email string, staging, agree bool) (deploy.Application, error) {
	root, err := t.directory(a)
	if err != nil {
		return a, err
	}
	if len(a.Domains) == 0 {
		return a, errors.New("ACME requires configured application domains")
	}
	if !agree {
		return a, errors.New("ACME issuance requires explicit agreement to CA terms")
	}
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email || strings.ContainsAny(email, "\r\n") {
		return a, errors.New("provide a valid ACME account email")
	}
	request := certificateRequest{Email: email, Staging: staging, Domains: a.Domains, Webroot: a.Proxy.Webroot}
	var previous certificateRequest
	if err = readPrivateJSON(filepath.Join(root, "request.json"), &previous); err == nil {
		request.RenewalHash = previous.RenewalHash
	} else if !errors.Is(err, os.ErrNotExist) {
		return a, err
	} else if _, err = os.Lstat(filepath.Join(root, "acme", "renewal", "ptui-"+a.ID+".conf")); !errors.Is(err, os.ErrNotExist) {
		return a, errors.New("refusing an existing unregistered Certbot lineage")
	}
	return t.issue(ctx, a, c, request)
}
func (t *TLS) issue(ctx context.Context, a deploy.Application, c deploy.Candidate, request certificateRequest) (updated deploy.Application, retErr error) {
	updated = a
	root, err := t.directory(a)
	if err != nil {
		return a, err
	}
	if request.Webroot == "" {
		return a, errors.New("HTTP-01 requires an explicitly configured served webroot")
	}
	if err = safePath(request.Webroot); err != nil {
		return a, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, t.record(a, false, retErr, time.Time{}, request.Staging))
		}
	}()
	if request.RenewalHash != "" {
		current, err := renewalHash(root, a.ID)
		if err != nil || current != request.RenewalHash {
			return a, errors.New("managed Certbot renewal configuration changed; refusing possible hook injection")
		}
	}
	for _, dir := range []string{"acme", "work", "logs"} {
		p := filepath.Join(root, dir)
		if err = os.MkdirAll(p, 0700); err != nil {
			return a, err
		}
		stat, e := os.Lstat(p)
		if e != nil || !stat.IsDir() || stat.Mode().Perm()&0077 != 0 {
			return a, errors.New("unsafe Certbot directory")
		}
	}
	emptyConfig := filepath.Join(root, "client.ini")
	if err = replace(emptyConfig, []byte("# podman-tui managed Certbot options are passed explicitly\n")); err != nil {
		return a, err
	}
	args := []string{"certonly", "--config", emptyConfig, "--webroot", "--webroot-path", request.Webroot, "--cert-name", "ptui-" + a.ID, "--config-dir", filepath.Join(root, "acme"), "--work-dir", filepath.Join(root, "work"), "--logs-dir", filepath.Join(root, "logs"), "--non-interactive", "--agree-tos", "--email", request.Email, "--preferred-challenges", "http-01", "--no-directory-hooks", "--keep-until-expiring"}
	if request.Staging {
		args = append(args, "--server", "https://acme-staging-v02.api.letsencrypt.org/directory")
	}
	for _, domain := range request.Domains {
		if err = deploy.ValidateDomain(domain); err != nil {
			return a, err
		}
		args = append(args, "-d", domain)
	}
	if request.Client == "" {
		request.Client, err = exec.LookPath("certbot")
		if err != nil {
			return a, errors.New("install a maintained Certbot client before ACME issuance")
		}
		request.Client, err = filepath.Abs(request.Client)
		if err != nil {
			return a, err
		}
	}
	if err = safePath(request.Client); err != nil {
		return a, err
	}
	command := exec.CommandContext(ctx, request.Client, args...)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.WaitDelay = 2 * time.Second
	if err = command.Run(); err != nil {
		if ctx.Err() != nil {
			return a, ctx.Err()
		}
		return a, errors.New("Certbot failed; check client installation, DNS, public HTTP-01 reachability and private managed Certbot logs")
	}
	request.RenewalHash, err = renewalHash(root, a.ID)
	if err != nil {
		return a, err
	}
	if err = writeJSON(filepath.Join(root, "request.json"), request); err != nil {
		return a, err
	}
	live := filepath.Join(root, "acme", "live", "ptui-"+a.ID)
	updated, err = t.Import(ctx, a, c, filepath.Join(live, "fullchain.pem"), filepath.Join(live, "privkey.pem"))
	if err != nil {
		return a, err
	}
	leaf, err := leafCertificateMust(updated.Proxy.Certificate)
	if err != nil {
		return updated, err
	}
	if err = t.record(updated, true, nil, leaf.NotAfter, request.Staging); err != nil {
		return updated, err
	}
	return updated, nil
}
func leafCertificateMust(file string) (*x509.Certificate, error) {
	data, err := readCertificateFile(file, false)
	if err != nil {
		return nil, err
	}
	return leafCertificate(data)
}
func (t *TLS) Renew(ctx context.Context, a deploy.Application, c deploy.Candidate) (deploy.Application, error) {
	root, err := t.directory(a)
	if err != nil {
		return a, err
	}
	var request certificateRequest
	if err = readPrivateJSON(filepath.Join(root, "request.json"), &request); err != nil {
		return a, errors.New("application has no managed ACME request; imported certificates cannot be renewed automatically")
	}
	if strings.Join(request.Domains, "\x00") != strings.Join(a.Domains, "\x00") || request.Webroot != a.Proxy.Webroot {
		return a, errors.New("ACME domains/webroot changed; explicitly issue a new certificate")
	}
	return t.issue(ctx, a, c, request)
}
func (t *TLS) Inspect(a deploy.Application) (CertificateStatus, error) {
	root, err := t.directory(a)
	if err != nil {
		return CertificateStatus{}, err
	}
	var status CertificateStatus
	if err = readPrivateJSON(filepath.Join(root, "status.json"), &status); err != nil && !errors.Is(err, os.ErrNotExist) {
		return status, err
	}
	if a.Proxy.Certificate != "" {
		leaf, e := leafCertificateMust(a.Proxy.Certificate)
		if e != nil {
			return status, e
		}
		status.ExpiresAt = leaf.NotAfter
	}
	var request certificateRequest
	status.Renewable = readPrivateJSON(filepath.Join(root, "request.json"), &request) == nil
	return status, nil
}

// RenewalUnits returns a user systemd timer/service. Parent installs only on a supported local Linux host.
func RenewalUnits(executable, stateDir, connection string, a deploy.Application) (string, string, error) {
	argv, err := renewalArguments(executable, stateDir, connection, a)
	if err != nil {
		return "", "", err
	}
	quoted := make([]string, len(argv))
	for i, v := range argv {
		if strings.ContainsAny(v, "\x00\r\n") {
			return "", "", errors.New("invalid renewal unit argument")
		}
		quoted[i] = quote(strings.ReplaceAll(strings.ReplaceAll(v, "%", "%%"), "$", "$$"))
	}
	marker := "# managed by podman-tui TLS application=" + a.ID + "\n"
	service := marker + "[Unit]\nDescription=Podman-TUI certificate renewal\n[Service]\nType=oneshot\nExecStart=" + strings.Join(quoted, " ") + "\n"
	timer := marker + "[Unit]\nDescription=Podman-TUI certificate renewal\n[Timer]\nOnCalendar=daily\nRandomizedDelaySec=1h\nPersistent=true\n[Install]\nWantedBy=timers.target\n"
	return service, timer, nil
}
