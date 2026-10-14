package deploy

import (
	"io"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const LogLimit = 64 << 10

// Sanitize removes terminal controls, including ESC and C1. tview color markup is disabled by callers.
func Sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
}

// LogBuffer is a concurrency-safe bounded UTF-8 tail. It never retains raw terminal controls.
type LogBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *LogBuffer) Write(p []byte) (int, error) {
	clean := []byte(Sanitize(string(p)))
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(clean) >= LogLimit {
		clean = clean[len(clean)-LogLimit:]
		for len(clean) > 0 && !utf8.RuneStart(clean[0]) {
			clean = clean[1:]
		}
		b.data = append(b.data[:0], clean...)
		return len(p), nil
	}
	excess := len(b.data) + len(clean) - LogLimit
	if excess > 0 {
		for excess < len(b.data) && !utf8.RuneStart(b.data[excess]) {
			excess++
		}
		copy(b.data, b.data[excess:])
		b.data = b.data[:len(b.data)-excess]
	}
	b.data = append(b.data, clean...)
	return len(p), nil
}
func (b *LogBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.data) }

type SafeWriter struct{ Writer io.Writer }

func (w SafeWriter) Write(p []byte) (int, error) {
	_, err := io.WriteString(w.Writer, Sanitize(string(p)))
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
