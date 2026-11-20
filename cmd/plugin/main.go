package main

import (
	"runtime/debug"

	metadata "sub2api-upstream-zstd"
	"sub2api-upstream-zstd/internal/plugin"

	"github.com/feeeei/sub2api-plugin-framework/sdk"
)

func main() {
	// This is a GC target, not a hard process memory limit.
	debug.SetMemoryLimit(192 << 20)
	p := plugin.New(metadata.Info)
	defer p.Close()
	sdk.Serve(p)
}
