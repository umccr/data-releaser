// Package integration contains end-to-end lifecycle tests that exercise the
// Data Releaser against real service emulators (DynamoDB Local and an S3
// emulator) rather than in-memory mocks.
//
// The tests in this package are gated behind the `integration` build tag so
// they never run during the default `go test` invocation. Run them explicitly
// with:
//
//	go test -tags=integration ./internal/integration/...
//
// A running Docker daemon is required; tests skip gracefully when Docker is
// unreachable.
package integration
