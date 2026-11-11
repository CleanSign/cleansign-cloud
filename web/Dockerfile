# Stage 1: The Builder 🏗️
# Install dependencies and build the project
FROM node:26 AS builder
WORKDIR /app

# Copy dependency-related files
COPY package.json package-lock.json ./

# npm ci installs the exact versions from the lockfile
RUN npm ci

# Copy the rest of the application source code
COPY . .

# rewrites (/api/v1/* -> server) are fixed at build time. The server address is taken as a build arg
ARG SERVER_URL=http://localhost:4000
ENV SERVER_URL=$SERVER_URL

# Run the Next.js build command
RUN npm run build

# ---

# Stage 2: The Runner 🚀
# Use the lightweight 'slim' image for the final production container
FROM node:26-slim AS runner
WORKDIR /app

# Set the environment to production
ENV NODE_ENV=production
# Used when server components and middleware call the server at runtime (same value as the build arg)
ARG SERVER_URL=http://localhost:4000
ENV SERVER_URL=$SERVER_URL

# Copy the self-contained standalone output from the builder stage
COPY --from=builder /app/.next/standalone ./

# Copy the public and static folders for assets
COPY --from=builder /app/public ./public
COPY --from=builder /app/.next/static ./.next/static

# Expose the port the app will run on
EXPOSE 3000

# The command to start the Next.js server
CMD ["node", "server.js"]
