/**
 * 完结图标 —— 圆圈内一个「完」字
 * 用法: <FinishedIcon size={16} color="currentColor" />
 */
export const FinishedIcon = ({ size = 16, color = 'currentColor', className }) => (
  <svg
    className={className}
    width={size}
    height={size}
    viewBox="0 0 1024 1024"
    fill="none"
    xmlns="http://www.w3.org/2000/svg"
    style={{ display: 'inline-block', verticalAlign: '-0.125em' }}
  >
    {/* 外圆 */}
    <circle cx="512" cy="512" r="460" stroke={color} strokeWidth="64" fill="none" />
    {/* 完 字 */}
    <text
      x="512"
      y="540"
      textAnchor="middle"
      dominantBaseline="central"
      fill={color}
      fontSize="520"
      fontWeight="bold"
      fontFamily="sans-serif"
    >
      完
    </text>
  </svg>
)

