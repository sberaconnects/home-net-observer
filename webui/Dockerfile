FROM golang:1.23-bookworm AS build

WORKDIR /src
COPY webui/go.mod webui/go.sum* ./
RUN go mod download
COPY webui/ ./
RUN CGO_ENABLED=0 go build -o /out/hno-webui ./cmd/webui

FROM gcr.io/distroless/base-debian12:nonroot

COPY --from=build /out/hno-webui /usr/local/bin/hno-webui

ENTRYPOINT ["/usr/local/bin/hno-webui"]
