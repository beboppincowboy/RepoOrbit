FROM node:22-alpine AS build
WORKDIR /app
# Next.js bakes rewrites in at build time, so API_ORIGIN is a build arg, not runtime env.
ARG API_ORIGIN=http://api:8080
ENV API_ORIGIN=$API_ORIGIN
COPY package*.json ./
RUN npm ci
COPY . .
RUN npm run build
FROM node:22-alpine
WORKDIR /app
ENV NODE_ENV=production HOSTNAME=0.0.0.0 PORT=3000
COPY --from=build --chown=node:node /app/.next/standalone ./
COPY --from=build --chown=node:node /app/.next/static ./.next/static
USER node
CMD ["node", "server.js"]
