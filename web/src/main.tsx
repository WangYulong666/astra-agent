import React from 'react'
import ReactDOM from 'react-dom/client'
import { Root } from './Root'
import './styles.css'
// 视觉层必须排在 styles.css 之后：它靠同选择器 + 后加载做级联覆盖，
// 只改颜色/字体/阴影/圆角/过渡，不碰布局。
import './theme.css'

// Root 而不是 App：启动时要先探测服务端要不要认证，再决定渲染登录页
// 还是工作台。把这个判断放在最外层，页面组件就不用各自关心登录态。
ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <Root />
  </React.StrictMode>,
)
