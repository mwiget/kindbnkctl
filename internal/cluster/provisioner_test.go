package cluster

import (
	"strings"
	"testing"
)

func TestNewProvisionerKind(t *testing.T) {
	p, err := NewProvisioner(BackendKind, RuntimeDocker, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Backend() != BackendKind || p.Tool() != "kind" {
		t.Fatalf("got backend %q tool %q", p.Backend(), p.Tool())
	}
	if got := p.WorkerNodeName("demo"); got != "demo-worker" {
		t.Errorf("kind worker node = %q, want demo-worker", got)
	}
	if got := p.NodeContainerLabel("demo"); got != "io.x-k8s.kind.cluster=demo" {
		t.Errorf("kind node label = %q", got)
	}
	if p.ConfigArtifact() != "kind.yaml" {
		t.Errorf("kind config artifact = %q", p.ConfigArtifact())
	}
}

func TestNewProvisionerK3d(t *testing.T) {
	p, err := NewProvisioner(BackendK3d, RuntimeDocker, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Backend() != BackendK3d || p.Tool() != "k3d" {
		t.Fatalf("got backend %q tool %q", p.Backend(), p.Tool())
	}
	if got := p.WorkerNodeName("demo"); got != "k3d-demo-agent-0" {
		t.Errorf("k3d worker node = %q, want k3d-demo-agent-0", got)
	}
	if got := p.NodeContainerLabel("demo"); got != "k3d.cluster=demo" {
		t.Errorf("k3d node label = %q", got)
	}
	if p.ConfigArtifact() != "k3d.yaml" {
		t.Errorf("k3d config artifact = %q", p.ConfigArtifact())
	}
	if p.DefaultNodeImage() == "" {
		t.Error("k3d default node image must be set")
	}
}

func TestNewProvisionerUnknown(t *testing.T) {
	if _, err := NewProvisioner(Backend("nope"), RuntimeDocker, nil); err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

func TestRenderConfigs(t *testing.T) {
	kind, err := NewProvisioner(BackendKind, RuntimeDocker, nil)
	if err != nil {
		t.Fatal(err)
	}
	kc, err := kind.RenderConfig("demo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(kc, "name: demo") || !strings.Contains(kc, "disableDefaultCNI: true") {
		t.Errorf("kind config missing expected content:\n%s", kc)
	}

	k3d, err := NewProvisioner(BackendK3d, RuntimeDocker, nil)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := k3d.RenderConfig("demo")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name: demo", "flannel-backend=none", "servers: 1", "agents: 1"} {
		if !strings.Contains(kd, want) {
			t.Errorf("k3d config missing %q:\n%s", want, kd)
		}
	}
}
