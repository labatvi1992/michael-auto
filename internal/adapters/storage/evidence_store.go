package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/labatvi1992/michael-auto/internal/domain"
)

const (
	defaultMaxEvidenceVaultBytes int64 = 150 * 1024 * 1024 // 150 MB hard disk cap (P1-4)
	maxEvidenceImageWidth              = 1600              // Downscale huge 4K/HiDPI PNGs cleanly after masking
)

var safeSlugRegex = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// MaskPNGEditorRects masks private code/chat editor bounding boxes on a valid PNG image at 1:1 scale.
func MaskPNGEditorRects(pngBytes []byte, rects []domain.RedactRect) ([]byte, int) {
	return MaskPNGEditorRectsWithFrame(pngBytes, rects, nil, "window")
}

// MaskPNGEditorRectsWithFrame masks private code/chat editor bounding boxes with HiDPI scale compensation
// and cropped-window origin offset correction (P0-2) before hashing and persisting to disk.
func MaskPNGEditorRectsWithFrame(
	pngBytes []byte,
	rects []domain.RedactRect,
	windowFrame *domain.RedactRect,
	captureScope string,
) ([]byte, int) {
	if len(pngBytes) == 0 {
		return pngBytes, 0
	}
	srcImg, _, err := image.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		// Synthetic non-decodable mock header bytes in lightweight unit tests pass through unchanged.
		return pngBytes, 0
	}

	bounds := srcImg.Bounds()
	imgW := bounds.Dx()
	imgH := bounds.Dy()
	if imgW <= 0 || imgH <= 0 {
		return pngBytes, 0
	}

	scaleX := 1.0
	scaleY := 1.0
	offsetX := 0
	offsetY := 0
	scope := strings.ToLower(strings.TrimSpace(captureScope))
	if windowFrame != nil && windowFrame.Width > 0 && windowFrame.Height > 0 {
		if scope == "" || scope == "window" {
			scaleX = float64(imgW) / float64(windowFrame.Width)
			scaleY = float64(imgH) / float64(windowFrame.Height)
			// Guard against wild ratios if metadata is stale.
			if scaleX < 0.25 || scaleX > 4.0 {
				scaleX = 1.0
			}
			if scaleY < 0.25 || scaleY > 4.0 {
				scaleY = 1.0
			}
			offsetX = windowFrame.X
			offsetY = windowFrame.Y
		} else if scope == "desktop" {
			// Full-desktop capture on HiDPI displays: preserve origin (0,0), infer integer scale if > 1.
			if float64(imgW)/float64(windowFrame.Width) >= 1.75 && float64(imgW)/float64(windowFrame.Width) <= 2.25 {
				scaleX = 2.0
				scaleY = 2.0
			}
		}
	}

	dst := image.NewNRGBA(image.Rect(0, 0, imgW, imgH))
	draw.Draw(dst, dst.Bounds(), srcImg, bounds.Min, draw.Src)

	maskFill := image.NewUniform(color.NRGBA{R: 11, G: 17, B: 32, A: 255})
	borderCol := image.NewUniform(color.NRGBA{R: 59, G: 130, B: 246, A: 255})

	applied := 0
	for _, r := range rects {
		if r.Width <= 0 || r.Height <= 0 {
			continue
		}
		adjX := r.X - offsetX
		adjY := r.Y - offsetY
		x0 := int(math.Floor(float64(adjX) * scaleX))
		y0 := int(math.Floor(float64(adjY) * scaleY))
		x1 := int(math.Ceil(float64(adjX+r.Width) * scaleX))
		y1 := int(math.Ceil(float64(adjY+r.Height) * scaleY))

		box := image.Rect(x0, y0, x1, y1).Intersect(dst.Bounds())
		if box.Empty() {
			continue
		}
		draw.Draw(dst, box, maskFill, image.Point{}, draw.Src)
		draw.Draw(dst, image.Rect(box.Min.X, box.Min.Y, box.Max.X, minInt(box.Max.Y, box.Min.Y+2)), borderCol, image.Point{}, draw.Src)
		draw.Draw(dst, image.Rect(box.Min.X, maxInt(box.Min.Y, box.Max.Y-2), box.Max.X, box.Max.Y), borderCol, image.Point{}, draw.Src)
		applied++
	}

	outImg := image.Image(dst)
	if imgW > maxEvidenceImageWidth {
		outImg = downscaleNRGBA(dst, maxEvidenceImageWidth)
	}

	if applied == 0 && imgW <= maxEvidenceImageWidth {
		return pngBytes, 0
	}

	var encoded bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&encoded, outImg); err != nil {
		return pngBytes, applied
	}
	return encoded.Bytes(), applied
}

