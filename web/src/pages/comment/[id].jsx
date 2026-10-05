import { asIdentifier, sameIdentifier } from '../../utils/idTransport'
import { useCallback, useEffect, useState, useRef } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { getDanmakuDetail, getEpisodes } from '../../apis'
import { Alert, Breadcrumb, Button, Card, Empty } from 'antd'
import { HomeOutlined } from '@ant-design/icons'
import { useScroll } from '../../hooks/useScroll'
import { useTranslation } from 'react-i18next'

export const CommentDetail = () => {
  const { t } = useTranslation()
  const { id } = useParams()
  const [searchParams] = useSearchParams()
  const episodeId = searchParams.get('episodeId')

  const [loading, setLoading] = useState(true)
  const [commentList, setCommentList] = useState([])
  const [episode, setEpisode] = useState({})
  const [error, setError] = useState('')
  const requestSequence = useRef(0)
  const [loadingMore, setLoadingMore] = useState(false)
  const [pagination, setPagination] = useState({
    current: 1,
    pageSize: 100,
    total: 0,
  })
  const { current, pageSize } = pagination
  const cancelRequests = useCallback(() => { requestSequence.current++ }, [])

  const navigate = useNavigate()

  /**
   * 处理加载更多逻辑
   */
  const handleLoadMore = () => {
    if (!loadingMore && commentList.length < pagination.total) {
      setPagination(prev => ({
        ...prev,
        current: prev.current + 1,
      }))
    }
  }

  /**
   * 使用自定义滚动hook实现下拉加载
   */
  const [setScrollTarget] = useScroll({
    canLoadMore: !loadingMore && commentList.length < pagination.total,
    onLoadMore: handleLoadMore,
  })

  /**
   * 获取弹幕详情
   * @param {boolean} isLoadMore - 是否为加载更多操作
   */
  const getDetail = useCallback(async (isLoadMore = false) => {
    const sequence = ++requestSequence.current
    try {
      setError('')
      if (isLoadMore) {
        setLoadingMore(true)
      } else {
        setLoading(true)
      }

      const [episodeRes, commentRes] = await Promise.all([
        episodeId ? getEpisodes({
          sourceId: asIdentifier(episodeId),
        }) : Promise.resolve({ data: { list: [] } }),
        getDanmakuDetail({
          id: asIdentifier(id),
          page: current,
          pageSize,
        }),
      ])

      if (sequence !== requestSequence.current) return
      if (!Array.isArray(commentRes.data?.list) || !Number.isFinite(commentRes.data?.total)) {
        throw new Error(t('common.fetch_failed'))
      }

      if (isLoadMore) {
        // 加载更多时追加数据
        setCommentList(prev => [...prev, ...(commentRes.data?.list || [])])
      } else {
        // 刷新时替换数据
        setCommentList(commentRes.data?.list || [])
      }

      setEpisode(
        episodeRes?.data?.list?.filter(
          it => sameIdentifier(it.episodeId, id)
        )?.[0] || {}
      )

      setPagination(prev => ({
        ...prev,
        total: commentRes.data?.total || 0,
      }))

      setLoading(false)
      setLoadingMore(false)
    } catch (error) {
      if (sequence !== requestSequence.current) return
      setError(error.message || t('common.fetch_failed'))
      setLoading(false)
      setLoadingMore(false)
    }
  }, [episodeId, id, current, pageSize, t])

  useEffect(() => {
    setCommentList([])
    setPagination(previous => ({ ...previous, current: 1 }))
  }, [id, episodeId])

  useEffect(() => {
    const isLoadMore = current > 1
    getDetail(isLoadMore)
    return cancelRequests
  }, [getDetail, current, cancelRequests])

  return (
    <div className="my-6">
      <Breadcrumb
        className="!mb-4"
        items={[
          {
            title: (
              <Link to="/">
                <HomeOutlined />
              </Link>
            ),
          },
          {
            title: <Link to="/library">{t('commentPage.breadcrumbLibrary')}</Link>,
          },
          {
            title: (
              <span
                style={{ cursor: 'pointer', color: '#1890ff' }}
                onClick={() => navigate(-1)}
              >
                {t('commentPage.breadcrumbEpisodeList')}
              </span>
            ),
          },
          {
            title: t('commentPage.pageTitleLabel'),
          },
        ]}
      />
      <Card loading={loading} title={t('commentPage.cardTitle', { title: episode?.title ?? '' })}>
        {error && <Alert type="error" showIcon message={error} action={<Button onClick={() => getDetail(false)}>{t('common.retry')}</Button>} />}
        <div>
          {commentList.length > 0 ? (
            <>
              {commentList?.map((it, index) => (
                <div key={index}>
                  <div className="my-1">
                    <pre
                      style={{
                        whiteSpace: 'pre-wrap',
                        wordWrap: 'break-word',
                        overflowWrap: 'break-word',
                        wordBreak: 'break-all',
                        maxWidth: '100%',
                        overflow: 'hidden',
                        margin: 0,
                        fontFamily: 'inherit',
                      }}
                    >
                      {it.p} | {it.m}
                    </pre>
                  </div>
                </div>
              ))}
              {/* 加载更多触发元素 */}
              {commentList.length < pagination.total && (
                <div
                  ref={setScrollTarget}
                  style={{
                    textAlign: 'center',
                    marginTop: 12,
                    height: 32,
                    lineHeight: '32px',
                    color: '#999',
                  }}
                >
                  {loadingMore ? t('commentPage.loadingMore') : t('commentPage.pullToLoad')}
                </div>
              )}
            </>
          ) : (
            !error && <Empty description={t('commentPage.noComments')} />
          )}
        </div>
      </Card>
    </div>
  )
}
