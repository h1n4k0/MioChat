package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

const attachmentMaxDimension = 4096
const attachmentMaxPixels int64 = 8 * 1024 * 1024
const attachmentMaxFrames = 60

var attachmentMIMEs = []string{"image/png", "image/jpeg", "image/gif", "text/plain", "text/markdown", "text/csv", "application/json", "application/pdf"}

func attachmentFilenameValid(name, mime string) bool {
	if name == "" || len(name) > 180 || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 100 || strings.TrimSpace(name) != name || strings.HasSuffix(name, ".") || strings.ContainsAny(name, `/\\:`) || name == "." || name == ".." {
		return false
	}
	for _, c := range name {
		if unicode.IsControl(c) || unicode.In(c, unicode.Cf, unicode.Zl, unicode.Zp) || strings.ContainsRune(`<>"|?*`, c) {
			return false
		}
	}
	stem := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || (len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '0' && stem[3] <= '9') {
		return false
	}
	ext := strings.ToLower(path.Ext(name))
	switch mime {
	case "image/png":
		return ext == ".png"
	case "image/jpeg":
		return ext == ".jpg" || ext == ".jpeg"
	case "image/gif":
		return ext == ".gif"
	case "text/plain":
		return ext == ".txt"
	case "text/markdown":
		return ext == ".md"
	case "text/csv":
		return ext == ".csv"
	case "application/json":
		return ext == ".json"
	case "application/pdf":
		return ext == ".pdf"
	}
	return false
}

func attachmentDigest(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func attachmentSHAValid(digest string) bool {
	if len(digest) != 64 || strings.ToLower(digest) != digest {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func attachmentImageBounds(w, h int) bool {
	return w > 0 && h > 0 && w <= attachmentMaxDimension && h <= attachmentMaxDimension && int64(w)*int64(h) <= attachmentMaxPixels
}

// Preflight GIF dimensions, frame count and cumulative full-canvas decode
// budget before gif.DecodeAll can allocate even its first image. No unbounded
// animation decoding, archive extraction, external decoder or new dependency.
func attachmentGIFBudget(data []byte) (int, int, int, error) {
	bad := errors.New("invalid or oversized GIF")
	if len(data) < 14 || (string(data[:6]) != "GIF87a" && string(data[:6]) != "GIF89a") {
		return 0, 0, 0, bad
	}
	w, h := int(binary.LittleEndian.Uint16(data[6:8])), int(binary.LittleEndian.Uint16(data[8:10]))
	if !attachmentImageBounds(w, h) {
		return 0, 0, 0, bad
	}
	i, frames := 13, 0
	if data[10]&0x80 != 0 {
		i += 3 * (1 << ((data[10] & 7) + 1))
	}
	skipBlocks := func() bool {
		for i < len(data) {
			n := int(data[i])
			i++
			if n == 0 {
				return true
			}
			if n > len(data)-i {
				return false
			}
			i += n
		}
		return false
	}
	for i < len(data) {
		tag := data[i]
		i++
		switch tag {
		case 0x3b:
			if i != len(data) || frames == 0 {
				return 0, 0, 0, bad
			}
			return w, h, frames, nil
		case 0x21:
			if i >= len(data) {
				return 0, 0, 0, bad
			}
			i++
			if !skipBlocks() {
				return 0, 0, 0, bad
			}
		case 0x2c:
			if len(data)-i < 9 {
				return 0, 0, 0, bad
			}
			x, y := int(binary.LittleEndian.Uint16(data[i:i+2])), int(binary.LittleEndian.Uint16(data[i+2:i+4]))
			fw, fh := int(binary.LittleEndian.Uint16(data[i+4:i+6])), int(binary.LittleEndian.Uint16(data[i+6:i+8]))
			packed := data[i+8]
			i += 9
			frames++
			if !attachmentImageBounds(fw, fh) || x+fw > w || y+fh > h || frames > attachmentMaxFrames || int64(w)*int64(h)*int64(frames) > attachmentMaxPixels {
				return 0, 0, 0, bad
			}
			if packed&0x80 != 0 {
				i += 3 * (1 << ((packed & 7) + 1))
			}
			if i >= len(data) || data[i] < 2 || data[i] > 8 {
				return 0, 0, 0, bad
			}
			i++
			if !skipBlocks() {
				return 0, 0, 0, bad
			}
		default:
			return 0, 0, 0, bad
		}
	}
	return 0, 0, 0, bad
}

func validateAttachmentContent(data []byte, info attachmentInfo) (attachmentInfo, error) {
	bad := errors.New("文件内容、类型、摘要或图像资源预算校验失败")
	if len(data) == 0 || int64(len(data)) != info.Size || info.Size > attachmentItemLimit || !attachmentFilenameValid(info.Filename, info.MIME) || attachmentDigest(data) != info.SHA256 {
		return attachmentInfo{}, bad
	}
	info.MetadataPolicy = "preserve_original"
	switch info.MIME {
	case "image/png", "image/jpeg":
		r := bytes.NewReader(data)
		var cfg image.Config
		var err error
		if info.MIME == "image/png" {
			cfg, err = png.DecodeConfig(r)
		} else {
			cfg, err = jpeg.DecodeConfig(r)
		}
		if err != nil || !attachmentImageBounds(cfg.Width, cfg.Height) {
			return attachmentInfo{}, bad
		}
		r.Reset(data)
		var im image.Image
		if info.MIME == "image/png" {
			im, err = png.Decode(r)
		} else {
			im, err = jpeg.Decode(r)
		}
		if err != nil || im.Bounds().Dx() != cfg.Width || im.Bounds().Dy() != cfg.Height {
			return attachmentInfo{}, bad
		}
		if info.MIME == "image/png" && r.Len() != 0 {
			return attachmentInfo{}, bad
		}
		if info.MIME == "image/jpeg" && (len(data) < 2 || data[len(data)-2] != 0xff || data[len(data)-1] != 0xd9) {
			return attachmentInfo{}, bad
		}
		info.Width, info.Height, info.Frames = cfg.Width, cfg.Height, 1
	case "image/gif":
		w, h, n, err := attachmentGIFBudget(data)
		if err != nil {
			return attachmentInfo{}, bad
		}
		decoded, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil || len(decoded.Image) != n || decoded.Config.Width != w || decoded.Config.Height != h {
			return attachmentInfo{}, bad
		}
		info.Width, info.Height, info.Frames = w, h, n
	case "text/plain", "text/markdown", "text/csv", "application/json":
		if !utf8.Valid(data) {
			return attachmentInfo{}, bad
		}
		for _, c := range string(data) {
			if unicode.IsControl(c) && c != '\n' && c != '\r' && c != '\t' {
				return attachmentInfo{}, bad
			}
		}
		if info.MIME == "application/json" && !json.Valid(data) {
			return attachmentInfo{}, bad
		}
	case "application/pdf":
		// Structural signature only, NOT an antivirus/sandbox guarantee. PDF is
		// explicit download-only, never inline rendered or executed by MioChat.
		if len(data) < 12 || !bytes.HasPrefix(data, []byte("%PDF-")) || !bytes.HasSuffix(bytes.TrimSpace(data), []byte("%%EOF")) {
			return attachmentInfo{}, bad
		}
	default:
		return attachmentInfo{}, bad
	}
	return info, nil
}
