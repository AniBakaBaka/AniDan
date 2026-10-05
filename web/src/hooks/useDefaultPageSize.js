import { useState, useEffect } from 'react'
import { getConfig } from '../apis'

// 各页面的默认分页大小
const DEFAULT_PAGE_SIZES = {
  library: 50,
  episode: 50,
  localItems: 50,
  mediaItems: 100,
  refreshModal: 20,
}

// 配置键名映射
const CONFIG_KEY_MAP = {
  library: 'pageSizeLibrary',
  episode: 'pageSizeEpisode',
  localItems: 'pageSizeLocalItems',
  mediaItems: 'pageSizeMediaItems',
  refreshModal: 'pageSizeRefreshModal',
}

// 缓存配置值，避免重复请求（按页面类型分别缓存）
const cachedPageSizes = {}
const cachePromises = {}

/**
 * 获取指定页面的分页大小的 Hook
 * 从后端配置中读取对应页面的分页设置，如果未设置则使用默认值
 *
 * @param {string} pageType - 页面类型: 'library' | 'episode' | 'localItems' | 'mediaItems' | 'refreshModal'
 * @param {number} fallback - 如果配置未加载完成时使用的临时默认值（可选）
 * @returns {number} 分页大小
 */
export function useDefaultPageSize(pageType, fallback) {
  const defaultSize = fallback ?? DEFAULT_PAGE_SIZES[pageType] ?? 50
  const configKey = CONFIG_KEY_MAP[pageType]

  const [pageSize, setPageSize] = useState(cachedPageSizes[pageType] ?? defaultSize)

  useEffect(() => {
    if (!configKey) {
      console.warn(`未知的页面类型: ${pageType}`)
      return
    }

    // 如果已有缓存，直接使用
    if (cachedPageSizes[pageType] !== undefined) {
      setPageSize(cachedPageSizes[pageType])
      return
    }

    // 如果正在加载，等待加载完成
    if (cachePromises[pageType]) {
      cachePromises[pageType].then((size) => {
        setPageSize(size)
      })
      return
    }

    // 发起请求
    cachePromises[pageType] = getConfig(configKey)
      .then((res) => {
        const value = res.data?.value
        const size = value ? parseInt(value, 10) : defaultSize
        cachedPageSizes[pageType] = isNaN(size) ? defaultSize : size
        setPageSize(cachedPageSizes[pageType])
        return cachedPageSizes[pageType]
      })
      .catch((err) => {
        console.error(`加载 ${pageType} 分页配置失败:`, err)
        cachedPageSizes[pageType] = defaultSize
        setPageSize(defaultSize)
        return defaultSize
      })
  }, [pageType, configKey, defaultSize])

  return pageSize
}

/**
 * 清除指定页面的缓存，用于配置更新后刷新
 * @param {string} pageType - 页面类型，不传则清除所有缓存
 */
export function clearPageSizeCache(pageType) {
  if (pageType) {
    delete cachedPageSizes[pageType]
    delete cachePromises[pageType]
  } else {
    // 清除所有缓存
    Object.keys(cachedPageSizes).forEach(key => delete cachedPageSizes[key])
    Object.keys(cachePromises).forEach(key => delete cachePromises[key])
  }
}

