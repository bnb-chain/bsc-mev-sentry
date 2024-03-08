FROM public.ecr.aws/docker/library/golang:1.21-alpine AS builder

ENV GO111MODULE=on \
    CGO_ENABLED=1 \
    GOOS=linux \
    GOARCH=amd64 \
    GIT_TERMINAL_PROMPT=1

RUN apk add --no-cache build-base git bash linux-headers eudev-dev curl ca-certificates

WORKDIR /build
COPY . .

ARG GH_TOKEN=""
RUN go env -w GOPRIVATE="github.com/bnb-chain/*"
RUN git config --global url."https://${GH_TOKEN}@github.com".insteadOf "https://github.com"

RUN go mod tidy
RUN go build -o .build/sentry ./cmd

FROM public.ecr.aws/docker/library/alpine:latest

RUN apk add --no-cache build-base bash vim curl busybox-extras

WORKDIR /opt/app

COPY --from=builder /build/.build/sentry /opt/app/

ENTRYPOINT /opt/app/sentry -config /opt/app/configs/config.toml
