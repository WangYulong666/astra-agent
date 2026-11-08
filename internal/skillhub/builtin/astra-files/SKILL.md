---
name: astra-files
description: 读写 Astra Agent 沙箱工作区里的文件——列出产物、看内容、下载到本地
version: 1
requires: [astra-cli]
---
# 何时使用

沙箱里的文件**不在本地磁盘上**，只有这几个动作能碰到它：

- 用户问「刚才那个任务产出了什么」（`files ls`）；
- 要看某个产物的内容（`files cat`）；
- 要把产物拿到本地继续处理、或发给用户（`files download`）。

反过来：用户本地仓库里的文件，直接用常规文件工具读写，别绕沙箱。

# 怎么用

```bash
astra-cli files ls <会话ID> [路径]              # 列目录（目录在前）
astra-cli files cat <会话ID> <路径>             # 直接输出内容（预览，512KB 截断）
astra-cli files download <会话ID> <路径> [--out 本地路径]
```

`files ls` 的输出：

```
TYPE  SIZE     MODIFIED          NAME
----  -------  ----------------  ----------------
dir   -        2026-11-05 23:41  src
file  1.2 kB   2026-11-05 23:42  README.md
file  18.3 MB  2026-11-06 00:02  build.tar.gz

/ · 3 项
```

三个动作对应三个不同的服务端端点，**不能互相替代**：

| 动作 | 端点 | 行为 |
|---|---|---|
| `ls` | `/v1/sessions/{id}/files` | 目录列表，不读内容 |
| `cat` | `/v1/sessions/{id}/file` | JSON 预览：512KB 截断、二进制不返内容 |
| `download` | `/v1/sessions/{id}/download` | 原始字节，落盘必须用这个 |

所以：**不要用 `cat` 的内容自己拼文件**——它可能被截断、可能对二进制返回空，
写出来的文件是坏的。要落盘一律 `download`。

# 注意

- `cat` 一个二进制文件时，stdout 什么都不输出，stderr 会提示
  「是二进制文件，用 files download 落盘」——这是正常的，不是失败。
- `cat` 大文件会在 stderr 提示「已截断」。看到这个提示就必须改用 `download`，
  否则你会基于残缺内容做判断。
- 路径是**沙箱工作区内的相对路径**，不接受绝对路径与 `..`（服务端会 403）。
- `download` 默认落到当前目录的原文件名，先写 `.part` 再改名，
  中途失败不会留下半个文件骗人。
- 会话 ID 不知道就先 `astra-cli session list`；下载前确认会话还在
  （沙箱被回收后会 404）。
- 沙箱是临时的：长期要留的产物必须 `download` 到本地，别指望它一直在。
