// Package metadata holds the identity shared by the runtime and signed package.
package metadata

import (
	_ "embed"

	"github.com/feeeei/sub2api-plugin-framework/sdk"
)

// ManifestSource is the exact manifest used to build the release.
//
//go:embed manifest.source.json
var ManifestSource []byte

// Info is validated before the plugin begins serving.
var Info = sdk.MustInfoFromManifestSource(ManifestSource)
