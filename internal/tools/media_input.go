package tools

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
)

// inputImageRawBudget 是 img2img 数据 URI 中允许封装的最大 JPEG 字节数。上游图像接口
// 接受的 data URI 总长度约为 80KB；base64 会让载荷增大约 33%，因此把原始 JPEG
// 限制为 50KB，可使最终 URI 稳定低于该阈值。
const inputImageRawBudget = 50 * 1024

// prepareInputImage 为上游 API 规范化一项 input_images 输入。
//
//   - http(s):// URL 原样传递，由 Provider 自行获取，无法对 URL 设置有意义的大小上限。
//   - data: URI 和本地文件路径会被解码、缩小，并反复编码为 JPEG，直到符合
//     inputImageRawBudget，随后以 data URI 返回。
//
// workDir 用于解析相对路径。文件不可读或图像无法解码时返回 ("", err)。
func prepareInputImage(in, workDir string) (string, error) {
	if in == "" {
		return "", errors.New("empty input image")
	}
	low := strings.ToLower(in)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
		return in, nil
	}

	var raw []byte
	switch {
	case strings.HasPrefix(low, "data:"):
		b, err := decodeDataURI(in)
		if err != nil {
			return "", err
		}
		raw = b
	default:
		path := in
		if !filepath.IsAbs(path) && workDir != "" {
			path = filepath.Join(workDir, path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read input image %q: %w", in, err)
		}
		raw = b
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("decode input image: %w", err)
	}

	jpegBytes, err := shrinkToBudget(img, inputImageRawBudget)
	if err != nil {
		return "", err
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpegBytes), nil
}

// shrinkToBudget 把 img 编码为 JPEG，先降低质量，再将尺寸减半，直到编码字节数不超过
// budget。函数返回能够生成的最小编码结果；只有连 8x8 缩略图都无法放入时才报错，
// 这通常表示预算小得不合理。
func shrinkToBudget(img image.Image, budget int) ([]byte, error) {
	qualities := []int{82, 65, 45, 28}
	for {
		for _, q := range qualities {
			buf := &bytes.Buffer{}
			if err := jpeg.Encode(buf, img, &jpeg.Options{Quality: q}); err != nil {
				return nil, fmt.Errorf("encode jpeg: %w", err)
			}
			if buf.Len() <= budget {
				return buf.Bytes(), nil
			}
		}
		b := img.Bounds()
		w, h := b.Dx()/2, b.Dy()/2
		if w < 8 || h < 8 {
			return nil, fmt.Errorf("cannot shrink image under %d bytes", budget)
		}
		img = halve(img)
	}
}

// halve 使用 2x2 方框平均把 img 的宽高各缩小一半。方框滤波开销低且缩小结果清晰；
// 由于结果用于 img2img 模型，不需要精确重建。
func halve(src image.Image) image.Image {
	sb := src.Bounds()
	w, h := sb.Dx()/2, sb.Dy()/2
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			sx, sy := sb.Min.X+x*2, sb.Min.Y+y*2
			r1, g1, b1, a1 := src.At(sx, sy).RGBA()
			r2, g2, b2, a2 := src.At(sx+1, sy).RGBA()
			r3, g3, b3, a3 := src.At(sx, sy+1).RGBA()
			r4, g4, b4, a4 := src.At(sx+1, sy+1).RGBA()
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8((r1 + r2 + r3 + r4) >> 10),
				G: uint8((g1 + g2 + g3 + g4) >> 10),
				B: uint8((b1 + b2 + b3 + b4) >> 10),
				A: uint8((a1 + a2 + a3 + a4) >> 10),
			})
		}
	}
	return dst
}

// validateVideoInputURL 要求 generate_video 的输入图像是公开 http(s) URL。上游视频接口
// 只接受 URL；本地路径和 data URI 会触发其 base64 或获取异常，使异步任务以含义不清的
// "Invalid image" 失败，过去调用方会把它误读为普通 "parse error" 并反复重试。
func validateVideoInputURL(in string) error {
	if in == "" {
		return errors.New("empty input image")
	}
	low := strings.ToLower(in)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
		return nil
	}
	if strings.HasPrefix(low, "data:") {
		return fmt.Errorf("video input_images must be public http(s) URLs; data URIs are not accepted by the upstream API")
	}
	return fmt.Errorf("video input_images must be public http(s) URLs (got %q); upload the image to a reachable URL first", in)
}

// decodeDataURI 从 "data:[mime];base64,..." 字符串返回原始字节。API 只支持 base64
// 往返，因此拒绝非 base64 的 data URI。
func decodeDataURI(s string) ([]byte, error) {
	comma := strings.IndexByte(s, ',')
	if comma < 0 {
		return nil, errors.New("data URI missing comma separator")
	}
	header := s[:comma]
	if !strings.Contains(header, ";base64") {
		return nil, errors.New("data URI must be base64-encoded")
	}
	return base64.StdEncoding.DecodeString(s[comma+1:])
}
