import { defineConfig } from "vite";
import preact from "@preact/preset-vite";
import { VitePWA } from "vite-plugin-pwa";

// Dev: proxy /api to the client API so the browser stays same-origin, mirroring
// the Cloudflare Worker in production. Override target with VITE_API_PROXY.
const apiTarget = process.env.VITE_API_PROXY || "http://localhost:8090";

export default defineConfig({
  plugins: [
    preact(),
    VitePWA({
      registerType: "autoUpdate",
      // Precache the app shell only. The service worker NEVER caches /api/*
      // (money data); those requests are network-only.
      workbox: {
        navigateFallback: "index.html",
        navigateFallbackDenylist: [/^\/api\//],
        runtimeCaching: [
          { urlPattern: /\/api\/.*/, handler: "NetworkOnly", method: "GET" },
        ],
      },
      manifest: {
        name: "bank0",
        short_name: "bank0",
        description: "bank0 customer app",
        // Catppuccin Latte crust / base — the installed splash and task-switcher
        // card are baked at install time, so they take the light pair.
        theme_color: "#dce0e8",
        background_color: "#eff1f5",
        display: "standalone",
        start_url: "/",
        icons: [
          { src: "/icon.svg", sizes: "any", type: "image/svg+xml", purpose: "any maskable" },
        ],
      },
    }),
  ],
  server: {
    proxy: {
      "/api": {
        target: apiTarget,
        changeOrigin: true,
        rewrite: (p) => p.replace(/^\/api/, ""),
      },
    },
  },
});
