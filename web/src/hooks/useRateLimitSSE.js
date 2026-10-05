import { parseAPIJSON } from '../utils/idTransport'
import { useEffect, useRef, useState } from 'react'
import Cookies from 'js-cookie'
import { fetchEventSource } from '@microsoft/fetch-event-source'

/**
 * 流控状态 SSE 推送 hook
 * 连接 /api/ui/rate-limit/status?stream=true，每秒接收最新流控数据
 * @returns {{ data: object|null, loading: boolean }}
 */
export const useRateLimitSSE = () => {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const abortRef = useRef(null)

  useEffect(() => {
    const token = Cookies.get('danmu_token')
    if (!token) {
      setLoading(false)
      return
    }

    const abortController = new AbortController()
    abortRef.current = abortController

    fetchEventSource('/api/ui/rate-limit/status?stream=true', {
      signal: abortController.signal,
      headers: {
        Authorization: `Bearer ${token}`,
      },
      onopen: async response => {
        if (response.ok) {
          setLoading(false)
        } else {
          throw new Error(`流控 SSE 连接失败: ${response.status}`)
        }
      },
      onmessage: event => {
        const raw = event.data?.trim()
        if (!raw) return
        try {
          const parsed = parseAPIJSON(raw)
          if (!parsed.error) {
            setData(parsed)
            if (loading) setLoading(false)
          }
        } catch {
          // 忽略非 JSON 消息（如心跳）
        }
      },
      onerror: error => {
        console.error('流控 SSE 连接错误:', error)
        setLoading(false)
        throw error // 抛出错误以停止自动重连
      },
    }).catch(error => {
      if (error.name !== 'AbortError') {
        console.error('流控 SSE 流错误:', error)
      }
    })

    return () => {
      if (abortRef.current) {
        abortRef.current.abort()
      }
    }
  }, [])

  return { data, loading }
}
