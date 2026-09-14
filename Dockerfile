FROM golang:1.26-alpine
RUN apk add --no-cache gcc musl-dev && \
    adduser -D -u 1000 runner && \
    mkdir -p /workspace /tmp/gocache /tmp/codhoot-workspace && \
    chown -R runner:runner /workspace /tmp/gocache /tmp/codhoot-workspace
WORKDIR /app
COPY go.mod .
COPY main.go .
RUN go build -ldflags="-s -w" -o codhoot-go-service .
# Pre-compile the Go standard library into GOCACHE so the first runtime
# compile does not rebuild the entire stdlib (which blows past the 60s cap
# on Render free tier's 0.1 CPU after every instance restart).
RUN GOCACHE=/tmp/gocache go build std && \
    chown -R runner:runner /tmp/gocache
RUN chown runner:runner codhoot-go-service && \
    chmod +x codhoot-go-service
ENV PORT=8086
EXPOSE 8086
USER runner
CMD ["./codhoot-go-service"]
