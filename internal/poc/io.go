package poc

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mwiget/kindbnkctl/internal/version"
)

// New returns a PoC populated with binary-pinned defaults. Caller fills
// in Metadata.Name and any per-customer specifics before writing.
func New(name string) *PoC {
	return &PoC{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata: Metadata{
			Name:              name,
			Created:           time.Now().UTC(),
			KindbnkctlVersion: version.Version,
			BNKVersion:        version.BNKVersion,
		},
		Versions: Versions{
			K8s:           version.K8sVersion,
			KindNodeImage: version.K8sNodeImage,
			CNEManifest:   version.CNEManifestVersion,
			// FLOChart resolved at deploy time.
		},
		Cluster: Cluster{
			Name:     name,
			Provider: "docker",
		},
		// Networks: left empty by default. Earlier versions
		// preallocated bnk-internal / bnk-external docker bridges and
		// attached them to the kind nodes as "scenery for external
		// test clients", but no scenario actually consumed them — the
		// Gateway IP pool (203.0.113.0/24) is plumbed via the bnk-bgp
		// Multus NAD bridge that scenarios create on demand. The
		// schema fields stay for backward compat with existing PoCs;
		// `cluster up` no longer creates them.
		Networks: Networks{},
		BNK: BNK{
			FARKeyRef: "keys/f5-far-auth-key.tgz",
			JWTRef:    "keys/.jwt",
			DemoMode:  true,
		},
		BNKForge: BNKForge{
			Enabled:  false,
			RepoPath: "~/git/bnk-forge",
			URL:      "https://localhost",
		},
		Status: Status{
			Cluster: "pending",
			Deploy:  "pending",
		},
	}
}

// Load reads poc.yaml from dir with strict (KnownFields) decoding so a
// typo doesn't silently drop a field.
func Load(dir string) (*PoC, error) {
	path := filepath.Join(dir, FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var p PoC
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &p, nil
}

func (p *PoC) Save(dir string) error {
	path := filepath.Join(dir, FileName)
	data, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal poc: %w", err)
	}
	header := []byte("# kindbnkctl PoC declarative state — source of truth for this PoC.\n" +
		"# All inputs needed to teardown and redeploy live here.\n\n")
	return os.WriteFile(path, append(header, data...), 0o644)
}
