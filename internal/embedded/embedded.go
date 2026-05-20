// Package embedded ships static assets inside the kindbnkctl binary.
//
//   - files/      → copied into a fresh PoC repo by `kindbnkctl init`
//                  (AGENTS.md, CLAUDE.md, .gitignore template).
//   - templates/  → binary-internal — kind cluster config, CNEInstance,
//                  FLO values, License CR. NOT copied to PoC repos.
package embedded

import "embed"

//go:embed files
var FS embed.FS

//go:embed templates
var Templates embed.FS
