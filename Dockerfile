FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod .
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o codhoot-go-service .

FROM golang:1.22-alpine
WORKDIR /app
COPY --from=builder /app/codhoot-go-service .
ENV PORT=8086
EXPOSE 8086
CMD ["./codhoot-go-service"]
