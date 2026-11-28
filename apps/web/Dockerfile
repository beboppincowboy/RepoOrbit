FROM oven/bun:1.4.5-alpine AS build
WORKDIR /app
COPY package.json bun.lock ./
RUN bun install --frozen-lockfile
COPY . .
RUN bun run build

FROM oven/bun:1.4.5-alpine
WORKDIR /app
# API_ORIGIN is read at runtime by server.mjs, so compose sets it; the image does not bake it in.
ENV NODE_ENV=production HOST=0.0.0.0 PORT=3000 API_ORIGIN=http://api:8080
COPY --from=build --chown=bun:bun /app/dist/client ./dist/client
COPY --from=build --chown=bun:bun /app/server.mjs ./server.mjs
USER bun
CMD ["bun", "server.mjs"]
