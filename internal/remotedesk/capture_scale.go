package remotedesk

import (
	"image"
	"math"
)

// ── 下采样（纯函数，任何平台可测） ──────────────────────────────
//
// 单独拆出来是因为这块最容易踩坑，而且踩了很难看出来：
//   - 只缩宽不缩高 → 帧变成「压扁的」，前端坐标系全歪（主控的探针里踩过
//     1920×1080 缩成 1280×1080 的 bug）；
//   - 缩小时 ratio 可能算出 0 → 每个目标像素取到空源区域，画面全黑。

// downscaleSize 算出 box 下采样后的目标尺寸：宽不超过 maxW，**高按同一比例缩**。
//
// maxW<=0 或源图本来就不宽于 maxW 时不放大（放大只会烧 CPU 还更糊）。
func downscaleSize(sw, sh, maxW int) (int, int) {
	if sw <= 0 || sh <= 0 {
		return 1, 1
	}
	if maxW <= 0 || sw <= maxW {
		return sw, sh
	}
	dw := maxW
	dh := int(math.Round(float64(sh) * float64(dw) / float64(sw)))
	if dh < 1 {
		dh = 1
	}
	if dh > sh {
		dh = sh
	}
	return dw, dh
}

// scaleBGRA 把 CGWindowListCreateImage 出来的原始像素（BGRA，行步长可能大于
// 宽×4）按 box 平均缩到 maxW 宽，同时把通道换成 RGBA。
//
// 为什么在 C 之外自己做：cgo 那条路已经是原始像素了，先拼一张 image.RGBA
// 再走通用 downscale 会多两次整图拷贝（15MB 级别的 memcpy），在 20fps 下
// 就是白烧 20% 的 CPU。
func scaleBGRA(pix []byte, sw, sh, stride, maxW int) *image.RGBA {
	if sw <= 0 || sh <= 0 || stride < sw*4 || len(pix) < stride*sh {
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}
	dw, dh := downscaleSize(sw, sh, maxW)
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))

	if dw == sw && dh == sh {
		// 不缩尺寸：只换通道，一遍扫过去。
		for y := 0; y < sh; y++ {
			srcOff := y * stride
			dstOff := y * dst.Stride
			for x := 0; x < sw; x++ {
				dst.Pix[dstOff] = pix[srcOff+2] // R ← 第 3 字节
				dst.Pix[dstOff+1] = pix[srcOff+1]
				dst.Pix[dstOff+2] = pix[srcOff] // B ← 第 1 字节
				dst.Pix[dstOff+3] = 0xff
				srcOff += 4
				dstOff += 4
			}
		}
		return dst
	}

	for y := 0; y < dh; y++ {
		y0 := y * sh / dh
		y1 := (y + 1) * sh / dh
		if y1 <= y0 { // 高比目标还小时保证至少取一行
			y1 = y0 + 1
		}
		for x := 0; x < dw; x++ {
			x0 := x * sw / dw
			x1 := (x + 1) * sw / dw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, b, n uint32
			for sy := y0; sy < y1; sy++ {
				off := sy*stride + x0*4
				for sx := x0; sx < x1; sx++ {
					b += uint32(pix[off])
					g += uint32(pix[off+1])
					r += uint32(pix[off+2])
					off += 4
					n++
				}
			}
			i := y*dst.Stride + x*4
			if n > 0 {
				dst.Pix[i] = uint8(r / n)
				dst.Pix[i+1] = uint8(g / n)
				dst.Pix[i+2] = uint8(b / n)
			}
			dst.Pix[i+3] = 0xff
		}
	}
	return dst
}
