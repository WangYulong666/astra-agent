package remotedesk

import (
	"image"
	"testing"
)

// 下采样尺寸：**高度必须跟着缩**。主控的探针里踩过「1920×1080 缩成 1280×1080」
// 的 bug——画面被压扁，前端坐标全歪，而且肉眼在小图上不明显。
func TestDownscaleSize(t *testing.T) {
	cases := []struct {
		sw, sh, maxW int
		wantW, wantH int
	}{
		{1920, 1080, 1280, 1280, 720},
		{2560, 1440, 1280, 1280, 720},
		{3840, 2160, 1280, 1280, 720},
		{1920, 1080, 0, 1920, 1080},  // MaxWidth=0 = 原始尺寸
		{1280, 720, 1280, 1280, 720}, // 正好等于上限：不缩
		{800, 600, 1600, 800, 600},   // 比上限小：不放大
		{1000, 3, 100, 100, 1},       // 极端细长：高至少 1 像素
		{0, 0, 1280, 1, 1},           // 空图兜底
	}
	for _, c := range cases {
		gotW, gotH := downscaleSize(c.sw, c.sh, c.maxW)
		if gotW != c.wantW || gotH != c.wantH {
			t.Fatalf("downscaleSize(%d,%d,%d) = %d×%d，期望 %d×%d",
				c.sw, c.sh, c.maxW, gotW, gotH, c.wantW, c.wantH)
		}
	}
}

// box 平均 + 通道换序：BGRA 进、RGBA 出。
func TestScaleBGRABoxAverage(t *testing.T) {
	// 4×4，左半边白、右半边黑（按 B,G,R,A 存）。
	const sw, sh = 4, 4
	stride := sw * 4
	pix := make([]byte, stride*sh)
	for y := 0; y < sh; y++ {
		for x := 0; x < sw; x++ {
			off := y*stride + x*4
			if x < 2 {
				// 白色：B=G=R=0xff
				pix[off], pix[off+1], pix[off+2], pix[off+3] = 0xff, 0xff, 0xff, 0xff
			} else {
				pix[off], pix[off+1], pix[off+2], pix[off+3] = 0, 0, 0, 0xff
			}
		}
	}

	dst := scaleBGRA(pix, sw, sh, stride, 2)
	if b := dst.Bounds(); b.Dx() != 2 || b.Dy() != 2 {
		t.Fatalf("下采样尺寸 %d×%d，期望 2×2", b.Dx(), b.Dy())
	}
	if v := dst.Pix[0]; v < 250 {
		t.Fatalf("左上（原图左半边白）应接近白色，实际 R=%d", v)
	}
	if v := dst.Pix[4]; v > 5 {
		t.Fatalf("右上（原图右半边黑）应接近黑色，实际 R=%d", v)
	}
	// 通道顺序：红色分量必须落在 RGBA 的第 0 字节。
	if dst.Pix[1] != dst.Pix[0] || dst.Pix[2] != dst.Pix[0] {
		t.Fatalf("白像素应 R=G=B，实际 %d,%d,%d", dst.Pix[0], dst.Pix[1], dst.Pix[2])
	}
	if dst.Pix[3] != 0xff {
		t.Fatalf("alpha 必须补成不透明，实际 %d", dst.Pix[3])
	}
}

// 通道换序单独验一遍：纯红像素（B=0,G=0,R=255）换完必须还是纯红。
func TestScaleBGRASwapsChannels(t *testing.T) {
	pix := []byte{
		0x00, 0x00, 0xff, 0xff, // 一个像素：B=0 G=0 R=255
	}
	dst := scaleBGRA(pix, 1, 1, 4, 1)
	if got := dst.Pix[0]; got != 0xff {
		t.Fatalf("R 应为 255，实际 %d（通道没换？）", got)
	}
	if got := dst.Pix[2]; got != 0x00 {
		t.Fatalf("B 应为 0，实际 %d（通道没换？）", got)
	}
}

// 步长大于 宽×4（CG 有时会按 16 字节对齐分配）时不能读错行。
func TestScaleBGRAPaddedStride(t *testing.T) {
	const sw, sh = 2, 2
	stride := sw*4 + 8 // 每行多 8 字节 padding
	pix := make([]byte, stride*sh)
	for y := 0; y < sh; y++ {
		for x := 0; x < sw; x++ {
			off := y*stride + x*4
			pix[off], pix[off+1], pix[off+2], pix[off+3] = 0x10, 0x20, 0x30, 0xff
		}
	}
	dst := scaleBGRA(pix, sw, sh, stride, sw)
	if dst.Bounds().Dx() != sw || dst.Bounds().Dy() != sh {
		t.Fatalf("尺寸 %v", dst.Bounds())
	}
	// B=0x10 G=0x20 R=0x30 → RGBA = 0x30,0x20,0x10
	if dst.Pix[0] != 0x30 || dst.Pix[1] != 0x20 || dst.Pix[2] != 0x10 {
		t.Fatalf("通道/行偏移不对：%x %x %x", dst.Pix[0], dst.Pix[1], dst.Pix[2])
	}
}

// 缓冲区不够时不能越界读（宁可变 1×1 也不能 panic）。
func TestScaleBGRAShortBuffer(t *testing.T) {
	dst := scaleBGRA([]byte{1, 2, 3}, 4, 4, 16, 2)
	if b := dst.Bounds(); b.Dx() != 1 || b.Dy() != 1 {
		t.Fatalf("短缓冲应退回 1×1，实际 %v", b)
	}
	if _, ok := any(dst).(*image.RGBA); !ok {
		t.Fatal("返回类型必须是 *image.RGBA")
	}
}
