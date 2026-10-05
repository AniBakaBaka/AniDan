// AniDan integration addition. The surrounding UI derives from Misaka (AGPL-3.0).
const sourceURL = import.meta.env.VITE_ANIDAN_SOURCE_URL || '/source-code'

export const SourceNotice = () => (
  <footer className="py-4 text-center text-xs opacity-70" aria-label="Source and license">
    <span>AniDan · </span>
    {(sourceURL === '/source-code' || /^https?:\/\//i.test(sourceURL)) && (
      <><a href={sourceURL} target="_blank" rel="noopener noreferrer">Source code</a><span> · </span></>
    )}
    <a href="/AGPL-3.0.txt" target="_blank" rel="noopener noreferrer">AGPL-3.0</a>
    <span> · Based on </span>
    <a href="https://github.com/l429609201/misaka_danmu_server" target="_blank" rel="noopener noreferrer">Misaka</a>
    <span> · </span>
    <a href="/ANIDAN-NOTICE.txt" target="_blank" rel="noopener noreferrer">Notices</a>
  </footer>
)
