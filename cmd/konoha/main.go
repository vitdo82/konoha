// Command konoha is the worker + debug-CLI entrypoint for the issue lifecycle
// pipeline. All logic lives in internal/konoha; this file only bootstraps it
// (Main() owns process exit semantics via log.Fatal / os.Exit).
package main

import "prreview/internal/konoha"

func main() {
	konoha.Main()
}
