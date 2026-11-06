# 语音能力（Web）

说话提问（STT）+ 朗读回复（TTS），全部用浏览器原生 Web Speech API 实现。
**没有引入任何 npm 依赖**，也没有后端参与。

```
voice/
├─ types.ts              SpeechRecognition 的最小类型声明（lib.dom 里没有）
├─ speech.ts             纯函数：切句、错误码翻译、音色挑选、Markdown 去噪
├─ useSpeechRecognition.ts   STT hook（实例复用 + 状态机）
├─ useSpeechSynthesis.ts     TTS hook（按句串行 + generation 防串台）
├─ composer.ts           把语音结果写进 Composer 并复用它自己的提交逻辑
├─ VoiceBar.tsx          输入区上方的工具条
├─ icons.tsx             麦克风 / 扬声器
├─ speech.test.ts        纯函数单测
└─ README.md             本文件
```

接入点只有一个：`src/App.tsx` 里 `<Composer />` 上方插了一行 `<VoiceBar />`。
其余页面、组件、样式一律没动。CSS 只往 `styles.css`（布局）和 `theme.css`
（颜色/动效）各追加了一段 `.voice-` 前缀的规则。

---

## 1. 能力矩阵

数据取自 MDN 的 browser-compat-data（`api/SpeechRecognition.json`、
`api/SpeechSynthesis.json`），2026-11 拉取。

### 语音识别（STT，`SpeechRecognition`）

| 浏览器 | 支持 | 说明 |
| --- | --- | --- |
| Chrome 桌面 | ✅ 33+（`webkit` 前缀）／139+ 无前缀 | **识别在 Google 云端**，必须联网；不走 WebView |
| Chrome Android | ✅ 同上 | 同样走云端 |
| Edge | ✅ 79+／139+ | 同 Chrome |
| Safari 桌面 | ✅ 14.1+（`webkit` 前缀） | Apple 自己的识别服务 |
| Safari iOS | ✅ 14.5+（`webkit` 前缀） | 系统「听写」被关掉时不可用 |
| Firefox 桌面 | ⚠️ 160+ | 标记为 *Preview*，**设备端**识别，40 种语言；需要较新的版本 |
| Firefox Android | ❌ | 至今未实现 |
| 安卓 WebView / 各 App 内置浏览器 | ⚠️ | WebView 139+ 才有无前缀版本；微信、钉钉等内置 WebView 普遍不可用 |
| 非安全上下文（http 非 localhost） | ❌ | 构造函数可能仍然存在，但 `start()` 必失败 |

### 语音合成（TTS，`speechSynthesis`）

| 浏览器 | 支持 | 说明 |
| --- | --- | --- |
| Chrome 桌面 | ✅ 33+ | 音色来自系统 + Google 网络音色 |
| Chrome Android | ✅ | |
| Edge | ✅ 14+ | |
| Safari 桌面 / iOS | ✅ 7+ | iOS 上如果开了静音开关，可能完全没声音 |
| Firefox 桌面 | ✅ 49+ | 走系统 TTS（Windows SAPI / macOS NSSpeechSynthesizer / Linux speech-dispatcher） |
| Firefox Android | ✅ 62+ | |
| 安卓 WebView | ❌（`speechSynthesis` 不存在） | 内置浏览器里「朗读」按钮会直接置灰 |
| Linux 无 speech-dispatcher | ⚠️ | `getVoices()` 返回空，我们会置灰并提示「系统未安装中文语音包」 |

> 结论：**STT 只有 Chrome 系 / Safari / 新版 Firefox 能用；TTS 覆盖面大得多，
> 但安卓 WebView 完全没有。** 两个按钮的可用性判断是分开的，不会互相牵连。

---

## 2. 交互与实现

### STT

- `lang = zh-CN`，顶部下拉可切 `en-US`；`continuous = false`、`interimResults = true`。
- 点击麦克风开始，**再点一次结束**（不是按住说话——按住在移动端会和滚动打架，
  而且松手即发容易误触）。
