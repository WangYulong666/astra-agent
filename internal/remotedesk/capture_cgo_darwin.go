//go:build darwin && cgo

package remotedesk

/*
#cgo CFLAGS: -Wno-deprecated-declarations
#cgo LDFLAGS: -framework CoreGraphics -framework CoreFoundation

#include <stdlib.h>
#include <string.h>
#include <dlfcn.h>
#include <CoreGraphics/CoreGraphics.h>
#include <CoreFoundation/CoreFoundation.h>

// CGWindowListCreateImage 在 macOS 15 SDK 里被标成 obsoleted：直接调用连
// 编译都过不去，但符号还在 CoreGraphics 里。所以这里只声明**自己的**函数
// 指针类型，运行时用 dlsym 去取 —— 符号哪天真被删了，取到 NULL，上层回退
// screencapture，而不是整个抓屏能力消失。
typedef CGImageRef (*astra_cg_window_image_fn)(CGRect, CGWindowListOption, CGWindowID, CGWindowImageOption);

static astra_cg_window_image_fn astra_cg_window_image(void) {
	return (astra_cg_window_image_fn)dlsym(RTLD_DEFAULT, "CGWindowListCreateImage");
}

static int astra_cg_available(void) {
	return astra_cg_window_image() != NULL ? 1 : 0;
}

typedef struct {
	void  *pixels;
	size_t width;
	size_t height;
	size_t stride;
} astra_shot;

// astra_cg_capture 抓一张全屏图像并转成 32 位不透明像素。
//
// 像素布局：kCGImageAlphaPremultipliedFirst | kCGBitmapByteOrder32Little
// → 32 位字是 ARGB，按小端落盘就是内存里的 B,G,R,A（Go 侧 scaleBGRA 负责换回 RGBA）。
//
// 返回值：0 成功；负数失败（Go 侧映射成人话）。失败时 out->pixels 一定是 NULL，
// 调用方不需要额外清理。
static int astra_cg_capture(astra_shot *out) {
	out->pixels = NULL;
	out->width = 0;
	out->height = 0;
	out->stride = 0;

	astra_cg_window_image_fn fn = astra_cg_window_image();
	if (fn == NULL) {
		return -1;
	}

	CGImageRef img = fn(CGRectInfinite, kCGWindowListOptionOnScreenOnly,
		kCGNullWindowID, kCGWindowImageDefault);
	if (img == NULL) {
		return -2;
	}

	size_t w = CGImageGetWidth(img);
	size_t h = CGImageGetHeight(img);
	if (w == 0 || h == 0) {
		CGImageRelease(img);
		return -3;
	}

	size_t stride = w * 4;
	void *buf = malloc(stride * h);
	if (buf == NULL) {
		CGImageRelease(img);
		return -4;
	}

	CGColorSpaceRef cs = CGColorSpaceCreateDeviceRGB();
	if (cs == NULL) {
		free(buf);
		CGImageRelease(img);
		return -5;
	}

	CGContextRef ctx = CGBitmapContextCreate(buf, w, h, 8, stride, cs,
		(CGBitmapInfo)(kCGImageAlphaPremultipliedFirst | kCGBitmapByteOrder32Little));
	CGColorSpaceRelease(cs);
	if (ctx == NULL) {
		free(buf);
		CGImageRelease(img);
		return -6;
	}

	// Copy 而不是 SourceOver：源图带 alpha 时不要和未初始化的缓冲区混合。
	CGContextSetBlendMode(ctx, kCGBlendModeCopy);
	CGContextDrawImage(ctx, CGRectMake(0, 0, (CGFloat)w, (CGFloat)h), img);

	CGContextRelease(ctx);
	CGImageRelease(img);

	out->pixels = buf;
	out->width = w;
	out->height = h;
	out->stride = stride;
	return 0;
}

// astra_cg_free 释放 astra_cg_capture 分配的像素缓冲（C 侧 malloc 必须 C 侧 free）。
static void astra_cg_free(astra_shot *s) {
	if (s->pixels != NULL) {
		free(s->pixels);
		s->pixels = NULL;
	}
}
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"
)

// cgFirstGrabTimeout 是构造时那次「试抓」的上限。屏幕休眠时单帧要 940ms，
// 所以给 3 秒——只用来排除结构性失败（符号在但调用必挂），不是性能断言。
const cgFirstGrabTimeout = 3 * time.Second

// cgDisplayCapture 是 cgo 抓屏后端：CGWindowListCreateImage → BGRA → box 下采样 → JPEG。
//
// 本机实测（Mac mini M4 / macOS 26.2，2560×1440 屏）：单帧 28–31ms，
// 加下采样与 JPEG 编码共 44–47ms，上限 21–23fps；同机 screencapture 是
// 110–140ms/帧（~5fps）。4~5 倍差距全在「不再 fork 一个进程 + 不再落 PNG」。
type cgDisplayCapture struct {
	mu     sync.Mutex
	caps   capabilityCache
	screen Screen
	closed bool
}

// newCGDisplayCapture 造 cgo 抓屏后端。任何一步失败都返回 error，
// 交给 defaultCapture 回退到 screencapture（回退路径必须永远可用）。
func newCGDisplayCapture(cfg Config) (Capture, error) {
	if C.astra_cg_available() == 0 {
		return nil, fmt.Errorf("remotedesk: 运行时找不到 CGWindowListCreateImage 符号")
	}
	c := &cgDisplayCapture{}
	// 首帧试抓：符号在但调用失败（无 GUI 会话、被沙箱拦）也要能被发现。
	// 休眠导致的超时不算结构性失败，照常返回后端（Info 会报不可用）。
	ctx, cancel := context.WithTimeout(context.Background(), cgFirstGrabTimeout)
	defer cancel()
	if _, err := c.Grab(ctx, GrabOptions{MaxWidth: 640, Quality: 50}); err != nil &&
		!errors.Is(err, errScreenSuspended) {
		return nil, err
	}
	if cfg.Logger != nil {
		cfg.Logger.Printf("remotedesk: 抓屏后端选用 cgo-cgwindow（CGWindowListCreateImage）")
	}
	return c, nil
}

func (c *cgDisplayCapture) Info() Capability {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caps.get(c.probeLocked)
}

func (c *cgDisplayCapture) ScreenSize() Screen {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.screen
}

// fastGrab 标记「这个后端够快」，帧流据此把默认帧率上限从 10 放宽到 20。
func (c *cgDisplayCapture) fastGrab() bool { return true }

func (c *cgDisplayCapture) Grab(ctx context.Context, opt GrabOptions) (Frame, error) {
	f, _, err := c.GrabThumb(ctx, opt)
	return f, err
}

func (c *cgDisplayCapture) GrabThumb(ctx context.Context, opt GrabOptions) (Frame, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.grabLocked(ctx, opt)
}

func (c *cgDisplayCapture) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *cgDisplayCapture) grabLocked(ctx context.Context, opt GrabOptions) (Frame, []byte, error) {
	if c.closed {
		return Frame{}, nil, fmt.Errorf("%w: 抓屏后端已关闭", ErrUnavailable)
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return Frame{}, nil, err
		}
	}

	start := time.Now()
	var shot C.astra_shot
	if rc := C.astra_cg_capture(&shot); rc != 0 {
		return Frame{}, nil, fmt.Errorf("remotedesk: cgo 抓屏失败（%s）", cgErrorText(int(rc)))
	}
	// 像素在 C 的堆上，必须拷回 Go 再 free：Go 的 GC 完全不知道这块内存。
	w, h, stride := int(shot.width), int(shot.height), int(shot.stride)
	n := stride * h
	pix := C.GoBytes(unsafe.Pointer(shot.pixels), C.int(n))
	C.astra_cg_free(&shot)
	elapsed := time.Since(start)

	c.screen = Screen{Width: w, Height: h}

	// 休眠检测：正常 28–31ms，休眠时 940ms（本机实测）。超过 500ms 一律
	// 认为屏幕睡了，直接返回特殊错误让帧流退避，别拿这个帧去编码。
	if elapsed >= suspendedGrabThreshold {
		return Frame{}, nil, fmt.Errorf("%w: cgo 单帧耗时 %s", errScreenSuspended, elapsed.Round(time.Millisecond))
	}

	img := scaleBGRA(pix, w, h, stride, opt.MaxWidth)
	return encodeImage(img, opt)
}

// probeLocked 真抓一帧判断可用性（截图是只读的）。全黑说明没给屏幕录制权限，
// 或者显示器睡着——两种情况对用户是两件事，文案里都要写。
func (c *cgDisplayCapture) probeLocked() Capability {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	_, thumb, err := c.grabLocked(ctx, GrabOptions{MaxWidth: 320, Quality: 50})
	if err != nil {
		if errors.Is(err, errScreenSuspended) {
			return Capability{
				Backend: "cgo-cgwindow",
				Reason:  suspendedReason,
				Hint:    hintScreenRecording + "；若已授权，先唤醒显示器再试（caffeinate -u -t 5）",
			}
		}
		return Capability{Backend: "cgo-cgwindow", Reason: clipReason(err.Error())}
	}
	if looksBlack(thumb) {
		return Capability{
			Backend: "cgo-cgwindow",
			Reason:  suspendedReason,
			Hint:    hintScreenRecording + "；若已授权，先唤醒显示器再试（caffeinate -u -t 5）",
		}
	}
	return Capability{Available: true, Backend: "cgo-cgwindow"}
}

// newAcceleratedCapture 让 defaultCapture 在 darwin+cgo 下优先选 cgo 后端。
func newAcceleratedCapture(cfg Config) (Capture, error) {
	return newCGDisplayCapture(cfg)
}

// cgErrorText 把 C 侧的返回码翻成人话（C 侧不写字符串，省一次跨语言缓冲）。
func cgErrorText(code int) string {
	switch code {
	case -1:
		return "运行时没有 CGWindowListCreateImage 符号"
	case -2:
		return "CGWindowListCreateImage 返回空图像（无 GUI 会话？）"
	case -3:
		return "抓到 0×0 图像"
	case -4:
		return "像素缓冲分配失败"
	case -5:
		return "创建色彩空间失败"
	case -6:
		return "创建位图上下文失败"
	default:
		return fmt.Sprintf("未知错误码 %d", code)
	}
}
