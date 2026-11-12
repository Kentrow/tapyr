// Package providers embeds the built-in provider packages, one directory
// per package (see docs/spec/03-configuration.md, "Provider packages").
package providers

import "embed"

// FS holds the built-in packages and the categories of the provider catalog
// (categories.yaml). A new package directory must be added to the embed
// pattern below.
//
//go:embed aruba-central-classic cisco-thousandeyes example-sim juniper-mist meraki-dashboard teltonika-rms teltonika-rutos categories.yaml
var FS embed.FS
