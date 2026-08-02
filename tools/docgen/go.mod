// Module docgen is a BUILD-TIME-ONLY generator that renders the markdown in
// docs/ to the static HTML committed under internal/docs/html/. It is a SEPARATE
// nested Go module ON PURPOSE: goldmark and bluemonday are its dependencies, and
// keeping them here means `go build ./...` in the parent module never sees them —
// they cannot enter the release binary's go.mod/go.sum or its dependency tree.
// Go excludes any subdirectory that has its own go.mod from the parent module's
// package set, so this isolation is structural, not conventional.
module github.com/tiermetric/tier/tools/docgen

go 1.26.5

require (
	github.com/microcosm-cc/bluemonday v1.0.27
	github.com/yuin/goldmark v1.8.4
)

require (
	github.com/aymerick/douceur v0.2.0 // indirect
	github.com/gorilla/css v1.0.1 // indirect
	golang.org/x/net v0.57.0 // indirect
)