// OptimizeEvidencePNG compresses/downscales oversized PNGs (> 1600px wide) to keep Evidence Vault lean (P1-4).
func OptimizeEvidencePNG(pngBytes []byte) []byte {
	if len(pngBytes) < 128*1024 {
		return pngBytes
	}
	srcImg, _, err := image.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return pngBytes
	}
	b := srcImg.Bounds()
	var outImg image.Image = srcImg
	if b.Dx() > maxEvidenceImageWidth {
		nrgba := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		draw.Draw(nrgba, nrgba.Bounds(), srcImg, b.Min, draw.Src)
		outImg = downscaleNRGBA(nrgba, maxEvidenceImageWidth)
	}
	var encoded bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&encoded, outImg); err != nil || encoded.Len() >= len(pngBytes) {
		return pngBytes
	}
	return encoded.Bytes()
}

func downscaleNRGBA(src *image.NRGBA, maxW int) *image.NRGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw <= maxW || sw <= 0 || sh <= 0 {
		return src
	}
	dh := int(math.Round(float64(sh) * float64(maxW) / float64(sw)))
	if dh < 1 {
		dh = 1
	}
	dst := image.NewNRGBA(image.Rect(0, 0, maxW, dh))
	for y := 0; y < dh; y++ {
		sy := y * sh / dh
		for x := 0; x < maxW; x++ {
			sx := x * sw / maxW
			sOff := src.PixOffset(sb.Min.X+sx, sb.Min.Y+sy)
			dOff := dst.PixOffset(x, y)
			copy(dst.Pix[dOff:dOff+4], src.Pix[sOff:sOff+4])
		}
	}
	return dst
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// EvidenceVault stores pre-actuation verification screenshots and SHA-256 metadata
// under $XDG_STATE_HOME/michael-auto/evidence/<runID>/ with 0700/0600 permissions,
// bounded by both maxRuns and maxVaultBytes (default 150 MB).
type EvidenceVault struct {
	mu            sync.Mutex
	rootDir       string
	maxRuns       int
	maxVaultBytes int64
	redactHelper  *JSONLAuditLogger
}

// DefaultXDGEvidenceRoot returns $XDG_STATE_HOME/michael-auto/evidence.
func DefaultXDGEvidenceRoot() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			base = filepath.Join(home, ".local", "state")
		} else {
			base = os.TempDir()
		}
	}
	return filepath.Join(base, "michael-auto", "evidence")
}

// NewEvidenceVault initializes an EvidenceVault with automatic run-count & disk-size pruning.
func NewEvidenceVault(customRoot string, maxRuns int) *EvidenceVault {
	if strings.TrimSpace(customRoot) == "" {
		customRoot = DefaultXDGEvidenceRoot()
	}
	if maxRuns < 1 {
		maxRuns = 20
	}
	return &EvidenceVault{
		rootDir:       filepath.Clean(customRoot),
		maxRuns:       maxRuns,
		maxVaultBytes: defaultMaxEvidenceVaultBytes,
		redactHelper:  NewJSONLAuditLogger(""),
	}
}

