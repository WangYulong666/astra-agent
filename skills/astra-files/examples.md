# astra-files 示例

## 场景 1：看看刚跑完的任务产出了什么

```bash
astra-cli session list
```

```
ID                        TITLE                            STATUS  CWD                                       LAST ACTIVE
------------------------  -------------------------------  ------  ----------------------------------------  ----------------
a37c3e69c36d97c0005f97cb  echo hi                          active  /Users/wangyulong/.astra/data/sandbox…   2026-11-06 00:24
9ae0aeaacde84e425176358d  跑测试并总结                       active  /Users/wangyulong/.astra/data/sandbox…   2026-11-05 23:58
```

```bash
astra-cli files ls a37c3e69c36d97c0005f97cb
```

```
TYPE  SIZE     MODIFIED          NAME
----  -------  ----------------  ----------------
dir   -        2026-11-06 00:23  .git
file  412 B    2026-11-06 00:24  hello.txt
file  1.1 kB   2026-11-06 00:24  run.log

/ · 3 项
```

要点：`ls` 只列一层，子目录要再带路径（`files ls <id> src`）。

## 场景 2：看一个小文本文件

```bash
astra-cli files cat a37c3e69c36d97c0005f97cb hello.txt
```

```
hi
```

如果文件超过 512KB：

```
…（前 512KB 内容）…
[已截断：只显示前 524288 字节，完整内容用 files download]
```

第二行在 stderr。看到它就别再基于 stdout 判断文件全貌。

## 场景 3：把产物下载到本地（二进制 / 大文件）

```bash
astra-cli files download a37c3e69c36d97c0005f97cb build.tar.gz --out /tmp/build.tar.gz
```

```
已保存 /tmp/build.tar.gz（18.3 MB）
```

不带 `--out` 时落到当前目录同名文件：

```bash
astra-cli files download a37c3e69c36d97c0005f97cb run.log
```

```
已保存 /Users/wangyulong/program/run.log（1.1 kB）
```

要点：

- 下载失败（文件不存在 / 越界）时退出码非 0，且**不会留下半个文件**——
  实现是先写 `run.log.part` 再改名。
- 二进制文件用 `cat` 只会得到一句提示，必须 `download`。
- 需要脚本消费时加 `--json`：

```bash
astra-cli files download a37c3e69c36d97c0005f97cb run.log --out /tmp/run.log --json
```

```json
{
  "sessionId": "a37c3e69c36d97c0005f97cb",
  "path": "run.log",
  "savedTo": "/tmp/run.log",
  "bytes": 1126
}
```
