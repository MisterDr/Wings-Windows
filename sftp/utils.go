package sftp

import (
	"io"
	"os"
)

const (
	// Extends the default SFTP server to return a quota exceeded error to the client.
	//
	// @see https://tools.ietf.org/id/draft-ietf-secsh-filexfer-13.txt
	ErrSSHQuotaExceeded = fxerr(15)
)

type ListerAt []os.FileInfo

// Returns the number of entries copied and an io.EOF error if we made it to the end of the file list.
// Take a look at the pkg/sftp godoc for more information about how this function should work.
func (l ListerAt) ListAt(f []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}

	if n := copy(f, l[offset:]); n < len(f) {
		return n, io.EOF
	} else {
		return n, nil
	}
}

type fxerr uint32

func (e fxerr) Error() string {
	switch e {
	case ErrSSHQuotaExceeded:
		return "Quota Exceeded"
	default:
		return "Failure"
	}
}
