// Package git retrieves explicitly trusted repository revisions without hooks or checkout filters.
package git

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/containers/podman-tui/deploy"
)

type Source struct{}

func New() *Source { return &Source{} }

const maxSourceBytes = int64(512 << 20)
const maxSourceFiles = 100000

var commitPattern = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)

type limitedOutput struct{ data []byte }

func (b *limitedOutput) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > 65536 {
		return 0, errors.New("Git response exceeds output limit")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func arguments(hooks string, args ...string) []string {
	return append([]string{"-c", "core.hooksPath=" + hooks, "-c", "core.fsmonitor=false", "-c", "init.templateDir=" + hooks, "-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "protocol.ssh.allow=always", "-c", "submodule.recurse=false"}, args...)
}
func command(ctx context.Context, hooks string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", arguments(hooks, args...)...)
	cmd.WaitDelay = 2 * time.Second
	// Keep SSH agent and established credential helpers. Remove command-injection Git environment settings.
	env := make([]string, 0, len(os.Environ()))
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(key, "GIT_") {
			continue
		}
		env = append(env, value)
	}
	cmd.Env = append(env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=yes")
	cmd.Stderr = io.Discard
	return cmd
}
func run(ctx context.Context, hooks string, args ...string) (string, error) {
	cmd := command(ctx, hooks, args...)
	var out limitedOutput
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("Git operation failed; check revision, SSH known_hosts and configured credential helper")
	}
	return strings.TrimSpace(string(out.data)), nil
}

func (s *Source) Retrieve(ctx context.Context, a deploy.Application, workdir string, writer io.Writer) (deploy.SourceTree, error) {
	if a.Repository == nil {
		return deploy.SourceTree{}, errors.New("application has no repository")
	}
	if err := deploy.ValidateGit(*a.Repository); err != nil {
		return deploy.SourceTree{}, err
	}
	if !a.Repository.Trusted {
		return deploy.SourceTree{}, errors.New("building repository instructions requires explicit repository trust")
	}
	if err := ctx.Err(); err != nil {
		return deploy.SourceTree{}, err
	}
	info, err := os.Lstat(workdir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return deploy.SourceTree{}, errors.New("Git work directory must be private and non-symlink")
	}
	hooks := filepath.Join(workdir, "hooks-disabled")
	repo := filepath.Join(workdir, "repository.git")
	tree := filepath.Join(workdir, "build")
	for _, dir := range []string{hooks, tree} {
		if err = os.Mkdir(dir, 0700); err != nil {
			return deploy.SourceTree{}, err
		}
	}
	if _, err = run(ctx, hooks, "init", "--bare", repo); err != nil {
		return deploy.SourceTree{}, err
	}
	// git archive performs working-tree conversion, including smudge/process
	// filters. Override attributes at their highest precedence instead of
	// discarding the user's global SSH and credential-helper configuration.
	attributesDir := filepath.Join(repo, "info")
	if err = os.MkdirAll(attributesDir, 0700); err != nil {
		return deploy.SourceTree{}, err
	}
	if err = os.WriteFile(filepath.Join(attributesDir, "attributes"), []byte("* -filter -working-tree-encoding -ident -text -eol -export-ignore -export-subst\n"), 0600); err != nil {
		return deploy.SourceTree{}, err
	}
	if _, err = run(ctx, hooks, "-C", repo, "remote", "add", "origin", a.Repository.URL); err != nil {
		return deploy.SourceTree{}, err
	}
	if writer != nil {
		fmt.Fprintln(deploy.SafeWriter{Writer: writer}, "Retrieving configured Git revision (hooks and checkout filters disabled)")
	}
	if _, err = run(ctx, hooks, "-C", repo, "fetch", "--no-tags", "--depth=1", "origin", a.Repository.Revision); err != nil {
		return deploy.SourceTree{}, err
	}
	commit, err := run(ctx, hooks, "-C", repo, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return deploy.SourceTree{}, err
	}
	if !commitPattern.MatchString(commit) {
		return deploy.SourceTree{}, errors.New("Git returned an invalid commit identity")
	}
	cmd := command(ctx, hooks, "-C", repo, "archive", "--format=tar", commit)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return deploy.SourceTree{}, err
	}
	if err = cmd.Start(); err != nil {
		return deploy.SourceTree{}, err
	}
	extractErr := extract(ctx, pipe, tree)
	if extractErr != nil {
		cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if err = ctx.Err(); err != nil {
		return deploy.SourceTree{}, err
	}
	if extractErr != nil {
		return deploy.SourceTree{}, extractErr
	}
	if waitErr != nil {
		return deploy.SourceTree{}, errors.New("Git archive failed")
	}
	containerfile := ""
	for _, name := range []string{"Containerfile", "Dockerfile"} {
		file := filepath.Join(tree, name)
		stat, e := os.Lstat(file)
		if e == nil && stat.Mode().IsRegular() {
			containerfile = file
			break
		}
	}
	if containerfile == "" {
		return deploy.SourceTree{}, errors.New("repository has no regular root Containerfile or Dockerfile")
	}
	return deploy.SourceTree{Directory: tree, Containerfile: containerfile, Commit: commit}, nil
}

func extract(ctx context.Context, input io.Reader, directory string) error {
	reader := tar.NewReader(input)
	var bytes int64
	files := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if err == io.EOF {
			return filepath.WalkDir(directory, func(file string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if entry.IsDir() {
					// Normalize once after extraction, independently of the host
					// umask. The private workdir still protects host access, while
					// COPY must retain traversable directories for non-root users.
					return os.Chmod(file, 0755)
				}
				if entry.Type()&os.ModeSymlink == 0 {
					return nil
				}
				target, err := filepath.EvalSymlinks(file)
				if err != nil {
					return errors.New("Git archive contains an unresolved or cyclic symlink")
				}
				relative, err := filepath.Rel(directory, target)
				if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
					return errors.New("Git symlink resolves outside build context")
				}
				return nil
			})
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(header.Name, "/")
		if name == "" || name == "." {
			continue
		}
		if path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\x00\\") {
			return errors.New("unsafe Git archive path")
		}
		for remaining := name; remaining != ""; {
			component, rest, _ := strings.Cut(remaining, "/")
			remaining = rest
			if strings.EqualFold(component, ".git") {
				return errors.New("Git metadata is not allowed in build context")
			}
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue // Git's commit metadata, not a filesystem entry.
		}
		files++
		if files > maxSourceFiles || header.Size < 0 || header.Size > maxSourceBytes-bytes {
			return errors.New("Git source exceeds extraction limits")
		}
		bytes += header.Size
		dest := filepath.Join(directory, filepath.FromSlash(name))
		// Never write through symlink ancestors, even if archive entry ordering is malicious.
		for parent := filepath.Dir(dest); parent != directory; parent = filepath.Dir(parent) {
			stat, e := os.Lstat(parent)
			if e == nil && !stat.IsDir() {
				return errors.New("archive path crosses a symlink or non-directory")
			}
			if e != nil && !errors.Is(e, os.ErrNotExist) {
				return e
			}
		}
		if err = os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err = os.MkdirAll(dest, 0755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			mode := os.FileMode(0644) // private build root protects host access; COPY retains runtime-readable modes
			if header.Mode&0111 != 0 {
				mode = 0755
			}
			file, e := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if e != nil {
				return e
			}
			if e = file.Chmod(mode); e != nil {
				file.Close()
				return e
			}
			_, e = io.CopyN(file, reader, header.Size)
			closeErr := file.Close()
			if e != nil {
				return e
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			target := header.Linkname
			resolved := path.Clean(path.Join(path.Dir(name), target))
			if path.IsAbs(target) || resolved == ".." || strings.HasPrefix(resolved, "../") || strings.ContainsAny(target, "\x00\\") {
				return errors.New("Git symlink escapes build context")
			}
			if err = os.Symlink(filepath.FromSlash(target), dest); err != nil {
				return err
			}
		default:
			return errors.New("unsupported Git archive entry type")
		}
	}
}

var _ deploy.Source = (*Source)(nil)
