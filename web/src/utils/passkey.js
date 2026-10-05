/**
 * PassKey / WebAuthn 可用性检测
 *
 * 只在 HTTPS 模式下启用 PassKey。
 * HTTP 模式（包括 localhost）一律不显示 PassKey 入口，
 * 避免和同 IP 不同端口的其它应用（如 MoviePilot）冲突。
 */

export function isPasskeySupported() {
  if (typeof window === 'undefined') return false
  if (!window.PublicKeyCredential) return false
  // 仅 HTTPS 启用（不含 localhost / http）
  return window.location.protocol === 'https:'
}