// SetRetentionRuns updates how many switch run directories are kept on disk.
func (v *EvidenceVault) SetRetentionRuns(maxRuns int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if maxRuns >= 1 {
		v.maxRuns = maxRuns
	}
}

// SetMaxVaultBytes configures the hard disk size cap for Evidence Vault pruning.
func (v *EvidenceVault) SetMaxVaultBytes(maxBytes int64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if maxBytes > 0 {
		v.maxVaultBytes = maxBytes
	}
}

// RootDir returns the canonical vault directory.
func (v *EvidenceVault) RootDir() string {
	return v.rootDir
}

// Clear removes all stored Evidence screenshots and metadata runs inside rootDir.
func (v *EvidenceVault) Clear() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	entries, err := os.ReadDir(v.rootDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		_ = os.RemoveAll(filepath.Join(v.rootDir, entry.Name()))
	}
	return nil
}

// SaveEvidence validates PNG bytes, computes SHA-256, writes <runID>/<step>.png and <step>.meta.json (0600),
// and prunes old runs beyond maxRuns or maxVaultBytes.
func (v *EvidenceVault) SaveEvidence(
	runID string,
	step string,
	pngBytes []byte,
	meta domain.EvidenceRecord,
) (domain.EvidenceRecord, error) {
	if len(pngBytes) == 0 {
		return domain.EvidenceRecord{}, errors.New("dữ liệu ảnh chụp Evidence trống — từ chối thực thi")
	}

	pngBytes = OptimizeEvidencePNG(pngBytes)

	v.mu.Lock()
	defer v.mu.Unlock()

	now := time.Now().UTC()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	cleanRun := sanitizeSlug(runID)
	if cleanRun == "" {
		cleanRun = now.Format("20060102_150405")
	}
	cleanStep := sanitizeSlug(step)
	if cleanStep == "" {
		cleanStep = "step"
	}

	runDir := filepath.Join(v.rootDir, cleanRun)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return domain.EvidenceRecord{}, fmt.Errorf("không tạo được thư mục Evidence %s: %w", runDir, err)
	}

	sum := sha256.Sum256(pngBytes)
	digest := hex.EncodeToString(sum[:])

	imgPath := filepath.Join(runDir, cleanStep+".png")
	metaPath := filepath.Join(runDir, cleanStep+".meta.json")

	if err := writeAtomic0600(imgPath, pngBytes); err != nil {
		return domain.EvidenceRecord{}, fmt.Errorf("ghi ảnh Evidence lỗi: %w", err)
	}

	meta.RunID = cleanRun
	meta.Step = cleanStep
	meta.WindowTitle = v.redactHelper.RedactString(meta.WindowTitle)
	meta.VerifiedDetail = v.redactHelper.RedactString(meta.VerifiedDetail)
	meta.SHA256 = digest
	meta.ImagePath = imgPath
	meta.MetaPath = metaPath

	rawMeta, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return domain.EvidenceRecord{}, err
	}
	if err := writeAtomic0600(metaPath, append(rawMeta, '\n')); err != nil {
		return domain.EvidenceRecord{}, fmt.Errorf("ghi metadata Evidence lỗi: %w", err)
	}

	v.pruneLocked()
	return meta, nil
}

// ListRecent returns up to limit most recent EvidenceRecords sorted newest-first.
func (v *EvidenceVault) ListRecent(limit int) ([]domain.EvidenceRecord, error) {
	if limit <= 0 {
		limit = 40
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	entries, err := os.ReadDir(v.rootDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []domain.EvidenceRecord{}, nil
		}
		return nil, err
	}

	var records []domain.EvidenceRecord
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runDir := filepath.Join(v.rootDir, entry.Name())
		files, readErr := os.ReadDir(runDir)
		if readErr != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".meta.json") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(runDir, f.Name()))
			if err != nil {
				continue
			}
			var rec domain.EvidenceRecord
			if json.Unmarshal(raw, &rec) == nil {
				records = append(records, rec)
			}
		}
	}

	sort.Slice(records, func(i, j int) bool {
		return records[i].CreatedAt.After(records[j].CreatedAt)
	})
	if len(records) > limit {
		records = records[:limit]
	}
	return records, nil
}

