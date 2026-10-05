import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react-swc'
import tailwindcss from '@tailwindcss/vite'
import { VitePWA } from 'vite-plugin-pwa'
import path from 'path'

console.log('当前环境:', process.env.NODE_ENV)
// https://vitejs.dev/config/
export default defineConfig({
  plugins: [
    react(),
    tailwindcss(),
    VitePWA({
      registerType: 'autoUpdate',
      workbox: {
        // 只缓存核心静态资源，不缓存 API 请求和 index.html
        // 注意：不缓存 html，确保每次导航都从服务器获取最新 index.html
        globPatterns: ['**/*.{js,css,ico,png,svg,woff2}'],
        // 主 JS 包较大（~3MB），需要提高缓存上限
        maximumFileSizeToCacheInBytes: 5 * 1024 * 1024,
        // 不缓存 API 和数据请求
        navigateFallbackDenylist: [/^\/api\//],
        // 导航请求始终走网络优先，确保更新后立即生效
        navigateFallback: null,
      },
      manifest: false, // 使用 public/manifest.json，不自动生成
    }),
  ],
  server: {
    host: '0.0.0.0', // 允许外部访问
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:7769',
        changeOrigin: true,
        secure: false,
      },
      '/data': {
        target: 'http://127.0.0.1:7769',
        changeOrigin: true,
        secure: false,
      },
      '/static': {
        target: 'http://127.0.0.1:7769',
        changeOrigin: true,
        secure: false,
      },
      '/openapi.json': {
        target: 'http://127.0.0.1:7769',
        changeOrigin: true,
        secure: false,
      }
    }
  },
  css: {
    preprocessorOptions: {
      less: {
        javascriptEnabled: true,
        modifyVars: {},
      },
    },
  },
  build: {
    minify: 'terser',
    terserOptions: {
      compress: {
        drop_console: process.env.NODE_ENV !== 'development',
        drop_debugger: process.env.NODE_ENV !== 'development',
      },
    },
  },
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
})
