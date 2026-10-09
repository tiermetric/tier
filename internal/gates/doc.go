// Package gates holds executable assertions about this repository's LOCAL gate
// wiring — the Makefile targets and scripts/ helpers that stand in for the CI
// this project deliberately does not have.
//
// It contains no production code. It exists because those gates have no other
// home in `go test ./...`, and a gate whose failure path is never exercised is
// this repository's recurring defect class rather than its fix: a `--selftest`
// wired into no make target is worthless, and a guard nothing can prove still
// fires is indistinguishable from a guard that was quietly deleted. Tests here
// run in the ordinary `make check` sweep, so removing a guard breaks a NAMED
// test rather than passing silently.
package gates
