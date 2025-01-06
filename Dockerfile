FROM golang:1.20 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o proxy

FROM gcr.io/distroless/static
WORKDIR /app
COPY --from=builder /app/proxy .
EXPOSE 9090
CMD ["/app/proxy"]