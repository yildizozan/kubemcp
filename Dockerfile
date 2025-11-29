# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o mcp-server .

# Runtime stage
FROM cgr.dev/chainguard/wolfi-base

RUN apk --no-cache add ca-certificates

WORKDIR /root/

# Copy the binary from builder
COPY --from=builder /app/mcp-server .

# Expose the port
EXPOSE 8080

# Run the application
CMD ["./mcp-server"]
