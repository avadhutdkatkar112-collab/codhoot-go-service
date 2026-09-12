FROM golang:1.22-bookworm AS builder
WORKDIR /app
COPY go.mod .
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o codhoot-go-service .

FROM golang:1.22-slim
WORKDIR /app
RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/codhoot-go-service .
ENV PORT=8086
EXPOSE 8086
CMD ["./codhoot-go-service"]
