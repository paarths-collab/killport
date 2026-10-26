# Development

## Repository layout

```text
killport/
├── main.go                 # Executable entry point and release version injection
├── internal/
│   └── cli/                # Commands, discovery, planning, and execution
│       ├── cli.go          # CLI setup, streams, and signal handling
│       ├── workflow.go     # inspect / plan / apply commands
│       ├── procid_*.go     # Platform-specific process identities
│       ├── *_test.go       # Unit, regression, and acceptance tests
│       └── testdata/       # Parser fixtures and scenario corpus
├── docs/                   # User and contributor reference material
│   ├── agents/             # Optional agent integration guides
│   └── evidence/           # Recorded validation evidence
├── scripts/                # Installation tests, platform checks, release helpers
├── .github/workflows/      # Cross-platform CI and release automation
├── .goreleaser.yaml         # Release archives and target platforms
├── go.mod                  # Go module and dependencies
└── install.sh              # Public installer; its URL stays stable
```

Tests live next to the package they exercise, following Go conventions. Keeping
fixtures under that package lets `go test ./...` find them without depending on
the caller's working directory. The internal package is an implementation detail,
not a public Go SDK. Its command, discovery, and execution files share private
types; the directory boundary avoids exposing those types just to group files.

The root executable stays small and preserves existing `go build .`, `go run .`,
module installation, and package-manager build commands. Release tooling still
injects `main.Version` with linker flags.

## Build and run

Use Go 1.25 or later, from the repository root:

```sh
go build -trimpath -o killport .
go run . --help
./killport --version
```

## Checks

```sh
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
sh scripts/test-install.sh
```

Run only fixture scenarios with:

```sh
go test -count=1 -run Corpus -v ./internal/cli
```

Fixture provenance is documented in [testdata](../internal/cli/testdata/README.md)
and the [scenario corpus](../internal/cli/testdata/corpus/README.md). Preserve
fixture bytes: `.gitattributes` disables newline conversion for this directory.

For a standalone Windows suite, compile `./internal/cli` with `go test -c` and
copy `internal/cli/testdata/` beside the resulting test executable as `testdata/`.
The CI workflow does this before invoking `scripts/test-windows.ps1`.

See [platform validation](platform-validation.md) for native Windows and Docker
checks, and [releasing](releasing.md) for packaging and publishing.
