package logstream

import (
	"io"
	"os"
)

// stderr is where console output goes (journald / docker logs). Tests replace it.
var stderr io.Writer = os.Stderr
