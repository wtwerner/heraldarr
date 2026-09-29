.PHONY: check test lint fmt build golden-update harness docker

# The gate every agent and CI runs. Must pass before a PR or a Stop.
check: lint test

test:
	go test -race ./...

lint:
	golangci-lint run ./...

fmt:
	golangci-lint fmt ./...

build:
	CGO_ENABLED=0 go build -o bin/heraldarr ./cmd/heraldarr

# Rewrites testdata/golden/*.json from the current Go renderer. Review the diff before committing.
golden-update:
	go test ./internal/render/... -update

# Regenerates the parity oracle: REFERENCE_PY=/path/to/arr_discord.py make harness (python3 ≥ 3.9).
harness:
	python3 testdata/harness/make_expected.py

docker:
	docker build -t heraldarr:dev .
