// Package deploy manages connection-scoped application releases independently of the UI.
package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/distribution/reference"
)

const (
	OwnerLabel   = "io.podman-tui.managed"
	AppLabel     = "io.podman-tui.application"
	ReleaseLabel = "io.podman-tui.release"
	OwnerValue   = "deployment-v1"
)

var (
	ErrNotFound         = errors.New("application or release not found")
	ErrBusy             = errors.New("deployment operation is already active")
	ErrRecoveryRequired = errors.New("routing or runtime recovery is incomplete; resources preserved for recover-abort")
	sshRepoPattern      = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[A-Za-z0-9/_.-]+$`)
	namePattern         = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	envPattern          = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	domainLabelPattern  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

type GitConfig struct {
	URL      string `json:"url"`
	Revision string `json:"revision"`
	Trusted  bool   `json:"trusted"`
}

type Port struct {
	Container uint16 `json:"container"`
	Host      uint16 `json:"host,omitempty"`
	IP        string `json:"ip,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
}

type Volume struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

type SecretRef struct {
	Name   string `json:"name"`
	Target string `json:"target"`
	Type   string `json:"type"` // env or mount; never a secret value
}

type RuntimeConfig struct {
	Env            map[string]string `json:"env,omitempty"` // explicitly non-sensitive only
	Secrets        []SecretRef       `json:"secrets,omitempty"`
	Ports          []Port            `json:"ports,omitempty"`
	Volumes        []Volume          `json:"volumes,omitempty"`
	Networks       []string          `json:"networks,omitempty"`
	Command        []string          `json:"command,omitempty"`
	MemoryBytes    int64             `json:"memory_bytes,omitempty"`
	CPUQuota       int64             `json:"cpu_quota,omitempty"` // microseconds per 100000us period
	RestartPolicy  string            `json:"restart_policy,omitempty"`
	ConcurrentSafe bool              `json:"concurrent_safe,omitempty"` // authorizes overlapping volume writers
}

