import { useEffect } from 'react'
import { useLocation, useNavigationType, createRoutesFromChildren, matchRoutes } from 'react-router-dom'
import { useResolvedPath } from 'react-router-dom'

/**
 * 路由滚动行为配置组件
 * 实现路由切换时自动滚动到顶部的功能
 * 可以根据需要扩展更多滚动行为配置
 */
export const RouterScrollBehavior = ({ children }) => {
  const location = useLocation()
  const navigationType = useNavigationType()
  const pathname = useResolvedPath(location.pathname).pathname

  useEffect(() => {
    // 对于POP操作（浏览器前进/后退），保持滚动位置
    // 对于PUSH或REPLACE操作，滚动到顶部
    if (navigationType !== 'POP') {
      // 使用requestAnimationFrame确保在DOM更新后执行滚动
      window.requestAnimationFrame(() => {
        window.scrollTo({
          top: 0,
          behavior: 'smooth'
        })
      })
    }
  }, [pathname, navigationType]) // 仅在路径或导航类型变化时触发

  return children
}

/**
 * 创建带有滚动行为的路由配置
 * @param {Array} routes - 原始路由配置
 * @param {Object} options - 滚动行为配置选项
 * @returns {Array} 增强后的路由配置
 */
export const createRoutesWithScrollBehavior = (routes, options = {}) => {
  // 递归处理路由配置，为每个路由添加滚动行为
  const enhanceRoutes = (routes) => {
    return routes.map(route => {
      // 克隆原始路由配置
      const enhancedRoute = { ...route }
      
      // 如果有子路由，递归处理
      if (route.children && route.children.length > 0) {
        enhancedRoute.children = enhanceRoutes(route.children)
      }
      
      // 为顶层路由包装滚动行为组件
      if (!route.path || route.path === '/') {
        const originalElement = enhancedRoute.element
        enhancedRoute.element = (
          <RouterScrollBehavior {...options}>
            {originalElement}
          </RouterScrollBehavior>
        )
      }
      
      return enhancedRoute
    })
  }
  
  return enhanceRoutes(routes)
}