- 中间结果实时显示在输入框上方（灰色斜体），旁边有「取消」可以整段丢弃。
- 正常说完：最终文本**追加**到输入框末尾（保留用户已输入的内容），然后触发
  Composer 自己的发送逻辑。
- 出错时（`network` / `not-allowed` 等）：已识别的文本仍然写进输入框，但**不自动发送**。
- 错误码翻译成人话，见 `speech.ts` 的 `humanizeRecognitionError`。

**实例复用**：识别器只 `new` 一次，之后 `start/stop` 都作用在同一个对象上。
Safari/iOS 每次 `new` 都会占一份底层音频资源，反复创建后会出现 `onend` 不触发、
静音等泄漏症状——这类问题在开发机上几乎复现不出来。

### TTS

- **默认关闭**，开关状态存 `localStorage`（`astra_voice_tts`）。突然出声比「记不住开关」严重得多。
- **只在 run 结束后朗读完整回复**，不逐 token 念。逐 token 会把句子切碎，听感差，
  而且没法收敛（一次 run 会产生几百次 speak）。
- 按 `。！？.!?\n` 切句，逐句串行播放（上一条 `onend` 驱动下一条）。原因：Chrome 对单条
  utterance 有约 15 秒的隐形上限，超时会静默暂停。
- 超长无标点句（模型经常一口气写 200 字）按逗号/顿号兜底再切。
- 朗读前剥掉 Markdown：代码块整块丢，行内代码留内容，链接留文字，去掉 `**`、`#`、表格线。
- 音色优先选 `zh-CN` 且 `localService === true` 的（本地音色延迟低、断网也能念）。
- 切换会话 / 发起新 run 时立刻 `stop()`。

### 朗读回复只针对「当前这一轮」

`VoiceBar` 接收 `runId` / `running` / `events`，只在 `running: true → false` 的那一次
状态翻转时朗读，并用 `spokenRun` 记住已经念过的 run id。刷新页面看到的历史消息不会
被朗读（`prevRunning` 的初值就是当前值，不产生翻转）。

---

## 3. 已知局限（都是真的，不是免责声明）

1. **识别延迟 0.5~2 秒**，取决于网络（Chrome 把音频送到云端）。中间结果能缓解
   「到底开始录没有」的焦虑，但改不了端到端延迟。
2. **不能打断**。说一半想改口，只能点「取消」重来。`continuous = false` 下浏览器
   自己也会在静音约 5 秒后结束。
3. **Safari 需要用户手势**：每次 `start()` 都必须由点击触发，无法在页面加载时自动
   开始监听。TTS 首次 `speak()` 也要求页面有过交互，所以打开开关时会先播一条
   `volume = 0` 的空 utterance 来解锁。
4. **Chrome 的识别不是本地的**。断网、企业防火墙拦 `speech.googleapis.com` 都会
   报 `network`。Firefox 160+ 是设备端的，但只覆盖桌面的新版本。
5. **TTS 音色不可控**。系统装了什么就用什么，同一个页面上 macOS 的「婷婷」和
   Windows 的「Microsoft Huihui」听感差别很大；中文包里没有的话我们只能置灰。
6. **iOS 静音开关会静音 TTS**，用户会以为坏了。
7. **中断体验粗糙**：朗读中发新消息会直接 `cancel()`，没有「渐弱」也没有续读。
8. **没有唤醒词、没有连续对话**，一次一轮，说完就停。

---

## 4. 这次的验证范围

**真机（headless Chrome 156 + CDP，服务在 127.0.0.1:7788）验证过：**

- 麦克风按钮存在、44×44、可点击、`title` 正确；
- 点「朗读回复」→ `localStorage` 变 `1`、按钮 `aria-pressed="true"`；
- 起一个真实 run，run 结束后自动朗读：`speechSynthesis.speaking` 由 `false` 变 `true`
  （40ms 采样命中 78 次），实际 `speak()` 到的是「这是一次语音朗读验证。」，
  `lang=zh-CN`、`voice=婷婷`；
