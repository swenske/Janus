// Package janus gives Go code the repository root's files a binary needs
// at run time: go:embed can't reach a parent directory.
package janus

import _ "embed"

// VariantsMk is variants.mk as this tree has it (internal/variants.Suspended).
//
//go:embed variants.mk
var VariantsMk []byte
