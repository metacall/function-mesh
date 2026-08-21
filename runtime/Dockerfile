# Stage 1: Base runtime image
ARG LANG_RUNTIME=metacall/core:latest
FROM ${LANG_RUNTIME}

# Stage 2: Install runtime server
WORKDIR /runtime

COPY package.json ./
RUN apt-get update && apt-get install -y npm \
    && npm install --production --no-audit --no-fund \
    && rm -rf /var/lib/apt/lists/* \
    && (ln -s /usr/lib/x86_64-linux-gnu/libruby-3.3.so.3.3 /usr/lib/x86_64-linux-gnu/libruby-3.3.so 2>/dev/null || true)

# Copy the server
COPY server.js ./

# Runtime Configuration
ENV PORT=8080
ENV FUNCTION_CONFIG=/app/metacall.json
ENV REQUEST_TIMEOUT_MS=30000
ENV METACALL_RPC_TIMEOUT_MS=5000
ENV METACALL_RPC_RETRY_COUNT=1
ENV METACALL_RPC_RETRY_DELAY_MS=0
ENV MESH_REMOTE_MAX_RETRIES=30
ENV MESH_REMOTE_RETRY_DELAY_MS=2000
ENV MESH_REMOTE_HEALTH_TIMEOUT_MS=2000

# User code will be mounted here by K8s (via ConfigMap or PVC volume)
VOLUME /app
WORKDIR /app

EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD wget -q --spider http://localhost:8080/health/ready || exit 1

# Start the runtime server
CMD ["/runtime/server.js"]
