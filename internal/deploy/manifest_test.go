package deploy

import "testing"

// Fixture mirrors the shape F5 ships. Trimmed for brevity but the
// shape is preserved so any change to the parser surfaces here first.
const fixture23 = `f5_helm_repo: oci://repo.f5.com
f5_docker_repo: repo.f5.com
releases:
  - version: 2.3.0-3.2598.3-0.0.170
    helm_charts:
      - name: charts/f5-lifecycle-operator
        version: v2.10.5-0.1.7
      - name: utils/f5-cert-gen
        version: 0.9.3
      - name: charts/cwc
        version: 0.49.7-0.0.16
    docker_images:
      - name: images/cert-manager-controller
        version: v2.5.2
      - name: images/f5-lifecycle-operator
        version: v2.10.5-0.1.7
`

func TestParseReleaseManifest_HappyPath(t *testing.T) {
	m, err := ParseReleaseManifest([]byte(fixture23))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Version != "2.3.0-3.2598.3-0.0.170" {
		t.Errorf("version: got %q", m.Version)
	}
	if m.HelmRepo != "oci://repo.f5.com" {
		t.Errorf("helmRepo: got %q", m.HelmRepo)
	}
	if got := m.Chart("charts/f5-lifecycle-operator"); got != "v2.10.5-0.1.7" {
		t.Errorf("FLO chart version: got %q", got)
	}
	if got := m.Chart("utils/f5-cert-gen"); got != "0.9.3" {
		t.Errorf("cert-gen chart version: got %q", got)
	}
	if got := m.Image("images/f5-lifecycle-operator"); got != "v2.10.5-0.1.7" {
		t.Errorf("FLO image version: got %q", got)
	}
	if got := m.Chart("does/not/exist"); got != "" {
		t.Errorf("unknown chart should be empty, got %q", got)
	}
}

func TestParseReleaseManifest_EmptyReleases(t *testing.T) {
	_, err := ParseReleaseManifest([]byte(`f5_helm_repo: oci://repo.f5.com
releases: []
`))
	if err == nil {
		t.Fatal("expected error for empty releases")
	}
}

func TestParseReleaseManifest_MissingVersion(t *testing.T) {
	_, err := ParseReleaseManifest([]byte(`f5_helm_repo: oci://repo.f5.com
releases:
  - helm_charts: []
`))
	if err == nil {
		t.Fatal("expected error for missing release version")
	}
}

func TestParseReleaseManifest_GarbageYAML(t *testing.T) {
	_, err := ParseReleaseManifest([]byte("not valid yaml: : :"))
	if err == nil {
		t.Fatal("expected parse error")
	}
}
