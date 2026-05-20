package poc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	p := New("rt-smoke")
	p.Metadata.Customer = "Acme"
	if err := p.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Metadata.Name != "rt-smoke" {
		t.Errorf("name: got %q", loaded.Metadata.Name)
	}
	if loaded.Metadata.Customer != "Acme" {
		t.Errorf("customer: got %q", loaded.Metadata.Customer)
	}
	if loaded.Cluster.Provider != "docker" {
		t.Errorf("provider: got %q", loaded.Cluster.Provider)
	}
	if loaded.Versions.KindNodeImage == "" {
		t.Errorf("kind_node_image dropped through roundtrip")
	}
}

func TestSaveWritesHeaderComment(t *testing.T) {
	dir := t.TempDir()
	p := New("hdr")
	if err := p.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasPrefix(string(body), "# kindbnkctl PoC declarative state") {
		t.Errorf("expected header comment at top; got first line: %q",
			strings.SplitN(string(body), "\n", 2)[0])
	}
}

func TestLoadStrictRejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	body := `apiVersion: kindbnkctl.f5.com/v1alpha1
kind: PoC
metadata:
  name: bad
  bnk_version: 2.3.0
cluster:
  name: bad
  provider: docker
  surprise: extra-field
versions:
  k8s: "1.30"
  kind_node_image: kindest/node:v1.30.8
  cne_manifest: x
networks:
  internal: {name: i, subnet: 10.0.0.0/24}
  external: {name: e, subnet: 10.0.1.0/24}
bnk: {far_key_ref: a, jwt_ref: b, demo_mode: true}
status: {cluster: pending, deploy: pending}
`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected strict-decode error for unknown field, got nil")
	}
	if !strings.Contains(err.Error(), "surprise") {
		t.Errorf("error should mention offending field; got: %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error loading from empty dir")
	}
	if !strings.Contains(err.Error(), "read") {
		t.Errorf("error message: got %v", err)
	}
}
