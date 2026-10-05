/**
 * 清理浏览器缓存工具（参考 MoviePilot）
 *
 * 清除 Cache Storage + 注销 Service Worker + 带时间戳强制刷新
 */

export async function clearBrowserCache() {
  try {
    // 1. 清除所有 Cache Storage
    if ('caches' in window) {
      const cacheNames = await caches.keys()
      await Promise.all(cacheNames.map(name => caches.delete(name)))
      console.log('[CacheClear] 已清除所有 Cache Storage')
    }

    // 2. 注销所有 Service Worker
    if ('serviceWorker' in navigator) {
      const registrations = await navigator.serviceWorker.getRegistrations()
      await Promise.all(registrations.map(reg => reg.unregister()))
      console.log('[CacheClear] 已注销所有 Service Worker')
    }

    // 3. 清除 localStorage 中的主题等缓存（保留 token）
    // 不清除 danmu_token，避免用户需要重新登录
  } catch (e) {
    console.error('[CacheClear] 清除缓存时出错:', e)
  } finally {
    // 4. 带时间戳强制刷新，绕过浏览器缓存
    const url = new URL(window.location.href)
    url.searchParams.set('__t', Date.now().toString())
    window.location.replace(url.pathname + url.search + url.hash)
  }
}
