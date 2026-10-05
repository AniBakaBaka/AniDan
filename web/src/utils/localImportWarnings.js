// SPDX-License-Identifier: AGPL-3.0-only
const messages = {
  tmdb_poster_identity_conflict: ['媒体身份冲突，已跳过 TMDB 封面', 'TMDB poster skipped: library identity conflict'],
  tmdb_poster_key_missing: ['未配置 TMDB API Key，已跳过封面', 'TMDB poster skipped: API key not configured'],
  tmdb_poster_route_unavailable: ['配置的封面访问线路不可用或不受支持', 'Configured poster route is unavailable or unsupported'],
  tmdb_poster_metadata_failed: ['未能获取 TMDB 元数据，已跳过封面', 'TMDB poster skipped: metadata unavailable'],
  tmdb_poster_identity_mismatch: ['TMDB 返回的媒体身份不一致，已跳过封面', 'TMDB poster skipped: returned identity mismatch'],
  tmdb_poster_missing: ['TMDB 元数据没有提供封面', 'TMDB metadata did not include a poster'],
  tmdb_poster_image_failed: ['未能安全下载 TMDB 封面', 'TMDB poster could not be safely downloaded'],
}

function itemID(value) {
  if (typeof value === 'number' && Number.isSafeInteger(value) && value > 0) return String(value)
  if (typeof value === 'string' && /^[1-9]\d{0,18}$/.test(value) && BigInt(value) <= 9223372036854775807n) return value
  return null
}

// Only server-defined codes become visible text. Never render raw messages,
// provider errors, URLs or unknown codes from a stored task result.
export function localImportWarnings(task, language = 'en') {
  if (task?.taskType !== 'import_local_items') return null
  const result = task.result
  if (!result || typeof result !== 'object' || Array.isArray(result)) return null
  const zh = language.startsWith('zh')
  if (result.warningsUnavailable === true) return {
    rows: [], truncated: false,
    heading: zh ? '封面补充提示' : 'Poster enrichment notices',
    notice: zh ? '提示摘要无法读取，请以任务状态为准' : 'Notice summary could not be read; refer to the task status',
  }
  const source = Array.isArray(result.warnings) ? result.warnings : []
  const rows = source.slice(0, 25).map(row => ({
    itemId: itemID(row?.itemId),
    text: (typeof row?.code === 'string' && Object.hasOwn(messages, row.code) ? messages[row.code] :
      ['封面补充未完成', 'Poster enrichment was not completed'])[zh ? 0 : 1],
  }))
  const declared = Number.isSafeInteger(result.warningCount) && result.warningCount >= 0
    ? Math.min(result.warningCount, 1000000) : 0
  const count = Math.max(declared, Math.min(source.length, 1000000))
  if (!count) return null
  return {
    rows, count,
    truncated: result.warningsTruncated === true || count > rows.length,
    heading: zh ? `封面补充提示（${count}）` : `Poster enrichment notices (${count})`,
    itemLabel: zh ? '条目' : 'Item',
    unknownItem: zh ? '条目 ID 不可用' : 'Item ID unavailable',
    more: zh ? '最多展示 25 条已保存提示，部分详情已省略；任务状态以状态标签为准' :
      'At most 25 saved notices are shown; some details are omitted. Refer to the task status label',
  }
}
