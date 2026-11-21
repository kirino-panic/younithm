package importers

import (
	"bufio"
	"io"
)

func newBufWriter(w io.Writer) *bufio.Writer { return bufio.NewWriterSize(w, 64*1024) }
