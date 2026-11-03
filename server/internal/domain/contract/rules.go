package contract

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

const (
	// MaxUploadBytes is the maximum size of an uploadable PDF.
	MaxUploadBytes = 10 * 1024 * 1024

	// StaleMessage is the message shown to the user when a lost analysis is changed to failed.
	StaleMessage = "분석이 중단됐어요. 다시 올려 주세요"

	DefaultPageSize = 20
	MaxPageSize     = 50

	int32Max = 2_147_483_647 // upper limit of an integer column
)

var (
	ErrFileRequired = errors.New("file이 필요합니다")
	ErrNotPDF       = errors.New("PDF 파일만 올릴 수 있어요")
	ErrBadSize      = errors.New("파일 크기는 10MB 이하여야 해요")
)

// ClampInt32 clamps a value to between 0 and the int32 maximum.
func ClampInt32(n int64) int32 {
	if n < 0 {
		return 0
	}
	if n > int32Max {
		return int32Max
	}
	return int32(n)
}

// ClampOptional returns nil if there is no value.
func ClampOptional(n *int64) *int32 {
	if n == nil {
		return nil
	}
	v := ClampInt32(*n)
	return &v
}

// ValidateUpload checks that the uploaded file satisfies the rules.
// PDF detection: the MIME type must be application/pdf or the file name must end with .pdf.
func ValidateUpload(fileName, contentType string, size int64) error {
	isPDF := contentType == "application/pdf" || strings.HasSuffix(strings.ToLower(fileName), ".pdf")
	if !isPDF {
		return ErrNotPDF
	}
	if size <= 0 || size > MaxUploadBytes {
		return ErrBadSize
	}
	return nil
}

// NormalizePage adjusts the page and limit strings received from the query.
//   - page: floored to an integer; 1 if not a number or 0; 1 if negative
//   - limit: 20 if not a number or 0; capped at 50; 1 if negative
func NormalizePage(pageRaw, limitRaw string) (page, limit int) {
	page = int(math.Max(1, orDefault(floorNumber(pageRaw), 1)))
	limit = int(math.Min(MaxPageSize, math.Max(1, orDefault(floorNumber(limitRaw), DefaultPageSize))))
	return page, limit
}

// floorNumber parses a string as a number and floors it. It returns 0 if it cannot be parsed.
func floorNumber(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return math.Floor(f)
}

// orDefault uses the default if the value is 0 (missing or invalid).
func orDefault(v, def float64) float64 {
	if v == 0 {
		return def
	}
	return v
}
