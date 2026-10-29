# Build stage
FROM golang:1.22-alpine AS builder

WORKDIR /build

# Use China Go proxy mirror (NAS cannot reach proxy.golang.org directly)
ENV GOPROXY=https://goproxy.cn,direct

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o substore-ui-go .

# Runtime stage
FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /build/substore-ui-go .
COPY --from=builder /build/public ./public

EXPOSE 8300

ENTRYPOINT ["./substore-ui-go"]
