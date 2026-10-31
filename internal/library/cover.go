package library

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"vrclib/internal/core"
)

// Covers.
//
// A card shows, in this order: the picture the player picked, the one made out of Unity, the one read out
// of a unitypackage, the Booth product's, and whatever picture lies in the asset's own folder. The first
// three are kept in the data folder, so that moving the library to another computer takes them along.

// OwnCoverDir: where the pictures the player picked are kept.
func OwnCoverDir() string { return filepath.Join(core.CoversDir(), "own") }

// genCoverDir: where the pictures made out of Unity are kept.
func genCoverDir() string { return filepath.Join(core.CoversDir(), "generated") }

// genCoverFile: where a card's picture made out of Unity is kept. Named after the key (hashed: a key holds
// characters a file name cannot).
func genCoverFile(key string) string {
	sum := sha1.Sum([]byte(key))
	return filepath.Join(genCoverDir(), hex.EncodeToString(sum[:])[:20]+".png")
}

// GeneratedCover: the picture made out of Unity for a card, "" when there is none.
func GeneratedCover(key string) string {
	if p := genCoverFile(key); core.StatOK(p) {
		return p
	}
	return ""
}

// SaveGeneratedCover keeps a picture rendered in Unity as a card's cover.
func SaveGeneratedCover(key string, b []byte) (string, error) {
	if pictureExt(b) == "" {
		return "", errors.New("Unity 生成的不是图片")
	}
	dst := genCoverFile(key)
	if err := os.MkdirAll(genCoverDir(), 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(dst+".part", b, 0644); err != nil {
		return "", err
	}
	if err := os.Rename(dst+".part", dst); err != nil {
		_ = os.Remove(dst + ".part")
		return "", err
	}
	return dst, nil
}

// pictureExt: the format a file begins with — whatever its name says. "" for anything that is no picture.
func pictureExt(head []byte) string {
	switch http.DetectContentType(head) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	}
	return ""
}

// CopyOwnCover copies a picture the player picked into the data folder and answers where it went. The
// picture itself is left alone; the copy is named after what is in it, so the same picture picked twice is
// the same copy.
func CopyOwnCover(src string) (string, error) {
	b, err := os.ReadFile(src)
	if err != nil {
		return "", errors.New("无法读取所选文件：" + core.TrimErr(err))
	}
	ext := pictureExt(b)
	if ext == "" {
		return "", errors.New("所选文件不是图片")
	}
	sum := sha1.Sum(b)
	dst := filepath.Join(OwnCoverDir(), "own-"+hex.EncodeToString(sum[:])[:16]+ext)
	if core.StatOK(dst) {
		return dst, nil // the same picture is already kept
	}
	if err := os.MkdirAll(OwnCoverDir(), 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(dst+".part", b, 0644); err != nil {
		return "", err
	}
	if err := os.Rename(dst+".part", dst); err != nil {
		_ = os.Remove(dst + ".part")
		return "", err
	}
	return dst, nil
}

// RemoveGeneratedCover drops the cover that was made for a card out of Unity.
func RemoveGeneratedCover(key string) {
	_ = os.Remove(genCoverFile(key))
	core.BumpRev()
}

// ---------- thumbnails ----------

// Thumbnail: a small copy of a picture, no wider than wd, made once and kept in the data folder. It answers
// "" when the picture is to be served as it is — it is small enough already, or of a format Go does not
// decode (WebP), which the window's browser does itself.
func Thumbnail(p string, wd int) string {
	if wd <= 0 || !ImageExt[core.LowerExt(p)] {
		return ""
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Size() < 64<<10 { // a small file is not worth a second copy
		return ""
	}
	sum := sha1.Sum([]byte(core.PathKey(p) + "|" + strconv.FormatInt(fi.ModTime().UnixNano(), 10) + "|" + strconv.Itoa(wd)))
	dst := filepath.Join(core.DataDir, "thumbs", hex.EncodeToString(sum[:])[:20]+".jpg")
	if core.StatOK(dst) {
		return dst
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	img, err := decodePicture(b)
	if err != nil || img == nil {
		return ""
	}
	box := img.Bounds()
	if box.Dx() <= wd {
		return "" // no smaller than it is
	}
	small := scaleDown(img, wd)
	var out bytes.Buffer
	if jpeg.Encode(&out, small, &jpeg.Options{Quality: 82}) != nil {
		return ""
	}
	if os.MkdirAll(filepath.Dir(dst), 0755) != nil {
		return ""
	}
	if os.WriteFile(dst+".part", out.Bytes(), 0644) != nil {
		return ""
	}
	if os.Rename(dst+".part", dst) != nil {
		_ = os.Remove(dst + ".part")
		return ""
	}
	return dst
}

// decodePicture reads the formats Go carries. Anything else is left to the window.
func decodePicture(b []byte) (image.Image, error) {
	switch pictureExt(b) {
	case ".jpg":
		return jpeg.Decode(bytes.NewReader(b))
	case ".png":
		return png.Decode(bytes.NewReader(b))
	case ".gif":
		return gif.Decode(bytes.NewReader(b))
	}
	return nil, errors.New("not a picture Go reads")
}

// scaleDown makes a copy no wider than wd, by averaging the pixels that fall into each new one (a box
// filter: good enough for a card's picture, and it needs nothing from outside).
func scaleDown(src image.Image, wd int) image.Image {
	b := src.Bounds()
	if b.Dx() <= wd || wd < 1 {
		wd = b.Dx()
	}
	ht := b.Dy() * wd / b.Dx()
	if ht < 1 {
		ht = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, wd, ht))
	for y := 0; y < ht; y++ {
		y0, y1 := b.Min.Y+y*b.Dy()/ht, b.Min.Y+(y+1)*b.Dy()/ht
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < wd; x++ {
			x0, x1 := b.Min.X+x*b.Dx()/wd, b.Min.X+(x+1)*b.Dx()/wd
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, a, n uint64
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					cr, cg, cb, ca := src.At(xx, yy).RGBA()
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			if n == 0 {
				continue
			}
			dst.Set(x, y, color.RGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(bl / n >> 8), uint8(a / n >> 8)})
		}
	}
	return dst
}
