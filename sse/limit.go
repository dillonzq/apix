package sse

import "errors"

// DefaultMaxFrameBytes limits one SSE frame, including comments and line
// separators, to 64 MiB. It does not limit the length of a stream.
const DefaultMaxFrameBytes = 64 << 20

// ErrFrameTooLarge reports a frame exceeding its configured byte limit.
var ErrFrameTooLarge = errors.New("sse: frame exceeds byte limit")

// FrameLimiter bounds frames across arbitrary chunks without buffering them.
// Its zero value uses DefaultMaxFrameBytes. Use one limiter per stream.
type FrameLimiter struct {
	// MaxBytes is fixed before the first Feed. Zero uses the default; a
	// negative value disables the limit for explicitly trusted input.
	MaxBytes int
	bytes    int
	nonempty bool
	cr       bool
	err      error
}

// Feed returns the prefix of chunk within the limit, without copying it.
// After an oversized frame, later calls return the same error and no bytes.
// Callers may process complete frames in the accepted prefix before failing.
func (limit *FrameLimiter) Feed(chunk []byte) ([]byte, error) {
	if limit.err != nil {
		return nil, limit.err
	}
	maximum := limit.MaxBytes
	if maximum < 0 {
		return chunk, nil
	}
	if maximum == 0 {
		maximum = DefaultMaxFrameBytes
	}
	for offset := 0; offset < len(chunk); {
		if limit.cr {
			limit.cr = false
			if chunk[offset] == '\n' {
				if limit.bytes == maximum {
					limit.err = ErrFrameTooLarge
					return chunk[:offset], limit.err
				}
				limit.bytes++
				offset++
			}
			limit.endLine()
			if offset == len(chunk) {
				break
			}
		}
		ending := lineEnd(chunk[offset:])
		count := len(chunk) - offset
		if ending >= 0 {
			count = ending + 1
		}
		accepted := min(count, maximum-limit.bytes)
		limit.bytes += accepted
		offset += accepted
		if accepted < count {
			limit.err = ErrFrameTooLarge
			return chunk[:offset], limit.err
		}
		limit.nonempty = limit.nonempty || ending != 0
		if ending < 0 {
			break
		}
		if chunk[offset-1] == '\r' {
			limit.cr = true
		} else {
			limit.endLine()
		}
	}
	return chunk, nil
}

func (limit *FrameLimiter) endLine() {
	if !limit.nonempty {
		limit.bytes = 0
	}
	limit.nonempty = false
}

// Err returns the terminal limit error, or nil.
func (limit *FrameLimiter) Err() error { return limit.err }
