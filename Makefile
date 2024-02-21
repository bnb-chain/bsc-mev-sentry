
.PHONY : tools mock docs

all: build

mod:
	go mod tidy

build: mod
	mkdir -p .build
	go build -o .build/sentry ./cmd

image:
	docker build --build-arg GIT_TOKEN=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 -t bsc-mev-sentry:latest .

test:
	go test `go list ./...`

cover:
	go test -cover `go list ./...`