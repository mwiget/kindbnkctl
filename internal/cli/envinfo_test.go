package cli

import (
	"context"
	"strings"
	"testing"
)

func TestCollectHostInfo_PopulatesCompiledInFields(t *testing.T) {
	// The collector probes the host best-effort, but the four
	// compiled-in fields must always populate regardless of the
	// build environment (no docker, no kind, no /proc, etc.).
	e := collectHostInfo(context.Background())

	if e.OS == "" {
		t.Error("OS empty (runtime.GOOS should always be set)")
	}
	if e.Arch == "" {
		t.Error("Arch empty (runtime.GOARCH should always be set)")
	}
	if e.CPUCores < 1 {
		t.Errorf("CPUCores=%d, want >=1", e.CPUCores)
	}
	if !strings.HasPrefix(e.GoVer, "go") {
		t.Errorf("GoVer=%q, want go-version string", e.GoVer)
	}
	if e.KindBNKCtlVersion == "" {
		t.Error("KindBNKCtlVersion empty")
	}
	if e.BNKVersion == "" {
		t.Error("BNKVersion empty")
	}
	if e.CNEManifestVersion == "" {
		t.Error("CNEManifestVersion empty")
	}
}

func TestRenderEnvironment_AllFieldsPresent(t *testing.T) {
	e := &EnvInfo{
		OS:                 "linux",
		Arch:               "amd64",
		Kernel:             "6.8.0-117-generic",
		Hostname:           "test-host",
		CPUCores:           24,
		CPUModel:           "Intel(R) Test CPU",
		MemTotalKB:         32 * 1024 * 1024,
		DockerVer:          "27.5.1",
		KindVer:            "kind v0.27.0",
		KubectlVer:         "v1.31.4",
		GoVer:              "go1.23.4",
		KindBNKCtlVersion:  "dev",
		BNKVersion:         "2.3.0",
		CNEManifestVersion: "2.3.0-3.2598.3-0.0.170",
		K8sServerVersion:   "v1.30.8",
		KindClusterName:    "smoke",
	}
	md := renderEnvironment(e)
	for _, want := range []string{
		"## Environment",
		"### Versions",
		"### Host",
		"kindbnkctl", "BNK", "CNE manifest",
		"kind", "kubectl (client)", "Kubernetes (server)", "Docker", "Go (build)", "kind cluster",
		"linux/amd64",
		"6.8.0-117-generic",
		"32768 MiB", "32.00 GiB",
		"test-host",
		"24",
		"v0.27.0",
		"v1.31.4",
		"v1.30.8",
		"27.5.1",
		"smoke",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("renderEnvironment missing %q in output:\n%s", want, md)
		}
	}
}

func TestRenderEnvironment_MissingFieldsRenderAsDash(t *testing.T) {
	// Only compiled-in fields populated — host probes failed
	// (no /proc, no docker, no kind). Output should still render
	// without panic and show "—" for missing strings.
	e := &EnvInfo{
		OS:                 "darwin",
		Arch:               "arm64",
		CPUCores:           8,
		GoVer:              "go1.23.4",
		KindBNKCtlVersion:  "dev",
		BNKVersion:         "2.3.0",
		CNEManifestVersion: "2.3.0-3.2598.3-0.0.170",
		// Kernel, Hostname, CPUModel, MemTotalKB, Docker, kind,
		// kubectl, server version, cluster name all empty.
	}
	md := renderEnvironment(e)
	if !strings.Contains(md, "| Docker | — |") {
		t.Errorf("expected dash for missing Docker, got:\n%s", md)
	}
	if !strings.Contains(md, "| Hostname | — |") {
		t.Errorf("expected dash for missing Hostname")
	}
	// Conditional rows must NOT appear when their value is zero/empty.
	if strings.Contains(md, "| CPU model |") {
		t.Errorf("CPU model row should be omitted when empty")
	}
	if strings.Contains(md, "| Memory |") {
		t.Errorf("Memory row should be omitted when zero")
	}
	if strings.Contains(md, "| kind cluster |") {
		t.Errorf("kind cluster row should be omitted when empty")
	}
}

func TestFormatMemMiB(t *testing.T) {
	cases := []struct {
		kb   int64
		want string
	}{
		{0, ""},
		{1024, "1 MiB (0.00 GiB)"},
		{32 * 1024 * 1024, "32768 MiB (32.00 GiB)"},
	}
	for _, c := range cases {
		if got := formatMemMiB(c.kb); got != c.want {
			t.Errorf("formatMemMiB(%d)=%q, want %q", c.kb, got, c.want)
		}
	}
}

func TestSafeSlug(t *testing.T) {
	cases := map[string]string{
		"":               "poc",
		"smoke":          "smoke",
		"smoke-prod":     "smoke-prod",
		"with space":     "with-space",
		"weird/poc!name": "weird-poc-name",
	}
	for in, want := range cases {
		if got := safeSlug(in); got != want {
			t.Errorf("safeSlug(%q)=%q, want %q", in, got, want)
		}
	}
}
