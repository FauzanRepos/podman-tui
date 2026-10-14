// Package proxy manages an explicitly included Nginx directory; it never edits the main config.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/containers/podman-tui/deploy"
)

type Router struct{ Local bool }

func New(local bool) *Router { return &Router{Local: local} }

const marker = "# managed by podman-tui deployment-v1 application="

var serverNames = regexp.MustCompile(`(?m)\bserver_name\s+([^;]+);`)

type commandOutput struct{ data []byte }

func (o *commandOutput) Write(p []byte) (int, error) {
	if len(o.data)+len(p) > 1<<20 {
		return 0, errors.New("nginx output exceeds limit")
	}
	o.data = append(o.data, p...)
	return len(p), nil
}
func execute(ctx context.Context, p *deploy.ProxyConfig, args ...string) (string, error) {
	binary := p.Binary
	if binary == "" {
		binary = "nginx"
	}
	cmd := exec.CommandContext(ctx, binary, append(args, "-c", p.ConfigFile)...)
	cmd.WaitDelay = 2 * time.Second
	var output commandOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("nginx command failed: %w: %s", err, deploy.Sanitize(string(output.data)))
	}
	return string(output.data), nil
}

func safePath(p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, "\x00\r\n$;") {
		return errors.New("nginx paths must be clean absolute paths without control characters, variables or semicolons")
	}
	return nil
}
func quote(s string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"` }

func config(a deploy.Application, c deploy.Candidate) ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	if a.Proxy == nil || len(a.Domains) == 0 {
		return nil, errors.New("routing requires domains and explicit proxy configuration")
	}
	p := a.Proxy
	for _, value := range []string{p.ManagedDir, p.ConfigFile} {
		if err := safePath(value); err != nil {
			return nil, err
		}
	}
	for _, value := range []string{p.Certificate, p.PrivateKey, p.Webroot} {
		if value != "" {
			if err := safePath(value); err != nil {
				return nil, err
			}
		}
	}
	if (p.Certificate == "") != (p.PrivateKey == "") {
		return nil, errors.New("certificate and private key must be configured together")
	}
	endpoint, err := url.Parse(c.Endpoint)
	if err != nil {
		return nil, err
	}
	port, e := strconv.ParseUint(endpoint.Port(), 10, 16)
	if endpoint.Scheme != "http" || endpoint.Hostname() != p.BackendHost || e != nil || port == 0 || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "" {
		return nil, errors.New("proxy endpoint must be the configured host's published HTTP port")
	}
	httpPort := p.HTTPPort
	if httpPort == 0 {
		httpPort = 80
	}
	httpsPort := p.HTTPSPort
	if httpsPort == 0 {
		httpsPort = 443
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s\n", marker, a.ID)
	names := strings.Join(a.Domains, " ")
	challenge := ""
	if p.Webroot != "" {
		challenge = "    location ^~ /.well-known/acme-challenge/ { root " + quote(p.Webroot) + "; try_files $uri =404; }\n"
	}
	location := "    location / {\n        proxy_pass " + c.Endpoint + ";\n        proxy_http_version 1.1;\n        proxy_set_header Host $host;\n        proxy_set_header X-Real-IP $remote_addr;\n        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n        proxy_set_header X-Forwarded-Proto $scheme;\n        proxy_set_header Upgrade $http_upgrade;\n        proxy_set_header Connection \"upgrade\";\n    }\n"
	fmt.Fprintf(&b, "server {\n    listen %d;\n    server_name %s;\n%s", httpPort, names, challenge)
	if p.RedirectHTTPS {
		authority := "$host"
		if httpsPort != 443 {
			authority += " :" + strconv.Itoa(int(httpsPort))
			authority = strings.ReplaceAll(authority, " ", "")
		}
		fmt.Fprintf(&b, "    location / { return 308 https://%s$request_uri; }\n", authority)
	} else {
		b.WriteString(location)
	}
	b.WriteString("}\n")
	if p.Certificate != "" {
		fmt.Fprintf(&b, "server {\n    listen %d ssl;\n    server_name %s;\n    ssl_certificate %s;\n    ssl_certificate_key %s;\n    ssl_protocols TLSv1.2 TLSv1.3;\n%s%s}\n", httpsPort, names, quote(p.Certificate), quote(p.PrivateKey), challenge, location)
	}
	return []byte(b.String()), nil
}

func readOwned(file, id string) ([]byte, error) {
	stat, err := os.Lstat(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > 1<<20 {
		return nil, errors.New("managed nginx target is not a regular bounded file")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(string(data), marker+id+"\n") {
		return nil, errors.New("refusing to overwrite unmanaged nginx configuration")
	}
	return data, nil
}
func replace(file string, data []byte) error {
	if data == nil {
		err := os.Remove(file)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(file), ".nginx-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err = temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, file)
}
func matchesDomain(pattern, domain string) bool {
	pattern = strings.Trim(pattern, `"'`)
	if pattern == domain {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		return strings.HasSuffix(domain, pattern[1:])
	}
	if strings.HasPrefix(pattern, ".") {
		return domain == pattern[1:] || strings.HasSuffix(domain, pattern)
	}
	if strings.HasSuffix(pattern, ".*") {
		return strings.HasPrefix(domain, strings.TrimSuffix(pattern, "*"))
	}
	return false
}
func conflicts(text, file string, domains []string) error {
	// nginx -T identifies each included file. Ignore only this application's exact file.
	sections := strings.Split(text, "# configuration file ")
	for _, section := range sections {
		if strings.HasPrefix(section, file+":\n") {
			continue
		}
		for _, match := range serverNames.FindAllStringSubmatch(section, -1) {
			for _, name := range strings.Fields(match[1]) {
				for _, domain := range domains {
					if matchesDomain(name, domain) {
						return fmt.Errorf("domain %s conflicts with an existing nginx server_name", domain)
					}
					if strings.HasPrefix(name, "~") {
						return errors.New("external regex server_name requires manual conflict resolution")
					}
				}
			}
		}
	}
	return nil
}

func (r *Router) lock(a deploy.Application) (*deploy.Session, string, error) {
	if !r.Local {
		return nil, "", errors.New("host nginx operations require a local Unix Podman connection on this host; run the CLI on the server")
	}
	if a.Proxy == nil {
		return nil, "", errors.New("application has no proxy configuration")
	}
	if err := safePath(a.Proxy.ManagedDir); err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(a.Proxy.ManagedDir, 0700); err != nil {
		return nil, "", err
	}
	stat, err := os.Lstat(a.Proxy.ManagedDir)
	if err != nil || !stat.IsDir() {
		return nil, "", errors.New("managed nginx directory must not be a symlink")
	}
	lock, err := deploy.NewStore(filepath.Join(a.Proxy.ManagedDir, ".podman-tui-lock"), "nginx")
	if err != nil {
		return nil, "", err
	}
	session, err := lock.Begin()
	return session, filepath.Join(a.Proxy.ManagedDir, "ptui-"+a.ID+".conf"), err
}
func activate(ctx context.Context, p *deploy.ProxyConfig) error {
	if _, err := execute(ctx, p, "-t"); err != nil {
		return err
	}
	_, err := execute(ctx, p, "-s", "reload")
	return err
}
func rollback(p *deploy.ProxyConfig, file string, old []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := replace(file, old); err != nil {
		return err
	}
	return activate(ctx, p)
}
func (r *Router) Switch(ctx context.Context, a deploy.Application, c deploy.Candidate) (func(context.Context) error, error) {
	data, err := config(a, c)
	if err != nil {
		return nil, err
	}
	session, file, err := r.lock(a)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	old, err := readOwned(file, a.ID)
	if err != nil {
		return nil, err
	}
	full, err := execute(ctx, a.Proxy, "-T")
	if err != nil {
		return nil, err
	}
	if err = conflicts(full, file, a.Domains); err != nil {
		return nil, err
	}
	if err = replace(file, data); err != nil {
		return nil, err
	}
	if err = activate(ctx, a.Proxy); err != nil {
		restoreErr := rollback(a.Proxy, file, old)
		if restoreErr != nil {
			return nil, errors.Join(err, restoreErr, deploy.ErrRecoveryRequired)
		}
		return nil, err
	}
	undo := func(undoCtx context.Context) error {
		lock, file, e := r.lock(a)
		if e != nil {
			return e
		}
		defer lock.Close()
		current, e := readOwned(file, a.ID)
		if e != nil {
			return e
		}
		if string(current) != string(data) {
			return errors.New("routing changed since switch; refusing stale rollback")
		}
		if e = replace(file, old); e != nil {
			return e
		}
		return activate(undoCtx, a.Proxy)
	}
	return undo, nil
}
func (r *Router) Remove(ctx context.Context, a deploy.Application) error {
	session, file, err := r.lock(a)
	if err != nil {
		return err
	}
	defer session.Close()
	old, err := readOwned(file, a.ID)
	if err != nil || old == nil {
		return err
	}
	if err = replace(file, nil); err != nil {
		return err
	}
	if err = activate(ctx, a.Proxy); err != nil {
		restoreErr := rollback(a.Proxy, file, old)
		if restoreErr != nil {
			return errors.Join(err, restoreErr, deploy.ErrRecoveryRequired)
		}
		return err
	}
	return nil
}

func BackendAddress(host string, port uint16) string {
	return "http://" + net.JoinHostPort(host, strconv.Itoa(int(port)))
}

var _ deploy.Router = (*Router)(nil)
