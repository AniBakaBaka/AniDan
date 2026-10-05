// SPDX-License-Identifier: AGPL-3.0-only
import React from 'react'
import { localImportWarnings } from '../../../utils/localImportWarnings.js'

export function LocalImportWarnings({ task, language }) {
  const view = localImportWarnings(task, language)
  if (!view) return null
  return (
    <section className="my-2 rounded border border-amber-400 p-2 text-sm break-words" aria-label={view.heading}>
      <div className="font-medium">{view.heading}</div>
      {view.notice && <div>{view.notice}</div>}
      <ul className="list-disc pl-5 max-h-48 overflow-y-auto">
        {view.rows.map((row, index) => (
          <li key={index}>
            {row.itemId ? `${view.itemLabel} ${row.itemId}` : view.unknownItem}: {row.text}
          </li>
        ))}
      </ul>
      {view.truncated && <div>{view.more}</div>}
    </section>
  )
}
