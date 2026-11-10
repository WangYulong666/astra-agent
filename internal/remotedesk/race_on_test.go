//go:build race

package remotedesk

// raceEnabled 让真机性能断言在 -race 下自动放宽：race detector 会给每次内存
// 访问插桩，cgo 边界与整图循环的成本能翻 4 倍以上，这时候断言「<80ms/帧」
// 测的是插桩开销而不是抓屏速度。
const raceEnabled = true