// ReadDataURL safely reads an Evidence PNG strictly inside rootDir (blocking path traversal and symlinks)
// and returns a base64 data URL for UI preview.
func (v *EvidenceVault) ReadDataURL(imagePath string) (string, error) {
	v.mu.Lock()
	root := v.rootDir
	v.mu.Unlock()

	cleanTarget := filepath.Clean(imagePath)
	rel, err := filepath.Rel(root, cleanTarget)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", errors.New("từ chối truy cập: đường dẫn Evidence nằm ngoài kho lưu trữ an toàn")
	}
	if !strings.HasSuffix(strings.ToLower(cleanTarget), ".png") {
		return "", errors.New("định dạng tệp Evidence không hợp lệ")
	}

	// Symlink confinement guard: reject symlinks pointing outside rootDir.
	if info, lErr := os.Lstat(cleanTarget); lErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("từ chối truy cập: tệp Evidence không được là symlink")
	}
	if realRoot, rErr := filepath.EvalSymlinks(root); rErr == nil {
		if realTarget, tErr := filepath.EvalSymlinks(cleanTarget); tErr == nil {
			realRel, relErr := filepath.Rel(realRoot, realTarget)
			if relErr != nil || strings.HasPrefix(realRel, "..") || filepath.IsAbs(realRel) {
				return "", errors.New("từ chối truy cập: đường dẫn thật của Evidence nằm ngoài kho an toàn")
			}
		}
	}

	raw, err := os.ReadFile(cleanTarget)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw), nil
}

func (v *EvidenceVault) pruneLocked() {
	entries, err := os.ReadDir(v.rootDir)
	if err != nil {
		return
	}
	type dirInfo struct {
		name    string
		modTime time.Time
		size    int64
	}
	var dirs []dirInfo
	var totalBytes int64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, statErr := e.Info()
		if statErr != nil {
			continue
		}
		sz := dirSizeBytes(filepath.Join(v.rootDir, e.Name()))
		totalBytes += sz
		dirs = append(dirs, dirInfo{name: e.Name(), modTime: info.ModTime(), size: sz})
	}

	sort.Slice(dirs, func(i, j int) bool {
		if dirs[i].modTime.Equal(dirs[j].modTime) {
			return dirs[i].name > dirs[j].name
		}
		return dirs[i].modTime.After(dirs[j].modTime)
	})

	// 1. Enforce maxRuns cap (keep newest maxRuns directories).
	if len(dirs) > v.maxRuns {
		for _, stale := range dirs[v.maxRuns:] {
			_ = os.RemoveAll(filepath.Join(v.rootDir, stale.name))
			totalBytes -= stale.size
		}
		dirs = dirs[:v.maxRuns]
	}

	// 2. Enforce hard disk-space cap (maxVaultBytes), always keeping at least the single newest run.
	if v.maxVaultBytes > 0 && totalBytes > v.maxVaultBytes && len(dirs) > 1 {
		for len(dirs) > 1 && totalBytes > v.maxVaultBytes {
			oldest := dirs[len(dirs)-1]
			_ = os.RemoveAll(filepath.Join(v.rootDir, oldest.name))
			totalBytes -= oldest.size
			dirs = dirs[:len(dirs)-1]
		}
	}
}

func dirSizeBytes(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func sanitizeSlug(s string) string {
	cleaned := safeSlugRegex.ReplaceAllString(strings.TrimSpace(s), "_")
	cleaned = strings.Trim(cleaned, "._-")
	if len(cleaned) > 80 {
		cleaned = cleaned[:80]
	}
	return cleaned
}

func writeAtomic0600(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
