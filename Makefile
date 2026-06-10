.PHONY: build-rust embed-lib clippy test test-rust bench bench-compare lint fmt check

build-rust:
	cargo build --release -p monty-ffi

embed-lib: build-rust
	scripts/install-ffi-library.sh

clippy:
	cargo clippy -p monty-ffi --all-targets --all-features -- -D warnings

test-rust:
	cargo test --release -p monty-ffi

test: embed-lib test-rust
	go test -race ./...

bench: build-rust
	go test -run '^$$' -bench . -benchmem .

bench-compare: build-rust
	uv run --script benchmarks/compare.py

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.0 run ./...

fmt:
	gofmt -w .
	cargo fmt --manifest-path crates/monty-ffi/Cargo.toml

# Everything CI checks, in one local command.
check: fmt clippy test lint
