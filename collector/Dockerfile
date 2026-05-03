FROM golang:1.23-bookworm AS build

WORKDIR /src
RUN apt-get update && apt-get install -y --no-install-recommends libpcap-dev gcc ca-certificates && rm -rf /var/lib/apt/lists/*
COPY collector/go.mod collector/go.sum* ./
RUN go mod download
COPY collector/ ./
RUN CGO_ENABLED=1 go build -o /out/hno-collector ./cmd/collector

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends libpcap0.8 ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/hno-collector /usr/local/bin/hno-collector

ENTRYPOINT ["hno-collector"]
