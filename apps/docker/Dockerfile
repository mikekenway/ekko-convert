# Development base image (used by docker-compose.dev.yml)
FROM node:20-alpine AS dev-base

RUN apk add --no-cache go ffmpeg git python3 make g++
WORKDIR /app

RUN corepack enable && corepack prepare pnpm@10.11.1 --activate

COPY package.json pnpm-lock.yaml pnpm-workspace.yaml turbo.json ./
COPY apps/(frontend)/package.json ./apps/(frontend)/
COPY apps/(backend)/package.json ./apps/(backend)/

# Build frontend assets
FROM node:20-alpine AS frontend-builder

RUN apk add --no-cache python3 make g++
WORKDIR /app

RUN corepack enable && corepack prepare pnpm@10.11.1 --activate

COPY package.json pnpm-lock.yaml pnpm-workspace.yaml turbo.json ./
COPY apps/(frontend)/package.json ./apps/(frontend)/

# Install workspace dependencies and build the frontend
COPY . .
RUN pnpm install --frozen-lockfile \
    && pnpm --filter web-ui build

# Build Go backend
FROM golang:1.22-alpine AS backend-builder

WORKDIR /src
COPY apps/(backend)/go.mod apps/(backend)/go.sum ./
RUN go mod download
COPY apps/(backend)/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/video-converter ./cmd/server

# Final runtime image
FROM alpine:3.20

RUN apk add --no-cache ffmpeg ca-certificates
WORKDIR /app

# Copy backend binary and frontend assets
COPY --from=backend-builder /bin/video-converter /app/video-converter
COPY --from=frontend-builder /app/apps/(frontend)/dist /app/public/frontend

# Ensure downloads directory exists at runtime
RUN mkdir -p /app/public/downloads

ENV PORT=3001 \
    DOWNLOADS_DIR=/app/public/downloads \
    FRONTEND_DIST=/app/public/frontend

EXPOSE 3001

ENTRYPOINT ["/app/video-converter"]
