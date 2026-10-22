FROM golang:1.27.2-alpine AS build

RUN apk add --no-cache ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -o /out/api ./cmd/api
RUN CGO_ENABLED=0 go build -trimpath -o /out/sqs-setup ./cmd/sqs-setup

FROM scratch
COPY --from=build /out/api /api
COPY --from=build /out/sqs-setup /sqs-setup
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 65532:65532
ENTRYPOINT ["/api"]
