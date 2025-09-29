FROM node:20-alpine

# Install FFmpeg and required build tools
RUN apk add --no-cache ffmpeg python3 make g++

# Set working directory
WORKDIR /app

# Install pnpm globally
RUN corepack enable && corepack prepare pnpm@10.11.1 --activate

# Copy root package files
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml turbo.json ./

# Copy app-specific package.json files
COPY apps/(frontend)/package.json ./apps/(frontend)/
COPY apps/(backend)/package.json ./apps/(backend)/

# Install dependencies
RUN pnpm install --frozen-lockfile

# Copy all source code
COPY . .

# Create downloads directory for backend
RUN mkdir -p apps/(backend)/public/downloads

# Build the application
RUN pnpm run build

# Expose ports
EXPOSE 3000 3001

# Start both servers in development mode
CMD ["pnpm", "run", "dev"]
