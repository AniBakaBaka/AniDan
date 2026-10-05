import React from 'react'
import { Modal, Drawer } from 'antd'
import { useAtomValue } from 'jotai'
import { isMobileAtom } from '../../store/index.js'

/**
 * 响应式弹窗组件
 * 移动端显示为抽屉，桌面端显示为对话框
 */
export const ResponsiveModal = ({
  children,
  open,
  onCancel,
  title,
  footer,
  width = 520,
  placement = 'bottom',
  height = 'auto',
  ...props
}) => {
  const isMobile = useAtomValue(isMobileAtom)

  if (isMobile) {
    return (
      <Drawer
        title={title}
        placement={placement}
        onClose={onCancel}
        open={open}
        footer={footer}
        height={height}
        {...props}
      >
        {children}
      </Drawer>
    )
  }

  return (
    <Modal
      title={title}
      open={open}
      onCancel={onCancel}
      footer={footer}
      width={width}
      {...props}
    >
      {children}
    </Modal>
  )
}