type Readiness struct {
	Mode           string `json:"mode"` // running, health or http
	Path           string `json:"path,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type ProxyConfig struct {
	ManagedDir    string `json:"managed_dir"`
	ConfigFile    string `json:"config_file"`
	Binary        string `json:"binary,omitempty"`
	BackendHost   string `json:"backend_host"` // address reachable by nginx AND controller
	ContainerPort uint16 `json:"container_port"`
	HTTPPort      uint16 `json:"http_port,omitempty"`
	HTTPSPort     uint16 `json:"https_port,omitempty"`
	Webroot       string `json:"webroot,omitempty"`
	Certificate   string `json:"certificate,omitempty"`
	PrivateKey    string `json:"private_key,omitempty"`
	RedirectHTTPS bool   `json:"redirect_https,omitempty"`
}

type Application struct {
	ID             string                   `json:"id"`
	Name           string                   `json:"name"`
	Kind           string                   `json:"kind,omitempty"` // application or service
	Image          string                   `json:"image,omitempty"`
	Repository     *GitConfig               `json:"repository,omitempty"`
	Profile        string                   `json:"profile"`
	Profiles       map[string]RuntimeConfig `json:"profiles"`
	Domains        []string                 `json:"domains,omitempty"`
	Proxy          *ProxyConfig             `json:"proxy,omitempty"`
	Readiness      Readiness                `json:"readiness"`
	Retention      int                      `json:"retention,omitempty"`
	CurrentRelease string                   `json:"current_release,omitempty"`
	CreatedAt      time.Time                `json:"created_at"`
}

type Release struct {
	ID            string      `json:"id"`
	ApplicationID string      `json:"application_id"`
	Timestamp     time.Time   `json:"timestamp"`
	Status        string      `json:"status"`
	Stage         string      `json:"stage"`
	Commit        string      `json:"commit,omitempty"`
	Repository    string      `json:"repository,omitempty"`
	ImageID       string      `json:"image_id,omitempty"`
	ContainerID   string      `json:"container_id,omitempty"`
	Endpoint      string      `json:"endpoint,omitempty"`
	Previous      string      `json:"previous,omitempty"`
	Health        string      `json:"health,omitempty"`
	Error         string      `json:"error,omitempty"`
	Snapshot      Application `json:"snapshot"`
}

type State struct {
	Version      int           `json:"version"`
	Applications []Application `json:"applications"`
	Releases     []Release     `json:"releases"`
}

type SourceTree struct{ Directory, Containerfile, Commit string }
type Candidate struct{ ID, Endpoint string }
type RuntimeStatus struct {
	Running bool
	Health  string
	ImageID string
}

// Runtime operates on one frozen Podman connection. Every container mutation must inspect ownership.
type Runtime interface {
	ResolveImage(context.Context, string, io.Writer) (string, error)
	Build(context.Context, SourceTree, string, io.Writer) (string, error)
	Create(context.Context, Application, Release) (Candidate, error)
	Inspect(context.Context, Application, string) (RuntimeStatus, error)
	Endpoint(context.Context, Application, string) (string, error)
	Find(context.Context, Application, string) (Candidate, error)
	Start(context.Context, Application, string) error
	Stop(context.Context, Application, string) error
	Remove(context.Context, Application, string) error
}

type Source interface {
	Retrieve(context.Context, Application, string, io.Writer) (SourceTree, error)
}

// Router activates a candidate and returns an undo operation. Failed activation must restore the old file.
type Router interface {
	Switch(context.Context, Application, Candidate) (func(context.Context) error, error)
	Remove(context.Context, Application) error
}

type Event struct{ Stage, Message string }
type Progress func(Event)

func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func ValidateName(s string) error {
	if !namePattern.MatchString(s) {
		return errors.New("name must start with a lowercase letter and contain at most 63 lowercase letters, digits or hyphens")
	}
	return nil
}

func ValidateDomain(s string) error {
	if len(s) > 253 || s != strings.ToLower(s) || !strings.Contains(s, ".") {
		return errors.New("domain must be a lowercase fully qualified DNS name")
	}
	for _, label := range strings.Split(s, ".") {
		if !domainLabelPattern.MatchString(label) {
			return errors.New("invalid domain label")
		}
	}
	if net.ParseIP(s) != nil {
		return errors.New("domain cannot be an IP address")
	}
	return nil
}

func ValidateGit(g GitConfig) error {
	if strings.ContainsAny(g.URL+g.Revision, "\x00\r\n") || strings.HasPrefix(g.Revision, "-") || g.Revision == "" {
		return errors.New("invalid repository or revision")
	}
	if strings.HasPrefix(g.URL, "-") {
		return errors.New("repository cannot be an option")
	}
	u, err := url.Parse(g.URL)
	if err == nil && (u.Scheme == "https" || u.Scheme == "ssh") && u.Host != "" {
		if u.RawQuery != "" || u.Fragment != "" {
			return errors.New("repository query strings and fragments are not supported")
		}
		if u.User != nil {
			if _, present := u.User.Password(); present || u.Scheme == "https" {
				return errors.New("use SSH or an existing credential helper, not embedded credentials")
			}
		}
		return nil
	}
	// Git's conventional user@host:path form. Do not accept local paths or arbitrary transports.
	if sshRepoPattern.MatchString(g.URL) {
		return nil
	}
	return errors.New("repository must use HTTPS or SSH")
}

func (a Application) Config() RuntimeConfig { return a.Profiles[a.Profile] }

func (a Application) Validate() error {
	if err := ValidateName(a.Name); err != nil {
		return err
	}
	if a.ID != "" {
		if b, err := hex.DecodeString(a.ID); err != nil || len(b) != 16 {
			return errors.New("invalid application identity")
		}
	}
	if a.Kind != "" && a.Kind != "application" && a.Kind != "service" {
		return errors.New("invalid application kind")
	}
	if (a.Image == "") == (a.Repository == nil) {
		return errors.New("configure exactly one image or Git repository")
	}
	if strings.ContainsAny(a.Image, "\x00\r\n ") || strings.HasPrefix(a.Image, "-") {
		return errors.New("invalid image reference")
	}
	if a.Image != "" {
		if _, err := reference.ParseAnyReference(a.Image); err != nil {
			return errors.New("image must be an OCI reference or immutable ID without embedded credentials")
		}
	}
	if a.Repository != nil {
		if err := ValidateGit(*a.Repository); err != nil {
			return err
		}
	}
	if a.Profile != "development" && a.Profile != "staging" && a.Profile != "production" {
		return errors.New("profile must be development, staging or production")
	}
	if _, ok := a.Profiles[a.Profile]; !ok {
		return errors.New("selected profile is not configured")
	}
	if a.Retention < 0 || a.Retention > 100 {
		return errors.New("retention must be between 0 and 100")
	}
	for profile, c := range a.Profiles {
		if profile != "development" && profile != "staging" && profile != "production" {
			return errors.New("invalid profile")
		}
		if c.MemoryBytes < 0 || c.CPUQuota < 0 {
			return errors.New("resource limits cannot be negative")
		}
		if c.RestartPolicy != "" && c.RestartPolicy != "no" && c.RestartPolicy != "always" && c.RestartPolicy != "on-failure" && c.RestartPolicy != "unless-stopped" {
			return errors.New("invalid restart policy")
		}
		for key, val := range c.Env {
			if !envPattern.MatchString(key) || strings.ContainsRune(val, 0) {
				return errors.New("invalid non-sensitive environment configuration")
			}
		}
		for _, s := range c.Secrets {
			if ValidateName(s.Name) != nil || (s.Type != "env" && s.Type != "mount") || (s.Type == "env" && !envPattern.MatchString(s.Target)) || (s.Type == "mount" && (!path.IsAbs(s.Target) || path.Clean(s.Target) != s.Target || strings.ContainsAny(s.Target, "\x00\r\n"))) {
				return errors.New("invalid secret reference")
			}
		}
		for _, p := range c.Ports {
			if p.Container == 0 || (p.IP != "" && net.ParseIP(p.IP) == nil) || (p.Protocol != "" && p.Protocol != "tcp" && p.Protocol != "udp") {
				return errors.New("invalid port mapping")
			}
		}
		for _, v := range c.Volumes {
			if v.Source == "" || strings.ContainsAny(v.Source+v.Target, "\x00\r\n:") || !path.IsAbs(v.Target) || path.Clean(v.Target) != v.Target {
				return errors.New("invalid volume mapping")
			}
			if !path.IsAbs(v.Source) && ValidateName(v.Source) != nil {
				return errors.New("invalid volume name")
			}
		}
		for _, n := range c.Networks {
			if ValidateName(n) != nil {
				return errors.New("invalid network name")
			}
		}
		for _, s := range c.Command {
			if strings.ContainsRune(s, 0) {
				return errors.New("invalid command argument")
			}
		}
	}
	seen := map[string]bool{}
	for _, d := range a.Domains {
		if err := ValidateDomain(d); err != nil {
			return err
		}
		if seen[d] {
			return errors.New("duplicate domain")
		}
		seen[d] = true
	}
	if len(a.Domains) > 0 && a.Proxy == nil {
		return errors.New("domains require an explicit local proxy configuration")
	}
	if a.Proxy != nil {
		p := a.Proxy
		if p.ManagedDir == "" || p.ConfigFile == "" || p.ContainerPort == 0 || p.BackendHost == "" {
			return errors.New("proxy requires managed directory, nginx config, backend host and container port")
		}
		if net.ParseIP(p.BackendHost) == nil && p.BackendHost != "localhost" {
			return errors.New("proxy backend host must be an IP address or localhost")
		}
		if p.RedirectHTTPS && (p.Certificate == "" || p.PrivateKey == "") {
			return errors.New("HTTPS redirect requires a certificate and key")
		}
		found := false
		for _, port := range a.Config().Ports {
			if port.Container == p.ContainerPort && port.Host == 0 && (port.Protocol == "" || port.Protocol == "tcp") {
				found = true
			}
		}
		if !found {
			return errors.New("proxy requires a dynamically allocated TCP host port")
		}
	}
	if a.Readiness.TimeoutSeconds < 0 || a.Readiness.TimeoutSeconds > 3600 {
		return errors.New("readiness timeout must be between 0 and 3600 seconds")
	}
	switch a.Readiness.Mode {
	case "", "running", "health":
	case "http":
		if a.Proxy == nil || !strings.HasPrefix(a.Readiness.Path, "/") || strings.ContainsAny(a.Readiness.Path, "\x00\r\n") {
			return errors.New("HTTP readiness requires a proxy endpoint and absolute path")
		}
	default:
		return fmt.Errorf("unsupported readiness mode %q", a.Readiness.Mode)
	}
	return nil
}