- 三句回复被切成三条 utterance 依次播放（`["第一句…","第二句…","第三句…"]`）；
- 朗读中切换会话 → `speaking` 立刻变 `false`；
- 关闭开关 → `speaking=false`、`localStorage` 变 `0`；
- 非安全上下文（`http://insecure.test`）→ `isSecureContext=false`，按钮 disabled，
  tooltip 与常驻提示都给出「不是安全上下文」的说明。

**没能验证（如实说明）：**

- **STT 的端到端**。headless 环境没有麦克风，`SpeechRecognition` 拿不到音频输入。
  「录音中」的截图是用注入的假识别器还原的视觉状态（脉冲圈、计时、中间结果、
  取消按钮），不是真的在录音。
- Safari / Firefox 的真实行为（只按文档标注）。
- 移动端真机（iOS 静音开关、Safari 手势限制都来自文档与普遍反馈，本地没跑）。

---

## 5. 要不要上云端 ASR/TTS

**当前这套够用的场景**：桌面 Chrome/Edge 里，对着电脑说一句 10~30 字的指令，
点一下、看一眼中间结果、说完自动发出。这个路径体验是合格的。

**该换云端的情况**，出现任意一条就该换：

| 信号 | 说明 |
| --- | --- |
| 主要用户在 Safari / iOS / 安卓内置浏览器 | 原生 STT 覆盖不到或质量不稳 |
| 需要低延迟（<300ms 首字） | 云端流式可以，浏览器原生不行 |
| 需要打断、连续多轮、说话人分离 | Web Speech API 根本不提供这些 |
| 需要固定音色 / 品牌音色 | 系统音色不可控 |
| 需要离线可用 | 只有 Firefox 160+ 的本地识别勉强沾边 |
| 中文识别质量要求高（专业术语、专有名词） | Chrome 的通用模型对术语很弱，可换域模型 |

**方案与成本（按 2026 年公开报价的量级估算，实际以供应商报价为准）：**

1. **流式 ASR —— 阿里云 / 腾讯云 / 讯飞**
   - 实时语音识别一般按音频时长计费，约 **¥1.5~3 / 小时**；一句话识别按次约
     **¥0.002~0.01 / 次**。中文准确率明显好于浏览器原生，支持热词表。
   - 接入方式：前端 `getUserMedia` + `AudioWorklet` 拿 PCM → WebSocket 推流 →
     后端转发（浏览器直连会暴露密钥，必须走后端签名/代理）。
   - 工程量：后端加一个 `/v1/voice/asr` 的 WS 中转 + 凭证管理；前端换掉
     `useSpeechRecognition` 的实现，接口保持 `{listening, interim, start, stop, error}`
     不变，`VoiceBar` 不用改。**这是当初把 hook 接口定成这样的原因。**
   - 断句/VAD 要自己做（原生 `no-speech` 那一套得自己实现）。

2. **流式 TTS —— 阿里云 / 腾讯云 / Azure / ElevenLabs**
   - 按字符计费，约 **¥0.2~1 / 万字符**（Azure 神经音色约 $15/百万字符）。
     一段 100 字的回复成本约 ¥0.002~0.01，可以忽略。
   - 好处：音色固定、可跨平台一致、支持 SSML 停顿和重音、首包延迟 200~500ms。
   - 代价：多一次网络往返 + 后端要管密钥和限流；离线不可用。
   - 备选：ElevenLabs 之类的音质更好但贵一个数量级，且中文一般。

3. **自建**（funasr / Whisper + CosyVoice / GPT-SoVITS）
   - 一次性 GPU 成本高，但量大之后边际成本低；适合有私有化要求（数据不出内网）的场景。
   - 运维成本不可忽略（模型版本、并发、显存），不建议在只有几个人用的控制面上做。

**建议**：现阶段保留原生实现作为默认（零成本、零依赖、够用），把它当成兜底；
真正要提升体验时，**先换 TTS**（便宜、收益直观、没有延迟焦虑），再考虑 ASR。
两者都换的话，把 `useSpeechRecognition` / `useSpeechSynthesis` 的实现换掉即可，
`VoiceBar` 与页面接入点不需要动。
