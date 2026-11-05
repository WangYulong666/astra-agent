# Astra Android（WebView 壳）

极简 Flutter WebView 壳，加载 Astra 控制面（Go 单二进制内嵌的 Web 工作台）。
**所有业务 UI（登录、会话、事件流、审批卡片、管理页）均由服务端 Web 提供，本壳不实现任何业务界面。**

## 功能

- 启动闪屏（品牌色 + Logo +「正在连接…」），首连探测 `/healthz` 最多 5 秒，超时直接进错误页
- 首启输入服务器地址（`http(s)://`），存 shared_preferences；输入过程也会存草稿，切走不丢
- 无 scheme 时自动补 `http://`（内网场景）
- 全屏错误页：图标 + 人话原因（连不上 / 5xx / 地址格式错）+「重试」「换地址」「复制诊断信息」；错误页下拉也能重试
- 页面顶部下拉刷新（只在滚动到顶部时触发）+ 顶部细进度条
- 文件下载接管：注入 `window.AstraBridge.saveFile/saveBlob/saveUrl`，并拦截 `<a download>` 的 blob/data/同源链接，
  落盘到系统「下载」目录（API 29+ 用 MediaStore，无需权限；API ≤28 首次保存时申请写权限），保存后可直接「打开」
- 外链（非本服务器的 http/https）与 `tel:`/`mailto:` 等协议交给系统处理，不困在 WebView 里
- 断网时顶部提示条「网络已断开」，恢复后自动重连（仅在错误页/空白页时自动 reload，不打断正在输入的表单）
- Android 返回键 = 网页历史后退；已在首页时**双击退出**（2 秒内再按一次）
- 深色状态栏 + 深色背景，edge-to-edge（Android 15 强制）下由页面自身背景托底
- `android:usesCleartextTraffic="true"`，允许明文 HTTP（内网 `http://192.168.x.x:7788` 是主要场景）
- 版本 0.2.0+1，应用名「Astra」，图标为项目自制的「A」字图标（源图 `desktop/icon.iconset/`）

## 代码结构

```
lib/main.dart             入口 + 启动闪屏 + 首连探测
lib/theme.dart            品牌色 / 深色主题 / SystemUiOverlayStyle
lib/bridge.dart           原生桥（MethodChannel）+ 注入页面的 JS 胶水（kAstraShimJs）+ 错误分类
lib/server_setup_page.dart 服务器地址（首启 / 编辑两态）
lib/web_view_page.dart    WebView 主体：加载态、错误页、下拉刷新、下载、外链、返回键、网络
lib/widgets/*.dart        闪屏 / 错误页 / 网络状态条
android/.../NativeBridge.kt  MediaStore 落盘、打开文件、外链 Intent、ConnectivityManager 事件流
```

## 明确不做

- 原生推送通知（审批提醒走 WebView 内的界面卡片）
- 离线缓存、多服务器管理、自动发现

## 构建

```bash
flutter build apk --release --split-per-abi --target-platform android-arm64 --no-tree-shake-icons
# 产物：build/app/outputs/flutter-apk/app-arm64-v8a-release.apk
```

首次构建前要注意：`gradlew`、`gradlew.bat`、`gradle/wrapper/gradle-wrapper.jar` 三个文件
被 Flutter 自带的 `android/.gitignore` 挡在库外（避免 wrapper 版本和本机 Flutter SDK 打架，
仓库里只留 `gradle-wrapper.properties`）。干净 clone 下来直接 `flutter build` 会报
找不到 `gradlew`，先补一次平台脚手架：

```bash
flutter create --platforms=android .    # 只补缺失的 android/ 骨架，不覆盖已有源码
```

（`app-android/ios/` 同理，需要 iOS 平台时用 `--platforms=ios`。）

依赖仅 `webview_flutter` + `shared_preferences`（未新增第三方包，下载/网络/外链/打开文件全部走手写 Platform Channel）。签名用 Flutter 默认 debug 签名，可直接安装运行；正式分发请自行配置 release keystore。

## 已知限制

- 依赖设备系统 WebView（Android System WebView），过旧的 WebView 内核可能导致工作台渲染异常
- 未做原生推送、离线缓存
- 下拉刷新只能在页面内识别（Android 平台视图会吃掉 Flutter 手势），因此由注入脚本识别手势后回调壳触发 reload；视觉上是顶部细进度条 + 页面内小提示
- `<a download>` 若是不挂到文档树的游离节点（未 append 就 click），点击事件不会冒泡到 document，壳收不到；Web 端可改用 `window.AstraBridge.saveBlob(blob, name)` 显式调用
- 跨域 `<a download>` 读不到响应体（CORS），这类链接会转交系统浏览器下载
