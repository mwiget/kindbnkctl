package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mwiget/kindbnkctl/internal/poc"
)

// resolvePoCDir returns the PoC repo path. Empty input means "current
// working directory"; relative paths are resolved against cwd.
func resolvePoCDir(input string) (string, error) {
	if input == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return cwd, nil
	}
	return filepath.Abs(expandTilde(input))
}

func expandTilde(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

// requireTwoGates enforces the destructive-action pattern: --yolo plus
// a typo-guard flag (--confirm-cluster NAME) that must equal poc.metadata.name.
func requireTwoGates(yolo bool, confirmFlag, confirmVal, expected, action string) error {
	if !yolo {
		return fmt.Errorf("refusing %s without --yolo (DESTRUCTIVE; re-run with --yolo and %s %s)",
			action, confirmFlag, expected)
	}
	if confirmVal == "" {
		return fmt.Errorf("refusing %s: %s is required (must equal poc.yaml.metadata.name = %q)",
			action, confirmFlag, expected)
	}
	if confirmVal != expected {
		return fmt.Errorf("%s mismatch: got %q, expected %q (poc.yaml.metadata.name)",
			confirmFlag, confirmVal, expected)
	}
	return nil
}

// resolveRef returns an absolute path given a poc-relative ref.
func resolveRef(repo, ref string) string {
	if filepath.IsAbs(ref) {
		return ref
	}
	return filepath.Join(repo, ref)
}

// savePoC writes poc.yaml + emits a brief confirmation line.
func savePoC(repo string, p *poc.PoC, out io.Writer) error {
	if err := p.Save(repo); err != nil {
		return err
	}
	fmt.Fprintf(out, "      poc.yaml updated\n")
	return nil
}

// requireKubeconfig ensures artifacts/kubeconfig exists; returns its
// absolute path. The hint is included in the error so the operator
// sees what to do next.
func requireKubeconfig(repo, hint string) (string, error) {
	p := filepath.Join(repo, "artifacts", "kubeconfig")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("kubeconfig not found at %s — %s", p, hint)
	}
	return p, nil
}

// prefixWriter prepends a prefix to every line written through it.
// Used to indent kubectl/helm output under section headers.
type prefixWriter struct {
	w      io.Writer
	prefix string
	buf    bytes.Buffer
}

func (p prefixWriter) Write(b []byte) (int, error) {
	// We treat each input write as opaque; emit prefix at line starts.
	// (Simple line buffer would help with trailing partials, but the
	// commands we wrap always print full lines.)
	lines := strings.SplitAfter(string(b), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		if _, err := io.WriteString(p.w, p.prefix+line); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

// readJWT returns the JWT body as a single-line string (trimmed).
func readJWT(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// appendJournal opens journal/<date>-<phase>.md (append) and writes
// a header. Returns the file for the caller to add details to.
func appendJournal(repo, phase, title string) (*os.File, error) {
	dir := filepath.Join(repo, "journal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	stamp := time.Now().UTC().Format("2006-01-02")
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.md", stamp, phase))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(f, "\n# %s — %s\n\n", time.Now().UTC().Format(time.RFC3339), title)
	return f, nil
}

// ErrAborted is returned when an operator confirms negatively at a
// prompt (e.g. a "destroy data?" gate).
var ErrAborted = errors.New("aborted")
