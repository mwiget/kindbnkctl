package poc

import (
	"strings"
	"testing"
	"time"

	"github.com/mwiget/kindbnkctl/internal/version"
)

// goodPoC returns a PoC that passes Validate clean. Tests then mutate
// one thing at a time to assert each rule fires individually. Same
// pattern as dpubnkctl/internal/poc/validate_test.go.
func goodPoC(t *testing.T) *PoC {
	t.Helper()
	p := New("smoke")
	// New() leaves Created at "now" — pin it for stable test output.
	p.Metadata.Created = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if r := p.Validate(); !r.Valid() {
		t.Fatalf("baseline goodPoC should be valid, got errors: %v", r.Errors)
	}
	return p
}

func TestValidateGood(t *testing.T) {
	p := goodPoC(t)
	r := p.Validate()
	if !r.Valid() {
		t.Fatalf("good PoC failed: %v", r.Errors)
	}
	if len(r.Warnings) != 0 {
		t.Fatalf("good PoC had warnings: %v", r.Warnings)
	}
}

func TestValidateRules(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*PoC)
		want   string // substring of one expected error line
	}{
		{"missing apiVersion",
			func(p *PoC) { p.APIVersion = "" },
			"apiVersion"},
		{"wrong apiVersion",
			func(p *PoC) { p.APIVersion = "dpubnkctl.f5.com/v1alpha1" },
			"apiVersion"},
		{"wrong kind",
			func(p *PoC) { p.Kind = "NotPoC" },
			"kind"},
		{"missing metadata.name",
			func(p *PoC) { p.Metadata.Name = "" },
			"metadata.name: required"},
		{"bad metadata.name with caps",
			func(p *PoC) { p.Metadata.Name = "ABC" },
			"must match"},
		{"bad metadata.name starts hyphen",
			func(p *PoC) { p.Metadata.Name = "-foo" },
			"must match"},
		{"missing bnk_version",
			func(p *PoC) { p.Metadata.BNKVersion = "" },
			"metadata.bnk_version: required"},
		{"missing cluster.name",
			func(p *PoC) { p.Cluster.Name = "" },
			"cluster.name: required"},
		{"missing cluster.provider",
			func(p *PoC) { p.Cluster.Provider = "" },
			"cluster.provider: required"},
		{"bad cluster.provider",
			func(p *PoC) { p.Cluster.Provider = "containerd" },
			"must be docker or podman"},
		{"missing versions.k8s",
			func(p *PoC) { p.Versions.K8s = "" },
			"versions.k8s"},
		{"missing kind_node_image",
			func(p *PoC) { p.Versions.KindNodeImage = "" },
			"kind_node_image"},
		{"missing cne_manifest",
			func(p *PoC) { p.Versions.CNEManifest = "" },
			"cne_manifest"},
		{"missing networks.internal.name",
			func(p *PoC) { p.Networks.Internal.Name = "" },
			"networks.internal.name"},
		{"missing networks.internal.subnet",
			func(p *PoC) { p.Networks.Internal.Subnet = "" },
			"networks.internal.subnet"},
		{"bad networks.internal.subnet",
			func(p *PoC) { p.Networks.Internal.Subnet = "not-a-cidr" },
			"networks.internal.subnet"},
		{"missing networks.external.name",
			func(p *PoC) { p.Networks.External.Name = "" },
			"networks.external.name"},
		{"duplicate network names",
			func(p *PoC) { p.Networks.External.Name = p.Networks.Internal.Name },
			"must differ"},
		{"missing bnk.far_key_ref",
			func(p *PoC) { p.BNK.FARKeyRef = "" },
			"bnk.far_key_ref"},
		{"missing bnk.jwt_ref",
			func(p *PoC) { p.BNK.JWTRef = "" },
			"bnk.jwt_ref"},
		{"bnk_forge bad url",
			func(p *PoC) {
				p.BNKForge.Enabled = true
				p.BNKForge.URL = "ftp://no"
			},
			"bnk_forge.url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := goodPoC(t)
			tc.mutate(p)
			r := p.Validate()
			if r.Valid() {
				t.Fatalf("expected error containing %q, got clean", tc.want)
			}
			joined := strings.Join(r.Errors, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("error %q not found in:\n%s", tc.want, joined)
			}
		})
	}
}

func TestValidateDemoModeOffEmitsWarning(t *testing.T) {
	p := goodPoC(t)
	p.BNK.DemoMode = false
	r := p.Validate()
	if !r.Valid() {
		t.Fatalf("demo_mode=false should be valid (just a warning), got errors: %v", r.Errors)
	}
	joined := strings.Join(r.Warnings, "\n")
	if !strings.Contains(joined, "demo_mode") {
		t.Fatalf("expected demo_mode warning, got: %v", r.Warnings)
	}
}

func TestNewPopulatesBinaryPins(t *testing.T) {
	p := New("demo")
	if p.Metadata.Name != "demo" {
		t.Errorf("name: got %q want demo", p.Metadata.Name)
	}
	if p.Metadata.BNKVersion != version.BNKVersion {
		t.Errorf("bnk_version: got %q want %q", p.Metadata.BNKVersion, version.BNKVersion)
	}
	if p.Versions.KindNodeImage != version.K8sNodeImage {
		t.Errorf("kind_node_image: got %q want %q", p.Versions.KindNodeImage, version.K8sNodeImage)
	}
	if p.Versions.CNEManifest != version.CNEManifestVersion {
		t.Errorf("cne_manifest: got %q want %q", p.Versions.CNEManifest, version.CNEManifestVersion)
	}
	if !p.BNK.DemoMode {
		t.Errorf("demo_mode: want true (kind requires demo TMM)")
	}
	if p.Cluster.Provider != "docker" {
		t.Errorf("cluster.provider: got %q want docker", p.Cluster.Provider)
	}
}

func TestTMMLabelDefault(t *testing.T) {
	var b BNK
	k, v := b.TMMLabel()
	if k != "app" || v != "f5-tmm" {
		t.Errorf("default TMM label: got %s=%s want app=f5-tmm", k, v)
	}
}

func TestTMMLabelOverride(t *testing.T) {
	b := BNK{TMMNodeLabelKey: "role", TMMNodeLabelValue: "edge"}
	k, v := b.TMMLabel()
	if k != "role" || v != "edge" {
		t.Errorf("override TMM label: got %s=%s want role=edge", k, v)
	}
}
