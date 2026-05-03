.PHONY: compose-config collector-build collector-format collector-test collector-tidy

compose-config:
	docker compose --env-file .env.example config

collector-build:
	docker build -f collector/Dockerfile -t home-net-observer-collector:dev .

collector-format:
	docker run --rm -v $(PWD)/collector:/src -w /src golang:1.23-bookworm gofmt -w cmd/collector/main.go cmd/collector/main_test.go

collector-test:
	docker run --rm -v $(PWD)/collector:/src -w /src golang:1.23-bookworm bash -lc 'apt-get update >/dev/null && apt-get install -y --no-install-recommends libpcap-dev gcc ca-certificates >/dev/null && /usr/local/go/bin/go test ./...'

collector-tidy:
	docker run --rm -v $(PWD)/collector:/src -w /src golang:1.23-bookworm go mod tidy
