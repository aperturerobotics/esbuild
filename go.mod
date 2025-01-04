module github.com/evanw/esbuild

// The upstream version supports Go 1.13.
//
// Given that that version is no longer updated by the Go developers, and that
// it's full of security vulnerabilities, we chose to support go 1.20 instead.
go 1.20

require golang.org/x/sys v0.29.0
