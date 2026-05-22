package bnkforge

import "testing"

func TestKubeconfigAPIServer(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{
			name: "standard kind kubeconfig",
			body: `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: LS0tLS1CRUdJTg==
    server: https://127.0.0.1:43601
  name: kind-smoke
contexts:
- context:
    cluster: kind-smoke
    user: kind-smoke
  name: kind-smoke
current-context: kind-smoke
kind: Config
`,
			want: "https://127.0.0.1:43601",
		},
		{
			name: "trims whitespace around URL",
			body: `apiVersion: v1
clusters:
- cluster:
    server: "  https://10.0.0.1:6443  "
  name: foo
`,
			want: "https://10.0.0.1:6443",
		},
		{
			name: "no clusters returns empty + no error",
			body: `apiVersion: v1
kind: Config
clusters: []
`,
			want: "",
		},
		{
			name:    "malformed yaml errors",
			body:    "this is: : not valid: yaml: :\n  - x",
			wantErr: true,
		},
		{
			// Multi-cluster kubeconfig (kubectl config view --merge style):
			// we take the FIRST entry. bnk-forge's stored cluster row
			// represents one kind cluster anyway, so first-entry is fine.
			name: "multi-cluster picks first",
			body: `apiVersion: v1
clusters:
- cluster:
    server: https://first.example:6443
  name: first
- cluster:
    server: https://second.example:6443
  name: second
`,
			want: "https://first.example:6443",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := KubeconfigAPIServer([]byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("KubeconfigAPIServer = %q, want %q", got, tc.want)
			}
		})
	}
}
