package terminal

import "bytes"

// cappedBuffer accumulates output up to limit bytes. Writes past the limit are
// discarded and the truncated flag is set. It satisfies io.Writer so it can be
// used directly as a command's Stdout/Stderr; because Write never returns a
// short count it does not provoke os/exec into reporting an I/O error.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

// Write appends as much of p as fits under the limit, dropping the rest. It
// always reports the full length as written so exec treats the write as
// successful.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	remaining := c.limit - c.buf.Len()
	if remaining <= 0 {
		if len(p) > 0 {
			c.truncated = true
		}
		return len(p), nil
	}
	if len(p) > remaining {
		c.buf.Write(p[:remaining])
		c.truncated = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

// String returns the accumulated (possibly truncated) output.
func (c *cappedBuffer) String() string { return c.buf.String() }
