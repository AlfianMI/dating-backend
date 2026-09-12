# ============================================================
# Stage 1: Build
# ============================================================
FROM golang:1.26-bookworm AS builder

WORKDIR /app

ENV CGO_ENABLED=1

# Install native build dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    bash \
    gcc \
    g++ \
    && rm -rf /var/lib/apt/lists/*

# Copy dependency definitions first for better Docker layer caching
COPY go.mod go.sum ./

RUN go mod download

# Copy application source
COPY . .

# Download ONNX Runtime + model
ENV FORCE_OS=Linux
ENV FORCE_ARCH=x86_64

RUN bash ./scripts/setup_ml.sh

# Build application
RUN go build -o main ./cmd/app/main.go


# ============================================================
# Stage 2: Runtime
# ============================================================
FROM debian:bookworm-slim

WORKDIR /app

# Runtime dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    libgomp1 \
    libpcre2-8-0 \
    && rm -rf /var/lib/apt/lists/*

# Application binary
COPY --from=builder /app/main .

# ONNX Runtime shared libraries
COPY --from=builder /app/lib ./lib

# ONNX model
COPY --from=builder /app/models ./models

# Runtime configuration
ENV ONNX_MODEL_PATH=/app/models/arcface_resnet50.onnx
ENV ONNXRUNTIME_SHARED_LIBRARY_PATH=/app/lib/libonnxruntime.so

EXPOSE 8080

USER 10001

CMD ["./main"]