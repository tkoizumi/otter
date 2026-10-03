package api

import (
	"github.com/tkoizumi/otter/sdk"
)

// SchemaVersion is the version of the JSON shapes the HTTP API returns and the
// CLI's `--json` output prints. It is independent of the product version: it
// changes only when a frozen shape changes, and
// [compatibility.md](../../docs/compatibility.md) makes that a deliberate act
// rather than an accident of refactoring.
//
// Adding an optional field does not change this. Removing one, renaming one,
// changing a type, or changing what a value means does.
const SchemaVersion = 1

// ContractVersion is the version of docs/runtime-contract.md, the document that
// states what the runtime promises and does not promise. It is incremented by
// the rules at the top of that document, independently of the product version.
//
// Version 2 is the v0.4.0 contract: first-class schedules that fire once per
// occurrence, pinned job configuration, and the honest-limits section.
const ContractVersion = 2

// ManifestSchemaVersion is the only `version:` value a manifest may carry. A
// contract test pins it to config.SupportedVersion, so the API package does not
// depend on the manifest parser.
const ManifestSchemaVersion = 1

// SupportedPlatforms is the OS/architecture contract: the static targets Otter
// ships and guarantees. Everything else compiles at best and carries no
// guarantee. It is pinned to runtime-contract.md §6 by a contract test.
var SupportedPlatforms = []string{
	"linux/amd64",
	"linux/arm64",
	"darwin/amd64",
	"darwin/arm64",
}

// VersionDocument is the machine-readable description of the frozen interfaces:
// the one document a script or a control plane reads to learn what it is talking
// to. Field names and meaning are themselves part of the frozen schema.
type VersionDocument struct {
	SchemaVersion      int      `json:"schema_version"`
	ContractVersion    int      `json:"contract_version"`
	ProductVersion     string   `json:"product_version"`
	ManifestSchema     int      `json:"manifest_schema"`
	SDKVersion         string   `json:"sdk_version"`
	SupportedPlatforms []string `json:"supported_platforms"`
}

// VersionDocumentFor builds the version document for a running build.
func VersionDocumentFor(productVersion string) VersionDocument {
	platforms := make([]string, len(SupportedPlatforms))
	copy(platforms, SupportedPlatforms)
	return VersionDocument{
		SchemaVersion:      SchemaVersion,
		ContractVersion:    ContractVersion,
		ProductVersion:     productVersion,
		ManifestSchema:     ManifestSchemaVersion,
		SDKVersion:         sdk.Version,
		SupportedPlatforms: platforms,
	}
}
