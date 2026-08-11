# SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
# SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

# Build stage
FROM node:20-alpine AS builder

WORKDIR /app

# Copy package files
COPY app/package*.json ./
RUN npm ci

# Copy source code
COPY app/ .

# Build the application (tsc -b + vite build)
RUN npm run build

# Run stage
FROM nginx:1.27-alpine

# Backend origin used by the /api reverse-proxy in nginx.conf.template.
ENV BACKEND_URL=http://localhost:8090

RUN apk --no-cache add curl

# Remove default nginx config and install the AI_LM template
RUN rm /etc/nginx/conf.d/default.conf
COPY app/nginx.conf.template /etc/nginx/templates/default.conf.template

# Copy built assets from builder
COPY --from=builder /app/dist /usr/share/nginx/html

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD curl -f http://localhost:8080/ || exit 1

CMD ["nginx", "-g", "daemon off;"]
