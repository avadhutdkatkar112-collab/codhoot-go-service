FROM golang:1.26-alpine
RUN apk add --no-cache gcc musl-dev && \
    adduser -D -u 1000 runner && \
    mkdir -p /workspace /tmp/gocache && \
    chown -R runner:runner /workspace /tmp/gocache
WORKDIR /app
COPY go.mod .
COPY main.go .
RUN go build -ldflags="-s -w" -o codhoot-go-service .
RUN chown runner:runner codhoot-go-service && \
    chmod +x codhoot-go-service
ENV PORT=8086
EXPOSE 8086
USER runner
CMD ["./codhoot-go-service"]
