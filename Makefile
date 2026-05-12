.PHONY: build-rust embed-lib clippy test bench bench-compare lint fmt

build-rust:
	cargo build --release -p monty-ffi

embed-lib: build-rust
	scripts/install-ffi-library.sh

clippy:
	cargo clippy -p monty-ffi -- -D warnings

test: embed-lib
	go test ./...

bench: build-rust
	go test -bench . -benchmem ./...

bench-compare: build-rust
	uv run --script benchmarks/compare.py

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.0 run ./...

fmt:
	gofmt -w .
	cargo fmt --manifest-path crates/monty-ffi/Cargo.toml
