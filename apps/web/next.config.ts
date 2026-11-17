import type { NextConfig } from "next";
const api = process.env.API_ORIGIN || "http://127.0.0.1:8080";
const config: NextConfig = {
  output: "standalone",
  async rewrites() {
    return [
      { source: "/graphql", destination: `${api}/graphql` },
      { source: "/api/:path*", destination: `${api}/api/:path*` },
      { source: "/auth/:path*", destination: `${api}/auth/:path*` },
    ];
  },
};
export default config;
